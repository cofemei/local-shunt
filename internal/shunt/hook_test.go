package shunt

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	healthy   = func() Health { return Health{OK: true} }
	unhealthy = func() Health { return Health{Reason: "down"} }
)

func mustNotCheckHealth(t *testing.T) func() Health {
	return func() Health {
		t.Error("health check should not run for this decision")
		return Health{OK: true}
	}
}

func hookConfig() Config {
	cfg := DefaultConfig()
	cfg.MinLines, cfg.MinBytes = 350, 32768
	return cfg
}

func readEvent(e *env, path string, input map[string]any) map[string]any {
	toolInput := map[string]any{"file_path": path}
	for k, v := range input {
		toolInput[k] = v
	}
	return map[string]any{"tool_name": "Read", "tool_input": toolInput, "cwd": e.dir}
}

func bashEvent(e *env, command string) map[string]any {
	return map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command}, "cwd": e.dir}
}

func expectRule(t *testing.T, d *Decision, allow bool, rule string) {
	t.Helper()
	if d == nil {
		t.Fatalf("no decision, want %v %s", allow, rule)
	}
	if d.Allow != allow || d.Rule != rule {
		t.Errorf("got (%v, %s), want (%v, %s)", d.Allow, d.Rule, allow, rule)
	}
}

func TestDecideReadRules(t *testing.T) {
	e := isolate(t)
	cfg := hookConfig()
	big := e.make("big.py", 1000)

	disabled := cfg
	disabled.Enabled = false
	expectRule(t, decide(readEvent(e, big, nil), disabled, mustNotCheckHealth(t)), true, "disabled")

	expectRule(t, decide(readEvent(e, big, map[string]any{"offset": 1.0, "limit": 1000.0}), cfg, mustNotCheckHealth(t)), true, "range")
	expectRule(t, decide(readEvent(e, big, map[string]any{"limit": 50.0}), cfg, mustNotCheckHealth(t)), true, "range")
	expectRule(t, decide(readEvent(e, big, map[string]any{"offset": 10.0}), cfg, mustNotCheckHealth(t)), true, "range")

	for _, name := range []string{"pic.png", "doc.pdf", "nb.ipynb"} {
		expectRule(t, decide(readEvent(e, e.make(name, 1000), nil), cfg, mustNotCheckHealth(t)), true, "native-type")
	}
	expectRule(t, decide(readEvent(e, filepath.Join(e.dir, "nope.py"), nil), cfg, mustNotCheckHealth(t)), true, "not-found")
	expectRule(t, decide(readEvent(e, e.make("blob.bin", 0, strings.Repeat("\x00", 100_000)), nil), cfg, mustNotCheckHealth(t)), true, "binary")
	for _, name := range []string{"CLAUDE.md", "sub/SKILL.md", ".claude/notes.md", ".env.local", "Cargo.lock"} {
		expectRule(t, decide(readEvent(e, e.make(name, 1000), nil), cfg, mustNotCheckHealth(t)), true, "excluded")
	}
	expectRule(t, decide(readEvent(e, e.make("small.py", 349), nil), cfg, mustNotCheckHealth(t)), true, "below-threshold")
	expectRule(t, decide(readEvent(e, e.make("edge.py", 350), nil), cfg, healthy), false, "threshold")

	minified := decide(readEvent(e, e.make("app.min.js", 0, strings.Repeat("x", 40_000)), nil), cfg, healthy)
	equal(t, minified.Allow, false)
	equal(t, minified.Info.Lines, 1)

	expectRule(t, decide(readEvent(e, big, nil), cfg, unhealthy), true, "worker-unavailable")
	equal(t, decide(readEvent(e, "rel.py", nil), cfg, healthy).Rule, "not-found")
	e.make("rel.py", 1000)
	equal(t, decide(readEvent(e, "rel.py", nil), cfg, healthy).Allow, false)

	grep := map[string]any{"tool_name": "Grep", "tool_input": map[string]any{"pattern": "x"}, "cwd": e.dir}
	if decide(grep, cfg, mustNotCheckHealth(t)) != nil {
		t.Error("other tools must be ignored")
	}
}

func TestWideRangeIsRecorded(t *testing.T) {
	e := isolate(t)
	big := e.make("big.py", 1000)
	d := decide(readEvent(e, big, map[string]any{"offset": 1.0, "limit": 1000.0}), hookConfig(), mustNotCheckHealth(t))
	equal(t, d.RangeLines, 1000)
	d = decide(readEvent(e, big, map[string]any{"offset": 900.0}), hookConfig(), mustNotCheckHealth(t))
	equal(t, d.RangeLines, 0)
}

