package shunt

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var isolatedVars = []string{
	"CODEX_PROJECT_DIR", "CLAUDE_PROJECT_DIR", "CLAUDE_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CONFIG_DIR",
	"XDG_STATE_HOME", "XDG_CONFIG_HOME", "LOCAL_SHUNT_ROOT",
	"OLLAMA_HOST", "OPENROUTER_API_KEY", "OPENAI_API_KEY", "LOCAL_SHUNT_DISABLE",
	"LOCAL_SHUNT_PROVIDER", "LOCAL_SHUNT_API_BASE", "LOCAL_SHUNT_API_KEY_ENV",
	"LOCAL_SHUNT_MODEL", "LOCAL_SHUNT_MIN_LINES", "LOCAL_SHUNT_MIN_BYTES", "LOCAL_SHUNT_NUM_CTX",
}

const fakeAnswer = "### Answer\n- `value_2` is set (L2).\n\n" +
	"### Relevant locations\n- `L2-L3`: values\n\n" +
	"### Not covered or uncertain\n- None"

func TestMain(m *testing.M) {
	if os.Getenv("LOCAL_SHUNT_FAKE_CLAUDE") == "1" {
		os.Exit(fakeClaude())
	}
	progressOut = io.Discard
	sleep = func(time.Duration) {}
	os.Exit(m.Run())
}

// env is a private project, config and state directory for one test.
type env struct {
	t   *testing.T
	dir string
}

func isolate(t *testing.T) *env {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range isolatedVars {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Chdir(dir)
	return &env{t: t, dir: dir}
}

// make writes a file of numbered lines, or the given content.
func (e *env) make(name string, lines int, content ...string) string {
	e.t.Helper()
	path := filepath.Join(e.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	var text string
	if len(content) > 0 {
		text = content[0]
	} else {
		var b strings.Builder
		for i := 1; i <= lines; i++ {
			fmt.Fprintf(&b, "value_%d = %d\n", i, i)
		}
		text = b.String()
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *env) writeJSON(path string, data any) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) writeUserConfig(data map[string]any) {
	e.writeJSON(filepath.Join(e.dir, "config", "local-shunt", "config.json"), data)
}

func (e *env) writeProjectConfig(data map[string]any) {
	e.writeJSON(filepath.Join(e.dir, ".claude", "local-shunt.json"), data)
}

func (e *env) logRecords() []map[string]any {
	records, _ := readJSONLines(filepath.Join(e.dir, "state", "local-shunt", "log.jsonl"))
	return records
}

func (e *env) lastLog() map[string]any {
	e.t.Helper()
	records := e.logRecords()
	if len(records) == 0 {
		e.t.Fatal("no log records")
	}
	return records[len(records)-1]
}

type fakeRequest struct {
	Method, Path string
	Body         map[string]any
	Header       http.Header
}

type fakeResponse struct {
	status  int
	body    any
	headers map[string]string
}

// fakeLLM serves Ollama (/api/...) and OpenAI-compatible (/v1/...) endpoints.
type fakeLLM struct {
	URL      string
	mu       sync.Mutex
	requests []fakeRequest
	reply    func(body map[string]any) string
	handler  func(r fakeRequest) fakeResponse
}

func newFakeLLM(t *testing.T) *fakeLLM {
	f := &fakeLLM{reply: func(map[string]any) string { return fakeAnswer }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := fakeRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &req.Body)
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		handler := f.handler
		f.mu.Unlock()
		if handler == nil {
			handler = f.defaultResponse
		}
		resp := handler(req)
		for k, v := range resp.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_ = json.NewEncoder(w).Encode(resp.body)
	}))
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}

func (f *fakeLLM) defaultResponse(r fakeRequest) fakeResponse {
	switch r.Path {
	case "/api/tags":
		return fakeResponse{200, map[string]any{"models": []any{map[string]any{"name": "fake:latest"}}}, nil}
	case "/api/chat":
		return fakeResponse{200, map[string]any{
			"message":     map[string]any{"role": "assistant", "content": f.reply(r.Body)},
			"done_reason": "stop", "prompt_eval_count": 100, "eval_count": 20,
		}, nil}
	case "/v1/models":
		return fakeResponse{200, map[string]any{"data": []any{map[string]any{"id": "fake-model"}}}, nil}
	case "/v1/chat/completions":
		return fakeResponse{200, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": f.reply(r.Body)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "cost": 0.001},
		}, nil}
	}
	return fakeResponse{404, map[string]any{"error": "not found"}, nil}
}

func (f *fakeLLM) chatRequests() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeRequest
	for _, r := range f.requests {
		if r.Path == "/api/chat" || r.Path == "/v1/chat/completions" {
			out = append(out, r)
		}
	}
	return out
}

func messages(r fakeRequest) (system, user string) {
	list, _ := r.Body["messages"].([]any)
	return asString(asMap(list[0])["content"]), asString(asMap(list[1])["content"])
}

func contain(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("missing %q in:\n%s", want, text)
	}
}

func notContain(t *testing.T, text, unwanted string) {
	t.Helper()
	if strings.Contains(text, unwanted) {
		t.Errorf("unexpected %q in:\n%s", unwanted, text)
	}
}

func matches(t *testing.T, text, pattern string) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(text) {
		t.Errorf("no match for %q in:\n%s", pattern, text)
	}
}

func equal[T comparable](t *testing.T, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func errorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q", want)
	}
	contain(t, err.Error(), want)
}

func isUsageError(err error) bool {
	_, ok := err.(*UsageError)
	return ok
}

func isLLMError(err error, kind string) bool {
	e, ok := err.(*LLMError)
	return ok && (kind == "" || e.Kind == kind)
}
