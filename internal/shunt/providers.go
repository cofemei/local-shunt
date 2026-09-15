package shunt

// LLM providers: Ollama's native API and OpenAI-compatible APIs (OpenRouter, OpenAI,
// LM Studio, llama.cpp, vLLM, ...).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	localHealthTTL  = 30
	remoteHealthTTL = 600
)

var (
	retryStatuses = map[int]bool{408: true, 409: true, 429: true, 500: true, 502: true, 503: true, 504: true}
	thinkBlock    = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)
)

// Error kinds. They appear in the log as "error:<kind>".
const (
	kindLLM         = "LLMError"
	kindModelOutput = "ModelOutputError" // the request succeeded but the output is unusable; retrying will not help
	kindTimeout     = "RequestTimeout"   // retried like any transient remote failure (a one-off network stall isn't a slow model)
	kindHTTPStatus  = "HTTPStatusError"
)

// LLMError is a failure of the worker model or its API.
type LLMError struct {
	Kind       string
	Msg        string
	Status     int
	RetryAfter *float64
}

func (e *LLMError) Error() string {
	if e.Kind == kindHTTPStatus {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg)
	}
	return e.Msg
}

func llmError(kind, format string, args ...any) *LLMError {
	return &LLMError{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// Health is the result of a provider health check.
type Health struct {
	OK     bool
	Reason string
}

// ChatResult is one model response.
type ChatResult struct {
	Text         string
	PromptTokens *int
	OutputTokens *int
	Truncated    bool
	Cost         *float64
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func errorMessage(body string) string {
	var data any
	if json.Unmarshal([]byte(body), &data) != nil {
		return truncateRunes(strings.TrimSpace(body), 500)
	}
	errValue := data
	if obj, ok := data.(map[string]any); ok {
		if inner, ok := obj["error"]; ok {
			errValue = inner
		}
	}
	if obj, ok := errValue.(map[string]any); ok {
		message := asString(obj["message"])
		if message == "" {
			raw, _ := marshalJSON(obj)
			message = string(raw)
		}
		// OpenRouter puts the upstream provider's explanation in metadata.
		metadata := asMap(obj["metadata"])
		detail := metadata["raw"]
		if !truthy(detail) {
			detail = metadata["reason"]
		}
		if truthy(detail) {
			name := asString(metadata["provider_name"])
			if name == "" {
				name = "upstream"
			}
			message = fmt.Sprintf("%s (%s: %s)", message, name, truncateRunes(pyStr(detail), 300))
		}
		return truncateRunes(message, 500)
	}
	return truncateRunes(pyStr(errValue), 500)
}

func stripQuery(url string) string {
	before, _, _ := strings.Cut(url, "?")
	return before
}

func httpJSON(url string, payload any, timeout time.Duration, headers map[string]string) (map[string]any, error) {
	method, body := http.MethodGet, io.Reader(nil)
	if payload != nil {
		data, err := marshalJSON(payload)
		if err != nil {
			return nil, llmError(kindLLM, "cannot encode request: %v", err)
		}
		method, body = http.MethodPost, bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, llmError(kindLLM, "cannot reach %s: %v", stripQuery(url), err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, llmError(kindTimeout, "%s did not respond within %s s", stripQuery(url), strconv.FormatFloat(timeout.Seconds(), 'g', -1, 64))
		}
		reason := err
		var urlErr interface{ Unwrap() error }
		if errors.As(err, &urlErr) && urlErr.Unwrap() != nil {
			reason = urlErr.Unwrap()
		}
		return nil, llmError(kindLLM, "cannot reach %s: %v", stripQuery(url), reason)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, llmError(kindTimeout, "%s did not respond within %s s", stripQuery(url), strconv.FormatFloat(timeout.Seconds(), 'g', -1, 64))
		}
		return nil, llmError(kindLLM, "cannot reach %s: %v", stripQuery(url), err)
	}
	if resp.StatusCode >= 400 {
		e := &LLMError{Kind: kindHTTPStatus, Status: resp.StatusCode, Msg: errorMessage(strings.ToValidUTF8(string(raw), "\uFFFD"))}
		if value := resp.Header.Get("Retry-After"); value != "" {
			if seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				e.RetryAfter = &seconds
			}
		}
		return nil, e
	}
	var result any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, llmError(kindLLM, "invalid JSON from %s: %v", url, err)
	}
	obj, ok := result.(map[string]any)
	if !ok {
		return nil, llmError(kindLLM, "invalid JSON from %s: expected an object", url)
	}
	if truthy(obj["error"]) {
		data, _ := marshalJSON(obj)
		return nil, llmError(kindLLM, "API error: %s", errorMessage(string(data)))
	}
	return obj, nil
}

// sleep is replaced in tests to avoid waiting.
var sleep = time.Sleep

// backoff is the delay before retry number attempt+1: 2, 4, 8 ... seconds, capped at 30.
func backoff(attempt int) time.Duration {
	return time.Duration(math.Min(math.Pow(2, float64(attempt+1)), 30) * float64(time.Second))
}

