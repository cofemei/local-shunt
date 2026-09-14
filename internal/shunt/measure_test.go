package shunt

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const denyText = "local-shunt: big.py is large (900 lines, 40,000 bytes; threshold 350 lines or 32,768 bytes)."

func usageBlock(input, cacheWrite, cacheRead, output, oneHour int) map[string]any {
	return map[string]any{
		"input_tokens":                input,
		"cache_creation_input_tokens": cacheWrite,
		"cache_read_input_tokens":     cacheRead,
		"output_tokens":               output,
		"cache_creation":              map[string]any{"ephemeral_1h_input_tokens": oneHour, "ephemeral_5m_input_tokens": cacheWrite - oneHour},
	}
}

func assistantEntry(id string, blocks []any, u map[string]any, model string) map[string]any {
	if model == "" {
		model = "claude-test"
	}
	if blocks == nil {
		blocks = []any{}
	}
	return map[string]any{"type": "assistant", "message": map[string]any{"id": id, "model": model, "content": blocks, "usage": u}}
}

func toolUse(id, name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func toolResultEntry(id string, content any, isError bool) map[string]any {
	block := map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
	if isError {
		block["is_error"] = true
	}
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{block}}}
}

func writeJSONL(t *testing.T, path string, entries ...any) string {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	var b strings.Builder
	for _, entry := range entries {
		if s, ok := entry.(string); ok {
			b.WriteString(s)
		} else {
			data, _ := json.Marshal(entry)
			b.Write(data)
		}
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func transcriptPath(e *env, session string) string {
	return filepath.Join(e.dir, "claude", "projects", "-proj", session+".jsonl")
}

func TestTranscriptSplitEntriesCountOneRequest(t *testing.T) {
	e := isolate(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(e.dir, "claude"))
	path := writeJSONL(t, transcriptPath(e, "s1"),
		map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "hi"}},
		assistantEntry("m1", []any{map[string]any{"type": "thinking", "thinking": ""}}, usageBlock(2, 1000, 5000, 50, 1000), ""),
		assistantEntry("m1", []any{map[string]any{"type": "text", "text": "ok"}}, usageBlock(2, 1000, 5000, 50, 1000), ""),
		assistantEntry("m2", []any{map[string]any{"type": "text", "text": "done"}}, usageBlock(1, 400, 6000, 20, 0), ""),
		assistantEntry("m3", []any{map[string]any{"type": "text", "text": "API error"}}, usageBlock(0, 0, 0, 0, 0), "<synthetic>"),
		"not json",
	)
	u := parseTranscript(path)
	equal(t, u.Requests, 2)
	equal(t, u.Models.len(), 1)
	equal(t, u.Models.get("claude-test"), 2)
	equal(t, u.InputTokens, 3)
	equal(t, u.CacheCreationTokens, 1400)
	equal(t, u.CacheReadTokens, 11000)
	equal(t, u.OutputTokens, 70)
	equal(t, u.totalInputTokens(), 12403)
	// 3 + 2.0 * 1000 + 1.25 * 400 + 0.1 * 11000
	equal(t, u.weightedInputTokens(), 3603)
}

func TestTranscriptToolCategoriesResultsAndDenials(t *testing.T) {
	e := isolate(t)
	path := writeJSONL(t, transcriptPath(e, "s1"),
		assistantEntry("m1", []any{
			toolUse("t1", "Read", map[string]any{"file_path": "/p/big.py"}),
			toolUse("t2", "Read", map[string]any{"file_path": "/p/big.py", "offset": 10, "limit": 20}),
		}, usageBlock(1, 0, 0, 0, 0), ""),
		toolResultEntry("t1", denyText, true),
		toolResultEntry("t2", strings.Repeat("x", 300), false),
		assistantEntry("m2", []any{
			toolUse("t3", "Bash", map[string]any{"command": `bulk-read --question 'where?' --paths big.py`}),
			toolUse("t4", "mcp__plugin_local-shunt_local-shunt__shunt_read", map[string]any{"files": []string{"big.py"}, "question": "q"}),
			toolUse("t5", "Grep", map[string]any{"pattern": "x"}),
			toolUse("t6", "Bash", map[string]any{"command": `code-write --spec s --target a.py`}),
		}, usageBlock(1, 0, 0, 0, 0), ""),
		toolResultEntry("t3", []any{map[string]any{"type": "text", "text": strings.Repeat("y", 60)}}, false),
		toolResultEntry("t4", strings.Repeat("z", 90), false),
		toolResultEntry("t5", denyText, false), // a denial text that is not an error result does not count
	)
	u := parseTranscript(path)
	for category, count := range map[string]int{
		"Read": 1, "Read (range)": 1, "shunt read (Bash)": 1, "shunt_read (MCP)": 1, "Grep": 1, "shunt write (Bash)": 1,
	} {
		equal(t, u.ToolCalls.get(category), count)
	}
	equal(t, u.ToolCalls.len(), 6)
	equal(t, u.Denials, 1)
	equal(t, u.delegations(), 2)
	equal(t, u.ToolResultTokens.get("Read (range)"), 100)
	equal(t, u.ToolResultTokens.get("shunt read (Bash)"), 20)
	equal(t, u.readResultTokens(), u.ToolResultTokens.get("Read")+100+20+30)
}

