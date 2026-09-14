#!/usr/bin/env python3
"""local-shunt worker CLI.

    shunt.py read [<file>|-]... [--diff [SPEC]] --question "..." [--provider NAME] [--model NAME] [--max-output N]
    shunt.py write --out PATH --spec "..." [--context FILE...] [--force] [--provider NAME] [--model NAME]
    shunt.py stats [--since YYYY-MM-DD] [--session ID]
    shunt.py usage [SESSION_ID | TRANSCRIPT.jsonl]... [--json]
    shunt.py bench TASKS.json [--runs N] [--task ID...] [--out FILE] [--claude PATH]
    shunt.py bench-report RESULTS.jsonl...
"""

from __future__ import annotations

import argparse
import json
import os
import re
import secrets
import shlex
import subprocess
import sys
from collections import Counter
from dataclasses import asdict, dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from common import (  # noqa: E402
    PLUGIN_ROOT,
    PROVIDERS,
    Config,
    apply_overrides,
    estimate_tokens,
    inspect_file,
    load_config,
    log_event,
    now_ms,
    session_id,
    state_dir,
)
from providers import LLMError, Provider, RequestTimeout, get_provider  # noqa: E402
import measure  # noqa: E402

CHUNK_OVERLAP_LINES = 50
EXIT_USAGE = 2
EXIT_FAILURE = 1


class UsageError(Exception):
    pass


@dataclass
class Usage:
    prompt_tokens: int | None = None
    output_tokens: int | None = None
    cost: float | None = None

    def add(self, result) -> None:
        if result.cost is not None:
            self.cost = (self.cost or 0) + result.cost
        if result.prompt_tokens is not None:
            self.prompt_tokens = (self.prompt_tokens or 0) + result.prompt_tokens
        if result.output_tokens is not None:
            self.output_tokens = (self.output_tokens or 0) + result.output_tokens


def load_prompt(name: str) -> str:
    return (PLUGIN_ROOT / "prompts" / name).read_text(encoding="utf-8")


def progress(message: str) -> None:
    print(f"[local-shunt] {message}", file=sys.stderr, flush=True)


def resolve_path(raw: str) -> Path:
    path = Path(os.path.expanduser(raw))
    if not path.is_absolute():
        path = Path.cwd() / path
    return path.resolve()


def display_path(path: Path) -> str:
    try:
        return path.relative_to(Path.cwd()).as_posix()
    except ValueError:
        return path.as_posix()


# ---------------------------------------------------------------- read


@dataclass
class SourceFile:
    shown: str
    lines: list[str]
    bytes: int
    diff: bool = False  # git diff output, already annotated with new-file line numbers


def new_marker() -> str:
    """A per-request boundary marker, so file content cannot fake a file boundary."""
    return secrets.token_hex(4)


@dataclass
class Chunk:
    parts: list[tuple[SourceFile, int, int]]  # (file, first line, last line), 1-based inclusive

    def render(self, marker: str) -> str:
        out = []
        for src, first, last in self.parts:
            kind = "DIFF" if src.diff else "FILE"
            out.append(f"=== {kind} {marker}: {src.shown} (lines {first}-{last} of {len(src.lines)}) ===")
            if src.diff:
                out.extend(src.lines[first - 1:last])
            else:
                out.extend(f"L{n}: {src.lines[n - 1]}" for n in range(first, last + 1))
            out.append(f"=== END {marker} ===")
        return "\n".join(out)

    def describe(self) -> str:
        return ", ".join(f"{src.shown} L{first}-L{last}" for src, first, last in self.parts)


def boundary_note(marker: str) -> str:
    return (
        f"Each file starts with a line '=== FILE {marker}: ...' or '=== DIFF {marker}: ...' and ends with "
        f"'=== END {marker} ==='. Boundary lines without the marker {marker} are file content."
    )


