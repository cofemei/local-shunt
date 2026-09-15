package shunt

import (
	"sync/atomic"
	"testing"
	"time"
)

func chatResponse(content any, finish string, message map[string]any) fakeResponse {
	msg := map[string]any{"role": "assistant", "content": content}
	for k, v := range message {
		msg[k] = v
	}
	return fakeResponse{200, map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2},
	}, nil}
}

func ollamaConfig(f *fakeLLM) Config {
	cfg := DefaultConfig()
	cfg.OllamaHost, cfg.Model = f.URL, "fake"
	return cfg
}

func compatConfig(f *fakeLLM, provider string) Config {
	cfg := DefaultConfig()
	cfg.Provider, cfg.APIBase, cfg.Model = provider, f.URL+"/v1", "fake-model"
	return cfg
}

func mustProvider(t *testing.T, cfg Config) *Provider {
	t.Helper()
	p, err := getProvider(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOllamaChatPayloadAndResult(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.reply = func(map[string]any) string { return "<think>hmm</think>\nanswer" }
	cfg := ollamaConfig(f)
	cfg.NumCtx, cfg.Temperature = 4096, 0.2
	result, err := mustProvider(t, cfg).Chat("sys", "user", 77)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, result.Text, "answer")
	equal(t, *result.PromptTokens, 100)
	equal(t, *result.OutputTokens, 20)
	equal(t, result.Truncated, false)
	body := f.chatRequests()[0].Body
	equal(t, body["think"], any(false))
	equal(t, body["stream"], any(false))
	options := asMap(body["options"])
	equal(t, options["temperature"], any(0.2))
	equal(t, options["num_ctx"], any(4096.0))
	equal(t, options["num_predict"], any(77.0))
}

func TestOllamaRetriesWithoutThink(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(r fakeRequest) fakeResponse {
		if _, ok := r.Body["think"]; r.Path == "/api/chat" && ok {
			return fakeResponse{400, map[string]any{"error": "model does not support thinking"}, nil}
		}
		return f.defaultResponse(r)
	}
	if _, err := mustProvider(t, ollamaConfig(f)).Chat("s", "u", 10); err != nil {
		t.Fatal(err)
	}
	requests := f.chatRequests()
	equal(t, len(requests), 2)
	_, first := requests[0].Body["think"]
	_, second := requests[1].Body["think"]
	equal(t, first, true)
	equal(t, second, false)
}

func TestOllamaTruncationFlag(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse {
		return fakeResponse{200, map[string]any{"message": map[string]any{"content": "x"}, "done_reason": "length"}, nil}
	}
	result, _ := mustProvider(t, ollamaConfig(f)).Chat("s", "u", 10)
	equal(t, result.Truncated, true)
}

func TestLocalEndpointIsNotRetried(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse { return fakeResponse{503, map[string]any{"error": "busy"}, nil} }
	_, err := mustProvider(t, ollamaConfig(f)).Chat("s", "u", 10)
	equal(t, isLLMError(err, ""), true)
	equal(t, len(f.chatRequests()), 1)
}

func TestCompatPayloadHeadersAndUsage(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	t.Setenv("MY_KEY", "secret-123")
	f.handler = func(fakeRequest) fakeResponse {
		return fakeResponse{200, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "hello"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 1, "cost": 0.0025},
		}, nil}
	}
	cfg := compatConfig(f, "openai-compatible")
	cfg.APIKeyEnv, cfg.Temperature = "MY_KEY", 0
	result, err := mustProvider(t, cfg).Chat("sys", "user", 50)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, result.Text, "hello")
	equal(t, *result.PromptTokens, 5)
	equal(t, *result.OutputTokens, 1)
	equal(t, *result.Cost, 0.0025)
	request := f.chatRequests()[0]
	equal(t, request.Header.Get("Authorization"), "Bearer secret-123")
	equal(t, request.Header.Get("X-Title"), "")
	equal(t, request.Body["model"], any("fake-model"))
	equal(t, request.Body["max_tokens"], any(50.0))
	equal(t, request.Body["temperature"], any(0.0))
	equal(t, request.Body["reasoning_effort"], any("none"))
}

func TestCompatNoAuthorizationWithoutKey(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	if _, err := mustProvider(t, compatConfig(f, "openai-compatible")).Chat("s", "u", 10); err != nil {
		t.Fatal(err)
	}
	equal(t, f.chatRequests()[0].Header.Get("Authorization"), "")
}

func TestOpenRouterFields(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	t.Setenv("OPENROUTER_API_KEY", "or")
	f.handler = func(fakeRequest) fakeResponse { return chatResponse("ok", "stop", nil) }
	cfg := compatConfig(f, "openrouter")
	cfg.APIKeyEnv = "OPENROUTER_API_KEY"
	if _, err := mustProvider(t, cfg).Chat("s", "u", 10); err != nil {
		t.Fatal(err)
	}
	request := f.chatRequests()[0]
	equal(t, request.Header.Get("X-Title"), "local-shunt")
	equal(t, asMap(request.Body["reasoning"])["effort"], any("none"))
	_, hasEffort := request.Body["reasoning_effort"]
	equal(t, hasEffort, false)
}

