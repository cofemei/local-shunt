// Package shunt implements local-shunt: configuration, the worker model client,
// delegated reads and writes, the Claude Code hook, the MCP server and measurement.
package shunt

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Version is reported by the MCP server and `local-shunt version`.
const Version = "0.4.0"

var defaultExclude = []string{
	"**/CLAUDE.md",
	"**/SKILL.md",
	"**/.claude/**",
	"**/.env*",
	"**/*.lock",
}

// Extensions the Read tool renders natively (images, PDFs, notebooks).
var nativeExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".ico": true,
	".tif": true, ".tiff": true, ".pdf": true, ".ipynb": true,
}

// Files larger than this are not line-counted in the hook (keeps it fast).
const lineCountByteCap = 64 * 1024 * 1024

// Tests lower this to exercise rotation.
var logRotateBytes int64 = 10 * 1024 * 1024

type providerPreset struct {
	apiBase   string
	apiKeyEnv string
}

// Provider presets: default API base and the environment variable holding the key.
var providerPresets = map[string]providerPreset{
	"ollama":            {},
	"openai-compatible": {},
	"openrouter":        {apiBase: "https://openrouter.ai/api/v1", apiKeyEnv: "OPENROUTER_API_KEY"},
	"openai":            {apiBase: "https://api.openai.com/v1", apiKeyEnv: "OPENAI_API_KEY"},
}

// providerNames lists the presets in documentation order; sortedProviderNames in sorted order.
var (
	providerNames       = []string{"ollama", "openai-compatible", "openrouter", "openai"}
	sortedProviderNames = []string{"ollama", "openai", "openai-compatible", "openrouter"}
)

// Config is the merged local-shunt configuration.
type Config struct {
	Enabled          bool
	Provider         string
	APIBase          string // OpenAI-compatible base URL, e.g. https://openrouter.ai/api/v1
	APIKeyEnv        string // name of the environment variable holding the API key
	EnvFiles         []string
	APIKeys          map[string]string // provider -> key
	MaxRetries       int
	DisableReasoning bool           // ask thinking models to skip reasoning (saves output budget)
	ExtraBody        map[string]any // merged into OpenAI-compatible request bodies
	OllamaHost       string
	Model            string
	MinLines         int
	MinBytes         int
	MaxFileBytes     int
	NumCtx           int
	MaxOutput        int
	Temperature      float64
	KeepAlive        string
	RequestTimeout   int
	HealthTimeoutMS  int
	InterceptBash    bool
	Exclude          []string
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:          true,
		Provider:         "ollama",
		EnvFiles:         []string{"~/.config/local-shunt/.env"},
		APIKeys:          map[string]string{},
		MaxRetries:       3,
		DisableReasoning: true,
		ExtraBody:        map[string]any{},
		OllamaHost:       "http://localhost:11434",
		Model:            "qwen2.5-coder:7b",
		MinLines:         350,
		MinBytes:         32768,
		MaxFileBytes:     2_000_000,
		NumCtx:           32768,
		MaxOutput:        1024,
		Temperature:      0.1,
		KeepAlive:        "15m",
		RequestTimeout:   300,
		HealthTimeoutMS:  300,
		InterceptBash:    true,
		Exclude:          append([]string(nil), defaultExclude...),
	}
}

// Settings that decide where file contents and API keys are sent. A project config
// may come from an untrusted repository, so these are read only from the user
// config file and the environment.
var trustedKeys = map[string]bool{
	"provider": true, "api_base": true, "api_key_env": true, "env_files": true,
	"api_keys": true, "ollama_host": true, "extra_body": true,
}

var envMap = [][2]string{
	{"provider", "LOCAL_SHUNT_PROVIDER"},
	{"api_base", "LOCAL_SHUNT_API_BASE"},
	{"api_key_env", "LOCAL_SHUNT_API_KEY_ENV"},
	{"ollama_host", "OLLAMA_HOST"},
	{"model", "LOCAL_SHUNT_MODEL"},
	{"min_lines", "LOCAL_SHUNT_MIN_LINES"},
	{"min_bytes", "LOCAL_SHUNT_MIN_BYTES"},
	{"num_ctx", "LOCAL_SHUNT_NUM_CTX"},
}