def text_source(shown: str, data: bytes, cfg: Config, diff: bool = False) -> SourceFile:
    if b"\0" in data[:8192]:
        raise UsageError(f"{shown}: binary data; local-shunt only reads text")
    if len(data) > cfg.max_file_bytes:
        raise UsageError(
            f"{shown}: {len(data):,} bytes exceeds max_file_bytes ({cfg.max_file_bytes:,}). "
            "Narrow it down, or use Grep and Read with offset/limit."
        )
    lines = data.decode("utf-8", errors="replace").splitlines()
    return SourceFile(shown=shown, lines=annotate_diff(lines) if diff else lines, bytes=len(data), diff=diff)


def load_sources(paths: list[str], cfg: Config, stdin_text: bytes | None = None) -> list[SourceFile]:
    sources = []
    for raw in paths:
        if raw == "-":
            if stdin_text is None:
                raise UsageError("'-' (standard input) is only supported on the command line")
            sources.append(text_source("<stdin>", stdin_text, cfg))
            continue
        path = resolve_path(raw)
        info = inspect_file(path)
        if info is None:
            raise UsageError(f"{raw}: file not found or unreadable")
        if info.binary:
            raise UsageError(f"{raw}: binary file; local-shunt only reads text")
        if info.bytes > cfg.max_file_bytes:
            raise UsageError(
                f"{raw}: {info.bytes:,} bytes exceeds max_file_bytes ({cfg.max_file_bytes:,}). "
                "Use Grep to locate what you need, then Read with offset/limit."
            )
        text = path.read_text(encoding="utf-8", errors="replace")
        sources.append(SourceFile(shown=display_path(path), lines=text.splitlines(), bytes=info.bytes))
    return sources


# Options that only select what to compare. Anything else (--output, --ext-diff, ...) is refused,
# because the spec may come from a model.
DIFF_OPTIONS = {"--cached", "--staged", "--merge-base"}
HUNK = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@")


def load_diff(spec: str, cfg: Config) -> SourceFile:
    """Run `git diff <spec>` in the working directory and return it as a source."""
    try:
        args = shlex.split(spec)
    except ValueError as e:
        raise UsageError(f"diff: {e}") from None
    revisions, paths = (args[:args.index("--")], args[args.index("--") + 1:]) if "--" in args else (args, [])
    for arg in revisions:
        if arg.startswith("-") and arg not in DIFF_OPTIONS:
            raise UsageError(
                f"diff: option {arg} is not supported; pass revisions, {', '.join(sorted(DIFF_OPTIONS))}, "
                "then -- and paths"
            )
    cmd = ["git", "--no-pager", "diff", "--no-color", "--no-ext-diff", "--no-textconv", *revisions, "--", *paths]
    try:
        proc = subprocess.run(cmd, capture_output=True, timeout=60)
    except FileNotFoundError:
        raise UsageError("diff: git is not installed") from None
    except subprocess.TimeoutExpired:
        raise UsageError("diff: git diff took longer than 60 s") from None
    label = f"git diff {spec}".strip()
    if proc.returncode != 0:
        raise UsageError(f"diff: {proc.stderr.decode('utf-8', 'replace').strip()[:300] or f'`{label}` failed'}")
    if not proc.stdout.strip():
        raise UsageError(f"diff: `{label}` is empty")
    return text_source(label, proc.stdout, cfg, diff=True)


def annotate_diff(lines: list[str]) -> list[str]:
    """Prefix context and added lines with their line number in the new file, e.g. 'L120 + code'."""
    out = []
    number = None
    for line in lines:
        hunk = HUNK.match(line)
        if hunk:
            number = int(hunk.group(1))
            out.append(line)
        elif line.startswith("diff --git") or number is None:
            number = None
            out.append(line)
        elif line[:1] in (" ", "+"):
            out.append(f"L{number} {line}")
            number += 1
        else:
            out.append(f"     {line}")  # removed line or "\ No newline at end of file"
    return out


