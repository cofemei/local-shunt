"""Tests for measurement: transcript parsing, worker log join, benchmark runs and reports."""

from __future__ import annotations

import json
import os
import stat
import subprocess
import sys
import textwrap
import unittest
from pathlib import Path

from helpers import SCRIPTS, IsolatedTestCase
from test_worker import WorkerTestCase

import measure
from common import log_event
from measure import MeasureError, find_transcript, format_report, load_tasks, parse_transcript, summarize, worker_usage
from shunt import UsageError, run_bench_report, run_read, run_usage

DENY_TEXT = "local-shunt: big.py is large (900 lines, 40,000 bytes; threshold 350 lines or 32,768 bytes)."


def usage(input_tokens=0, cache_write=0, cache_read=0, output=0, one_hour=0) -> dict:
    return {
        "input_tokens": input_tokens,
        "cache_creation_input_tokens": cache_write,
        "cache_read_input_tokens": cache_read,
        "output_tokens": output,
        "cache_creation": {"ephemeral_1h_input_tokens": one_hour, "ephemeral_5m_input_tokens": cache_write - one_hour},
    }


def assistant(msg_id: str, blocks: list[dict], u: dict | None, model: str = "claude-test") -> dict:
    return {"type": "assistant", "message": {"id": msg_id, "model": model, "content": blocks, "usage": u}}


def tool_use(tool_id: str, name: str, **tool_input) -> dict:
    return {"type": "tool_use", "id": tool_id, "name": name, "input": tool_input}


def tool_result(tool_id: str, text, is_error: bool = False) -> dict:
    block = {"type": "tool_result", "tool_use_id": tool_id, "content": text}
    if is_error:
        block["is_error"] = True
    return {"type": "user", "message": {"role": "user", "content": [block]}}


def write_jsonl(path: Path, entries: list) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join((e if isinstance(e, str) else json.dumps(e)) + "\n" for e in entries))
    return path


class TranscriptTest(IsolatedTestCase):
    def setUp(self):
        super().setUp()
        os.environ["CLAUDE_CONFIG_DIR"] = str(self.dir / "claude")

    def transcript(self, session: str, entries: list) -> Path:
        return write_jsonl(self.dir / "claude" / "projects" / "-proj" / f"{session}.jsonl", entries)

    def test_split_entries_count_one_request(self):
        path = self.transcript("s1", [
            {"type": "user", "message": {"role": "user", "content": "hi"}},
            assistant("m1", [{"type": "thinking", "thinking": ""}], usage(2, 1000, 5000, 50, one_hour=1000)),
            assistant("m1", [{"type": "text", "text": "ok"}], usage(2, 1000, 5000, 50, one_hour=1000)),
            assistant("m2", [{"type": "text", "text": "done"}], usage(1, 400, 6000, 20)),
            assistant("m3", [{"type": "text", "text": "API error"}], usage(0), model="<synthetic>"),
            "not json",
        ])
        u = parse_transcript(path)
        self.assertEqual(u.requests, 2)
        self.assertEqual(u.models, {"claude-test": 2})
        self.assertEqual((u.input_tokens, u.cache_creation_tokens, u.cache_read_tokens, u.output_tokens),
                         (3, 1400, 11000, 70))
        self.assertEqual(u.total_input_tokens, 12403)
        # 3 + 2.0 * 1000 + 1.25 * 400 + 0.1 * 11000
        self.assertEqual(u.weighted_input_tokens, 3603)

    def test_tool_categories_results_and_denials(self):
        path = self.transcript("s1", [
            assistant("m1", [
                tool_use("t1", "Read", file_path="/p/big.py"),
                tool_use("t2", "Read", file_path="/p/big.py", offset=10, limit=20),
            ], usage(1)),
            tool_result("t1", DENY_TEXT, is_error=True),
            tool_result("t2", "x" * 300),
            assistant("m2", [
                tool_use("t3", "Bash", command="python3 '/a b/scripts/shunt.py' read big.py -q 'where?'"),
                tool_use("t4", "mcp__plugin_local-shunt_local-shunt__shunt_read", files=["big.py"], question="q"),
                tool_use("t5", "Grep", pattern="x"),
            ], usage(1)),
            tool_result("t3", [{"type": "text", "text": "y" * 60}]),
            tool_result("t4", "z" * 90),
            tool_result("t5", DENY_TEXT),  # a denial text that is not an error result does not count
        ])
        u = parse_transcript(path)
        self.assertEqual(u.tool_calls, {
            "Read": 1, "Read (range)": 1, "shunt read (Bash)": 1, "shunt_read (MCP)": 1, "Grep": 1,
        })
        self.assertEqual(u.denials, 1)
        self.assertEqual(u.delegations, 2)
        self.assertEqual(u.tool_result_tokens["Read (range)"], 100)
        self.assertEqual(u.tool_result_tokens["shunt read (Bash)"], 20)
        self.assertEqual(u.read_result_tokens, u.tool_result_tokens["Read"] + 100 + 20 + 30)

    def test_subagent_transcripts_are_included(self):
        path = self.transcript("s1", [assistant("m1", [], usage(10, output=5))])
        write_jsonl(path.with_suffix("") / "subagents" / "agent-1.jsonl", [
            assistant("m9", [tool_use("t1", "Read", file_path="/p/a.py")], usage(20, output=7), model="claude-small"),
            tool_result("t1", "abc"),
        ])
        u = parse_transcript(path)
        self.assertEqual(u.transcripts, 2)
        self.assertEqual(u.requests, 2)
        self.assertEqual(u.input_tokens, 30)
        self.assertEqual(u.models, {"claude-test": 1, "claude-small": 1})
        self.assertEqual(u.tool_calls, {"Read": 1})

    def test_find_transcript(self):
        path = self.transcript("abc-123", [])
        self.assertEqual(find_transcript("abc-123"), path)
        self.assertEqual(find_transcript(str(path)), path)
        self.assertIsNone(find_transcript("missing"))
        self.assertIsNone(find_transcript("../../etc"))
        self.assertIsNone(find_transcript(str(self.dir / "missing.jsonl")))


