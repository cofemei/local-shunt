"""Measure Claude's own token usage.

Two sources:

- Session transcripts. Claude Code writes every API response, with its `usage`, to
  ~/.claude/projects/<project>/<session>.jsonl, so these counts are Claude's billed
  tokens, not estimates.
- A/B benchmark runs. The same prompt goes through `claude -p` with local-shunt
  enabled and disabled. Only a paired run shows what a task costs without the plugin.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import statistics
import subprocess
import time
import uuid
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path

from common import PLUGIN_ROOT, SHUNT_SCRIPT, estimate_tokens, state_dir

MCP_PREFIX = "mcp__plugin_local-shunt_local-shunt__"
WORKER_TOOLS = [MCP_PREFIX + "shunt_read", MCP_PREFIX + "shunt_write"]
DEFAULT_ALLOWED_TOOLS = ["Read", "Grep", "Glob", f"Bash(python3 {SHUNT_SCRIPT} read:*)", MCP_PREFIX + "shunt_read"]
DEFAULT_TIMEOUT = 900

SHUNT_BASH = re.compile(r"shunt\.py['\"]?\s+read\b")
SHUNT_BASH_WRITE = re.compile(r"shunt\.py['\"]?\s+write\b")
DENIAL = re.compile(r"local-shunt: .+ is large \(")
VERIFY_TIMEOUT = 300
VERIFY_OUTPUT_CHARS = 1000

# Prompt-caching price multipliers relative to uncached input tokens.
CACHE_WRITE_5M = 1.25
CACHE_WRITE_1H = 2.0
CACHE_READ = 0.1

READ_CATEGORIES = ("Read", "Read (range)", "shunt_read (MCP)", "shunt read (Bash)")
DELEGATION_CATEGORIES = ("shunt_read (MCP)", "shunt read (Bash)")
# Names usable in a task's expect_tools and forbid_tools besides the tool categories themselves.
TOOL_GROUPS = {
    "delegate-read": DELEGATION_CATEGORIES,
    "delegate-write": ("shunt_write (MCP)", "shunt write (Bash)"),
}
TARGET_SAVING = 0.70


class MeasureError(ValueError):
    pass


# ---------------------------------------------------------------- transcripts


@dataclass
class ClaudeUsage:
    session_id: str = ""
    transcripts: int = 0  # main transcript plus subagent transcripts
    requests: int = 0
    input_tokens: int = 0  # uncached input
    cache_creation_tokens: int = 0
    cache_creation_1h_tokens: int = 0
    cache_read_tokens: int = 0
    output_tokens: int = 0
    models: dict[str, int] = field(default_factory=dict)  # API requests per model
    tool_calls: dict[str, int] = field(default_factory=dict)
    tool_result_tokens: dict[str, int] = field(default_factory=dict)  # estimated from the result text
    denials: int = 0

    @property
    def total_input_tokens(self) -> int:
        return self.input_tokens + self.cache_creation_tokens + self.cache_read_tokens

    @property
    def weighted_input_tokens(self) -> int:
        """Input tokens weighted by their price relative to uncached input."""
        five_min = self.cache_creation_tokens - self.cache_creation_1h_tokens
        return round(
            self.input_tokens
            + CACHE_WRITE_5M * five_min
            + CACHE_WRITE_1H * self.cache_creation_1h_tokens
            + CACHE_READ * self.cache_read_tokens
        )

    @property
    def read_result_tokens(self) -> int:
        return sum(self.tool_result_tokens.get(c, 0) for c in READ_CATEGORIES)

    @property
    def delegations(self) -> int:
        return sum(self.tool_calls.get(c, 0) for c in DELEGATION_CATEGORIES)

    def to_dict(self) -> dict:
        data = asdict(self)
        data.update(
            total_input_tokens=self.total_input_tokens,
            weighted_input_tokens=self.weighted_input_tokens,
            read_result_tokens=self.read_result_tokens,
            delegations=self.delegations,
        )
        return data


def claude_home() -> Path:
    return Path(os.environ.get("CLAUDE_CONFIG_DIR") or Path.home() / ".claude")


def find_transcript(ref: str) -> Path | None:
    """Resolve a session ID or a transcript path to the transcript file."""
    path = Path(os.path.expanduser(ref))
    if path.suffix == ".jsonl":
        return path if path.is_file() else None
    if not re.fullmatch(r"[\w-]+", ref):
        return None
    matches = [p for p in (claude_home() / "projects").glob(f"*/{ref}.jsonl") if p.is_file()]
    return max(matches, key=lambda p: p.stat().st_mtime) if matches else None


def _text_of(content) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(
            str(item.get("text", "")) for item in content if isinstance(item, dict) and item.get("type") == "text"
        )
    return ""


def _categorize(name: str, tool_input: dict) -> str:
    if name == "Read":
        ranged = tool_input.get("offset") is not None or tool_input.get("limit") is not None
        return "Read (range)" if ranged else "Read"
    if name == "Bash" and SHUNT_BASH.search(str(tool_input.get("command", ""))):
        return "shunt read (Bash)"
    if name == "Bash" and SHUNT_BASH_WRITE.search(str(tool_input.get("command", ""))):
        return "shunt write (Bash)"
    if name.endswith("__shunt_read"):
        return "shunt_read (MCP)"
    if name.endswith("__shunt_write"):
        return "shunt_write (MCP)"
    return name


def _bump(counter: dict[str, int], key: str, amount: int = 1) -> None:
    counter[key] = counter.get(key, 0) + amount


def parse_transcript(path: Path) -> ClaudeUsage:
    """Sum API usage and tool calls over a transcript and its subagent transcripts."""
    usage = ClaudeUsage(session_id=path.stem)
    subagents = path.with_suffix("") / "subagents"
    files = [path, *sorted(subagents.glob("*.jsonl"))] if subagents.is_dir() else [path]
    for file in files:
        usage.transcripts += 1
        # Claude Code writes one entry per content block, each repeating the response's usage.
        responses: dict[str, tuple[str, dict]] = {}
        tool_categories: dict[str, str] = {}
        seen_results: set[str] = set()
        try:
            with file.open(encoding="utf-8") as f:
                lines = list(f)
        except OSError:
            continue
        for n, line in enumerate(lines):
            try:
                entry = json.loads(line)
            except ValueError:
                continue
            message = entry.get("message") if isinstance(entry, dict) else None
            if not isinstance(message, dict):
                continue
            content = message.get("content")
            blocks = [b for b in content if isinstance(b, dict)] if isinstance(content, list) else []

            if entry.get("type") == "assistant":
                model = str(message.get("model") or "unknown")
                if isinstance(message.get("usage"), dict) and model != "<synthetic>":
                    key = message.get("id") or entry.get("requestId") or f"line-{n}"
                    responses[str(key)] = (model, message["usage"])
                for block in blocks:
                    tool_id = str(block.get("id"))
                    if block.get("type") != "tool_use" or tool_id in tool_categories:
                        continue
                    tool_input = block.get("input") if isinstance(block.get("input"), dict) else {}
                    category = _categorize(str(block.get("name", "")), tool_input)
                    tool_categories[tool_id] = category
                    _bump(usage.tool_calls, category)

            elif entry.get("type") == "user":
                for block in blocks:
                    tool_id = str(block.get("tool_use_id"))
                    if block.get("type") != "tool_result" or tool_id in seen_results:
                        continue
                    seen_results.add(tool_id)
                    text = _text_of(block.get("content"))
                    if block.get("is_error") and DENIAL.search(text):
                        usage.denials += 1
                    _bump(usage.tool_result_tokens, tool_categories.get(tool_id, "unknown"), estimate_tokens(text))

        for model, u in responses.values():
            usage.requests += 1
            _bump(usage.models, model)
            usage.input_tokens += int(u.get("input_tokens") or 0)
            usage.cache_creation_tokens += int(u.get("cache_creation_input_tokens") or 0)
            usage.cache_read_tokens += int(u.get("cache_read_input_tokens") or 0)
            usage.output_tokens += int(u.get("output_tokens") or 0)
            details = u.get("cache_creation")
            if isinstance(details, dict):
                usage.cache_creation_1h_tokens += int(details.get("ephemeral_1h_input_tokens") or 0)
    return usage


# ---------------------------------------------------------------- worker log


@dataclass
class WorkerUsage:
    reads: int = 0
    writes: int = 0
    errors: int = 0
    est_source_tokens: int = 0
    est_summary_tokens: int = 0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    cost_usd: float = 0.0


def read_log() -> list[dict]:
    records = []
    for name in ("log.jsonl.1", "log.jsonl"):
        try:
            with (state_dir() / name).open(encoding="utf-8") as f:
                for line in f:
                    try:
                        record = json.loads(line)
                    except ValueError:
                        continue
                    if isinstance(record, dict):
                        records.append(record)
        except OSError:
            continue
    return records


def worker_usage(session: str, records: list[dict] | None = None) -> WorkerUsage:
    usage = WorkerUsage()
    for r in read_log() if records is None else records:
        if r.get("session_id") != session or r.get("event") not in ("read", "write"):
            continue
        if r.get("outcome") != "ok":
            usage.errors += 1
            continue
        if r["event"] == "read":
            usage.reads += 1
            usage.est_source_tokens += r.get("est_input_tokens") or 0
            usage.est_summary_tokens += r.get("est_output_tokens") or 0
        else:
            usage.writes += 1
        usage.prompt_tokens += r.get("prompt_tokens") or 0
        usage.completion_tokens += r.get("completion_tokens") or 0
        usage.cost_usd += r.get("cost_usd") or 0
    return usage


def format_usage(claude: ClaudeUsage, worker: WorkerUsage) -> str:
    def row(label: str, value, indent: int = 0) -> str:
        text = f"{value:,}" if isinstance(value, int) else str(value)
        return f"{' ' * indent}{label:<{36 - indent}} {text:>14}"

    subagents = claude.transcripts - 1
    lines = [f"Session {claude.session_id}" + (f" (+{subagents} subagent transcripts)" if subagents else ""), ""]
    lines.append(row("Claude API requests", claude.requests))
    lines += [row(model, count, 2) for model, count in sorted(claude.models.items(), key=lambda i: -i[1])]
    lines += [
        row("Input tokens", claude.total_input_tokens),
        row("uncached", claude.input_tokens, 2),
        row("cache write", claude.cache_creation_tokens, 2),
        row("cache read", claude.cache_read_tokens, 2),
        row("Input tokens, price-weighted", claude.weighted_input_tokens),
        row("Output tokens", claude.output_tokens),
        "",
        f"{'Tool calls':<28} {'calls':>7} {'est. result tokens':>20}",
    ]
    for name, count in sorted(claude.tool_calls.items(), key=lambda i: -i[1]):
        lines.append(f"  {name:<26} {count:>7,} {claude.tool_result_tokens.get(name, 0):>20,}")
    lines += [
        row("File-read result tokens (est.)", claude.read_result_tokens),
        row("Hook denials", claude.denials),
        "",
        "local-shunt worker in this session",
        row("delegated reads", worker.reads, 2),
        row("delegated writes", worker.writes, 2),
        row("errors", worker.errors, 2),
        row("est. source tokens", worker.est_source_tokens, 2),
        row("est. summary tokens", worker.est_summary_tokens, 2),
        row("worker prompt tokens", worker.prompt_tokens, 2),
        row("worker completion tokens", worker.completion_tokens, 2),
        row("reported API cost (USD)", f"{worker.cost_usd:.4f}", 2),
        "",
        "Claude's token counts come from the transcript; tool result and source tokens are estimates.",
        "A single session cannot show the saving. Run the same task with local-shunt disabled: shunt.py bench.",
    ]
    return "\n".join(lines)


# ---------------------------------------------------------------- benchmark


@dataclass
class Task:
    id: str
    prompt: str
    cwd: Path
    expect: list[str] = field(default_factory=list)
    model: str | None = None
    allowed_tools: list[str] = field(default_factory=lambda: list(DEFAULT_ALLOWED_TOOLS))
    timeout: int = DEFAULT_TIMEOUT
    expect_tools: list[str] = field(default_factory=list)  # checked in 'on' runs only
    forbid_tools: list[str] = field(default_factory=list)  # checked in 'on' runs only
    isolate: bool = False  # run in a fresh copy of cwd, so edits do not touch the original
    verify: list[str] = field(default_factory=list)  # command run in the working directory after the run

    @property
    def checks_answer(self) -> bool:
        return bool(self.expect or self.verify)


def _string_list(value, name: str, where: str) -> list[str]:
    value = [] if value is None else [value] if isinstance(value, str) else value
    if not isinstance(value, list) or not all(isinstance(v, str) for v in value):
        raise MeasureError(f"{where}: {name} must be a string or a list of strings")
    return value


def tool_check_failures(task: Task, tool_calls: dict[str, int]) -> list[str]:
    """Return the expect_tools and forbid_tools rules a run broke."""
    def used(name: str) -> bool:
        return any(tool_calls.get(c, 0) for c in TOOL_GROUPS.get(name, (name,)))

    failures = [f"did not use {name}" for name in task.expect_tools if not used(name)]
    failures += [f"used {name}" for name in task.forbid_tools if used(name)]
    return failures


def load_tasks(path: Path) -> list[Task]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except OSError as e:
        raise MeasureError(f"{path}: {e.strerror or e}") from None
    except ValueError as e:
        raise MeasureError(f"{path}: invalid JSON ({e})") from None
    defaults: dict = {}
    raw_tasks = data
    if isinstance(data, dict):
        defaults = data.get("defaults") or {}
        raw_tasks = data.get("tasks")
    if not isinstance(raw_tasks, list) or not raw_tasks or not isinstance(defaults, dict):
        raise MeasureError(f'{path}: expected a non-empty "tasks" list')

    tasks: list[Task] = []
    for i, raw in enumerate(raw_tasks, 1):
        if not isinstance(raw, dict):
            raise MeasureError(f"{path}: task {i} must be an object")
        spec = {**defaults, **raw}
        task_id, prompt = spec.get("id"), spec.get("prompt")
        if not isinstance(task_id, str) or not re.fullmatch(r"[\w.-]+", task_id):
            raise MeasureError(f"{path}: task {i} needs an id of letters, digits, '.', '_' or '-'")
        if any(t.id == task_id for t in tasks):
            raise MeasureError(f"{path}: duplicate task id {task_id!r}")
        if not isinstance(prompt, str) or not prompt.strip():
            raise MeasureError(f"{path}: task {task_id} needs a prompt")
        cwd = Path(os.path.expanduser(str(spec.get("cwd") or ".")))
        cwd = (cwd if cwd.is_absolute() else path.parent / cwd).resolve()
        if not cwd.is_dir():
            raise MeasureError(f"{path}: task {task_id}: cwd {cwd} is not a directory")
        where = f"{path}: task {task_id}"
        expect = _string_list(spec.get("expect"), "expect", where)
        tools = _string_list(spec.get("allowed_tools"), "allowed_tools", where) or DEFAULT_ALLOWED_TOOLS
        expect_tools = _string_list(spec.get("expect_tools"), "expect_tools", where)
        forbid_tools = _string_list(spec.get("forbid_tools"), "forbid_tools", where)
        verify = _string_list(spec.get("verify"), "verify", where)
        verify = [arg.replace("{tasks_dir}", str(path.parent.resolve())) for arg in verify]
        isolate = spec.get("isolate", False)
        if not isinstance(isolate, bool):
            raise MeasureError(f"{where}: isolate must be true or false")
        for pattern in expect:
            try:
                re.compile(pattern)
            except re.error as e:
                raise MeasureError(f"{path}: task {task_id}: invalid expect pattern {pattern!r}: {e}") from None
        try:
            timeout = int(spec.get("timeout") or DEFAULT_TIMEOUT)
        except (TypeError, ValueError):
            raise MeasureError(f"{path}: task {task_id}: timeout must be a number of seconds") from None
        model = spec.get("model")
        tasks.append(Task(
            task_id, prompt, cwd, list(expect), str(model) if model else None, list(tools), timeout,
            expect_tools, forbid_tools, isolate, verify,
        ))
    return tasks


def prepare_workspace(task: Task) -> Path:
    """Return the directory a run works in: the task's cwd, or a fresh copy of it."""
    if not task.isolate:
        return task.cwd
    # A stable path per task keeps the transcripts of all its runs in one Claude Code project
    # and leaves the last run's files for inspection.
    workspace = state_dir() / "bench" / "workspaces" / task.id
    shutil.rmtree(workspace, ignore_errors=True)
    shutil.copytree(task.cwd, workspace, ignore=shutil.ignore_patterns("__pycache__", ".git"))
    return workspace