def build_chunks(sources: list[SourceFile], budget_tokens: int) -> list[Chunk]:
    """Pack files into chunks under the token budget, splitting large files with overlap."""
    chunks: list[Chunk] = []
    current: list[tuple[SourceFile, int, int]] = []
    used = 0

    def line_cost(src: SourceFile, n: int) -> int:
        return estimate_tokens(f"L{n}: {src.lines[n - 1]}\n") + 1

    for src in sources:
        costs = [line_cost(src, n) for n in range(1, len(src.lines) + 1)]
        total = sum(costs) + 20
        if not src.lines:
            current.append((src, 1, 0))
            continue
        if used + total <= budget_tokens:
            current.append((src, 1, len(src.lines)))
            used += total
            continue
        if total <= budget_tokens:
            chunks.append(Chunk(current))
            current, used = [(src, 1, len(src.lines))], total
            continue

        # Split this file into overlapping windows, each in its own chunk.
        if current:
            chunks.append(Chunk(current))
            current, used = [], 0
        first = 1
        while first <= len(src.lines):
            spent, last = 20, first - 1
            while last < len(src.lines) and (spent + costs[last] <= budget_tokens or last < first):
                spent += costs[last]
                last += 1
            chunks.append(Chunk([(src, first, last)]))
            if last >= len(src.lines):
                break
            first = max(first + 1, last - CHUNK_OVERLAP_LINES + 1)

    if current:
        chunks.append(Chunk(current))
    return [c for c in chunks if c.parts]


LINE_REF = re.compile(
    r"(?:(?P<path>[\w./-]+):)?(?<![\w])L(?P<a>\d+)(?:\s*[-–]\s*L?(?P<b>\d+))?"
)


def validate_line_refs(text: str, sources: list[SourceFile]) -> tuple[str, int]:
    """Remove line references outside the files. Returns (text, removed count).

    References into a diff point at files that were not loaded, so with a diff among the
    sources only references that name a loaded file are checked.
    """
    by_path = {src.shown: len(src.lines) for src in sources if not src.diff}
    default_bound = max(by_path.values(), default=0)
    check_unnamed = all(not src.diff for src in sources)
    removed = 0

    def bound_for(path: str | None) -> int | None:
        if path:
            for shown, count in by_path.items():
                if shown == path or shown.endswith("/" + path) or path.endswith("/" + shown):
                    return count
        return default_bound if check_unnamed else None

    def is_valid(m: re.Match) -> bool:
        a = int(m.group("a"))
        b = int(m.group("b")) if m.group("b") else a
        bound = bound_for(m.group("path"))
        return bound is None or 1 <= a <= b <= bound

    out_lines = []
    section = ""
    for line in text.splitlines():
        if line.startswith("#"):
            section = line.lower()
        refs = list(LINE_REF.finditer(line))
        bad = [m for m in refs if not is_valid(m)]
        if not bad:
            out_lines.append(line)
            continue
        removed += len(bad)
        if "location" in section and line.lstrip().startswith("-"):
            continue  # a location bullet with a bogus range carries no value
        out_lines.append(LINE_REF.sub(lambda m: m.group(0) if is_valid(m) else "L?", line))
    return "\n".join(out_lines), removed


def reduce_findings(
    llm: Provider, question: str, findings: list[tuple[str, str]], max_output: int, usage: Usage | None = None
) -> tuple[str, int]:
    """Merge (label, text) findings, batching when they exceed the context budget."""
    cfg = llm.cfg
    system = load_prompt("read_reduce.md")
    budget = int(cfg.num_ctx * 0.6) - estimate_tokens(system) - max_output
    calls = 0
    while True:
        batches: list[list[tuple[str, str]]] = [[]]
        used = 0
        for label, text in findings:
            cost = estimate_tokens(label + text) + 10
            if batches[-1] and used + cost > budget:
                batches.append([])
                used = 0
            batches[-1].append((label, text))
            used += cost

        merged = []
        for i, batch in enumerate(batches, 1):
            body = "\n\n".join(f"## Part: {label}\n{text}" for label, text in batch)
            progress(f"merging findings ({i}/{len(batches)})")
            result = llm.chat(system, f"Question: {question}\n\n{body}", max_output)
            if usage is not None:
                usage.add(result)
            calls += 1
            labels = "; ".join(label for label, _ in batch)
            merged.append((labels, result.text))
        if len(merged) == 1:
            return merged[0][1], calls
        findings = merged