func TestDenyReason(t *testing.T) {
	e := isolate(t)
	cfg := hookConfig()
	reason := decide(readEvent(e, e.make("src/big.py", 1000), nil), cfg, healthy).Reason
	contain(t, reason, "shunt_read MCP tool")
	contain(t, reason, `bulk-read --question "<what you need to know>" --paths src/big.py`)
	contain(t, reason, "limit=1000")

	var src strings.Builder
	src.WriteString("LIMIT = 50\n\n")
	for i := 0; i < 200; i++ {
		src.WriteString("def f" + pyStr(float64(i)) + "():\n    return 1\n\n")
	}
	outlined := decide(readEvent(e, e.make("src/outlined.py", 0, src.String()), nil), cfg, healthy).Reason
	contain(t, outlined, "Outline of src/outlined.py")
	contain(t, outlined, "L1           LIMIT = 50")
	contain(t, outlined, "L3-L4        def f0():")
	contain(t, outlined, "answer without reading more")
	pathJSON, _ := json.Marshal(filepath.Join(e.dir, "src", "outlined.py"))
	contain(t, outlined, string(pathJSON))

	plain := decide(readEvent(e, e.make("big.txt", 1000), nil), cfg, healthy).Reason
	notContain(t, plain, "utline")
	contain(t, plain, "Read only the lines you need with offset/limit")

	cfg.MaxFileBytes = 50_000
	huge := decide(readEvent(e, e.make("huge.log", 10_000), nil), cfg, healthy)
	equal(t, huge.Allow, false)
	contain(t, huge.Reason, "Grep")
	notContain(t, huge.Reason, "bulk-read")
}

func TestDecideBashRules(t *testing.T) {
	e := isolate(t)
	cfg := hookConfig()
	e.make("big.py", 1000)
	equal(t, decide(bashEvent(e, "cat big.py"), cfg, healthy).Allow, false)
	expectRule(t, decide(bashEvent(e, "head -n 100 big.py"), cfg, mustNotCheckHealth(t)), true, "range")
	equal(t, decide(bashEvent(e, "head -n 1000 big.py"), cfg, healthy).Allow, false)
	if decide(bashEvent(e, "cat big.py | grep foo"), cfg, mustNotCheckHealth(t)) != nil {
		t.Error("pipelines must be ignored")
	}
	if decide(bashEvent(e, "ls -la"), cfg, mustNotCheckHealth(t)) != nil {
		t.Error("unrelated commands must be ignored")
	}
	contain(t, decide(bashEvent(e, "cat big.py"), cfg, healthy).Reason, "intercepted too")
	cfg.InterceptBash = false
	if decide(bashEvent(e, "cat big.py"), cfg, mustNotCheckHealth(t)) != nil {
		t.Error("intercept_bash=false must ignore Bash")
	}
}

func TestParseBashRead(t *testing.T) {
	type want struct {
		path            string
		maxLines, bytes int
	}
	for command, w := range map[string]want{
		"cat a.py":             {"a.py", -1, -1},
		"cat -n a.py":          {"a.py", -1, -1},
		"head a.py":            {"a.py", 10, -1},
		"head -n 20 a.py":      {"a.py", 20, -1},
		"head -n20 a.py":       {"a.py", 20, -1},
		"head -20 a.py":        {"a.py", 20, -1},
		"head --lines=20 a.py": {"a.py", 20, -1},
		"head -c 100 a.py":     {"a.py", -1, 100},
		"tail -n +5 a.py":      {"a.py", -1, -1},
		"head -n -5 a.py":      {"a.py", -1, -1},
		"less a.py":            {"a.py", -1, -1},
		"/bin/cat a.py":        {"a.py", -1, -1},
		"cat 'my file.py'":     {"my file.py", -1, -1},
	} {
		parsed := parseBashRead(command)
		if parsed == nil {
			t.Errorf("%q: not recognized", command)
			continue
		}
		if (want{parsed.path, parsed.maxLines, parsed.maxBytes}) != w {
			t.Errorf("%q: got %+v, want %+v", command, *parsed, w)
		}
	}
	for _, command := range []string{
		"cat a.py b.py", "cat a.py > b.py", "cd x && cat a.py", "tail -f app.log", "head -x a.py",
		"cat $FILE", `cat "unterminated`, "grep foo a.py", "", "head -n",
	} {
		if parseBashRead(command) != nil {
			t.Errorf("%q: should not be recognized", command)
		}
	}
}

func runHook(t *testing.T, mode string, stdin string) string {
	t.Helper()
	var out bytes.Buffer
	equal(t, RunHook(mode, strings.NewReader(stdin), &out), 0)
	return out.String()
}