def run_verify(task: Task, workspace: Path) -> dict:
    try:
        proc = subprocess.run(
            task.verify, capture_output=True, text=True, cwd=workspace, timeout=VERIFY_TIMEOUT,
            env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"},
        )
    except subprocess.TimeoutExpired:
        return {"exit_code": None, "output": f"timed out after {VERIFY_TIMEOUT} s"}
    except OSError as e:
        return {"exit_code": None, "output": f"cannot run {task.verify[0]}: {e.strerror or e}"}
    return {"exit_code": proc.returncode, "output": (proc.stdout + proc.stderr)[-VERIFY_OUTPUT_CHARS:]}


def claude_command(task: Task, mode: str, session: str, claude: str, plugin_dir: Path) -> tuple[list[str], dict]:
    """Build the `claude -p` command and environment for one run. The prompt goes to stdin."""
    cmd = [claude, "-p", "--output-format", "json", "--session-id", session, "--plugin-dir", str(plugin_dir)]
    if task.model:
        cmd += ["--model", task.model]
    env = dict(os.environ)
    for name in ("CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID"):
        env.pop(name, None)
    allowed = task.allowed_tools
    if mode == "off":
        # The plugin stays loaded so both modes see the same tool list; only its behavior is off.
        env["LOCAL_SHUNT_DISABLE"] = "1"
        allowed = [t for t in allowed if t not in WORKER_TOOLS]
        cmd += ["--disallowedTools", *WORKER_TOOLS]
    else:
        env.pop("LOCAL_SHUNT_DISABLE", None)
    cmd += ["--allowedTools", *allowed]
    return cmd, env