def run_read(
    cfg: Config,
    files: list[str],
    question: str,
    max_output: int | None = None,
    diff: str | None = None,
    stdin_text: bytes | None = None,
) -> str:
    started = now_ms()
    question = (question or "").strip()
    if not question:
        raise UsageError("question must not be empty")
    if not files and diff is None:
        raise UsageError("at least one file or a diff is required")
    sources = load_sources(files or [], cfg, stdin_text)
    if diff is not None:
        sources.append(load_diff(diff, cfg))
    system = load_prompt("read.md")
    max_output = max_output or cfg.max_output
    llm = get_provider(cfg)
    usage = Usage()
    marker = new_marker()
    preamble = f"Question: {question}\n\n{boundary_note(marker)}\n\n"

    fixed = estimate_tokens(system + preamble) + 50
    single_budget = int(cfg.num_ctx * 0.6) - fixed - max_output
    whole = Chunk([(src, 1, len(src.lines)) for src in sources])
    whole_text = whole.render(marker)
    est_input = estimate_tokens(whole_text)

    if est_input <= single_budget:
        chunks = [whole]
    else:
        chunks = build_chunks(sources, int(cfg.num_ctx * 0.5) - fixed)

    truncated = False
    if len(chunks) == 1:
        progress(f"asking {cfg.model} (~{est_input:,} tokens)")
        result = llm.chat(system, preamble + chunks[0].render(marker), max_output)
        usage.add(result)
        answer, calls, truncated = result.text, 1, result.truncated
    else:
        findings = []
        for i, chunk in enumerate(chunks, 1):
            progress(f"reading part {i}/{len(chunks)}: {chunk.describe()}")
            user = (
                preamble
                + f"This is part {i} of {len(chunks)}. It covers {chunk.describe()}. "
                "Answer from this part only; list what this part does not show under 'Not covered or uncertain'.\n\n"
                + chunk.render(marker)
            )
            result = llm.chat(system, user, max_output)
            usage.add(result)
            truncated = truncated or result.truncated
            findings.append((chunk.describe(), result.text))
        answer, reduce_calls = reduce_findings(llm, question, findings, max_output, usage)
        calls = len(chunks) + reduce_calls

    if not answer.strip():
        raise LLMError("the model returned an empty response")
    answer, removed = validate_line_refs(answer, sources)

    elapsed = (now_ms() - started) / 1000
    title = ", ".join(f"{src.shown} ({len(src.lines):,} lines)" for src in sources)
    notes = [
        f"{cfg.provider} {cfg.model}",
        f"~{est_input:,} tokens of source -> ~{estimate_tokens(answer):,} tokens",
        f"{len(chunks)} part{'s' if len(chunks) > 1 else ''}",
        f"{elapsed:.1f} s",
    ]
    if removed:
        notes.append(f"removed {removed} invalid line reference{'s' if removed > 1 else ''}")
    if truncated:
        notes.append("output hit --max-output and may be cut off")
    output = f"## local-shunt: {title}\n\n{answer}\n\n---\n" + " · ".join(notes)

    log_event({
        "session_id": session_id(),
        "event": "read",
        "files": [src.shown for src in sources],
        "lines": sum(len(src.lines) for src in sources),
        "bytes": sum(src.bytes for src in sources),
        "est_input_tokens": est_input,
        "est_output_tokens": estimate_tokens(output),
        "prompt_tokens": usage.prompt_tokens,
        "completion_tokens": usage.output_tokens,
        "cost_usd": usage.cost,
        "provider": cfg.provider,
        "model": cfg.model,
        "chunks": len(chunks),
        "calls": calls,
        "invalid_refs_removed": removed,
        "latency_ms": int(elapsed * 1000),
        "outcome": "ok",
    })
    return output


# ---------------------------------------------------------------- write

FENCED = re.compile(r"```[^\n`]*\n(.*?)\n?```", re.DOTALL)


def strip_code_fence(text: str) -> str:
    stripped = text.strip()
    blocks = FENCED.findall(stripped)
    if stripped.startswith("```") and blocks:
        return blocks[0]
    if len(blocks) == 1 and stripped.count("```") == 2:
        return blocks[0]
    return stripped