// ProjectDir is the Claude Code project directory, else cwd, else the working directory.
func ProjectDir(cwd string) string {
	if dir := os.Getenv("CLAUDE_PROJECT_DIR"); dir != "" {
		return dir
	}
	if cwd != "" {
		return cwd
	}
	wd, _ := os.Getwd()
	return wd
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func expandUser(path string) string {
	if path == "~" {
		return homeDir()
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(homeDir(), path[2:])
	}
	return path
}

func readJSONObject(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return map[string]any{}
	}
	if obj, ok := value.(map[string]any); ok {
		return obj
	}
	return map[string]any{}
}

func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

func coerceBool(value any) bool {
	if s, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return truthy(value)
}

func coerceInt(value any) (int, error) {
	switch v := value.(type) {
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("not an integer")
		}
		return int(v), nil
	case string:
		return strconv.Atoi(strings.TrimSpace(v))
	}
	return 0, fmt.Errorf("not an integer")
}

func coerceFloat(value any) (float64, error) {
	switch v := value.(type) {
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case float64:
		return v, nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(v), 64)
	}
	return 0, fmt.Errorf("not a number")
}

func coerceString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func coerceStringList(name string, value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", name)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, coerceString(item))
	}
	return out, nil
}

// setConfigKey applies one configuration value. Malformed values return an error and
// leave the field unchanged.
func setConfigKey(cfg *Config, key string, value any) error {
	var err error
	setInt := func(field *int) {
		var n int
		if n, err = coerceInt(value); err == nil {
			*field = n
		}
	}
	switch key {
	case "enabled":
		cfg.Enabled = coerceBool(value)
	case "disable_reasoning":
		cfg.DisableReasoning = coerceBool(value)
	case "intercept_bash":
		cfg.InterceptBash = coerceBool(value)
	case "provider":
		cfg.Provider = coerceString(value)
	case "api_base":
		cfg.APIBase = coerceString(value)
	case "api_key_env":
		cfg.APIKeyEnv = coerceString(value)
	case "ollama_host":
		cfg.OllamaHost = coerceString(value)
	case "model":
		cfg.Model = coerceString(value)
	case "keep_alive":
		cfg.KeepAlive = coerceString(value)
	case "max_retries":
		setInt(&cfg.MaxRetries)
	case "min_lines":
		setInt(&cfg.MinLines)
	case "min_bytes":
		setInt(&cfg.MinBytes)
	case "max_file_bytes":
		setInt(&cfg.MaxFileBytes)
	case "num_ctx":
		setInt(&cfg.NumCtx)
	case "max_output":
		setInt(&cfg.MaxOutput)
	case "request_timeout":
		setInt(&cfg.RequestTimeout)
	case "health_timeout_ms":
		setInt(&cfg.HealthTimeoutMS)
	case "temperature":
		var f float64
		if f, err = coerceFloat(value); err == nil {
			cfg.Temperature = f
		}
	case "env_files", "exclude":
		var list []string
		if list, err = coerceStringList(key, value); err == nil {
			if key == "env_files" {
				cfg.EnvFiles = list
			} else {
				cfg.Exclude = list
			}
		}
	case "api_keys":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("api_keys must be an object")
		}
		keys := make(map[string]string, len(obj))
		for k, v := range obj {
			keys[k] = coerceString(v)
		}
		cfg.APIKeys = keys
	case "extra_body":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("extra_body must be an object")
		}
		cfg.ExtraBody = obj
	}
	return err
}

var (
	schemeRE   = regexp.MustCompile(`^https?://`)
	portSuffix = regexp.MustCompile(`:\d+$`)
)