def run_once(task: Task, mode: str, run: int, claude: str = "claude", plugin_dir: Path = PLUGIN_ROOT) -> dict:
    session = str(uuid.uuid4())
    cmd, env = claude_command(task, mode, session, claude, plugin_dir)
    record: dict = {
        "ts": datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds"),
        "task": task.id,
        "mode": mode,
        "run": run,
        "session_id": session,
        "model": task.model,
    }
    for key in ("expect_tools", "forbid_tools"):
        if getattr(task, key):
            record[key] = getattr(task, key)
    try:
        workspace = prepare_workspace(task)
    except OSError as e:
        raise MeasureError(f"cannot copy {task.cwd} for task {task.id}: {e.strerror or e}") from None
    started = time.monotonic()
    try:
        proc = subprocess.run(
            cmd, input=task.prompt, capture_output=True, text=True, cwd=workspace, env=env, timeout=task.timeout
        )
    except subprocess.TimeoutExpired:
        record["error"] = f"timed out after {task.timeout} s"
    except OSError as e:
        raise MeasureError(f"cannot run {claude}: {e.strerror or e}") from None
    else:
        record["exit_code"] = proc.returncode
        try:
            result = json.loads(proc.stdout)
        except ValueError:
            result = None
        if isinstance(result, dict):
            answer = str(result.get("result") or "")
            record.update(
                num_turns=result.get("num_turns"),
                cost_usd=result.get("total_cost_usd"),
                result_usage=result.get("usage"),
                answer=answer,
            )
            if result.get("is_error") or proc.returncode:
                record["error"] = answer[:300] or str(result.get("subtype") or f"exit code {proc.returncode}")
        else:
            record["error"] = (proc.stderr or proc.stdout).strip()[:300] or f"exit code {proc.returncode}, no JSON output"
    record["duration_ms"] = int((time.monotonic() - started) * 1000)
    if task.checks_answer:
        answer = record.get("answer", "")
        passed = "error" not in record and all(re.search(p, answer, re.IGNORECASE) for p in task.expect)
        if task.verify and "error" not in record:
            record["verify"] = run_verify(task, workspace)
            passed = passed and record["verify"]["exit_code"] == 0
        record["passed"] = passed

    transcript = find_transcript(session)
    if transcript is not None:
        record["claude"] = parse_transcript(transcript).to_dict()
        if mode == "on" and (task.expect_tools or task.forbid_tools):
            record["tool_check_failures"] = tool_check_failures(task, record["claude"]["tool_calls"])
    elif "error" not in record:
        record["error"] = "transcript not found"
    record["worker"] = asdict(worker_usage(session))
    return record


