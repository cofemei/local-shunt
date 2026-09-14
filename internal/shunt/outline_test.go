package shunt

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// dedent removes the common leading indentation, like textwrap.dedent.
func dedent(text string) string {
	lines := strings.Split(text, "\n")
	common := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if common < 0 || indent < common {
			common = indent
		}
	}
	if common <= 0 {
		return text
	}
	for i, line := range lines {
		if len(line) >= common {
			lines[i] = line[common:]
		} else {
			lines[i] = strings.TrimLeft(line, " \t")
		}
	}
	return strings.Join(lines, "\n")
}

func outlineRows(source, suffix string, maxChars int) []string {
	text := buildOutline(splitLines(dedent(source)), suffix, maxChars)
	var rows []string
	for _, row := range splitLines(text) {
		rows = append(rows, strings.Join(strings.Fields(row), " "))
	}
	return rows
}

func expectRows(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPythonOutline(t *testing.T) {
	rows := outlineRows(`
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
	`, ".py", 3000)
	expectRows(t, rows, []string{
		"L5 CHUNK_OVERLAP = 50",
		"L8-L15 class Chunk:",
		"L9-L10 def render(self):",
		"L12-L15 def describe(self):",
		"L13-L14 def inner():",
		"L17-L20 async def fetch(",
		"L23-L24 def last():",
	})
	equal(t, buildOutline([]string{"just", "text"}, ".txt", 3000), "")
	equal(t, buildOutline([]string{"# comment", "x = 1", "LIMIT == 3"}, ".py", 3000), "")
}

func TestLongSignaturesAreShortened(t *testing.T) {
	rows := outlineRows("def f("+strings.Repeat("a, ", 60)+"):\n    pass\n", ".py", 3000)
	equal(t, len(rows), 1)
	equal(t, strings.HasSuffix(rows[0], "…"), true)
	equal(t, utf8.RuneCountInString(rows[0]) < 130, true)
}

func TestOtherLanguagesOutline(t *testing.T) {
	expectRows(t, outlineRows(`
		export async function load(path) {
		  return read(path);
		}

		export const parse = (text) => {
		  return JSON.parse(text);
		};

		interface Options {
		  strict: boolean;
		}
	`, ".ts", 3000), []string{
		"L2-L4 export async function load(path) {",
		"L6-L8 export const parse = (text) => {",
		"L10-L12 interface Options {",
	})
	expectRows(t, outlineRows("func (s *Server) Start() error {\n\treturn nil\n}\n", ".go", 3000),
		[]string{"L1-L3 func (s *Server) Start() error {"})
	expectRows(t, outlineRows("pub(crate) fn run() {\n    todo!()\n}\nimpl Foo {\n}\n", ".rs", 3000),
		[]string{"L1-L3 pub(crate) fn run() {", "L4-L5 impl Foo {"})
	expectRows(t, outlineRows("class Foo\n  def bar\n  end\nend\n", ".rb", 3000),
		[]string{"L1-L4 class Foo", "L2-L3 def bar"})
}

func TestMarkdownOutline(t *testing.T) {
	expectRows(t, outlineRows("# Title\n\n## Install\n\n```bash\n# not a heading\n```\n\n### Linux\ntext\n\n## Usage\ntext\n", ".md", 3000), []string{
		"L1-L13 # Title",
		"L3-L10 ## Install",
		"L9-L10 ### Linux",
		"L12-L13 ## Usage",
	})
}

func TestOutlineDropsDeepestEntriesFirst(t *testing.T) {
	var src strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&src, "class C%d:\n", i)
		for j := 0; j < 10; j++ {
			fmt.Fprintf(&src, "    def m%d(self):\n        pass\n", j)
		}
	}
	equal(t, len(outlineRows(src.String(), ".py", 100_000)), 220)
	limited := buildOutline(splitLines(src.String()), ".py", 1000)
	equal(t, utf8.RuneCountInString(limited) <= 1000, true)
	notContain(t, limited, "def m0")
	contain(t, limited, "class C19:")
	equal(t, strings.HasSuffix(limited, "200 more entries not shown"), true)
}

func TestOutlineTruncatesTopLevel(t *testing.T) {
	var src strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&src, "def function_number_%d():\n    pass\n", i)
	}
	rows := splitLines(buildOutline(splitLines(src.String()), ".py", 500))
	if len(rows) <= 5 {
		t.Fatalf("only %d rows", len(rows))
	}
	contain(t, rows[0], "def function_number_0():")
	equal(t, rows[len(rows)-1], fmt.Sprintf("… %d more entries not shown", 200-(len(rows)-1)))
}
