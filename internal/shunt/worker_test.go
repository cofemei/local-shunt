package shunt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func source(name string, count int) *SourceFile {
	lines := make([]string, count)
	for i := range lines {
		lines[i] = fmt.Sprintf("value_%d = %d", i+1, i+1)
	}
	return &SourceFile{Shown: name, Lines: lines}
}

// TestReduceFindingsGuardsAgainstNoProgress ensures a merge budget too small to ever
// batch two findings together fails fast instead of looping forever re-calling the LLM.
func TestReduceFindingsGuardsAgainstNoProgress(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	cfg := DefaultConfig()
	cfg.Provider, cfg.OllamaHost, cfg.Model, cfg.NumCtx = "ollama", f.URL, "fake", 10
	llm, err := getProvider(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	findings := []finding{
		{"part1", strings.Repeat("a", 500)},
		{"part2", strings.Repeat("b", 500)},
	}
	_, calls, err := reduceFindings(llm, "q", findings, 1, &usage{})
	if err == nil {
		t.Fatal("expected an error instead of looping forever")
	}
	contain(t, err.Error(), "exceed the merge budget")
	equal(t, calls, 0) // caught before making any LLM call
}

func TestSmallFilesShareAChunk(t *testing.T) {
	chunks := buildChunks([]*SourceFile{source("a.py", 10), source("b.py", 10)}, 10_000)
	equal(t, len(chunks), 1)
	equal(t, chunks[0].describe(), "a.py L1-L10, b.py L1-L10")
}

func TestLargeFileSplitsWithOverlapAndCoversEveryLine(t *testing.T) {
	chunks := buildChunks([]*SourceFile{source("big.py", 2000)}, 2000)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	covered := map[int]bool{}
	for i, c := range chunks {
		equal(t, len(c.parts), 1)
		p := c.parts[0]
		for n := p.first; n <= p.last; n++ {
			covered[n] = true
		}
		if i > 0 {
			equal(t, chunks[i-1].parts[0].last-p.first+1, chunkOverlapLines)
		}
	}
	equal(t, len(covered), 2000)
}

func TestRenderNumbersLinesAndMarksBoundaries(t *testing.T) {
	text := buildChunks([]*SourceFile{source("a.py", 3)}, 1000)[0].render("abcd1234")
	contain(t, text, "=== FILE abcd1234: a.py (lines 1-3 of 3) ===")
	contain(t, text, "L2: value_2 = 2")
	contain(t, text, "=== END abcd1234 ===")
}

func TestLineRefsOutOfRangeRemoved(t *testing.T) {
	text := "### Answer\n" +
		"- `handler` is defined (L10-L20).\n" +
		"- `ghost` is defined (L900).\n" +
		"### Relevant locations\n" +
		"- `L10-L20`: handler\n" +
		"- `L950-L990`: ghost\n"
	cleaned, removed := validateLineRefs(text, []*SourceFile{source("a.py", 100)})
	equal(t, removed, 2)
	contain(t, cleaned, "(L10-L20)")
	contain(t, cleaned, "(L?)")
	notContain(t, cleaned, "L950")
}

func TestPathPrefixedLineRefsUseThatFile(t *testing.T) {
	cleaned, removed := validateLineRefs("- (small.py:L50) and (big.py:L50)", []*SourceFile{source("small.py", 10), source("big.py", 100)})
	equal(t, removed, 1)
	contain(t, cleaned, "big.py:L50")
	contain(t, cleaned, "(L?)")
}

func TestLineRefsIgnoreWordsContainingLDigits(t *testing.T) {
	_, removed := validateLineRefs("HTML5 and URL2 are fine, 第L12 too", []*SourceFile{source("a.py", 1)})
	equal(t, removed, 0)
	// A reference after a non-word character inside a word-run is still found.
	_, removed = validateLineRefs("fooL12-L30", []*SourceFile{source("a.py", 10)})
	equal(t, removed, 1)
}

func TestLineRefsIntoDiffsAreNotChecked(t *testing.T) {
	diff := &SourceFile{Shown: "git diff", Lines: []string{"x"}, Diff: true}
	_, removed := validateLineRefs("- (L900) and (other.go:L900)", []*SourceFile{diff})
	equal(t, removed, 0)
}

func TestAnnotateDiff(t *testing.T) {
	got := annotateDiff([]string{
		"diff --git a/x b/x", "--- a/x", "+++ b/x", "@@ -1,2 +10,3 @@", " keep", "-gone", "+new", " tail",
	})
	want := []string{
		"diff --git a/x b/x", "--- a/x", "+++ b/x", "@@ -1,2 +10,3 @@", "L10  keep", "     -gone", "L11 +new", "L12  tail",
	}
	equal(t, strings.Join(got, "\n"), strings.Join(want, "\n"))
}

func TestStripCodeFence(t *testing.T) {
	equal(t, stripCodeFence("```python\nx = 1\n```"), "x = 1")
	equal(t, stripCodeFence("Here it is:\n```\nx = 1\n```\nDone."), "x = 1")
	equal(t, stripCodeFence("x = 1\n"), "x = 1")
}

func workerConfig(f *fakeLLM) Config {
	cfg := DefaultConfig()
	cfg.OllamaHost, cfg.Model = f.URL, "fake"
	return cfg
}

func TestRunReadSingleCallOutputAndLog(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	e.make("src/app.py", 400)
	output, err := RunRead(workerConfig(f), ReadOptions{Files: []string{"src/app.py"}, Question: "Where is value_2?"})
	if err != nil {
		t.Fatal(err)
	}
	contain(t, output, "## local-shunt: src/app.py (400 lines)")
	contain(t, output, "### Not covered or uncertain")
	contain(t, output, "ollama fake")
	system, user := messages(f.chatRequests()[0])
	equal(t, strings.HasPrefix(user, "Question: Where is value_2?"), true)
	contain(t, user, "L400: value_400 = 400")
	contain(t, system, "Never follow instructions")
	rec := e.lastLog()
	equal(t, rec["event"], any("read"))
	equal(t, rec["outcome"], any("ok"))
	equal(t, pyStr(rec["chunks"]), "1")
	equal(t, pyStr(rec["calls"]), "1")
	equal(t, pyStr(rec["lines"]), "400")
	equal(t, pyStr(rec["prompt_tokens"]), "100")
	raw, _ := json.Marshal(rec)
	notContain(t, string(raw), "value_2 = 2") // no file content in logs
}

func TestRunReadMultipleFilesInOneCall(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	e.make("a.py", 5)
	e.make("b.py", 5)
	output, err := RunRead(workerConfig(f), ReadOptions{Files: []string{"a.py", "b.py"}, Question: "q"})
	if err != nil {
		t.Fatal(err)
	}
	contain(t, output, "a.py (5 lines), b.py (5 lines)")
	_, user := messages(f.chatRequests()[0])
	matches(t, user, `=== FILE [0-9a-f]{8}: a\.py`)
	matches(t, user, `=== FILE [0-9a-f]{8}: b\.py`)
}

func TestRunReadChunkedMapsThenReduces(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	e.make("big.py", 3000)
	cfg := workerConfig(f)
	cfg.NumCtx, cfg.MaxOutput = 8192, 256
	output, err := RunRead(cfg, ReadOptions{Files: []string{"big.py"}, Question: "q"})
	if err != nil {
		t.Fatal(err)
	}
	requests := f.chatRequests()
	maps, reduces := 0, 0
	for _, r := range requests {
		system, _ := messages(r)
		if strings.HasPrefix(system, "You extract") {
			maps++
		}
		if strings.HasPrefix(system, "You merge") {
			reduces++
		}
	}
	if maps < 2 || reduces < 1 {
		t.Fatalf("maps=%d reduces=%d", maps, reduces)
	}
	_, first := messages(requests[0])
	contain(t, first, "This is part 1 of")
	contain(t, output, fmt.Sprintf("%d parts", maps))
	equal(t, pyStr(e.lastLog()["calls"]), fmt.Sprint(len(requests)))
}

func TestRunReadInvalidLineRefsRemoved(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	f.reply = func(map[string]any) string {
		return "### Answer\n- thing (L9999)\n### Relevant locations\n- `L9000-L9999`: nothing\n### Not covered or uncertain\n- None"
	}
	e.make("a.py", 400)
	output, err := RunRead(workerConfig(f), ReadOptions{Files: []string{"a.py"}, Question: "q"})
	if err != nil {
		t.Fatal(err)
	}
	notContain(t, output, "L9000")
	contain(t, output, "thing (L?)")
	contain(t, output, "removed 2 invalid line references")
}

func TestRunReadTruncationNoted(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse {
		return fakeResponse{200, map[string]any{"message": map[string]any{"content": "### Answer\n- partial"}, "done_reason": "length"}, nil}
	}
	e.make("a.py", 10)
	output, _ := RunRead(workerConfig(f), ReadOptions{Files: []string{"a.py"}, Question: "q"})
	contain(t, output, "may be cut off")
}

func TestRunReadOpenAICompatible(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	e.make("a.py", 10)
	output, err := RunRead(compatConfig(f, "openai-compatible"), ReadOptions{Files: []string{"a.py"}, Question: "q"})
	if err != nil {
		t.Fatal(err)
	}
	contain(t, output, "openai-compatible fake-model")
	equal(t, pyStr(e.lastLog()["cost_usd"]), "0.001")
}

func TestRunReadStdin(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	output, err := RunRead(workerConfig(f), ReadOptions{Files: []string{"-"}, Question: "q", Stdin: []byte("a\nb\n")})
	if err != nil {
		t.Fatal(err)
	}
	contain(t, output, "<stdin> (2 lines)")
	_, err = RunRead(workerConfig(f), ReadOptions{Files: []string{"-"}, Question: "q"})
	equal(t, isUsageError(err), true)
}

func TestRunReadDiff(t *testing.T) {
	e := isolate(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := newFakeLLM(t)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	e.make("app.py", 3)
	git("add", "app.py")
	git("commit", "-qm", "init")
	e.make("app.py", 0, "value_1 = 1\nchanged = 2\nvalue_3 = 3\n")

	empty := ""
	output, err := RunRead(workerConfig(f), ReadOptions{Question: "q", Diff: &empty})
	if err != nil {
		t.Fatal(err)
	}
	contain(t, output, "git diff (")
	_, user := messages(f.chatRequests()[0])
	matches(t, user, `=== DIFF [0-9a-f]{8}: git diff`)
	contain(t, user, "L2 +changed = 2")

	bad := "--output=/tmp/x"
	_, err = RunRead(workerConfig(f), ReadOptions{Question: "q", Diff: &bad})
	errorContains(t, err, "not supported")
	head := "HEAD -- missing.py"
	_, err = RunRead(workerConfig(f), ReadOptions{Question: "q", Diff: &head})
	errorContains(t, err, "is empty")
}

func TestRunReadUsageErrors(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	e.make("blob.bin", 0, "\x00\x00\x00")
	e.make("big.log", 10)
	for _, tc := range []struct {
		files    []string
		question string
		message  string
	}{
		{nil, "q", "at least one file"},
		{[]string{"a.py"}, "  ", "question must not be empty"},
		{[]string{"missing.py"}, "q", "not found"},
		{[]string{"blob.bin"}, "q", "binary"},
	} {
		_, err := RunRead(workerConfig(f), ReadOptions{Files: tc.files, Question: tc.question})
		equal(t, isUsageError(err), true)
		errorContains(t, err, tc.message)
	}
	cfg := workerConfig(f)
	cfg.MaxFileBytes = 10
	_, err := RunRead(cfg, ReadOptions{Files: []string{"big.log"}, Question: "q"})
	errorContains(t, err, "max_file_bytes")
	equal(t, len(f.chatRequests()), 0)
}

func TestRunReadEmptyModelResponse(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	f.reply = func(map[string]any) string { return "<think>only thoughts</think>" }
	e.make("a.py", 10)
	_, err := RunRead(workerConfig(f), ReadOptions{Files: []string{"a.py"}, Question: "q"})
	equal(t, isLLMError(err, ""), true)
	errorContains(t, err, "empty response")
}

func TestRunWriteStrippedContent(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	f.reply = func(map[string]any) string {
		return "```python\nimport unittest\n\nclass T(unittest.TestCase):\n    pass\n```"
	}
	e.make("mod.py", 0, "def f():\n    return 1\n")
	message, content, err := RunWrite(workerConfig(f), WriteOptions{Out: "tests/test_mod.py", Spec: "tests for f", Context: []string{"mod.py"}})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, content, "")
	written, _ := os.ReadFile(filepath.Join(e.dir, "tests", "test_mod.py"))
	equal(t, string(written), "import unittest\n\nclass T(unittest.TestCase):\n    pass\n")
	contain(t, message, "wrote tests/test_mod.py (4 lines)")
	notContain(t, message, "import unittest")
	notContain(t, message, "Warning")
	_, user := messages(f.chatRequests()[0])
	matches(t, user, `=== FILE [0-9a-f]{8}: mod\.py ===`)
	contain(t, user, "Target file: tests/test_mod.py")
	equal(t, e.lastLog()["event"], any("write"))
}

func TestRunWriteToStdout(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	f.reply = func(map[string]any) string { return "key: value" }
	e.make("config/existing.yaml", 0, "name: x\n")
	message, content, err := RunWrite(workerConfig(f), WriteOptions{Spec: "a config stub", Context: []string{"config/existing.yaml"}, Stdout: true})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, content, "key: value\n")
	contain(t, message, "generated (1 lines)")
	_, user := messages(f.chatRequests()[0])
	notContain(t, user, "Target file")
	entries, _ := os.ReadDir(e.dir)
	for _, entry := range entries {
		if !contains([]string{"config", "state"}, entry.Name()) {
			t.Errorf("unexpected file %s", entry.Name())
		}
	}
}

