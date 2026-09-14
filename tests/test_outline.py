"""Tests for the file outline shown in denial messages."""

from __future__ import annotations

import textwrap
import unittest

from helpers import SCRIPTS  # noqa: F401  (puts scripts/ on sys.path)

from outline import build_outline


def outline(source: str, suffix: str = ".py", **kwargs) -> list[str]:
    text = build_outline(textwrap.dedent(source).splitlines(), suffix, **kwargs)
    return [" ".join(row.split()) for row in text.splitlines()]


class PythonOutlineTest(unittest.TestCase):
    def test_definitions_constants_and_ranges(self):
        rows = outline('''\
            """Doc."""
            import os

            CHUNK_OVERLAP = 50
            timeout = 3

            class Chunk:
                def render(self):
                    return ""

                def describe(self):
                    def inner():
                        pass
                    return inner

            async def fetch(
                url: str,
            ) -> str:
                return url


            def last():
                pass
        ''')
        self.assertEqual(rows, [
            "L4 CHUNK_OVERLAP = 50",
            "L7-L14 class Chunk:",
            "L8-L9 def render(self):",
            "L11-L14 def describe(self):",
            "L12-L13 def inner():",
            "L16-L19 async def fetch(",
            "L22-L23 def last():",
        ])

    def test_no_definitions(self):
        self.assertEqual(build_outline(["just", "text"], ".txt"), "")

    def test_long_signatures_are_shortened(self):
        (row,) = outline("def f(" + "a, " * 60 + "):\n    pass\n")
        self.assertTrue(row.endswith("…"))
        self.assertLess(len(row), 130)


class OtherLanguagesTest(unittest.TestCase):
    def test_braces_and_end_belong_to_the_block(self):
        rows = outline('''\
            export async function load(path) {
              return read(path);
            }

            export const parse = (text) => {
              return JSON.parse(text);
            };

            interface Options {
              strict: boolean;
            }
        ''', ".ts")
        self.assertEqual(rows, [
            "L1-L3 export async function load(path) {",
            "L5-L7 export const parse = (text) => {",
            "L9-L11 interface Options {",
        ])

    def test_go_rust_and_ruby(self):
        self.assertEqual(outline("func (s *Server) Start() error {\n\treturn nil\n}\n", ".go"),
                         ["L1-L3 func (s *Server) Start() error {"])
        self.assertEqual(outline("pub(crate) fn run() {\n    todo!()\n}\nimpl Foo {\n}\n", ".rs"),
                         ["L1-L3 pub(crate) fn run() {", "L4-L5 impl Foo {"])
        self.assertEqual(outline("class Foo\n  def bar\n  end\nend\n", ".rb"),
                         ["L1-L4 class Foo", "L2-L3 def bar"])


class MarkdownOutlineTest(unittest.TestCase):
    def test_headings_and_sections(self):
        rows = outline('''\
            # Title

            ## Install

            ```bash
            # not a heading
            ```

            ### Linux
            text

            ## Usage
            text
        ''', ".md")
        self.assertEqual(rows, [
            "L1-L13 # Title",
            "L3-L10 ## Install",
            "L9-L10 ### Linux",
            "L12-L13 ## Usage",
        ])

    def test_python_comments_are_not_headings(self):
        self.assertEqual(build_outline(["# comment", "x = 1"], ".py"), "")


class BudgetTest(unittest.TestCase):
    def test_deepest_entries_are_dropped_first(self):
        source = "".join(
            f"class C{i}:\n" + "".join(f"    def m{j}(self):\n        pass\n" for j in range(10))
            for i in range(20)
        )
        full = outline(source, max_chars=100_000)
        limited = build_outline(source.splitlines(), ".py", max_chars=1000)
        self.assertEqual(len(full), 220)
        self.assertLessEqual(len(limited), 1000)
        self.assertNotIn("def m0", limited)
        self.assertIn("class C19:", limited)
        self.assertTrue(limited.endswith("200 more entries not shown"))

    def test_truncates_when_top_level_alone_is_too_long(self):
        source = "".join(f"def function_number_{i}():\n    pass\n" for i in range(200))
        rows = build_outline(source.splitlines(), ".py", max_chars=500).splitlines()
        self.assertLessEqual(sum(len(r) + 1 for r in rows), 500 + len(rows[-1]) + 1)
        self.assertGreater(len(rows), 5)
        self.assertIn("def function_number_0():", rows[0])
        self.assertEqual(rows[-1], f"… {200 - (len(rows) - 1)} more entries not shown")


if __name__ == "__main__":
    unittest.main()