func normalizeHost(host string) string {
	host = strings.TrimRight(strings.TrimSpace(host), "/")
	if !schemeRE.MatchString(host) {
		// OLLAMA_HOST is often set as "0.0.0.0:11434" or "127.0.0.1".
		host = "http://" + host
	}
	if !portSuffix.MatchString(strings.SplitN(host, "//", 2)[1]) {
		host += ":11434"
	}
	return strings.Replace(host, "//0.0.0.0", "//127.0.0.1", 1)
}

// LoadConfig merges defaults < user config < project config < environment.
func LoadConfig(cwd string) Config {
	cfg := DefaultConfig()

	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(homeDir(), ".config")
	}
	userLayer := readJSONObject(filepath.Join(configHome, "local-shunt", "config.json"))
	projectLayer := readJSONObject(filepath.Join(ProjectDir(cwd), ".claude", "local-shunt.json"))
	for key := range trustedKeys {
		delete(projectLayer, key)
	}
	envLayer := map[string]any{}
	for _, pair := range envMap {
		if value := os.Getenv(pair[1]); value != "" {
			envLayer[pair[0]] = value
		}
	}
	if os.Getenv("LOCAL_SHUNT_DISABLE") == "1" {
		envLayer["enabled"] = false
	}

	for _, layer := range []map[string]any{userLayer, projectLayer, envLayer} {
		for key, value := range layer {
			_ = setConfigKey(&cfg, key, value) // Ignore malformed values; keep the previous layer.
		}
	}

	cfg.OllamaHost = normalizeHost(cfg.OllamaHost)
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if preset, ok := providerPresets[cfg.Provider]; ok {
		if cfg.APIBase == "" && preset.apiBase != "" {
			cfg.APIBase = preset.apiBase
		}
		if cfg.APIKeyEnv == "" {
			cfg.APIKeyEnv = preset.apiKeyEnv
		}
	}
	cfg.APIBase = strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/")
	return cfg
}

// ApplyOverrides applies command-line overrides. A new provider resets its preset
// endpoint and key variable.
func ApplyOverrides(cfg Config, provider, model string) Config {
	if provider != "" {
		cfg.Provider = strings.ToLower(strings.TrimSpace(provider))
		preset := providerPresets[cfg.Provider]
		if preset.apiBase != "" {
			cfg.APIBase = preset.apiBase
		}
		if cfg.Provider == "openrouter" || cfg.Provider == "openai" {
			cfg.APIKeyEnv = preset.apiKeyEnv
		}
	}
	if model != "" {
		cfg.Model = model
	}
	return cfg
}

var (
	dotenvKey     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	dotenvComment = regexp.MustCompile(`\s+#`)
)

// parseDotenv parses KEY=VALUE lines. Supports `export`, quotes and trailing comments.
func parseDotenv(text string) map[string]string {
	values := map[string]string{}
	for _, raw := range splitLines(text) {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimLeft(line[len("export "):], " \t")
		}
		key, value, _ := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !dotenvKey.MatchString(key) {
			continue
		}
		if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
			value = value[1 : len(value)-1]
		} else if loc := dotenvComment.FindStringIndex(value); loc != nil {
			value = strings.TrimSpace(value[:loc[0]])
		}
		values[key] = value
	}
	return values
}