def run_write(
    cfg: Config,
    out: str,
    spec: str,
    context: list[str] | None = None,
    force: bool = False,
    max_output: int | None = None,
) -> str:
    started = now_ms()
    if not out:
        raise UsageError("out must not be empty")
    raw_out = out
    out_path = resolve_path(out)
    if out_path.exists() and not force:
        raise UsageError(f"{raw_out} already exists; pass --force to overwrite")
    if out_path.exists() and not out_path.is_file():
        raise UsageError(f"{raw_out} is not a regular file")
    if not (spec or "").strip():
        raise UsageError("spec must not be empty")

    marker = new_marker()
    contexts = []
    for src in load_sources(context or [], cfg):
        contexts.append(f"=== FILE {marker}: {src.shown} ===\n" + "\n".join(src.lines) + f"\n=== END {marker} ===")

    system = load_prompt("write.md")
    max_output = max_output or 4096
    user = f"Specification:\n{spec.strip()}\n\nTarget file: {display_path(out_path)}"
    if contexts:
        user += f"\n\nContext files. {boundary_note(marker)}\n\n" + "\n\n".join(contexts)
    needed = estimate_tokens(system + user) + max_output
    if needed > cfg.num_ctx:
        raise UsageError(
            f"prompt needs ~{needed:,} tokens but num_ctx is {cfg.num_ctx:,}; "
            "pass fewer or smaller --context files"
        )

    llm = get_provider(cfg)
    progress(f"asking {cfg.model} to write {display_path(out_path)}")
    result = llm.chat(system, user, max_output)
    if result.truncated:
        raise LLMError(
            f"output reached --max-output ({max_output} tokens) and is incomplete; nothing was written. "
            "Raise --max-output or split the file"
        )
    content = strip_code_fence(result.text)
    if not content.strip():
        raise LLMError("the model returned an empty file; nothing was written")
    if not content.endswith("\n"):
        content += "\n"

    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(content, encoding="utf-8")
    line_count = content.count("\n")
    elapsed = (now_ms() - started) / 1000
    log_event({
        "session_id": session_id(),
        "event": "write",
        "files": [display_path(out_path)],
        "context_files": len(contexts),
        "lines": line_count,
        "prompt_tokens": result.prompt_tokens,
        "completion_tokens": result.output_tokens,
        "cost_usd": result.cost,
        "provider": cfg.provider,
        "model": cfg.model,
        "latency_ms": int(elapsed * 1000),
        "outcome": "ok",
    })
    message = (
        f"local-shunt: wrote {display_path(out_path)} ({line_count:,} lines) · {cfg.provider} {cfg.model} · {elapsed:.1f} s\n"
        "Review the file and run the tests or linter before relying on it."
    )
    if not contexts:
        message += (
            "\nWarning: no context file was given, so the file follows no example from this project. "
            "Next time pass an existing file to imitate (and the code under test) as context."
        )
    return message


# ---------------------------------------------------------------- stats