class WorkerJoinTest(WorkerTestCase):
    def test_worker_records_carry_the_claude_code_session_id(self):
        os.environ["CLAUDE_CODE_SESSION_ID"] = "sess-1"
        self.make("a.py", 400)
        run_read(self.cfg(), ["a.py"], "q")
        self.assertEqual(self.log_records()[-1]["session_id"], "sess-1")

    def test_worker_usage_filters_by_session(self):
        log_event({"session_id": "s1", "event": "read", "outcome": "ok", "est_input_tokens": 1000,
                   "est_output_tokens": 50, "prompt_tokens": 1100, "completion_tokens": 40, "cost_usd": 0.01})
        log_event({"session_id": "s1", "event": "write", "outcome": "ok", "prompt_tokens": 10, "cost_usd": None})
        log_event({"session_id": "s1", "event": "read", "outcome": "error:LLMError"})
        log_event({"session_id": "s1", "event": "hook", "outcome": "denied"})
        log_event({"session_id": "s2", "event": "read", "outcome": "ok", "est_input_tokens": 999})
        u = worker_usage("s1")
        self.assertEqual((u.reads, u.writes, u.errors), (1, 1, 1))
        self.assertEqual((u.est_source_tokens, u.est_summary_tokens, u.prompt_tokens), (1000, 50, 1110))
        self.assertAlmostEqual(u.cost_usd, 0.01)

    def test_run_usage_text_and_json(self):
        os.environ["CLAUDE_CONFIG_DIR"] = str(self.dir / "claude")
        write_jsonl(self.dir / "claude" / "projects" / "-p" / "s1.jsonl", [assistant("m1", [], usage(5, 10, 20, 3))])
        log_event({"session_id": "s1", "event": "read", "outcome": "ok", "est_input_tokens": 1000})
        text = run_usage(["s1"])
        self.assertIn("Session s1", text)
        self.assertIn("delegated reads", text)
        data = json.loads(run_usage(["s1"], as_json=True))
        self.assertEqual(data["claude"]["total_input_tokens"], 35)
        self.assertEqual(data["worker"]["est_source_tokens"], 1000)
        os.environ["CLAUDE_CODE_SESSION_ID"] = "s1"
        self.assertIn("Session s1", run_usage([]))
        with self.assertRaisesRegex(UsageError, "transcript not found"):
            run_usage(["nope"])


