package shunt

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// mcpClient drives RunMCP in-process over pipes.
type mcpClient struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Reader
	nextID int
}

func startMCP(t *testing.T) *mcpClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		RunMCP(inR, outW)
		outW.Close()
		close(done)
	}()
	t.Cleanup(func() {
		inW.Close()
		<-done
	})
	c := &mcpClient{t: t, in: inW, out: bufio.NewReader(outR), nextID: 1}
	init := c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"},
	})
	result := asMap(init["result"])
	equal(t, result["protocolVersion"], any("2025-06-18"))
	equal(t, asMap(result["serverInfo"])["name"], any("local-shunt"))
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return c
}

func (c *mcpClient) send(message any) {
	raw, ok := message.(string)
	if !ok {
		data, _ := json.Marshal(message)
		raw = string(data)
	}
	if _, err := io.WriteString(c.in, raw+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *mcpClient) readResponse() map[string]any {
	c.t.Helper()
	line, err := c.out.ReadBytes('\n')
	if err != nil {
		c.t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(line, &response); err != nil {
		c.t.Fatalf("%v: %s", err, line)
	}
	return response
}

func (c *mcpClient) request(method string, params map[string]any) map[string]any {
	c.t.Helper()
	id := c.nextID
	c.nextID++
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	response := c.readResponse()
	equal(c.t, pyStr(response["id"]), pyStr(float64(id)))
	return response
}

func (c *mcpClient) call(name string, args map[string]any) (string, bool) {
	c.t.Helper()
	result := asMap(c.request("tools/call", map[string]any{"name": name, "arguments": args})["result"])
	content, _ := result["content"].([]any)
	return asString(asMap(content[0])["text"]), result["isError"] == true
}

func mcpEnv(t *testing.T) (*env, *fakeLLM) {
	e := isolate(t)
	f := newFakeLLM(t)
	t.Setenv("OLLAMA_HOST", f.URL)
	t.Setenv("LOCAL_SHUNT_MODEL", "fake")
	t.Chdir(t.TempDir())
	return e, f
}

func TestMCPToolsList(t *testing.T) {
	mcpEnv(t)
	c := startMCP(t)
	tools := map[string]map[string]any{}
	for _, item := range c.request("tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
		tool := item.(map[string]any)
		tools[asString(tool["name"])] = tool
	}
	equal(t, len(tools), 3)
	required := asMap(tools["shunt_read"]["inputSchema"])["required"].([]any)
	equal(t, len(required), 1)
	equal(t, required[0], any("question"))
	// Only shunt_read skips tool search; the others stay deferred.
	equal(t, asMap(tools["shunt_read"]["_meta"])["anthropic/alwaysLoad"], any(true))
	_, hasMeta := tools["shunt_write"]["_meta"]
	equal(t, hasMeta, false)
	force := asMap(asMap(asMap(tools["shunt_write"]["inputSchema"])["properties"])["force"])
	equal(t, force["default"], any(false))
}

func TestMCPPingUnknownMethodAndParseError(t *testing.T) {
	mcpEnv(t)
	c := startMCP(t)
	equal(t, len(asMap(c.request("ping", nil)["result"])), 0)
	equal(t, pyStr(asMap(c.request("resources/list", nil)["error"])["code"]), "-32601")
	c.send("{broken")
	equal(t, pyStr(asMap(c.readResponse()["error"])["code"]), "-32700")
	equal(t, pyStr(asMap(c.request("tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})["error"])["code"]), "-32602")
	equal(t, len(asMap(c.request("ping", nil)["result"])), 0)
}

func TestMCPShuntRead(t *testing.T) {
	e, f := mcpEnv(t)
	path := e.make("big.py", 400)
	c := startMCP(t)
	text, isError := c.call("shunt_read", map[string]any{"files": []string{path}, "question": "Where is value_2?"})
	equal(t, isError, false)
	contain(t, text, "(400 lines)")
	contain(t, text, "### Answer")
	equal(t, len(f.chatRequests()), 1)
}

func TestMCPRelativePathsUseProjectDir(t *testing.T) {
	e, _ := mcpEnv(t)
	e.make("rel.py", 10)
	c := startMCP(t)
	text, isError := c.call("shunt_read", map[string]any{"files": []string{"rel.py"}, "question": "q"})
	equal(t, isError, false)
	contain(t, text, "rel.py (10 lines)")
}

func TestMCPErrorsAreToolErrors(t *testing.T) {
	mcpEnv(t)
	c := startMCP(t)
	missing := filepath.Join(os.TempDir(), "local-shunt-missing.py")
	text, isError := c.call("shunt_read", map[string]any{"files": []string{missing}, "question": "q"})
	equal(t, isError, true)
	contain(t, text, "not found")
	text, _ = c.call("shunt_read", map[string]any{"files": []string{"/a.py"}, "question": 42})
	contain(t, text, "argument question must be a valid string")
	text, _ = c.call("shunt_read", map[string]any{"files": []string{"/a.py"}})
	contain(t, text, "missing required argument: question")
	text, _ = c.call("shunt_read", map[string]any{"files": []string{"/a.py"}, "question": "q", "bogus": 1})
	contain(t, text, "unknown argument")
	text, _ = c.call("shunt_read", map[string]any{"files": []string{"/a.py"}, "question": "q", "max_output": 64.5})
	contain(t, text, "argument max_output must be a valid integer")
}

func TestMCPProviderFailureIsToolError(t *testing.T) {
	e, _ := mcpEnv(t)
	path := e.make("a.py", 10)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9")
	c := startMCP(t)
	text, isError := c.call("shunt_read", map[string]any{"files": []string{path}, "question": "q"})
	equal(t, isError, true)
	contain(t, text, "Fall back")
}

func TestMCPProviderAndModelOverride(t *testing.T) {
	e, f := mcpEnv(t)
	path := e.make("a.py", 10)
	t.Setenv("LOCAL_SHUNT_API_BASE", f.URL+"/v1")
	c := startMCP(t)
	text, isError := c.call("shunt_read", map[string]any{"files": []string{path}, "question": "q", "provider": "openai-compatible", "model": "fake-model"})
	equal(t, isError, false)
	contain(t, text, "openai-compatible fake-model")
	equal(t, f.chatRequests()[0].Path, "/v1/chat/completions")
	_, isError = c.call("shunt_read", map[string]any{"files": []string{path}, "question": "q", "provider": "nope"})
	equal(t, isError, true)
}

func TestMCPShuntWriteAndStats(t *testing.T) {
	e, f := mcpEnv(t)
	f.reply = func(map[string]any) string { return "x = 1" }
	out := filepath.Join(e.dir, "gen", "out.py")
	c := startMCP(t)
	_, isError := c.call("shunt_write", map[string]any{"out": out, "spec": "a constant"})
	equal(t, isError, false)
	data, _ := os.ReadFile(out)
	equal(t, string(data), "x = 1\n")
	_, isError = c.call("shunt_write", map[string]any{"out": out, "spec": "a constant"})
	equal(t, isError, true)
	text, _ := c.call("shunt_stats", map[string]any{})
	contain(t, text, "Delegated writes")
}

func TestMCPLenientArgumentTypes(t *testing.T) {
	e, f := mcpEnv(t)
	path := e.make("a.py", 10)
	c := startMCP(t)
	encoded, _ := json.Marshal([]string{path})
	for _, files := range []any{string(encoded), path} {
		_, isError := c.call("shunt_read", map[string]any{"files": files, "question": "q", "max_output": "64"})
		equal(t, isError, false)
	}
	requests := f.chatRequests()
	equal(t, asMap(requests[len(requests)-1].Body["options"])["num_predict"], any(64.0))
}