def run_bench(
    task_file: Path,
    runs: int = 3,
    out: Path | None = None,
    only: list[str] | None = None,
    claude: str = "claude",
    progress=lambda message: None,
) -> Path:
    """Run every task with local-shunt on and off, `runs` times each. Returns the results file."""
    if runs < 1:
        raise MeasureError("runs must be at least 1")
    tasks = load_tasks(task_file)
    if only:
        unknown = sorted(set(only) - {t.id for t in tasks})
        if unknown:
            raise MeasureError(f"unknown task id: {', '.join(unknown)}")
        tasks = [t for t in tasks if t.id in only]
    if out is None:
        stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
        out = state_dir() / "bench" / f"{stamp}.jsonl"
    out.parent.mkdir(parents=True, exist_ok=True)

    total, done = len(tasks) * runs * 2, 0
    for run in range(1, runs + 1):
        for index, task in enumerate(tasks):
            # Alternate the order so prompt-cache warmth and drift do not favor one mode.
            modes = ("on", "off") if (run + index) % 2 else ("off", "on")
            for mode in modes:
                done += 1
                progress(f"[{done}/{total}] {task.id} · local-shunt {mode} · run {run}")
                record = run_once(task, mode, run, claude)
                with out.open("a", encoding="utf-8") as f:
                    f.write(json.dumps(record, ensure_ascii=False) + "\n")
                if "error" in record:
                    progress(f"  failed: {record['error'][:200]}")
    return out