// Provider talks to one worker model endpoint.
type Provider struct {
	Cfg              Config
	cwd              string
	base             string
	IsLocal          bool
	sendReasoningOff bool
}

// getProvider validates the configuration and returns a provider for it.
func getProvider(cfg Config, cwd string) (*Provider, error) {
	if problem := configProblem(cfg, cwd); problem != "" {
		return nil, llmError(kindLLM, "%s", problem)
	}
	base := endpoint(cfg)
	return &Provider{Cfg: cfg, cwd: cwd, base: base, IsLocal: isLocalEndpoint(base), sendReasoningOff: cfg.DisableReasoning}, nil
}

func (p *Provider) ollama() bool { return p.Cfg.Provider == "ollama" }

func (p *Provider) requestTimeout() time.Duration {
	return time.Duration(p.Cfg.RequestTimeout) * time.Second
}

// Chat sends one system and user message, retrying transient failures of remote endpoints.
func (p *Provider) Chat(system, user string, maxOutput int) (ChatResult, error) {
	attempts := 1
	if !p.IsLocal {
		attempts += max(0, p.Cfg.MaxRetries)
	}
	for attempt := 0; ; attempt++ {
		var (
			result ChatResult
			err    error
		)
		if p.ollama() {
			result, err = p.ollamaChat(system, user, maxOutput)
		} else {
			result, err = p.compatChat(system, user, maxOutput)
		}
		if err == nil {
			result.Text = strings.TrimSpace(thinkBlock.ReplaceAllString(result.Text, ""))
			return result, nil
		}
		var e *LLMError
		if !errors.As(err, &e) || attempt == attempts-1 {
			return result, err
		}
		delay := backoff(attempt)
		switch e.Kind {
		case kindHTTPStatus:
			if !retryStatuses[e.Status] {
				return result, err
			}
			if e.RetryAfter != nil && *e.RetryAfter <= 60 {
				delay = time.Duration(*e.RetryAfter * float64(time.Second))
			}
		case kindModelOutput:
			return result, err
		}
		sleep(delay)
	}
}

// listModels returns the model names, or nil with ok=false when the server has no listing.
func (p *Provider) listModels(timeout time.Duration) (names []string, ok bool, err error) {
	if p.ollama() {
		tags, err := httpJSON(p.base+"/api/tags", nil, timeout, nil)
		if err != nil {
			return nil, false, err
		}
		models, _ := tags["models"].([]any)
		for _, m := range models {
			names = append(names, asString(asMap(m)["name"]))
		}
		return names, true, nil
	}
	data, err := httpJSON(p.base+"/models", nil, timeout, p.headers())
	if err != nil {
		var e *LLMError
		if errors.As(err, &e) && e.Kind == kindHTTPStatus && (e.Status == 404 || e.Status == 405) {
			return nil, false, nil // server has no model listing; cannot verify
		}
		return nil, false, err
	}
	models, ok := data["data"].([]any)
	if !ok {
		return nil, false, nil // response doesn't match the expected shape; cannot verify
	}
	for _, m := range models {
		if obj, isObj := m.(map[string]any); isObj {
			names = append(names, asString(obj["id"]))
		}
	}
	return names, true, nil
}

func (p *Provider) ollamaChat(system, user string, maxOutput int) (ChatResult, error) {
	cfg := p.Cfg
	payload := map[string]any{
		"model":      cfg.Model,
		"messages":   []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
		"stream":     false,
		"think":      false,
		"keep_alive": cfg.KeepAlive,
		"options":    map[string]any{"temperature": cfg.Temperature, "num_ctx": cfg.NumCtx, "num_predict": maxOutput},
	}
	url := p.base + "/api/chat"
	resp, err := httpJSON(url, payload, p.requestTimeout(), nil)
	var e *LLMError
	if err != nil && errors.As(err, &e) && e.Kind == kindHTTPStatus && strings.Contains(strings.ToLower(e.Error()), "think") {
		// Some models reject the `think` field entirely; retry without it.
		delete(payload, "think")
		resp, err = httpJSON(url, payload, p.requestTimeout(), nil)
	}
	if err != nil {
		return ChatResult{}, err
	}
	return ChatResult{
		Text:         asString(asMap(resp["message"])["content"]),
		PromptTokens: intPtr(resp["prompt_eval_count"]),
		OutputTokens: intPtr(resp["eval_count"]),
		Truncated:    resp["done_reason"] == "length",
	}, nil
}

func (p *Provider) headers() map[string]string {
	headers := map[string]string{}
	if key := resolveAPIKey(p.Cfg, p.cwd); key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	if p.Cfg.Provider == "openrouter" {
		headers["X-Title"] = "local-shunt"
	}
	return headers
}

func (p *Provider) reasoningOffFields() map[string]any {
	if p.Cfg.Provider == "openrouter" {
		return map[string]any{"reasoning": map[string]any{"effort": "none"}}
	}
	return map[string]any{"reasoning_effort": "none"}
}