def run_stats(since: str | None = None, session: str | None = None) -> str:
    log_file = state_dir() / "log.jsonl"
    records = []
    try:
        with log_file.open(encoding="utf-8") as f:
            for line in f:
                try:
                    records.append(json.loads(line))
                except ValueError:
                    continue
    except OSError:
        return f"No log yet ({log_file})."

    if since:
        records = [r for r in records if str(r.get("ts", ""))[:10] >= since]
    if session:
        records = [r for r in records if r.get("session_id") == session]

    hook = Counter(r["outcome"] for r in records if r.get("event") == "hook" and "outcome" in r)
    reads = [r for r in records if r.get("event") == "read" and r.get("outcome") == "ok"]
    writes = [r for r in records if r.get("event") == "write" and r.get("outcome") == "ok"]
    errors = Counter(r["outcome"] for r in records if str(r.get("outcome", "")).startswith("error"))

    lines = [f"Log: {log_file}  ({len(records):,} records)", "", "Hook decisions"]
    if hook:
        lines += [f"  {outcome:<32} {count:>8,}" for outcome, count in hook.most_common()]
        wide = [r for r in records if r.get("event") == "hook" and r.get("range_lines") is not None]
        if wide:
            lines += [
                f"  {'wide ranges (≥ threshold lines)':<32} {len(wide):>8,}",
                f"  {'lines returned by wide ranges':<32} {sum(r['range_lines'] for r in wide):>8,}",
            ]
    else:
        lines.append("  (none)")

    lines += ["", "Delegated reads"]
    if reads:
        est_in = sum(r.get("est_input_tokens") or 0 for r in reads)
        est_out = sum(r.get("est_output_tokens") or 0 for r in reads)
        prompt = sum(r.get("prompt_tokens") or 0 for r in reads)
        completion = sum(r.get("completion_tokens") or 0 for r in reads)
        cost = sum(r.get("cost_usd") or 0 for r in records if r.get("outcome") == "ok")
        latency = sum(r.get("latency_ms") or 0 for r in reads) / len(reads) / 1000
        saved = 1 - est_out / est_in if est_in else 0
        lines += [
            f"  {'count':<32} {len(reads):>8,}",
            f"  {'est. source tokens':<32} {est_in:>8,}",
            f"  {'est. summary tokens':<32} {est_out:>8,}",
            f"  {'est. saving':<32} {saved:>8.1%}",
            f"  {'worker prompt tokens':<32} {prompt:>8,}",
            f"  {'worker completion tokens':<32} {completion:>8,}",
            f"  {'average latency':<32} {latency:>7.1f}s",
            f"  {'reported API cost (read+write)':<32} {cost:>8.4f} USD",
        ]
        by_model = Counter(f"{r.get('provider', 'ollama')} {r.get('model')}" for r in reads)
        lines += [f"  {name:<32} {count:>8,}" for name, count in by_model.most_common()]
    else:
        lines.append("  (none)")

    lines += ["", f"{'Delegated writes':<34} {len(writes):>8,}"]
    if errors:
        lines += ["", "Errors"]
        lines += [f"  {outcome:<32} {count:>8,}" for outcome, count in errors.most_common()]
    lines += [
        "",
        "Source and summary token counts are estimates, not Claude's billed tokens.",
        "For billed tokens use `shunt.py usage` (one session) or `shunt.py bench` (on/off comparison).",
    ]
    return "\n".join(lines)


# ---------------------------------------------------------------- measurement


def run_usage(refs: list[str], as_json: bool = False) -> str:
    refs = refs or [ref for ref in [session_id()] if ref]
    if not refs:
        raise UsageError("pass a session ID or transcript path (no Claude Code session in the environment)")
    records = measure.read_log()
    reports = []
    for ref in refs:
        transcript = measure.find_transcript(ref)
        if transcript is None:
            raise UsageError(f"{ref}: transcript not found under {measure.claude_home() / 'projects'}")
        claude = measure.parse_transcript(transcript)
        worker = measure.worker_usage(claude.session_id, records)
        if as_json:
            reports.append(json.dumps({"claude": claude.to_dict(), "worker": asdict(worker)}, ensure_ascii=False))
        else:
            reports.append(measure.format_usage(claude, worker))
    return "\n".join(reports) if as_json else "\n\n".join(reports)


def run_bench(task_file: str, runs: int, only: list[str] | None, out: str | None, claude: str) -> str:
    try:
        results = measure.run_bench(
            resolve_path(task_file), runs, resolve_path(out) if out else None, only, claude, progress
        )
        report = measure.format_report(measure.load_results([results]))
    except measure.MeasureError as e:
        raise UsageError(str(e)) from None
    return f"{report}\n\nResults: {results}"


def run_bench_report(paths: list[str]) -> str:
    try:
        return measure.format_report(measure.load_results([resolve_path(p) for p in paths]))
    except measure.MeasureError as e:
        raise UsageError(str(e)) from None