func hookReadEvent(e *env, path string) string {
	data, _ := json.Marshal(map[string]any{"session_id": "s1", "tool_name": "Read", "tool_input": map[string]any{"file_path": path}, "cwd": e.dir})
	return string(data)
}

func TestHookProcess(t *testing.T) {
	e := isolate(t)
	equal(t, runHook(t, "pre-tool-use", "not json"), "")

	big := e.make("big.py", 1000)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9")
	equal(t, runHook(t, "pre-tool-use", hookReadEvent(e, big)), "")
	equal(t, e.lastLog()["outcome"], any("allowed:worker-unavailable"))
}

func TestHookDeniesWithFakeOllamaAndLogs(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	t.Setenv("OLLAMA_HOST", f.URL)
	t.Setenv("LOCAL_SHUNT_MODEL", "fake")
	output := runHook(t, "pre-tool-use", hookReadEvent(e, e.make("big.py", 1000)))
	var decoded map[string]map[string]string
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("%v: %q", err, output)
	}
	equal(t, decoded["hookSpecificOutput"]["permissionDecision"], "deny")
	contain(t, decoded["hookSpecificOutput"]["permissionDecisionReason"], "bulk-read")
	rec := e.lastLog()
	equal(t, rec["outcome"], any("denied"))
	equal(t, rec["session_id"], any("s1"))
	equal(t, pyStr(rec["lines"]), "1000")
}

func TestHookRemoteProviderDoesNotWaitOnNetwork(t *testing.T) {
	e := isolate(t)
	// 10.255.255.1 is unroutable: a network probe would hang until the timeout.
	t.Setenv("LOCAL_SHUNT_PROVIDER", "openrouter")
	t.Setenv("LOCAL_SHUNT_API_BASE", "http://10.255.255.1/v1")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("LOCAL_SHUNT_MODEL", "some/model")
	started := time.Now()
	output := runHook(t, "pre-tool-use", hookReadEvent(e, e.make("big.py", 1000)))
	if time.Since(started) > 2*time.Second {
		t.Error("the hook waited on the network")
	}
	contain(t, output, `"permissionDecision":"deny"`)
}

func TestHookRemoteProviderWithoutKeyFailsOpen(t *testing.T) {
	e := isolate(t)
	t.Setenv("LOCAL_SHUNT_PROVIDER", "openrouter")
	equal(t, runHook(t, "pre-tool-use", hookReadEvent(e, e.make("big.py", 1000))), "")
}

func sessionStartText(t *testing.T, output string) string {
	t.Helper()
	var decoded map[string]map[string]string
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("%v: %q", err, output)
	}
	return decoded["hookSpecificOutput"]["additionalContext"]
}

func TestSessionStart(t *testing.T) {
	e := isolate(t)
	f := newFakeLLM(t)
	event, _ := json.Marshal(map[string]any{"cwd": e.dir})

	t.Setenv("LOCAL_SHUNT_PROVIDER", "openai-compatible")
	t.Setenv("LOCAL_SHUNT_API_BASE", f.URL+"/v1")
	t.Setenv("LOCAL_SHUNT_MODEL", "fake-model")
	text := sessionStartText(t, runHook(t, "session-start", string(event)))
	contain(t, text, "local-shunt is active (provider openai-compatible, model fake-model")
	contain(t, text, "includes an outline of the file")
	contain(t, text, "bulk-read --question")
	notContain(t, text, "sent to that service")

	t.Setenv("LOCAL_SHUNT_PROVIDER", "openrouter")
	t.Setenv("LOCAL_SHUNT_API_BASE", "")
	text = sessionStartText(t, runHook(t, "session-start", string(event)))
	contain(t, text, "inactive")
	contain(t, text, "OPENROUTER_API_KEY")

	t.Setenv("LOCAL_SHUNT_DISABLE", "1")
	equal(t, runHook(t, "session-start", string(event)), "")
}

func TestSessionStartRemoteWarnsAboutDataLeavingMachine(t *testing.T) {
	e := isolate(t)
	t.Setenv("LOCAL_SHUNT_PROVIDER", "openrouter")
	t.Setenv("OPENROUTER_API_KEY", "k")
	probed := false
	output := handleSessionStart(map[string]any{"cwd": e.dir}, func(cfg Config, opts healthOptions) Health {
		probed = opts.probeRemote
		return Health{OK: true}
	})
	equal(t, probed, true)
	raw, _ := marshalJSON(output)
	text := sessionStartText(t, string(raw))
	contain(t, text, "https://openrouter.ai/api/v1")
	contain(t, text, "sent to that service")
}