func (p *Provider) compatChat(system, user string, maxOutput int) (ChatResult, error) {
	cfg := p.Cfg
	payload := map[string]any{
		"model":       cfg.Model,
		"messages":    []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
		"temperature": cfg.Temperature,
		"max_tokens":  maxOutput,
		"stream":      false,
	}
	if p.sendReasoningOff {
		for key, value := range p.reasoningOffFields() {
			payload[key] = value
		}
	}
	for key, value := range cfg.ExtraBody {
		payload[key] = value
	}
	url := p.base + "/chat/completions"
	resp, err := httpJSON(url, payload, p.requestTimeout(), p.headers())
	var e *LLMError
	if err != nil && p.sendReasoningOff && errors.As(err, &e) && e.Kind == kindHTTPStatus && (e.Status == 400 || e.Status == 422) {
		// Models without adjustable reasoning may reject the field; retry without it.
		p.sendReasoningOff = false
		for key := range p.reasoningOffFields() {
			if _, set := cfg.ExtraBody[key]; !set {
				delete(payload, key)
			}
		}
		resp, err = httpJSON(url, payload, p.requestTimeout(), p.headers())
	}
	if err != nil {
		return ChatResult{}, err
	}

	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ChatResult{}, llmError(kindModelOutput, "API returned no choices")
	}
	choice := asMap(choices[0])
	message := asMap(choice["message"])
	usage := asMap(resp["usage"])
	text := asString(message["content"])
	truncated := choice["finish_reason"] == "length"
	if truncated && strings.TrimSpace(text) == "" && (truthy(message["reasoning"]) || truthy(message["reasoning_content"])) {
		return ChatResult{}, llmError(kindModelOutput,
			"the model used the whole output budget on reasoning and returned no answer; "+
				"choose a non-reasoning model or raise --max-output")
	}
	result := ChatResult{
		Text:         text,
		PromptTokens: intPtr(usage["prompt_tokens"]),
		OutputTokens: intPtr(usage["completion_tokens"]),
		Truncated:    truncated,
	}
	if cost, ok := number(usage["cost"]); ok {
		result.Cost = &cost
	}
	return result, nil
}

func modelAvailable(model string, names []string) bool {
	for _, name := range names {
		if name == model || (!strings.Contains(model, ":") && name == model+":latest") {
			return true
		}
	}
	return false
}

// healthOptions tune checkHealth.
type healthOptions struct {
	timeoutMS   int // 0 uses health_timeout_ms
	noCache     bool
	probeRemote bool
	cwd         string
}

// checkHealth checks that the provider is configured, reachable and has the model.
//
// Local endpoints are probed over HTTP. Remote endpoints are probed only when
// probeRemote is set (SessionStart); the hook relies on the cached result or,
// without one, on the configuration check alone, so it never waits on the network.
func checkHealth(cfg Config, opts healthOptions) Health {
	if problem := configProblem(cfg, opts.cwd); problem != "" {
		return Health{Reason: problem}
	}

	base := endpoint(cfg)
	local := isLocalEndpoint(base)
	ttl := float64(remoteHealthTTL)
	if local {
		ttl = localHealthTTL
	}
	cacheFile := filepath.Join(stateDir(), "health.json")
	key := cfg.Provider + "|" + base + "|" + cfg.Model
	now := float64(time.Now().UnixNano()) / 1e9
	if !opts.noCache {
		if data, err := os.ReadFile(cacheFile); err == nil {
			var cached struct {
				Key    string  `json:"key"`
				TS     float64 `json:"ts"`
				OK     *bool   `json:"ok"`
				Reason string  `json:"reason"`
			}
			if json.Unmarshal(data, &cached) == nil && cached.OK != nil && cached.Key == key && now-cached.TS < ttl {
				return Health{OK: *cached.OK, Reason: cached.Reason}
			}
		}
	}
	if !local && !opts.probeRemote {
		return Health{OK: true, Reason: "remote endpoint not probed"}
	}

	timeoutMS := opts.timeoutMS
	if timeoutMS == 0 {
		timeoutMS = cfg.HealthTimeoutMS
	}
	provider, err := getProvider(cfg, opts.cwd)
	if err != nil {
		return Health{Reason: err.Error()}
	}
	var health Health
	names, listed, err := provider.listModels(time.Duration(timeoutMS) * time.Millisecond)
	switch {
	case err != nil:
		health = Health{Reason: err.Error()}
	case !listed || modelAvailable(cfg.Model, names):
		health = Health{OK: true}
	default:
		health = Health{Reason: fmt.Sprintf("model %s is not available from %s", cfg.Model, cfg.Provider)}
	}

	if os.MkdirAll(filepath.Dir(cacheFile), 0o755) == nil {
		data, _ := marshalJSON(record{{"key", key}, {"ts", now}, {"ok", health.OK}, {"reason", health.Reason}})
		_ = os.WriteFile(cacheFile, data, 0o644)
	}
	return health
}
