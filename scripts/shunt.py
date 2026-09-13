#!/usr/bin/env python3
"""local-shunt worker CLI.

    shunt.py read <file>... --question "..." [--model NAME] [--max-output N]
    shunt.py write --out PATH --spec "..." [--context FILE...] [--force]
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
    Config,
    estimate_tokens,
    inspect_file,
    load_config,
    log_event,
    now_ms,
    state_dir,
)
from ollama_client import OllamaError, chat  # noqa: E402

CHUNK_OVERLAP_LINES = 50
EXIT_USAGE = 2
EXIT_FAILURE = 1


class UsageError(Exception):
    pass


def load_prompt(name: str) -> str:
    return (PLUGIN_ROOT / "prompts" / name).read_text(encoding="utf-8")


def progress(message: str) -> None:
    print(f"[local-shunt] {message}", file=sys.stderr, flush=True)


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
        path = Path(os.path.expanduser(raw)).resolve()
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


def reduce_findings(cfg: Config, question: str, findings: list[tuple[str, str]], max_output: int) -> tuple[str, int]:
    """Merge (label, text) findings, batching when they exceed the context budget."""
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
            result = chat(cfg, system, f"Question: {question}\n\n{body}", max_output)
            calls += 1
            labels = "; ".join(label for label, _ in batch)
            merged.append((labels, result.text))
        if len(merged) == 1:
            return merged[0][1], calls
        findings = merged


def cmd_read(args: argparse.Namespace, cfg: Config) -> int:
    started = now_ms()
    sources = load_sources(args.files, cfg)
    system = load_prompt("read.md")
    max_output = args.max_output or cfg.max_output
    question = args.question.strip()
    if not question:
        raise UsageError("--question must not be empty")

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
        result = chat(cfg, system, f"Question: {question}\n\n{chunks[0].render()}", max_output)
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
            result = chat(cfg, system, user, max_output)
            truncated = truncated or result.truncated
            findings.append((chunk.describe(), result.text))
        answer, reduce_calls = reduce_findings(cfg, question, findings, max_output)
        calls = len(chunks) + reduce_calls

    if not answer.strip():
        raise OllamaError("the model returned an empty response")
    answer, removed = validate_line_refs(answer, sources)

    elapsed = (now_ms() - started) / 1000
    title = ", ".join(f"{src.shown} ({len(src.lines):,} lines)" for src in sources)
    notes = [
        f"model {cfg.model}",
        f"~{est_input:,} tokens of source -> ~{estimate_tokens(answer):,} tokens",
        f"{len(chunks)} part{'s' if len(chunks) > 1 else ''}",
        f"{elapsed:.1f} s",
    ]
    if removed:
        notes.append(f"removed {removed} invalid line reference{'s' if removed > 1 else ''}")
    if truncated:
        notes.append("output hit --max-output and may be cut off")
    output = f"## local-shunt: {title}\n\n{answer}\n\n---\n" + " · ".join(notes)
    print(output)

    log_event({
        "session_id": os.environ.get("CLAUDE_SESSION_ID"),
        "event": "read",
        "files": [src.shown for src in sources],
        "lines": sum(len(src.lines) for src in sources),
        "bytes": sum(src.bytes for src in sources),
        "est_input_tokens": est_input,
        "est_output_tokens": estimate_tokens(output),
        "model": cfg.model,
        "chunks": len(chunks),
        "calls": calls,
        "invalid_refs_removed": removed,
        "latency_ms": int(elapsed * 1000),
        "outcome": "ok",
    })
    return 0


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


def cmd_write(args: argparse.Namespace, cfg: Config) -> int:
    started = now_ms()
    out = Path(os.path.expanduser(args.out)).resolve()
    if out.exists() and not args.force:
        raise UsageError(f"{args.out} already exists; pass --force to overwrite")
    if out.exists() and not out.is_file():
        raise UsageError(f"{args.out} is not a regular file")
    if not args.spec.strip():
        raise UsageError("--spec must not be empty")

    contexts = []
    for src in load_sources(args.context or [], cfg):
        contexts.append(f"=== CONTEXT FILE: {src.shown} ===\n" + "\n".join(src.lines))

    system = load_prompt("write.md")
    max_output = args.max_output or 4096
    user = f"Specification:\n{args.spec.strip()}\n\nTarget file: {display_path(out)}"
    if contexts:
        user += "\n\n" + "\n\n".join(contexts)
    needed = estimate_tokens(system + user) + max_output
    if needed > cfg.num_ctx:
        raise UsageError(
            f"prompt needs ~{needed:,} tokens but num_ctx is {cfg.num_ctx:,}; "
            "pass fewer or smaller --context files"
        )

    progress(f"asking {cfg.model} to write {display_path(out)}")
    result = chat(cfg, system, user, max_output)
    if result.truncated:
        raise OllamaError(
            f"output reached --max-output ({max_output} tokens) and is incomplete; nothing was written. "
            "Raise --max-output or split the file"
        )
    content = strip_code_fence(result.text)
    if not content.strip():
        raise OllamaError("the model returned an empty file; nothing was written")
    if not content.endswith("\n"):
        content += "\n"

    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(content, encoding="utf-8")
    line_count = content.count("\n")
    elapsed = (now_ms() - started) / 1000
    print(
        f"local-shunt: wrote {display_path(out)} ({line_count:,} lines) · model {cfg.model} · {elapsed:.1f} s\n"
        "Review the file and run the tests or linter before relying on it."
    )
    log_event({
        "session_id": os.environ.get("CLAUDE_SESSION_ID"),
        "event": "write",
        "files": [display_path(out)],
        "lines": line_count,
        "model": cfg.model,
        "latency_ms": int(elapsed * 1000),
        "outcome": "ok",
    })
    return 0


# ---------------------------------------------------------------- stats


def cmd_stats(args: argparse.Namespace, cfg: Config) -> int:
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
        print(f"No log yet ({log_file}).")
        return 0

    if args.since:
        records = [r for r in records if str(r.get("ts", ""))[:10] >= args.since]
    if args.session:
        records = [r for r in records if r.get("session_id") == args.session]

    hook = Counter(r["outcome"] for r in records if r.get("event") == "hook")
    reads = [r for r in records if r.get("event") == "read" and r.get("outcome") == "ok"]
    writes = [r for r in records if r.get("event") == "write" and r.get("outcome") == "ok"]
    errors = Counter(r["outcome"] for r in records if str(r.get("outcome", "")).startswith("error"))

    print(f"Log: {log_file}  ({len(records):,} records)")
    print("\nHook decisions")
    if hook:
        for outcome, count in hook.most_common():
            print(f"  {outcome:<32} {count:>6,}")
    else:
        print("  (none)")

    print("\nDelegated reads")
    if reads:
        est_in = sum(r.get("est_input_tokens", 0) for r in reads)
        est_out = sum(r.get("est_output_tokens", 0) for r in reads)
        latency = sum(r.get("latency_ms", 0) for r in reads) / len(reads) / 1000
        saved = 1 - est_out / est_in if est_in else 0
        print(f"  count                            {len(reads):>6,}")
        print(f"  est. source tokens               {est_in:>6,}")
        print(f"  est. summary tokens              {est_out:>6,}")
        print(f"  est. saving                      {saved:>6.1%}")
        print(f"  average latency                  {latency:>6.1f} s")
    else:
        print("  (none)")

    print(f"\nDelegated writes                   {len(writes):>6,}")
    if errors:
        print("\nErrors")
        for outcome, count in errors.most_common():
            print(f"  {outcome:<32} {count:>6,}")
    print("\nToken counts are estimates, not Claude's billed tokens.")
    return 0


# ---------------------------------------------------------------- main


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="shunt.py", description="Delegate file reading and writing to a local Ollama model.")
    sub = parser.add_subparsers(dest="command", required=True)

    read = sub.add_parser("read", help="answer a question about one or more files")
    read.add_argument("files", nargs="+")
    read.add_argument("--question", "-q", required=True)
    read.add_argument("--model")
    read.add_argument("--max-output", type=int)

    write = sub.add_parser("write", help="generate a boilerplate file from a specification")
    write.add_argument("--out", required=True)
    write.add_argument("--spec", required=True)
    write.add_argument("--context", nargs="*")
    write.add_argument("--force", action="store_true")
    write.add_argument("--model")
    write.add_argument("--max-output", type=int)

    stats = sub.add_parser("stats", help="summarize the local-shunt log")
    stats.add_argument("--since", help="YYYY-MM-DD")
    stats.add_argument("--session")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    cfg = load_config()
    if getattr(args, "model", None):
        cfg.model = args.model
    handler = {"read": cmd_read, "write": cmd_write, "stats": cmd_stats}[args.command]
    try:
        return handler(args, cfg)
    except UsageError as e:
        print(f"local-shunt: {e}", file=sys.stderr)
        return EXIT_USAGE
    except OllamaError as e:
        print(
            f"local-shunt: {e}\n"
            "Fall back to reading the file yourself with Read and offset/limit.",
            file=sys.stderr,
        )
        log_event({
            "session_id": os.environ.get("CLAUDE_SESSION_ID"),
            "event": args.command,
            "model": cfg.model,
            "outcome": f"error:{type(e).__name__}",
            "detail": str(e)[:200],
        })
        return EXIT_FAILURE


if __name__ == "__main__":
    sys.exit(main())