// resolveAPIKey finds the API key: environment variable, then env_files, then
// api_keys in the user config.
func resolveAPIKey(cfg Config, cwd string) string {
	if cfg.APIKeyEnv != "" {
		if value := os.Getenv(cfg.APIKeyEnv); value != "" {
			return value
		}
		for _, name := range cfg.EnvFiles {
			path := expandUser(name)
			if !filepath.IsAbs(path) {
				path = filepath.Join(ProjectDir(cwd), path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if value := parseDotenv(string(data))[cfg.APIKeyEnv]; value != "" {
				return value
			}
		}
	}
	return cfg.APIKeys[cfg.Provider]
}

// configProblem describes a configuration error, or returns "".
func configProblem(cfg Config, cwd string) string {
	if _, ok := providerPresets[cfg.Provider]; !ok {
		return fmt.Sprintf("unknown provider %s (expected one of: %s)", pyQuote(cfg.Provider), strings.Join(providerNames, ", "))
	}
	if cfg.Provider != "ollama" && cfg.APIBase == "" {
		return fmt.Sprintf("provider %s needs api_base", cfg.Provider)
	}
	if cfg.APIKeyEnv != "" && resolveAPIKey(cfg, cwd) == "" {
		return fmt.Sprintf(
			"API key not found: set %s, add it to one of %s, or set api_keys.%s in ~/.config/local-shunt/config.json",
			cfg.APIKeyEnv, pyList(cfg.EnvFiles), cfg.Provider,
		)
	}
	return ""
}

func endpoint(cfg Config) string {
	if cfg.Provider == "ollama" {
		return cfg.OllamaHost
	}
	return cfg.APIBase
}

func isLocalEndpoint(url string) bool {
	if _, after, ok := strings.Cut(url, "//"); ok {
		url = after
	}
	hostname, _, _ := strings.Cut(url, "/")
	if strings.HasPrefix(hostname, "[") {
		hostname, _, _ = strings.Cut(hostname[1:], "]")
	} else if i := strings.LastIndex(hostname, ":"); i >= 0 {
		hostname = hostname[:i]
	}
	return hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1"
}

// globToRegex translates a glob with `**` support into a regex matching whole paths.
func globToRegex(pattern string) *regexp.Regexp {
	var out strings.Builder
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			out.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			out.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			out.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			out.WriteString("[^/]")
			i++
		default:
			out.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			i++
		}
	}
	return regexp.MustCompile("(?s)^" + out.String() + "$")
}

// relativeTo returns path relative to base when path lies inside base.
func relativeTo(path, base string) (string, bool) {
	path, base = filepath.Clean(path), filepath.Clean(base)
	if path == base {
		return ".", true
	}
	prefix := base
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if strings.HasPrefix(path, prefix) {
		return path[len(prefix):], true
	}
	return "", false
}

// isExcluded returns the first matching pattern, or "".
func isExcluded(path string, patterns []string, base string) string {
	candidates := []string{filepath.ToSlash(path)}
	if rel, ok := relativeTo(path, base); ok {
		candidates = append(candidates, filepath.ToSlash(rel))
	}
	for _, pattern := range patterns {
		regex := globToRegex(pattern)
		for _, c := range candidates {
			if regex.MatchString(c) {
				return pattern
			}
		}
	}
	return ""
}

// FileInfo describes a file inspected by the hook or the worker.
type FileInfo struct {
	Path   string
	Bytes  int64
	Lines  int // -1 when the file was too large to count
	Binary bool
}

// inspectFile returns size, line count and binary flag, or nil if unreadable.
func inspectFile(path string) *FileInfo {
	stat, err := os.Stat(path)
	if err != nil || !stat.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	head := make([]byte, 8192)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil
	}
	head = head[:n]
	info := &FileInfo{Path: path, Bytes: stat.Size(), Lines: -1, Binary: strings.IndexByte(string(head), 0) >= 0}
	if info.Binary || stat.Size() > lineCountByteCap {
		return info
	}
	lines := countByte(head, '\n')
	last := head
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			lines += countByte(buf[:n], '\n')
			last = buf[:n]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil
		}
	}
	if len(last) > 0 && last[len(last)-1] != '\n' {
		lines++
	}
	info.Lines = lines
	return info
}

func countByte(data []byte, b byte) int {
	count := 0
	for _, c := range data {
		if c == b {
			count++
		}
	}
	return count
}

// estimateTokens is a rough token estimate: ASCII characters / 3, other characters count as 1 each.
func estimateTokens(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if r > 127 {
			other++
		} else {
			ascii++
		}
	}
	return ascii/3 + other
}

// sessionID is the Claude Code session ID inherited from the environment (Bash tool or MCP server).
func sessionID() any {
	for _, name := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID"} {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return nil
}

func stateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".local", "state")
	}
	return filepath.Join(base, "local-shunt")
}