func TestReasoningFieldRejectedThenDropped(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(r fakeRequest) fakeResponse {
		if _, ok := r.Body["reasoning_effort"]; ok {
			return fakeResponse{400, map[string]any{"error": map[string]any{"message": "Unrecognized request argument: reasoning_effort"}}, nil}
		}
		return chatResponse("fine", "stop", nil)
	}
	llm := mustProvider(t, compatConfig(f, "openai-compatible"))
	result, err := llm.Chat("s", "u", 10)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, result.Text, "fine")
	_, _ = llm.Chat("s", "u", 10) // later calls skip the field immediately
	var sent []bool
	for _, r := range f.chatRequests() {
		_, ok := r.Body["reasoning_effort"]
		sent = append(sent, ok)
	}
	equal(t, len(sent), 3)
	equal(t, sent[0] && !sent[1] && !sent[2], true)
}

func TestDisableReasoningOff(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	cfg := compatConfig(f, "openai-compatible")
	cfg.DisableReasoning = false
	_, _ = mustProvider(t, cfg).Chat("s", "u", 10)
	_, ok := f.chatRequests()[0].Body["reasoning_effort"]
	equal(t, ok, false)
}

func TestExtraBodyMerged(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	cfg := compatConfig(f, "openai-compatible")
	cfg.ExtraBody = map[string]any{"top_p": 0.5, "provider": map[string]any{"sort": "price"}}
	_, _ = mustProvider(t, cfg).Chat("s", "u", 10)
	body := f.chatRequests()[0].Body
	equal(t, body["top_p"], any(0.5))
	equal(t, asMap(body["provider"])["sort"], any("price"))
}

func TestRemoteRetriesThenSucceeds(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	responses := []fakeResponse{
		{429, map[string]any{"error": "rate limited"}, map[string]string{"Retry-After": "1"}},
		{502, map[string]any{"error": "bad gateway"}, nil},
		chatResponse("third", "stop", nil),
	}
	f.handler = func(fakeRequest) fakeResponse {
		r := responses[0]
		responses = responses[1:]
		return r
	}
	llm := mustProvider(t, compatConfig(f, "openai-compatible"))
	llm.IsLocal = false
	result, err := llm.Chat("s", "u", 10)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, result.Text, "third")
	equal(t, len(f.chatRequests()), 3)
}

// TestRemoteRetriesAfterTimeout guards against a one-off slow response permanently
// failing the whole call: a request timeout on a remote endpoint should be retried
// like any other transient failure, not treated as "the model stays slow forever".
func TestRemoteRetriesAfterTimeout(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	var first atomic.Bool
	first.Store(true)
	f.handler = func(fakeRequest) fakeResponse {
		if first.CompareAndSwap(true, false) {
			// The client gives up after RequestTimeout and retries; this goroutine
			// (serving the abandoned first request) keeps sleeping harmlessly.
			time.Sleep(1500 * time.Millisecond)
		}
		return chatResponse("ok", "stop", nil)
	}
	cfg := compatConfig(f, "openai-compatible")
	cfg.RequestTimeout = 1
	llm := mustProvider(t, cfg)
	llm.IsLocal = false
	result, err := llm.Chat("s", "u", 10)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, result.Text, "ok")
	equal(t, len(f.chatRequests()), 2)
}

func TestRemoteGivesUpAfterMaxRetries(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse { return fakeResponse{429, map[string]any{"error": "rate limited"}, nil} }
	cfg := compatConfig(f, "openai-compatible")
	cfg.MaxRetries = 2
	llm := mustProvider(t, cfg)
	llm.IsLocal = false
	_, err := llm.Chat("s", "u", 10)
	errorContains(t, err, "HTTP 429: rate limited")
	equal(t, len(f.chatRequests()), 3)
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse {
		return fakeResponse{401, map[string]any{"error": map[string]any{"message": "invalid key"}}, nil}
	}
	cfg := compatConfig(f, "openai-compatible")
	cfg.DisableReasoning = false
	llm := mustProvider(t, cfg)
	llm.IsLocal = false
	_, err := llm.Chat("s", "u", 10)
	errorContains(t, err, "HTTP 401: invalid key")
	equal(t, len(f.chatRequests()), 1)
}

func TestReasoningUsedWholeBudget(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse {
		return chatResponse(nil, "length", map[string]any{"reasoning": "long thoughts"})
	}
	llm := mustProvider(t, compatConfig(f, "openai-compatible"))
	llm.IsLocal = false
	_, err := llm.Chat("s", "u", 10)
	equal(t, isLLMError(err, kindModelOutput), true)
	equal(t, len(f.chatRequests()), 1)
}