func TestCategorizeLegacyPythonCommand(t *testing.T) {
	equal(t, categorize("Bash", map[string]any{"command": "python3 '/a b/scripts/shunt.py' read big.py -q 'where?'"}), "shunt read (Bash)")
	equal(t, categorize("Bash", map[string]any{"command": "local-shunt bulk-read -q x a.py"}), "shunt read (Bash)")
	equal(t, categorize("Bash", map[string]any{"command": "cat bulk-reader.md"}), "Bash")
}

func TestSubagentTranscriptsAreIncluded(t *testing.T) {
	e := isolate(t)
	path := writeJSONL(t, transcriptPath(e, "s1"), assistantEntry("m1", nil, usageBlock(10, 0, 0, 5, 0), ""))
	writeJSONL(t, filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-1.jsonl"),
		assistantEntry("m9", []any{toolUse("t1", "Read", map[string]any{"file_path": "/p/a.py"})}, usageBlock(20, 0, 0, 7, 0), "claude-small"),
		toolResultEntry("t1", "abc", false),
	)
	u := parseTranscript(path)
	equal(t, u.Transcripts, 2)
	equal(t, u.Requests, 2)
	equal(t, u.InputTokens, 30)
	equal(t, u.Models.get("claude-test"), 1)
	equal(t, u.Models.get("claude-small"), 1)
	equal(t, u.ToolCalls.get("Read"), 1)
}

func TestFindTranscript(t *testing.T) {
	e := isolate(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(e.dir, "claude"))
	path := writeJSONL(t, transcriptPath(e, "abc-123"))
	equal(t, findTranscript("abc-123"), path)
	equal(t, findTranscript(path), path)
	equal(t, findTranscript("missing"), "")
	equal(t, findTranscript("../../etc"), "")
	equal(t, findTranscript(filepath.Join(e.dir, "missing.jsonl")), "")
}

func TestWorkerRecordsCarryTheSessionID(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-1")
	e.make("a.py", 400)
	if _, err := RunRead(workerConfig(f), ReadOptions{Files: []string{"a.py"}, Question: "q"}); err != nil {
		t.Fatal(err)
	}
	equal(t, e.lastLog()["session_id"], any("sess-1"))
}

func TestWorkerUsageFiltersBySession(t *testing.T) {
	isolate(t)
	logEvent(record{{"session_id", "s1"}, {"event", "read"}, {"outcome", "ok"}, {"est_input_tokens", 1000},
		{"est_output_tokens", 50}, {"prompt_tokens", 1100}, {"completion_tokens", 40}, {"cost_usd", 0.01}})
	logEvent(record{{"session_id", "s1"}, {"event", "write"}, {"outcome", "ok"}, {"prompt_tokens", 10}, {"cost_usd", nil}})
	logEvent(record{{"session_id", "s1"}, {"event", "read"}, {"outcome", "error:LLMError"}})
	logEvent(record{{"session_id", "s1"}, {"event", "hook"}, {"outcome", "denied"}})
	logEvent(record{{"session_id", "s2"}, {"event", "read"}, {"outcome", "ok"}, {"est_input_tokens", 999}})
	u := workerUsage("s1", readLog())
	equal(t, u.Reads, 1)
	equal(t, u.Writes, 1)
	equal(t, u.Errors, 1)
	equal(t, u.EstSourceTokens, 1000)
	equal(t, u.EstSummaryTokens, 50)
	equal(t, u.PromptTokens, 1110)
	equal(t, math.Abs(u.CostUSD-0.01) < 1e-9, true)
}