class TaskFileTest(IsolatedTestCase):
    def write_tasks(self, data) -> Path:
        path = self.dir / "bench" / "tasks.json"
        path.parent.mkdir(exist_ok=True)
        path.write_text(json.dumps(data))
        return path

    def test_defaults_and_relative_cwd(self):
        path = self.write_tasks({
            "defaults": {"cwd": "..", "model": "haiku", "timeout": 60},
            "tasks": [{"id": "a", "prompt": "p", "expect": "x"}, {"id": "b", "prompt": "p", "model": "sonnet"}],
        })
        a, b = load_tasks(path)
        self.assertEqual(a.cwd, self.dir.resolve())
        self.assertEqual((a.model, a.timeout, a.expect), ("haiku", 60, ["x"]))
        self.assertEqual(b.model, "sonnet")
        self.assertEqual(a.allowed_tools, measure.DEFAULT_ALLOWED_TOOLS)

    def test_invalid_task_files(self):
        cases = [
            ([], "non-empty"),
            ([{"prompt": "p"}], "needs an id"),
            ([{"id": "a", "prompt": "p"}, {"id": "a", "prompt": "q"}], "duplicate"),
            ([{"id": "a", "prompt": " "}], "needs a prompt"),
            ([{"id": "a", "prompt": "p", "cwd": "missing"}], "not a directory"),
            ([{"id": "a", "prompt": "p", "expect": ["("]}], "invalid expect"),
        ]
        for tasks, message in cases:
            with self.subTest(message), self.assertRaisesRegex(MeasureError, message):
                load_tasks(self.write_tasks(tasks))
        with self.assertRaisesRegex(MeasureError, "invalid JSON"):
            bad = self.dir / "bad.json"
            bad.write_text("{")
            load_tasks(bad)

    def test_example_task_file_loads(self):
        tasks = load_tasks(SCRIPTS.parent / "bench" / "example-tasks.json")
        self.assertGreaterEqual(len(tasks), 5)
        self.assertTrue(all(t.cwd == SCRIPTS.parent for t in tasks))

    def test_off_mode_disables_the_plugin_and_its_tools(self):
        task = load_tasks(self.write_tasks([{"id": "a", "prompt": "p", "model": "haiku"}]))[0]
        os.environ["LOCAL_SHUNT_DISABLE"] = "1"
        on_cmd, on_env = measure.claude_command(task, "on", "sid", "claude", SCRIPTS.parent)
        off_cmd, off_env = measure.claude_command(task, "off", "sid", "claude", SCRIPTS.parent)
        self.assertNotIn("LOCAL_SHUNT_DISABLE", on_env)
        self.assertEqual(off_env["LOCAL_SHUNT_DISABLE"], "1")
        for cmd in (on_cmd, off_cmd):
            self.assertIn("--plugin-dir", cmd)
            self.assertEqual(cmd[cmd.index("--session-id") + 1], "sid")
            self.assertEqual(cmd[cmd.index("--model") + 1], "haiku")
        self.assertNotIn("--disallowedTools", on_cmd)
        allowed_off = off_cmd[off_cmd.index("--allowedTools") + 1:]
        self.assertFalse(set(measure.WORKER_TOOLS) & set(allowed_off))
        self.assertIn(measure.WORKER_TOOLS[0], off_cmd[off_cmd.index("--disallowedTools") + 1:])


FAKE_CLAUDE = textwrap.dedent("""\
    import json, os, sys
    from pathlib import Path
    args = sys.argv[1:]
    prompt = sys.stdin.read()
    session = args[args.index("--session-id") + 1]
    off = os.environ.get("LOCAL_SHUNT_DISABLE") == "1"
    if "FAIL" in prompt:
        print(json.dumps({"type": "result", "is_error": True, "result": "boom", "num_turns": 1}))
        sys.exit(1)
    project = Path(os.environ["CLAUDE_CONFIG_DIR"]) / "projects" / "-fake"
    project.mkdir(parents=True, exist_ok=True)
    def usage(total):
        return {"input_tokens": 0, "cache_creation_input_tokens": 1000, "cache_read_input_tokens": total - 1000,
                "output_tokens": 100}
    if off:
        entries = [
            {"type": "assistant", "message": {"id": "a", "model": "m", "usage": usage(10000),
             "content": [{"type": "tool_use", "id": "t1", "name": "Read", "input": {"file_path": "big.py"}}]}},
            {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "t1", "content": "x" * 30000}]}},
            {"type": "assistant", "message": {"id": "b", "model": "m", "usage": usage(20000), "content": []}},
        ]
        answer = "The answer is 50."
    else:
        entries = [
            {"type": "assistant", "message": {"id": "a", "model": "m", "usage": usage(8000),
             "content": [{"type": "tool_use", "id": "t1", "name": "mcp__plugin_local-shunt_local-shunt__shunt_read",
                          "input": {"files": ["big.py"], "question": "q"}}]}},
            {"type": "user", "message": {"content": [{"type": "tool_result", "tool_use_id": "t1", "content": "y" * 600}]}},
            {"type": "assistant", "message": {"id": "b", "model": "m", "usage": usage(8200), "content": []}},
        ]
        answer = "It is 50." if "WRONG_ON" not in prompt else "No idea."
    with (project / f"{session}.jsonl").open("w") as f:
        f.write("".join(json.dumps(e) + "\\n" for e in entries))
    print(json.dumps({"type": "result", "is_error": False, "result": answer, "num_turns": 2,
                      "total_cost_usd": 0.01 if off else 0.006, "usage": usage(1)}))
""")


