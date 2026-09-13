"""Tests for the hook's interception decision. Run: python3 -m unittest discover tests"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

from common import Config, glob_to_regex  # noqa: E402
from ollama_client import Health, model_available  # noqa: E402
from shunt_hook import decide, parse_bash_read  # noqa: E402

HEALTHY = lambda: Health(ok=True)  # noqa: E731
UNHEALTHY = lambda: Health(ok=False, reason="down")  # noqa: E731


def must_not_check_health():
    raise AssertionError("health check should not run for this decision")


class DecisionTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.env = {k: os.environ.get(k) for k in ("CLAUDE_PROJECT_DIR", "XDG_STATE_HOME", "XDG_CONFIG_HOME")}
        os.environ["CLAUDE_PROJECT_DIR"] = str(self.dir)
        os.environ["XDG_STATE_HOME"] = str(self.dir / "state")
        os.environ["XDG_CONFIG_HOME"] = str(self.dir / "config")
        self.cfg = Config(min_lines=350, min_bytes=32768)

    def tearDown(self):
        for key, value in self.env.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        self.tmp.cleanup()

    def make(self, name: str, lines: int = 0, content: bytes | None = None) -> Path:
        path = self.dir / name
        path.parent.mkdir(parents=True, exist_ok=True)
        if content is None:
            content = "".join(f"line {i}\n" for i in range(lines)).encode()
        path.write_bytes(content)
        return path

    def read(self, path: Path, health=HEALTHY, **tool_input):
        event = {"tool_name": "Read", "tool_input": {"file_path": str(path), **tool_input}, "cwd": str(self.dir)}
        return decide(event, self.cfg, health)

    def bash(self, command: str, health=HEALTHY):
        event = {"tool_name": "Bash", "tool_input": {"command": command}, "cwd": str(self.dir)}
        return decide(event, self.cfg, health)

    # Read rules, in evaluation order

    def test_disabled(self):
        self.cfg.enabled = False
        d = self.read(self.make("big.py", 1000), health=must_not_check_health)
        self.assertEqual((d.allow, d.rule), (True, "disabled"))

    def test_offset_or_limit_allows(self):
        path = self.make("big.py", 1000)
        self.assertEqual(self.read(path, must_not_check_health, offset=1, limit=1000).rule, "range")
        self.assertEqual(self.read(path, must_not_check_health, limit=50).rule, "range")

    def test_offset_without_limit_allows(self):
        d = self.read(self.make("big.py", 1000), must_not_check_health, offset=10)
        self.assertEqual((d.allow, d.rule), (True, "range"))

    def test_native_types_allowed(self):
        for name in ("pic.png", "doc.pdf", "nb.ipynb"):
            d = self.read(self.make(name, 1000), must_not_check_health)
            self.assertEqual((d.allow, d.rule), (True, "native-type"), name)

    def test_missing_file_allowed(self):
        d = self.read(self.dir / "nope.py", must_not_check_health)
        self.assertEqual((d.allow, d.rule), (True, "not-found"))

    def test_binary_allowed(self):
        d = self.read(self.make("blob.bin", content=b"\0" * 100_000), must_not_check_health)
        self.assertEqual((d.allow, d.rule), (True, "binary"))

    def test_excluded_paths_allowed(self):
        for name in ("CLAUDE.md", "sub/SKILL.md", ".claude/notes.md", ".env.local", "Cargo.lock"):
            d = self.read(self.make(name, 1000), must_not_check_health)
            self.assertEqual((d.allow, d.rule), (True, "excluded"), name)

    def test_below_threshold_allowed(self):
        d = self.read(self.make("small.py", 349), must_not_check_health)
        self.assertEqual((d.allow, d.rule), (True, "below-threshold"))

    def test_exactly_threshold_denied(self):
        d = self.read(self.make("edge.py", 350))
        self.assertEqual((d.allow, d.rule), (False, "threshold"))

    def test_long_single_line_denied_by_bytes(self):
        d = self.read(self.make("app.min.js", content=b"x" * 40_000))
        self.assertFalse(d.allow)
        self.assertEqual(d.info.lines, 1)

    def test_ollama_unavailable_fails_open(self):
        d = self.read(self.make("big.py", 1000), health=UNHEALTHY)
        self.assertEqual((d.allow, d.rule), (True, "ollama-unavailable"))

    def test_deny_reason_contains_command_and_bypass(self):
        d = self.read(self.make("src/big.py", 1000))
        self.assertIn("shunt.py", d.reason)
        self.assertIn("read src/big.py --question", d.reason)
        self.assertIn("limit=1000", d.reason)

    def test_oversized_file_suggests_grep(self):
        self.cfg.max_file_bytes = 50_000
        d = self.read(self.make("huge.log", 10_000))
        self.assertFalse(d.allow)
        self.assertIn("Grep", d.reason)
        self.assertNotIn("shunt.py", d.reason)

    def test_relative_path_resolved_against_cwd(self):
        self.make("rel.py", 1000)
        d = self.read(Path("rel.py"))
        self.assertFalse(d.allow)

    def test_other_tools_ignored(self):
        event = {"tool_name": "Grep", "tool_input": {"pattern": "x"}, "cwd": str(self.dir)}
        self.assertIsNone(decide(event, self.cfg, must_not_check_health))

    # Bash rules

    def test_bash_cat_denied(self):
        self.make("big.py", 1000)
        self.assertFalse(self.bash("cat big.py").allow)

    def test_bash_head_small_allowed(self):
        self.make("big.py", 1000)
        d = self.bash("head -n 100 big.py", must_not_check_health)
        self.assertEqual((d.allow, d.rule), (True, "range"))

    def test_bash_head_large_denied(self):
        self.make("big.py", 1000)
        self.assertFalse(self.bash("head -n 1000 big.py").allow)

    def test_bash_pipeline_ignored(self):
        self.make("big.py", 1000)
        self.assertIsNone(self.bash("cat big.py | grep foo", must_not_check_health))

    def test_bash_unrelated_command_ignored(self):
        self.assertIsNone(self.bash("ls -la", must_not_check_health))

    def test_bash_disabled_by_config(self):
        self.make("big.py", 1000)
        self.cfg.intercept_bash = False
        self.assertIsNone(self.bash("cat big.py", must_not_check_health))


class ParseBashReadTest(unittest.TestCase):
    def test_forms(self):
        cases = {
            "cat a.py": ("a.py", None, None),
            "cat -n a.py": ("a.py", None, None),
            "head a.py": ("a.py", 10, None),
            "head -n 20 a.py": ("a.py", 20, None),
            "head -n20 a.py": ("a.py", 20, None),
            "head -20 a.py": ("a.py", 20, None),
            "head --lines=20 a.py": ("a.py", 20, None),
            "head -c 100 a.py": ("a.py", None, 100),
            "tail -n +5 a.py": ("a.py", None, None),
            "head -n -5 a.py": ("a.py", None, None),
            "less a.py": ("a.py", None, None),
            "cat 'my file.py'": ("my file.py", None, None),
        }
        for command, expected in cases.items():
            parsed = parse_bash_read(command)
            self.assertIsNotNone(parsed, command)
            self.assertEqual((parsed.path, parsed.max_lines, parsed.max_bytes), expected, command)

    def test_rejected(self):
        for command in (
            "cat a.py b.py",
            "cat a.py > b.py",
            "cd x && cat a.py",
            "tail -f app.log",
            "head -x a.py",
            "cat $FILE",
            "cat \"unterminated",
            "grep foo a.py",
            "",
        ):
            self.assertIsNone(parse_bash_read(command), command)


class HelpersTest(unittest.TestCase):
    def test_glob(self):
        self.assertTrue(glob_to_regex("**/.claude/**").match("/p/.claude/x/y.md"))
        self.assertTrue(glob_to_regex("**/CLAUDE.md").match("CLAUDE.md"))
        self.assertFalse(glob_to_regex("*.lock").match("a/b.lock"))

    def test_model_available(self):
        self.assertTrue(model_available("mistral", ["mistral:latest"]))
        self.assertTrue(model_available("qwen3:8b", ["qwen3:8b"]))
        self.assertFalse(model_available("qwen3:4b", ["qwen3:8b"]))


class HookProcessTest(unittest.TestCase):
    """Run the hook as Claude Code does: JSON on stdin, JSON on stdout."""

    def run_hook(self, stdin: str, *args: str) -> subprocess.CompletedProcess:
        env = dict(os.environ, OLLAMA_HOST="http://127.0.0.1:9", XDG_STATE_HOME=tempfile.gettempdir() + "/local-shunt-test")
        return subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "shunt_hook.py"), *args],
            input=stdin, capture_output=True, text=True, env=env, timeout=10,
        )

    def test_invalid_stdin_fails_open(self):
        result = self.run_hook("not json", "pre-tool-use")
        self.assertEqual((result.returncode, result.stdout), (0, ""))

    def test_unreachable_ollama_fails_open(self):
        with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as f:
            f.write("x = 1\n" * 1000)
        try:
            event = {"tool_name": "Read", "tool_input": {"file_path": f.name}, "cwd": "/"}
            result = self.run_hook(json.dumps(event), "pre-tool-use")
            self.assertEqual((result.returncode, result.stdout), (0, ""))
        finally:
            os.unlink(f.name)


if __name__ == "__main__":
    unittest.main()