func TestRunUsageTextAndJSON(t *testing.T) {
	e := isolate(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(e.dir, "claude"))
	writeJSONL(t, filepath.Join(e.dir, "claude", "projects", "-p", "s1.jsonl"), assistantEntry("m1", nil, usageBlock(5, 10, 20, 3, 0), ""))
	logEvent(record{{"session_id", "s1"}, {"event", "read"}, {"outcome", "ok"}, {"est_input_tokens", 1000}})
	text, err := RunUsage([]string{"s1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	contain(t, text, "Session s1")
	contain(t, text, "delegated reads")
	raw, _ := RunUsage([]string{"s1"}, true)
	var data map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatal(err)
	}
	equal(t, data["claude"]["total_input_tokens"], any(35.0))
	equal(t, data["worker"]["est_source_tokens"], any(1000.0))
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	text, _ = RunUsage(nil, false)
	contain(t, text, "Session s1")
	_, err = RunUsage([]string{"nope"}, false)
	errorContains(t, err, "transcript not found")
}

func writeTasks(t *testing.T, e *env, data any) string {
	path := filepath.Join(e.dir, "bench", "tasks.json")
	e.writeJSON(path, data)
	return path
}

func TestLoadTasksDefaultsAndRelativeCwd(t *testing.T) {
	e := isolate(t)
	path := writeTasks(t, e, map[string]any{
		"defaults": map[string]any{"cwd": "..", "model": "haiku", "timeout": 60},
		"tasks":    []any{map[string]any{"id": "a", "prompt": "p", "expect": "x"}, map[string]any{"id": "b", "prompt": "p", "model": "sonnet"}},
	})
	tasks, err := loadTasks(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := tasks[0], tasks[1]
	equal(t, a.Cwd, e.dir)
	equal(t, a.Model, "haiku")
	equal(t, a.Timeout, 60)
	equal(t, strings.Join(a.Expect, ","), "x")
	equal(t, b.Model, "sonnet")
	equal(t, strings.Join(a.AllowedTools, ","), strings.Join(defaultAllowedTools, ","))
}

func TestLoadTasksInvalidFiles(t *testing.T) {
	e := isolate(t)
	for _, tc := range []struct {
		tasks   any
		message string
	}{
		{[]any{}, "non-empty"},
		{[]any{map[string]any{"prompt": "p"}}, "needs an id"},
		{[]any{map[string]any{"id": "a", "prompt": "p"}, map[string]any{"id": "a", "prompt": "q"}}, "duplicate"},
		{[]any{map[string]any{"id": "a", "prompt": " "}}, "needs a prompt"},
		{[]any{map[string]any{"id": "a", "prompt": "p", "cwd": "missing"}}, "not a directory"},
		{[]any{map[string]any{"id": "a", "prompt": "p", "expect": []string{"("}}}, "invalid expect"},
	} {
		_, err := loadTasks(writeTasks(t, e, tc.tasks))
		errorContains(t, err, tc.message)
	}
	bad := e.make("bad.json", 0, "{")
	_, err := loadTasks(bad)
	errorContains(t, err, "invalid JSON")
}

func TestExampleTaskFileLoads(t *testing.T) {
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	root = resolveSymlinks(root)
	tasks, err := loadTasks(filepath.Join(root, "bench", "example-tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) < 5 {
		t.Fatalf("only %d tasks", len(tasks))
	}
	for _, task := range tasks {
		equal(t, task.Cwd, root)
	}
}

func TestOffModeDisablesThePluginAndItsTools(t *testing.T) {
	e := isolate(t)
	tasks, _ := loadTasks(writeTasks(t, e, []any{map[string]any{"id": "a", "prompt": "p", "model": "haiku"}}))
	t.Setenv("LOCAL_SHUNT_DISABLE", "1")
	onCmd, onEnv := claudeCommand(tasks[0], "on", "sid", "claude", e.dir)
	offCmd, offEnv := claudeCommand(tasks[0], "off", "sid", "claude", e.dir)
	notContain(t, strings.Join(onEnv, "\n"), "LOCAL_SHUNT_DISABLE")
	contain(t, strings.Join(offEnv, "\n"), "LOCAL_SHUNT_DISABLE=1")
	index := func(cmd []string, flag string) int {
		for i, arg := range cmd {
			if arg == flag {
				return i
			}
		}
		return -1
	}
	for _, cmd := range [][]string{onCmd, offCmd} {
		equal(t, index(cmd, "--plugin-dir") > 0, true)
		equal(t, cmd[index(cmd, "--session-id")+1], "sid")
		equal(t, cmd[index(cmd, "--model")+1], "haiku")
	}
	equal(t, index(onCmd, "--disallowedTools"), -1)
	allowedOff := offCmd[index(offCmd, "--allowedTools")+1:]
	for _, tool := range workerTools {
		equal(t, contains(allowedOff, tool), false)
	}
	equal(t, contains(offCmd[index(offCmd, "--disallowedTools")+1:], workerTools[0]), true)
}

// fakeClaude imitates `claude -p` when the test binary runs with LOCAL_SHUNT_FAKE_CLAUDE=1.
func fakeClaude() int {
	args := os.Args[1:]
	promptBytes, _ := io.ReadAll(os.Stdin)
	prompt := string(promptBytes)
	session := ""
	for i, arg := range args {
		if arg == "--session-id" && i+1 < len(args) {
			session = args[i+1]
		}
	}
	off := os.Getenv("LOCAL_SHUNT_DISABLE") == "1"
	emit := func(v any) {
		data, _ := json.Marshal(v)
		fmt.Println(string(data))
	}
	if strings.Contains(prompt, "FAIL") {
		emit(map[string]any{"type": "result", "is_error": true, "result": "boom", "num_turns": 1})
		return 1
	}
	project := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-fake")
	_ = os.MkdirAll(project, 0o755)
	usage := func(total int) map[string]any {
		return map[string]any{"input_tokens": 0, "cache_creation_input_tokens": 1000, "cache_read_input_tokens": total - 1000, "output_tokens": 100}
	}
	var entries []any
	var answer string
	cost := 0.006
	if off {
		entries = []any{
			map[string]any{"type": "assistant", "message": map[string]any{"id": "a", "model": "m", "usage": usage(10000),
				"content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "big.py"}}}}},
			map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": strings.Repeat("x", 30000)}}}},
			map[string]any{"type": "assistant", "message": map[string]any{"id": "b", "model": "m", "usage": usage(20000), "content": []any{}}},
		}
		answer, cost = "The answer is 50.", 0.01
	} else {
		entries = []any{
			map[string]any{"type": "assistant", "message": map[string]any{"id": "a", "model": "m", "usage": usage(8000),
				"content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "mcp__plugin_local-shunt_local-shunt__shunt_read",
					"input": map[string]any{"files": []string{"big.py"}, "question": "q"}}}}},
			map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": strings.Repeat("y", 600)}}}},
			map[string]any{"type": "assistant", "message": map[string]any{"id": "b", "model": "m", "usage": usage(8200), "content": []any{}}},
		}
		answer = "It is 50."
		if strings.Contains(prompt, "WRONG_ON") {
			answer = "No idea."
		}
	}
	var b strings.Builder
	for _, entry := range entries {
		data, _ := json.Marshal(entry)
		b.Write(data)
		b.WriteByte('\n')
	}
	_ = os.WriteFile(filepath.Join(project, session+".jsonl"), []byte(b.String()), 0o644)
	emit(map[string]any{"type": "result", "is_error": false, "result": answer, "num_turns": 2, "total_cost_usd": cost, "usage": usage(1001)})
	return 0
}

type benchEnv struct {
	*env
	claude string
	tasks  string
}

func setupBench(t *testing.T) *benchEnv {
	e := isolate(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(e.dir, "claude"))
	t.Setenv("LOCAL_SHUNT_FAKE_CLAUDE", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tasks := filepath.Join(e.dir, "tasks.json")
	e.writeJSON(tasks, map[string]any{"tasks": []any{
		map[string]any{"id": "good", "prompt": "question", "expect": []string{`\b50\b`}},
		map[string]any{"id": "worse", "prompt": "question WRONG_ON", "expect": []string{"50"}},
	}})
	return &benchEnv{env: e, claude: exe, tasks: tasks}
}

func TestBenchRunsBothModesAndReportsSavings(t *testing.T) {
	b := setupBench(t)
	var messages []string
	out, err := runBench(b.tasks, 2, "", nil, b.claude, b.dir, func(m string) { messages = append(messages, m) })
	if err != nil {
		t.Fatal(err)
	}
	records, _ := loadResults([]string{out})
	equal(t, len(records), 8)
	equal(t, len(messages), 8)
	equal(t, filepath.Dir(out), filepath.Join(b.dir, "state", "local-shunt", "bench"))
	// The order of modes alternates between runs.
	var good []string
	for _, r := range records {
		if r["task"] == "good" {
			good = append(good, r["mode"].(string))
		}
	}
	equal(t, strings.Join(good, ","), "on,off,off,on")

	var on map[string]any
	for _, r := range records {
		if r["task"] == "good" && r["mode"] == "on" {
			on = r
			break
		}
	}
	equal(t, on["passed"], any(true))
	equal(t, pyStr(asMap(on["claude"])["total_input_tokens"]), "16200")
	equal(t, pyStr(asMap(on["claude"])["delegations"]), "1")
	equal(t, pyStr(on["cost_usd"]), "0.006")

	summary := summarize(records)[0]
	equal(t, summary.id, "good")
	equal(t, math.Abs(summary.saving["input"].value-(1-16200.0/30000)) < 1e-9, true)
	equal(t, math.Abs(summary.saving["read"].value-(1-200.0/10000)) < 1e-9, true)
	equal(t, summary.on.passed, 2)
	equal(t, summary.on.checked, 2)

	report := formatReport(records)
	contain(t, report, "Task good")
	contain(t, report, "46.0%")
	contain(t, report, "98.0%")
	matches(t, report, `tasks saving ≥70% of file-read tokens +2 / 2`)
	contain(t, report, "worse: fewer answers passed with local-shunt on (0 vs 2)")
}

func TestBenchFailedRunsAreRecordedAndExcluded(t *testing.T) {
	b := setupBench(t)
	b.writeJSON(b.tasks, []any{map[string]any{"id": "bad", "prompt": "FAIL", "expect": "x"}})
	out, err := runBench(b.tasks, 1, filepath.Join(b.dir, "r.jsonl"), nil, b.claude, b.dir, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	records, _ := loadResults([]string{out})
	equal(t, len(records), 2)
	for _, r := range records {
		equal(t, r["error"], any("boom"))
		equal(t, r["passed"], any(false))
	}
	report := formatReport(records)
	contain(t, report, "2 runs (2 failed)")
	contain(t, report, "no completed run in one mode")
}

func TestBenchUnknownTaskAndMissingClaude(t *testing.T) {
	b := setupBench(t)
	_, err := runBench(b.tasks, 3, "", []string{"nope"}, b.claude, b.dir, func(string) {})
	errorContains(t, err, "unknown task id: nope")
	_, err = runBench(b.tasks, 1, "", []string{"good"}, filepath.Join(b.dir, "missing"), b.dir, func(string) {})
	errorContains(t, err, "cannot run")
}

func TestReportContaminatedBaseline(t *testing.T) {
	var records []map[string]any
	for _, mode := range []string{"on", "off"} {
		records = append(records, map[string]any{"task": "t", "mode": mode, "claude": map[string]any{
			"total_input_tokens": 10.0, "weighted_input_tokens": 10.0, "output_tokens": 1.0, "read_result_tokens": 5.0,
			"delegations": 1.0, "denials": 0.0,
		}})
	}
	contain(t, formatReport(records), "baseline is contaminated")
	equal(t, formatReport(nil), "No benchmark records found.")
}

func TestCLIBenchAndReport(t *testing.T) {
	b := setupBench(t)
	out := filepath.Join(b.dir, "results.jsonl")
	code, stdout, stderr := runCLI(t, "", "bench", b.tasks, "--runs", "1", "--task", "good", "--out", out, "--claude", b.claude)
	equal(t, code, 0)
	contain(t, stdout, "Task good")
	contain(t, stdout, "Results: "+out)
	contain(t, stderr, "[1/2] good")
	report, err := RunBenchReport([]string{out})
	if err != nil {
		t.Fatal(err)
	}
	contain(t, report, "Task good")
	_, err = RunBenchReport([]string{filepath.Join(b.dir, "missing.jsonl")})
	equal(t, isUsageError(err), true)
	errorContains(t, err, "missing.jsonl")
}