class BenchTest(IsolatedTestCase):
    def setUp(self):
        super().setUp()
        os.environ["CLAUDE_CONFIG_DIR"] = str(self.dir / "claude")
        self.claude = self.dir / "fake-claude"
        self.claude.write_text(f"#!{sys.executable}\n{FAKE_CLAUDE}")
        self.claude.chmod(self.claude.stat().st_mode | stat.S_IEXEC)
        self.tasks = self.dir / "tasks.json"
        self.tasks.write_text(json.dumps({"tasks": [
            {"id": "good", "prompt": "question", "expect": ["\\b50\\b"]},
            {"id": "worse", "prompt": "question WRONG_ON", "expect": ["50"]},
        ]}))

    def test_runs_both_modes_and_reports_savings(self):
        messages = []
        out = measure.run_bench(self.tasks, runs=2, claude=str(self.claude), progress=messages.append)
        records = measure.load_results([out])
        self.assertEqual(len(records), 8)
        self.assertEqual(len(messages), 8)
        self.assertEqual(out.parent, self.dir / "state" / "local-shunt" / "bench")
        # The order of modes alternates between runs.
        good = [r["mode"] for r in records if r["task"] == "good"]
        self.assertEqual(good, ["on", "off", "off", "on"])

        on = next(r for r in records if r["task"] == "good" and r["mode"] == "on")
        self.assertTrue(on["passed"])
        self.assertEqual(on["claude"]["total_input_tokens"], 16200)
        self.assertEqual(on["claude"]["delegations"], 1)
        self.assertEqual(on["cost_usd"], 0.006)

        summary = summarize(records)["good"]
        self.assertAlmostEqual(summary["saving"]["input"], 1 - 16200 / 30000)
        self.assertAlmostEqual(summary["saving"]["read"], 1 - 200 / 10000)
        self.assertEqual((summary["on"]["passed"], summary["on"]["checked"]), (2, 2))

        report = format_report(records)
        self.assertIn("Task good", report)
        self.assertIn("46.0%", report)
        self.assertIn("98.0%", report)
        self.assertRegex(report, r"tasks saving ≥70% of file-read tokens +2 / 2")
        self.assertIn("worse: fewer answers passed with local-shunt on (0 vs 2)", report)

    def test_failed_runs_are_recorded_and_excluded(self):
        self.tasks.write_text(json.dumps([{"id": "bad", "prompt": "FAIL", "expect": "x"}]))
        out = measure.run_bench(self.tasks, runs=1, out=self.dir / "r.jsonl", claude=str(self.claude))
        records = measure.load_results([out])
        self.assertEqual({r["mode"] for r in records}, {"on", "off"})
        self.assertTrue(all(r["error"] == "boom" and r["passed"] is False for r in records))
        report = format_report(records)
        self.assertIn("2 runs (2 failed)", report)
        self.assertIn("no completed run in one mode", report)

    def test_unknown_task_and_missing_claude(self):
        with self.assertRaisesRegex(MeasureError, "unknown task id: nope"):
            measure.run_bench(self.tasks, only=["nope"], claude=str(self.claude))
        with self.assertRaisesRegex(MeasureError, "cannot run"):
            measure.run_bench(self.tasks, runs=1, only=["good"], claude=str(self.dir / "missing"))

    def test_contaminated_baseline_warning(self):
        records = [
            {"task": "t", "mode": mode, "claude": {"total_input_tokens": 10, "weighted_input_tokens": 10,
             "output_tokens": 1, "read_result_tokens": 5, "delegations": 1, "denials": 0}}
            for mode in ("on", "off")
        ]
        self.assertIn("baseline is contaminated", format_report(records))

    def test_cli_bench_and_report(self):
        out = self.dir / "results.jsonl"
        result = subprocess.run(
            [sys.executable, str(SCRIPTS / "shunt.py"), "bench", str(self.tasks), "--runs", "1",
             "--task", "good", "--out", str(out), "--claude", str(self.claude)],
            capture_output=True, text=True, cwd=self.dir, env=self.isolated_env(), timeout=60,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Task good", result.stdout)
        self.assertIn(f"Results: {out.resolve()}", result.stdout)  # macOS: /var is a symlink
        self.assertIn("[1/2] good", result.stderr)
        self.assertIn("Task good", run_bench_report([str(out)]))
        with self.assertRaisesRegex(UsageError, "missing.jsonl"):
            run_bench_report([str(self.dir / "missing.jsonl")])
        self.assertEqual(format_report([]), "No benchmark records found.")


if __name__ == "__main__":
    unittest.main()