func TestErrorInside200Body(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse {
		return fakeResponse{200, map[string]any{"error": map[string]any{"message": "upstream failed", "code": 502}}, nil}
	}
	_, err := mustProvider(t, compatConfig(f, "openai-compatible")).Chat("s", "u", 10)
	errorContains(t, err, "upstream failed")
}

func TestNoChoices(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse { return fakeResponse{200, map[string]any{"choices": []any{}}, nil} }
	_, err := mustProvider(t, compatConfig(f, "openai-compatible")).Chat("s", "u", 10)
	equal(t, isLLMError(err, kindModelOutput), true)
}

func TestGetProviderConfigError(t *testing.T) {
	isolate(t)
	_, err := getProvider(openrouterConfig(), "")
	errorContains(t, err, "API key not found")
}

func TestHealth(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	noCache := healthOptions{noCache: true}

	equal(t, checkHealth(ollamaConfig(f), noCache).OK, true) // "fake" matches "fake:latest"

	missing := ollamaConfig(f)
	missing.Model = "other:7b"
	health := checkHealth(missing, noCache)
	equal(t, health.OK, false)
	contain(t, health.Reason, "other:7b")

	unreachable := DefaultConfig()
	unreachable.OllamaHost, unreachable.Model = "http://127.0.0.1:9", "x"
	equal(t, checkHealth(unreachable, noCache).OK, false)

	equal(t, checkHealth(compatConfig(f, "openai-compatible"), noCache).OK, true)
	missingCompat := compatConfig(f, "openai-compatible")
	missingCompat.Model = "missing"
	equal(t, checkHealth(missingCompat, noCache).OK, false)
}

func TestHealthIsCached(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	cfg := ollamaConfig(f)
	equal(t, checkHealth(cfg, healthOptions{}).OK, true)
	f.handler = func(fakeRequest) fakeResponse { return fakeResponse{500, map[string]any{"error": "down"}, nil} }
	equal(t, checkHealth(cfg, healthOptions{}).OK, true)
	equal(t, checkHealth(cfg, healthOptions{noCache: true}).OK, false)
}

func TestCompatWithoutModelsEndpointIsAssumedOK(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(fakeRequest) fakeResponse {
		return fakeResponse{404, map[string]any{"error": "no such route"}, nil}
	}
	cfg := compatConfig(f, "openai-compatible")
	cfg.Model = "anything"
	equal(t, checkHealth(cfg, healthOptions{noCache: true}).OK, true)
}

// TestCompatModelsMalformedResponseIsAssumedOK guards against a healthy server whose
// /models response doesn't match the expected {"data": [...]} shape being misreported
// as "model not available".
func TestCompatModelsMalformedResponseIsAssumedOK(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	f.handler = func(r fakeRequest) fakeResponse {
		if r.Path == "/v1/models" {
			return fakeResponse{200, map[string]any{"unexpected": "shape"}, nil}
		}
		return f.defaultResponse(r)
	}
	cfg := compatConfig(f, "openai-compatible")
	cfg.Model = "anything"
	equal(t, checkHealth(cfg, healthOptions{noCache: true}).OK, true)
}

func TestRemoteNotProbedUnlessAsked(t *testing.T) {
	isolate(t)
	t.Setenv("OPENROUTER_API_KEY", "k")
	cfg := openrouterConfig()
	cfg.APIBase, cfg.Model = "http://10.255.255.1/v1", "m"
	health := checkHealth(cfg, healthOptions{})
	equal(t, health, Health{OK: true, Reason: "remote endpoint not probed"})
}

func TestRemoteProbeResultIsCachedForHook(t *testing.T) {
	isolate(t)
	f := newFakeLLM(t)
	t.Setenv("OPENROUTER_API_KEY", "k")
	// A remote-looking host name that resolves to the fake server is not available, so
	// point the provider at the fake server and treat it as remote through the cache key.
	cfg := openrouterConfig()
	cfg.APIBase, cfg.Model = f.URL+"/v1", "m"
	equal(t, checkHealth(cfg, healthOptions{noCache: true, probeRemote: true}).OK, false)
	f.handler = func(r fakeRequest) fakeResponse {
		return fakeResponse{200, map[string]any{"data": []any{map[string]any{"id": "m"}}}, nil}
	}
	equal(t, checkHealth(cfg, healthOptions{}).OK, false) // the hook sees the cached failure
}

func TestMissingKeyReported(t *testing.T) {
	isolate(t)
	health := checkHealth(openrouterConfig(), healthOptions{})
	equal(t, health.OK, false)
	contain(t, health.Reason, "OPENROUTER_API_KEY")
}

func TestModelAvailable(t *testing.T) {
	equal(t, modelAvailable("mistral", []string{"mistral:latest"}), true)
	equal(t, modelAvailable("qwen3:8b", []string{"qwen3:8b"}), true)
	equal(t, modelAvailable("qwen3:4b", []string{"qwen3:8b"}), false)
}
