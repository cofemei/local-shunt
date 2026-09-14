"""Build a short outline of a text file: definitions and headings with their line ranges.

The hook puts the outline in its denial message, so Claude can Read just the relevant
range, or answer from the outline, without another round trip. No model is involved:
the outline comes from line patterns and indentation, so it is fast but approximate.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

MARKDOWN_EXTENSIONS = {".md", ".markdown", ".mdx", ".rst"}
MAX_TEXT_CHARS = 110

# Definitions in common languages, matched at the start of a line.
DEFINITION = re.compile(
    r"^(?:"
    r"(?:async\s+)?def\s+\w+"  # Python
    r"|class\s+\w+"  # Python, JS/TS, Ruby
    r"|module\s+\w+"  # Ruby
    r"|(?:export\s+)?(?:default\s+)?(?:async\s+)?function\*?\s+\w+"  # JS/TS, shell
    r"|(?:export\s+)?(?:abstract\s+)?(?:class|interface|enum)\s+\w+"  # TS
    r"|(?:export\s+)?type\s+\w+\s*(?:<[^>]*>)?\s*="  # TS
    r"|(?:export\s+)?(?:const|let|var)\s+\w+\s*=\s*(?:async\s*)?(?:\([^)]*\)|\w+)\s*=>"  # JS/TS arrow functions
    r"|func\s+(?:\([^)]*\)\s*)?\w+"  # Go
    r"|type\s+\w+\s+(?:struct|interface)"  # Go
    r"|(?:pub(?:\([\w:]+\))?\s+)?(?:async\s+)?(?:unsafe\s+)?(?:fn|struct|enum|trait|mod)\s+\w+"  # Rust
    r"|impl\b"  # Rust
    r"|(?:public|private|protected|internal)\s+(?:static\s+|final\s+|abstract\s+|sealed\s+)*"
    r"(?:class|interface|enum|record)\s+\w+"  # Java, C#, Kotlin
    r"|\w+\s*\(\)\s*\{"  # shell
    r")"
)
# Top-level constants such as `CHUNK_OVERLAP_LINES = 50`: their value often answers a question.
CONSTANT = re.compile(r"^(?:export\s+)?(?:const\s+|final\s+)?[A-Z][A-Z0-9_]{2,}\s*(?::[^=]+)?=(?!=)")
HEADING = re.compile(r"^(#{1,6})\s+\S")
FENCE = re.compile(r"^\s*(```|~~~)")


@dataclass
class Entry:
    first: int  # 1-based
    last: int
    depth: int
    text: str


def _indent(line: str) -> int:
    expanded = line.expandtabs(4)
    return len(expanded) - len(expanded.lstrip())


def _block_end(lines: list[str], start: int, indent: int) -> int:
    """1-based last line of the block whose header is lines[start], judged by indentation."""
    last = start
    for j in range(start + 1, len(lines)):
        stripped = lines[j].strip()
        if not stripped:
            continue
        if _indent(lines[j]) > indent or stripped[0] in ")]":
            last = j  # body, or the end of a multi-line signature
            continue
        if stripped[0] == "}" or stripped == "end" or stripped.startswith("end "):
            return j + 1  # closing brace or Ruby `end` belongs to the block
        break
    return last + 1


def _entries(lines: list[str], markdown: bool) -> list[Entry]:
    entries: list[Entry] = []
    if markdown:
        headings: list[tuple[int, int, str]] = []  # (line index, level, text)
        in_fence = False
        for i, line in enumerate(lines):
            if FENCE.match(line):
                in_fence = not in_fence
            elif not in_fence and (m := HEADING.match(line)):
                headings.append((i, len(m.group(1)), line.strip()))
        for n, (i, level, text) in enumerate(headings):
            # A section ends before the next heading at the same or a higher level.
            end = next((j for j, next_level, _ in headings[n + 1:] if next_level <= level), len(lines))
            while end > i + 1 and not lines[end - 1].strip():
                end -= 1
            entries.append(Entry(i + 1, end, level - 1, text))
    else:
        found: list[tuple[int, int, str, bool]] = []  # (line index, indent, text, is definition)
        for i, line in enumerate(lines):
            stripped = line.strip()
            if not stripped:
                continue
            indent = _indent(line)
            if DEFINITION.match(stripped):
                found.append((i, indent, stripped, True))
            elif indent == 0 and CONSTANT.match(stripped):
                found.append((i, 0, stripped, False))
        # Turn indentation into nesting depth: 0 for top level, 1 for methods, ...
        rank = {indent: n for n, indent in enumerate(sorted({indent for _, indent, _, _ in found}))}
        for i, indent, text, definition in found:
            end = _block_end(lines, i, indent) if definition else i + 1
            entries.append(Entry(i + 1, end, rank[indent], text))

    for entry in entries:
        if len(entry.text) > MAX_TEXT_CHARS:
            entry.text = entry.text[: MAX_TEXT_CHARS - 1] + "…"
    return entries


def build_outline(lines: list[str], suffix: str, max_chars: int = 3000) -> str:
    """Return outline lines such as `L126-L167  def build_chunks(...)`, or "" when nothing matched.

    When the outline exceeds max_chars, the deepest entries are dropped first.
    """
    entries = _entries(lines, suffix.lower() in MARKDOWN_EXTENSIONS)
    if not entries:
        return ""

    def render(items: list[Entry]) -> list[str]:
        rows = []
        for e in items:
            where = f"L{e.first}" if e.first == e.last else f"L{e.first}-L{e.last}"
            rows.append(f"{where:<12} {'  ' * e.depth}{e.text}")
        return rows

    kept = entries
    for max_depth in sorted({e.depth for e in entries if e.depth > 0}, reverse=True):
        if sum(len(r) + 1 for r in render(kept)) <= max_chars:
            break
        kept = [e for e in kept if e.depth < max_depth]
    rows = render(kept)
    omitted = len(entries) - len(kept)
    while rows and sum(len(r) + 1 for r in rows) > max_chars:
        rows.pop()
        omitted += 1
    if omitted:
        rows.append(f"… {omitted} more entries not shown")
    return "\n".join(rows)