# ---------------------------------------------------------------- main


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="shunt.py", description="Delegate file reading and writing to a worker model (Ollama or an OpenAI-compatible API)."
    )
    sub = parser.add_subparsers(dest="command", required=True)

    read = sub.add_parser("read", help="answer a question about files, a git diff or standard input")
    read.add_argument("files", nargs="*", help="file paths; '-' reads standard input")
    read.add_argument("--question", "-q", required=True)
    read.add_argument(
        "--diff", nargs="?", const="", metavar="SPEC",
        help="also read `git diff SPEC`, e.g. HEAD, main...HEAD, --cached, or HEAD -- src/ (default: unstaged changes)",
    )
    read.add_argument("--provider", choices=sorted(PROVIDERS))
    read.add_argument("--model")
    read.add_argument("--max-output", type=int)

    write = sub.add_parser("write", help="generate a boilerplate file from a specification")
    write.add_argument("--out", required=True)
    write.add_argument("--spec", required=True)
    write.add_argument("--context", nargs="*")
    write.add_argument("--force", action="store_true")
    write.add_argument("--provider", choices=sorted(PROVIDERS))
    write.add_argument("--model")
    write.add_argument("--max-output", type=int)

    stats = sub.add_parser("stats", help="summarize the local-shunt log")
    stats.add_argument("--since", help="YYYY-MM-DD")
    stats.add_argument("--session")

    usage = sub.add_parser("usage", help="Claude's token usage in a session, from its transcript")
    usage.add_argument("sessions", nargs="*", help="session IDs or transcript paths (default: the current session)")
    usage.add_argument("--json", action="store_true")

    bench = sub.add_parser("bench", help="run tasks through claude -p with local-shunt on and off")
    bench.add_argument("tasks", help="task file (JSON)")
    bench.add_argument("--runs", type=int, default=3, help="runs per task and mode (default 3)")
    bench.add_argument("--task", action="append", dest="only", metavar="ID", help="run only this task (repeatable)")
    bench.add_argument("--out", help="results file (default: $XDG_STATE_HOME/local-shunt/bench/<time>.jsonl)")
    bench.add_argument("--claude", default="claude", help="claude executable")

    report = sub.add_parser("bench-report", help="summarize benchmark results")
    report.add_argument("results", nargs="+")
    return parser


def execute(command: str, cfg: Config, run) -> tuple[int, str]:
    """Run a command and map errors to (exit code, message). Shared with the MCP server."""
    try:
        return 0, run()
    except UsageError as e:
        return EXIT_USAGE, f"local-shunt: {e}"
    except LLMError as e:
        log_event({
            "session_id": session_id(),
            "event": command,
            "provider": cfg.provider,
            "model": cfg.model,
            "outcome": f"error:{type(e).__name__}",
            "detail": str(e)[:200],
        })
        hint = ""
        if isinstance(e, RequestTimeout):
            hint = (
                f"The worker model did not answer within request_timeout ({cfg.request_timeout} s). "
                "Ask about fewer or smaller files, or raise request_timeout in ~/.config/local-shunt/config.json.\n"
            )
        return EXIT_FAILURE, (
            f"local-shunt: {e}\n{hint}"
            "Fall back to reading the file yourself with Grep and Read with offset/limit."
        )


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    cfg = apply_overrides(load_config(), getattr(args, "provider", None), getattr(args, "model", None))
    if args.command == "read":
        stdin_text = sys.stdin.buffer.read() if "-" in args.files else None
        run = lambda: run_read(cfg, args.files, args.question, args.max_output, args.diff, stdin_text)  # noqa: E731
    elif args.command == "write":
        run = lambda: run_write(cfg, args.out, args.spec, args.context, args.force, args.max_output)  # noqa: E731
    elif args.command == "usage":
        run = lambda: run_usage(args.sessions, args.json)  # noqa: E731
    elif args.command == "bench":
        run = lambda: run_bench(args.tasks, args.runs, args.only, args.out, args.claude)  # noqa: E731
    elif args.command == "bench-report":
        run = lambda: run_bench_report(args.results)  # noqa: E731
    else:
        run = lambda: run_stats(args.since, args.session)  # noqa: E731
    code, text = execute(args.command, cfg, run)
    print(text, file=sys.stdout if code == 0 else sys.stderr)
    return code


if __name__ == "__main__":
    sys.exit(main())