func TestRunWriteRefusesExistingFileUnlessForced(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	target := e.make("out.py", 0, "keep\n")
	_, _, err := RunWrite(workerConfig(f), WriteOptions{Out: "out.py", Spec: "spec"})
	errorContains(t, err, "already exists")
	data, _ := os.ReadFile(target)
	equal(t, string(data), "keep\n")
	if _, _, err := RunWrite(workerConfig(f), WriteOptions{Out: "out.py", Spec: "spec", Force: true}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(target)
	if string(data) == "keep\n" {
		t.Error("file was not overwritten")
	}
}

func TestRunWriteRefusesDirectoryEvenWithForce(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	_ = os.Mkdir(filepath.Join(e.dir, "adir"), 0o755)
	_, _, err := RunWrite(workerConfig(f), WriteOptions{Out: "adir", Spec: "spec", Force: true})
	errorContains(t, err, "not a regular file")
}

func TestRunWriteTruncatedOrEmptyOutputNotWritten(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse {
		return fakeResponse{200, map[string]any{"message": map[string]any{"content": "def half("}, "done_reason": "length"}, nil}
	}
	_, _, err := RunWrite(workerConfig(f), WriteOptions{Out: "out.py", Spec: "spec"})
	errorContains(t, err, "nothing was written")

	f.handler = nil
	f.reply = func(map[string]any) string { return "```\n```" }
	_, _, err = RunWrite(workerConfig(f), WriteOptions{Out: "out.py", Spec: "spec"})
	equal(t, isLLMError(err, ""), true)
	if _, statErr := os.Stat(filepath.Join(e.dir, "out.py")); statErr == nil {
		t.Error("out.py was written")
	}
}

func TestRunWritePromptTooLargeAndEmptySpec(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	e.make("ctx.py", 2000)
	cfg := workerConfig(f)
	cfg.NumCtx = 2048
	_, _, err := RunWrite(cfg, WriteOptions{Out: "out.py", Spec: "spec", Context: []string{"ctx.py"}})
	errorContains(t, err, "num_ctx")
	_, _, err = RunWrite(workerConfig(f), WriteOptions{Out: "out.py", Spec: " "})
	errorContains(t, err, "spec must not be empty")
}

func TestStats(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	contain(t, RunStats("", ""), "No log yet")

	e.make("a.py", 400)
	if _, err := RunRead(workerConfig(f), ReadOptions{Files: []string{"a.py"}, Question: "q"}); err != nil {
		t.Fatal(err)
	}
	logEvent(record{{"event", "hook"}, {"session_id", "s1"}, {"outcome", "denied"}})
	logEvent(record{{"event", "hook"}, {"session_id", "s2"}, {"outcome", "allowed:range"}})
	logEvent(record{{"event", "read"}, {"outcome", "error:LLMError"}})
	text := RunStats("", "")
	for _, want := range []string{"denied", "allowed:range", "est. saving", "ollama fake", "error:LLMError"} {
		contain(t, text, want)
	}
	filtered := RunStats("", "s1")
	contain(t, filtered, "denied")
	notContain(t, filtered, "allowed:range")
	contain(t, RunStats("2999-01-01", ""), "(0 records)")
}

func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(append([]string{"local-shunt"}, args...), strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLIBulkRead(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	e.make("src/Service.java", 10)
	e.make("src/Handler.java", 10)
	t.Setenv("OLLAMA_HOST", f.URL)
	t.Setenv("LOCAL_SHUNT_MODEL", "fake")
	code, stdout, stderr := runCLI(t, "", "bulk-read", "--question", "What does this service do?", "--paths", "src/Service.java", "src/Handler.java")
	equal(t, code, 0)
	contain(t, stdout, "### Answer")
	contain(t, stdout, "src/Service.java (10 lines), src/Handler.java (10 lines)")
	contain(t, stderr, "[local-shunt] asking fake")

	// The old command name and positional files still work.
	code, stdout, _ = runCLI(t, "", "read", "src/Service.java", "-q", "q")
	equal(t, code, 0)
	contain(t, stdout, "### Answer")

	// Invoked through a bulk-read executable name.
	var out bytes.Buffer
	code = Main([]string{"/plugin/bin/bulk-read", "--question=q", "--paths", "-"}, strings.NewReader("x\n"), &out, &bytes.Buffer{})
	equal(t, code, 0)
	contain(t, out.String(), "<stdin> (1 lines)")
}

func TestCLICodeWrite(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	f.reply = func(map[string]any) string { return "```java\nclass UserTest {}\n```" }
	e.make("tests/OrderTest.java", 0, "class OrderTest {}\n")
	t.Setenv("OLLAMA_HOST", f.URL)
	t.Setenv("LOCAL_SHUNT_MODEL", "fake")

	code, stdout, stderr := runCLI(t, "", "code-write", "--spec", "Write tests for UserService", "--reference", "tests/OrderTest.java", "--target", "tests/UserTest.java")
	equal(t, code, 0)
	contain(t, stdout, "wrote tests/UserTest.java (1 lines)")
	written, _ := os.ReadFile(filepath.Join(e.dir, "tests", "UserTest.java"))
	equal(t, string(written), "class UserTest {}\n")

	code, _, stderr = runCLI(t, "", "code-write", "--spec", "again", "--reference", "tests/OrderTest.java", "--target", "tests/UserTest.java")
	equal(t, code, 2)
	contain(t, stderr, "already exists")

	code, stdout, stderr = runCLI(t, "", "code-write", "--spec", "Generate a config stub", "--reference", "tests/UserTest.java")
	equal(t, code, 0)
	equal(t, stdout, "class UserTest {}\n")
	contain(t, stderr, "local-shunt: generated")

	code, _, stderr = runCLI(t, "", "code-write", "--reference", "tests/UserTest.java")
	equal(t, code, 2)
	contain(t, stderr, "--spec is required")
}

func TestCLIErrors(t *testing.T) {
	e := isolate(t)
	code, _, stderr := runCLI(t, "", "bulk-read", "--question", "q", "--paths", "missing.py")
	equal(t, code, 2)
	contain(t, stderr, "not found")

	e.make("a.py", 10)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9")
	code, _, stderr = runCLI(t, "", "bulk-read", "--question", "q", "--paths", "a.py")
	equal(t, code, 1)
	contain(t, stderr, "Fall back")
	equal(t, strings.HasPrefix(pyStr(e.lastLog()["outcome"]), "error:"), true)

	code, _, stderr = runCLI(t, "", "bulk-read", "--question", "q", "--paths", "a.py", "--provider", "openrouter")
	equal(t, code, 1)
	contain(t, stderr, "OPENROUTER_API_KEY")

	code, _, stderr = runCLI(t, "", "bulk-read", "--question", "q", "--provider", "nope", "a.py")
	equal(t, code, 2)
	contain(t, stderr, "invalid provider")

	code, _, stderr = runCLI(t, "", "bulk-read", "--bogus")
	equal(t, code, 2)
	contain(t, stderr, "unrecognized option")

	code, _, _ = runCLI(t, "", "nope")
	equal(t, code, 2)
}

// TestCLIShortFlagNotSwallowedAsValue guards against a registered short flag (like -q)
// being silently consumed as another flag's value instead of raising "needs a value".
func TestCLIShortFlagNotSwallowedAsValue(t *testing.T) {
	e := isolate(t)
	e.make("a.py", 10)
	code, _, stderr := runCLI(t, "", "bulk-read", "--paths", "a.py", "--question", "-q")
	equal(t, code, 2)
	contain(t, stderr, "--question needs a value")
}

// TestCLIFlagListAllowsZeroValues matches the old CLI's nargs="*": --reference with no
// following files is an empty list, not a parse error.
func TestCLIFlagListAllowsZeroValues(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.reply = func(map[string]any) string { return "```java\nclass Empty {}\n```" }
	t.Setenv("OLLAMA_HOST", f.URL)
	t.Setenv("LOCAL_SHUNT_MODEL", "fake")
	code, stdout, stderr := runCLI(t, "", "code-write", "--reference", "--spec", "Generate a stub")
	equal(t, code, 0)
	equal(t, stdout, "class Empty {}\n")
	contain(t, stderr, "local-shunt: generated")
}

func TestCLIProviderFlagOverridesConfig(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	e.make("a.py", 10)
	t.Setenv("LOCAL_SHUNT_API_BASE", f.URL+"/v1")
	code, _, stderr := runCLI(t, "", "bulk-read", "--question", "q", "--paths", "a.py", "--provider", "openai-compatible", "--model", "fake-model")
	equal(t, code, 0)
	notContain(t, stderr, "local-shunt:")
	equal(t, f.chatRequests()[0].Path, "/v1/chat/completions")
}

func TestCLIStatsAndHelp(t *testing.T) {
	isolate(t)
	code, stdout, _ := runCLI(t, "", "stats")
	equal(t, code, 0)
	contain(t, stdout, "No log yet")
	code, stdout, _ = runCLI(t, "", "bulk-read", "--help")
	equal(t, code, 0)
	contain(t, stdout, "--paths")
	code, stdout, _ = runCLI(t, "", "version")
	equal(t, code, 0)
	contain(t, stdout, Version)
}