# ---------------------------------------------------------------- report


def load_results(paths: list[Path]) -> list[dict]:
    records = []
    for path in paths:
        try:
            text = path.read_text(encoding="utf-8")
        except OSError as e:
            raise MeasureError(f"{path}: {e.strerror or e}") from None
        for line in text.splitlines():
            try:
                record = json.loads(line)
            except ValueError:
                continue
            if isinstance(record, dict) and record.get("mode") in ("on", "off") and record.get("task"):
                records.append(record)
    return records


METRICS = [
    # (key, label, getter)
    ("input", "input tokens", lambda r: r["claude"]["total_input_tokens"]),
    ("weighted", "input tokens, price-weighted", lambda r: r["claude"]["weighted_input_tokens"]),
    ("output", "output tokens", lambda r: r["claude"]["output_tokens"]),
    ("read", "file-read result tokens (est.)", lambda r: r["claude"]["read_result_tokens"]),
    ("cost", "cost USD (reported by claude)", lambda r: r.get("cost_usd")),
    ("turns", "turns", lambda r: r.get("num_turns")),
    ("seconds", "duration s", lambda r: r.get("duration_ms", 0) / 1000),
]


def saving(on, off) -> float | None:
    if on is None or not off:
        return None
    return 1 - on / off


def summarize(records: list[dict]) -> dict:
    """Median of each metric per task and mode, over runs that completed."""
    tasks: dict[str, dict] = {}
    for record in records:
        tasks.setdefault(record["task"], {"on": [], "off": []})[record["mode"]].append(record)

    summary = {}
    for task_id, modes in tasks.items():
        entry: dict = {}
        for mode, runs in modes.items():
            good = [r for r in runs if "error" not in r and isinstance(r.get("claude"), dict)]
            checked = [r for r in runs if "passed" in r]
            stats: dict = {
                "runs": len(runs),
                "ok": len(good),
                "passed": sum(1 for r in checked if r["passed"]),
                "checked": len(checked),
                "delegations": sum(r["claude"].get("delegations", 0) for r in good),
                "worker_calls": sum((r.get("worker") or {}).get("reads", 0) for r in good),
                "denials": sum(r["claude"].get("denials", 0) for r in good),
            }
            for key, _, getter in METRICS:
                values = [v for v in (getter(r) for r in good) if v is not None]
                stats[key] = statistics.median(values) if values else None
            entry[mode] = stats
        entry["saving"] = {key: saving(entry["on"][key], entry["off"][key]) for key, _, _ in METRICS}
        summary[task_id] = entry
    return summary


