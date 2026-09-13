"""Tests for worker helpers that do not need Ollama."""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

from shunt import SourceFile, build_chunks, strip_code_fence, validate_line_refs  # noqa: E402


def source(name: str, count: int) -> SourceFile:
    return SourceFile(shown=name, lines=[f"value_{i} = {i}" for i in range(1, count + 1)], bytes=0)


class ChunkTest(unittest.TestCase):
    def test_small_files_share_a_chunk(self):
        chunks = build_chunks([source("a.py", 10), source("b.py", 10)], budget_tokens=10_000)
        self.assertEqual(len(chunks), 1)
        self.assertEqual(chunks[0].describe(), "a.py L1-L10, b.py L1-L10")

    def test_large_file_splits_with_overlap_and_covers_every_line(self):
        src = source("big.py", 2000)
        chunks = build_chunks([src], budget_tokens=2000)
        self.assertGreater(len(chunks), 1)
        covered = set()
        for i, chunk in enumerate(chunks):
            (_, first, last), = chunk.parts
            covered.update(range(first, last + 1))
            if i:
                previous_last = chunks[i - 1].parts[0][2]
                self.assertEqual(previous_last - first + 1, 50)
        self.assertEqual(covered, set(range(1, 2001)))

    def test_render_numbers_lines(self):
        chunk = build_chunks([source("a.py", 3)], budget_tokens=1000)[0]
        self.assertIn("L2: value_2 = 2", chunk.render())


class LineRefTest(unittest.TestCase):
    def test_removes_out_of_range_refs(self):
        text = (
            "### Answer\n"
            "- `handler` is defined (L10-L20).\n"
            "- `ghost` is defined (L900).\n"
            "### Relevant locations\n"
            "- `L10-L20`: handler\n"
            "- `L950-L990`: ghost\n"
        )
        cleaned, removed = validate_line_refs(text, [source("a.py", 100)])
        self.assertEqual(removed, 2)
        self.assertIn("(L10-L20)", cleaned)
        self.assertIn("(L?)", cleaned)
        self.assertNotIn("L950", cleaned)

    def test_path_prefixed_refs_use_that_file(self):
        text = "- (small.py:L50) and (big.py:L50)"
        cleaned, removed = validate_line_refs(text, [source("small.py", 10), source("big.py", 100)])
        self.assertEqual(removed, 1)
        self.assertIn("big.py:L50", cleaned)

    def test_ignores_words_containing_l_digits(self):
        cleaned, removed = validate_line_refs("HTML5 and URL2 are fine", [source("a.py", 1)])
        self.assertEqual(removed, 0)


class FenceTest(unittest.TestCase):
    def test_strips_outer_fence(self):
        self.assertEqual(strip_code_fence("```python\nx = 1\n```"), "x = 1")

    def test_single_block_with_prose(self):
        self.assertEqual(strip_code_fence("Here it is:\n```\nx = 1\n```\nDone."), "x = 1")

    def test_plain_text_untouched(self):
        self.assertEqual(strip_code_fence("x = 1\n"), "x = 1")


if __name__ == "__main__":
    unittest.main()
