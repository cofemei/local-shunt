#!/usr/bin/env python3
"""local-shunt worker CLI.

    shunt.py read <file>... --question "..." [--provider NAME] [--model NAME] [--max-output N]
    shunt.py write --out PATH --spec "..." [--context FILE...] [--force] [--provider NAME] [--model NAME]
    shunt.py stats [--since YYYY-MM-DD] [--session ID]
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from collections import Counter
from dataclasses import dataclass
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
    state_dir,
)
from providers import LLMError, Provider, get_provider  # noqa: E402

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


@dataclass
class Chunk:
    parts: list[tuple[SourceFile, int, int]]  # (file, first line, last line), 1-based inclusive

    def render(self) -> str:
        out = []
        for src, first, last in self.parts:
            out.append(f"=== FILE: {src.shown} (lines {first}-{last} of {len(src.lines)}) ===")
            out.extend(f"L{n}: {src.lines[n - 1]}" for n in range(first, last + 1))
        return "\n".join(out)

    def describe(self) -> str:
        return ", ".join(f"{src.shown} L{first}-L{last}" for src, first, last in self.parts)


def load_sources(paths: list[str], cfg: Config) -> list[SourceFile]:
    sources = []
    for raw in paths:
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
    """Remove line references outside the files. Returns (text, removed count)."""
    by_path = {src.shown: len(src.lines) for src in sources}
    default_bound = max(by_path.values(), default=0)
    removed = 0

    def bound_for(path: str | None) -> int:
        if path:
            for shown, count in by_path.items():
                if shown == path or shown.endswith("/" + path) or path.endswith("/" + shown):
                    return count
        return default_bound

    def is_valid(m: re.Match) -> bool:
        a = int(m.group("a"))
        b = int(m.group("b")) if m.group("b") else a
        bound = bound_for(m.group("path"))
        return 1 <= a <= b <= bound

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


def run_read(cfg: Config, files: list[str], question: str, max_output: int | None = None) -> str:
    started = now_ms()
    question = (question or "").strip()
    if not question:
        raise UsageError("question must not be empty")
    if not files:
        raise UsageError("at least one file is required")
    sources = load_sources(files, cfg)
    system = load_prompt("read.md")
    max_output = max_output or cfg.max_output
    llm = get_provider(cfg)
    usage = Usage()

    fixed = estimate_tokens(system + question) + 50
    single_budget = int(cfg.num_ctx * 0.6) - fixed - max_output
    whole = Chunk([(src, 1, len(src.lines)) for src in sources])
    whole_text = whole.render()
    est_input = estimate_tokens(whole_text)

    if est_input <= single_budget:
        chunks = [whole]
    else:
        chunks = build_chunks(sources, int(cfg.num_ctx * 0.5) - fixed)

    truncated = False
    if len(chunks) == 1:
        progress(f"asking {cfg.model} (~{est_input:,} tokens)")
        result = llm.chat(system, f"Question: {question}\n\n{chunks[0].render()}", max_output)
        usage.add(result)
        answer, calls, truncated = result.text, 1, result.truncated
    else:
        findings = []
        for i, chunk in enumerate(chunks, 1):
            progress(f"reading part {i}/{len(chunks)}: {chunk.describe()}")
            user = (
                f"Question: {question}\n\n"
                f"This is part {i} of {len(chunks)}. It covers {chunk.describe()}. "
                "Answer from this part only; list what this part does not show under 'Not covered or uncertain'.\n\n"
                f"{chunk.render()}"
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
        "session_id": os.environ.get("CLAUDE_SESSION_ID"),
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

    contexts = []
    for src in load_sources(context or [], cfg):
        contexts.append(f"=== CONTEXT FILE: {src.shown} ===\n" + "\n".join(src.lines))

    system = load_prompt("write.md")
    max_output = max_output or 4096
    user = f"Specification:\n{spec.strip()}\n\nTarget file: {display_path(out_path)}"
    if contexts:
        user += "\n\n" + "\n\n".join(contexts)
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
        "session_id": os.environ.get("CLAUDE_SESSION_ID"),
        "event": "write",
        "files": [display_path(out_path)],
        "lines": line_count,
        "prompt_tokens": result.prompt_tokens,
        "completion_tokens": result.output_tokens,
        "cost_usd": result.cost,
        "provider": cfg.provider,
        "model": cfg.model,
        "latency_ms": int(elapsed * 1000),
        "outcome": "ok",
    })
    return (
        f"local-shunt: wrote {display_path(out_path)} ({line_count:,} lines) · {cfg.provider} {cfg.model} · {elapsed:.1f} s\n"
        "Review the file and run the tests or linter before relying on it."
    )


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
    lines += ["", "Source and summary token counts are estimates, not Claude's billed tokens."]
    return "\n".join(lines)


# ---------------------------------------------------------------- main


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="shunt.py", description="Delegate file reading and writing to a worker model (Ollama or an OpenAI-compatible API)."
    )
    sub = parser.add_subparsers(dest="command", required=True)

    read = sub.add_parser("read", help="answer a question about one or more files")
    read.add_argument("files", nargs="+")
    read.add_argument("--question", "-q", required=True)
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
    return parser


def execute(command: str, cfg: Config, run) -> tuple[int, str]:
    """Run a command and map errors to (exit code, message). Shared with the MCP server."""
    try:
        return 0, run()
    except UsageError as e:
        return EXIT_USAGE, f"local-shunt: {e}"
    except LLMError as e:
        log_event({
            "session_id": os.environ.get("CLAUDE_SESSION_ID"),
            "event": command,
            "provider": cfg.provider,
            "model": cfg.model,
            "outcome": f"error:{type(e).__name__}",
            "detail": str(e)[:200],
        })
        return EXIT_FAILURE, (
            f"local-shunt: {e}\n"
            "Fall back to reading the file yourself with Grep and Read with offset/limit."
        )


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    cfg = apply_overrides(load_config(), getattr(args, "provider", None), getattr(args, "model", None))
    if args.command == "read":
        run = lambda: run_read(cfg, args.files, args.question, args.max_output)  # noqa: E731
    elif args.command == "write":
        run = lambda: run_write(cfg, args.out, args.spec, args.context, args.force, args.max_output)  # noqa: E731
    else:
        run = lambda: run_stats(args.since, args.session)  # noqa: E731
    code, text = execute(args.command, cfg, run)
    print(text, file=sys.stdout if code == 0 else sys.stderr)
    return code


if __name__ == "__main__":
    sys.exit(main())