def _fmt(value, key: str) -> str:
    if value is None:
        return "-"
    if key == "cost":
        return f"{value:.4f}"
    if key == "seconds":
        return f"{value:.1f}"
    return f"{round(value):,}"


def _pct(value: float | None) -> str:
    return "-" if value is None else f"{value:.1%}"


def format_report(records: list[dict]) -> str:
    if not records:
        return "No benchmark records found."
    summary = summarize(records)
    failed = sum(1 for r in records if "error" in r)
    lines = [f"Benchmark: {len(summary)} tasks, {len(records)} runs ({failed} failed). Values are medians.", ""]
    warnings = []
    header = f"  {'':<32} {'on':>12} {'off':>12} {'saving':>8}"

    def pair(label: str, template: str, a: str, b: str) -> str:
        on_text, off_text = template.format(on[a], on[b]), template.format(off[a], off[b])
        return f"  {label:<32} {on_text:>12} {off_text:>12}"

    for task_id, entry in summary.items():
        on, off = entry["on"], entry["off"]
        lines += [f"Task {task_id}", header, pair("runs ok", "{}/{}", "ok", "runs")]
        for key, label, _ in METRICS:
            lines.append(
                f"  {label:<32} {_fmt(on[key], key):>12} {_fmt(off[key], key):>12} {_pct(entry['saving'][key]):>8}"
            )
        lines.append(pair("delegations / hook denials", "{} / {}", "delegations", "denials"))
        if on["checked"] or off["checked"]:
            lines.append(pair("answers passed", "{}/{}", "passed", "checked"))
        lines.append("")

        if not on["ok"] or not off["ok"]:
            warnings.append(f"{task_id}: no completed run in one mode; it is left out of the totals")
        if off["delegations"] or off["worker_calls"] or off["denials"]:
            warnings.append(f"{task_id}: local-shunt was used in 'off' runs, so the baseline is contaminated")
        if on["ok"] and not on["delegations"] and not on["denials"]:
            warnings.append(f"{task_id}: 'on' runs never delegated a read; the task does not exercise local-shunt")
        if on["passed"] < off["passed"]:
            warnings.append(f"{task_id}: fewer answers passed with local-shunt on ({on['passed']} vs {off['passed']})")

    complete = {k: v for k, v in summary.items() if v["on"]["ok"] and v["off"]["ok"]}
    lines += [f"Overall ({len(complete)} tasks with runs in both modes; sums of per-task medians)", header]
    for key, label, _ in METRICS[:5]:
        on_values = [v["on"][key] for v in complete.values()]
        off_values = [v["off"][key] for v in complete.values()]
        if any(x is None for x in on_values + off_values):
            continue
        on_sum, off_sum = sum(on_values), sum(off_values)
        lines.append(f"  {label:<32} {_fmt(on_sum, key):>12} {_fmt(off_sum, key):>12} {_pct(saving(on_sum, off_sum)):>8}")
    for key, label in (("read", "file-read tokens"), ("input", "input tokens")):
        met = sum(1 for v in complete.values() if (v["saving"][key] or 0) >= TARGET_SAVING)
        lines.append(f"  tasks saving ≥{TARGET_SAVING:.0%} of {label:<17} {met:>3} / {len(complete)}")

    if warnings:
        lines += ["", "Warnings"] + [f"  - {w}" for w in warnings]
    lines += [
        "",
        "Input and output tokens are Claude's billed counts from the transcripts. File-read result tokens",
        "are estimated from the text of Read and local-shunt tool results. Worker model tokens are not included.",
    ]
    return "\n".join(lines)
