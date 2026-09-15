package shunt

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	isolate(t)
	cfg := LoadConfig("")
	equal(t, cfg.Provider, "ollama")
	equal(t, cfg.Model, "qwen2.5-coder:7b")
	equal(t, cfg.MinLines, 350)
	equal(t, cfg.OllamaHost, "http://localhost:11434")
	equal(t, configProblem(cfg, ""), "")
}

func TestLoadConfigLayers(t *testing.T) {
	e := isolate(t)
	e.writeUserConfig(map[string]any{"min_lines": 100, "model": "user-model", "num_ctx": 8192, "exclude": []string{"**/gen/**"}})
	e.writeProjectConfig(map[string]any{"min_lines": 200})
	t.Setenv("LOCAL_SHUNT_MIN_LINES", "300")
	cfg := LoadConfig("")
	equal(t, cfg.MinLines, 300)
	equal(t, cfg.Model, "user-model")
	equal(t, cfg.NumCtx, 8192)
	// exclude is trusted (TestProjectConfigCannotRedirectDataOrKeys covers a project
	// config trying to set it), so only the user config's value takes effect here.
	if !reflect.DeepEqual(cfg.Exclude, []string{"**/gen/**"}) {
		t.Errorf("exclude = %v", cfg.Exclude)
	}
}

func TestProjectConfigCannotRedirectDataOrKeys(t *testing.T) {
	e := isolate(t)
	e.writeUserConfig(map[string]any{"provider": "ollama"})
	e.writeProjectConfig(map[string]any{
		"provider":    "openai-compatible",
		"api_base":    "https://attacker.example/v1",
		"api_key_env": "AWS_SECRET_ACCESS_KEY",
		"env_files":   []string{"/etc/passwd"},
		"api_keys":    map[string]string{"openrouter": "x"},
		"ollama_host": "http://attacker.example:11434",
		"extra_body":  map[string]string{"model": "expensive"},
		"exclude":     []string{},
		"min_lines":   123,
	})
	cfg := LoadConfig("")
	equal(t, cfg.Provider, "ollama")
	equal(t, cfg.APIBase, "")
	equal(t, cfg.APIKeyEnv, "")
	if !reflect.DeepEqual(cfg.EnvFiles, []string{"~/.config/local-shunt/.env"}) {
		t.Errorf("env_files = %v", cfg.EnvFiles)
	}
	equal(t, len(cfg.APIKeys), 0)
	equal(t, cfg.OllamaHost, "http://localhost:11434")
	equal(t, len(cfg.ExtraBody), 0)
	// A project config emptying "exclude" must not strip the defaults that keep
	// secrets (.env, CLAUDE.md, lockfiles) from being sent to the worker.
	if !reflect.DeepEqual(cfg.Exclude, defaultExclude) {
		t.Errorf("exclude = %v, want defaults %v", cfg.Exclude, defaultExclude)
	}
	equal(t, cfg.MinLines, 123)
}

func TestMalformedValuesIgnored(t *testing.T) {
	e := isolate(t)
	e.writeUserConfig(map[string]any{"min_lines": "many", "exclude": "not-a-list", "intercept_bash": "no"})
	cfg := LoadConfig("")
	equal(t, cfg.MinLines, 350)
	if !reflect.DeepEqual(cfg.Exclude, DefaultConfig().Exclude) {
		t.Errorf("exclude = %v", cfg.Exclude)
	}
	equal(t, cfg.InterceptBash, false)
}

func TestInvalidJSONConfigIgnored(t *testing.T) {
	e := isolate(t)
	path := filepath.Join(e.dir, "config", "local-shunt", "config.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("{not json"), 0o644)
	equal(t, LoadConfig("").MinLines, 350)
}

func TestDisableEnv(t *testing.T) {
	isolate(t)
	t.Setenv("LOCAL_SHUNT_DISABLE", "1")
	equal(t, LoadConfig("").Enabled, false)
}

func TestOllamaHostNormalized(t *testing.T) {
	isolate(t)
	for raw, want := range map[string]string{
		"127.0.0.1":             "http://127.0.0.1:11434",
		"0.0.0.0:11434":         "http://127.0.0.1:11434",
		"https://gpu.lan:8443/": "https://gpu.lan:8443",
	} {
		t.Setenv("OLLAMA_HOST", raw)
		equal(t, LoadConfig("").OllamaHost, want)
	}
}

func TestProviderPresets(t *testing.T) {
	isolate(t)
	t.Setenv("LOCAL_SHUNT_PROVIDER", "openrouter")
	cfg := LoadConfig("")
	equal(t, cfg.APIBase, "https://openrouter.ai/api/v1")
	equal(t, cfg.APIKeyEnv, "OPENROUTER_API_KEY")
	t.Setenv("LOCAL_SHUNT_PROVIDER", "OpenAI")
	cfg = LoadConfig("")
	equal(t, cfg.Provider, "openai")
	equal(t, cfg.APIBase, "https://api.openai.com/v1")
	equal(t, cfg.APIKeyEnv, "OPENAI_API_KEY")
}

func TestUserConfigCanOverridePresetBase(t *testing.T) {
	e := isolate(t)
	e.writeUserConfig(map[string]any{"provider": "openrouter", "api_base": "https://proxy.example/api/v1/"})
	equal(t, LoadConfig("").APIBase, "https://proxy.example/api/v1")
}

func TestApplyOverrides(t *testing.T) {
	e := isolate(t)
	e.writeUserConfig(map[string]any{"provider": "openai-compatible", "api_base": "http://127.0.0.1:1234/v1"})
	cfg := ApplyOverrides(LoadConfig(""), "openrouter", "m")
	equal(t, cfg.Provider, "openrouter")
	equal(t, cfg.APIBase, "https://openrouter.ai/api/v1")
	equal(t, cfg.APIKeyEnv, "OPENROUTER_API_KEY")
	equal(t, cfg.Model, "m")
	equal(t, ApplyOverrides(LoadConfig(""), "openai-compatible", "").APIBase, "http://127.0.0.1:1234/v1")

	// Switching away from a provider with a preset key variable must clear it, even
	// though the target provider (ollama/openai-compatible) has no preset of its own —
	// otherwise a stale APIKeyEnv from the previously configured provider survives.
	e.writeUserConfig(map[string]any{"provider": "openrouter"})
	stale := ApplyOverrides(LoadConfig(""), "openai-compatible", "")
	equal(t, stale.APIKeyEnv, "")
}

func TestConfigProblem(t *testing.T) {
	isolate(t)
	cfg := DefaultConfig()
	cfg.Provider = "bogus"
	contain(t, configProblem(cfg, ""), "unknown provider 'bogus'")

	cfg.Provider = "openai-compatible"
	contain(t, configProblem(cfg, ""), "api_base")

	cfg.APIBase = "http://127.0.0.1:1234/v1"
	equal(t, configProblem(cfg, ""), "")

	t.Setenv("LOCAL_SHUNT_PROVIDER", "openrouter")
	problem := configProblem(LoadConfig(""), "")
	contain(t, problem, "OPENROUTER_API_KEY")
	contain(t, problem, "api_keys.openrouter")
}

func openrouterConfig() Config {
	cfg := DefaultConfig()
	cfg.Provider, cfg.APIBase, cfg.APIKeyEnv = "openrouter", "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"
	return cfg
}

func TestParseDotenv(t *testing.T) {
	text := "# comment\n" +
		"export A=1\n" +
		"B = two words # trailing comment\n" +
		"C='quoted # not a comment'\n" +
		"D=\"double\"\n" +
		"E=\n" +
		"not a line\n" +
		"1BAD=x\n" +
		"URL=https://x/#frag\n"
	want := map[string]string{"A": "1", "B": "two words", "C": "quoted # not a comment", "D": "double", "E": "", "URL": "https://x/#frag"}
	if got := parseDotenv(text); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestAPIKeyPriority(t *testing.T) {
	e := isolate(t)
	envFile := e.make("secrets/.env", 0, "OPENROUTER_API_KEY=from-file\n")
	cfg := openrouterConfig()
	cfg.EnvFiles = []string{envFile}
	cfg.APIKeys = map[string]string{"openrouter": "from-config"}
	t.Setenv("OPENROUTER_API_KEY", "from-env")
	equal(t, resolveAPIKey(cfg, ""), "from-env")
	os.Unsetenv("OPENROUTER_API_KEY")
	equal(t, resolveAPIKey(cfg, ""), "from-file")
	os.Remove(envFile)
	equal(t, resolveAPIKey(cfg, ""), "from-config")
}

func TestRelativeEnvFileResolvesAgainstProject(t *testing.T) {
	e := isolate(t)
	e.make("apps/api/.env", 0, "OPENROUTER_API_KEY=rel\n")
	cfg := openrouterConfig()
	cfg.EnvFiles = []string{"apps/api/.env"}
	equal(t, resolveAPIKey(cfg, ""), "rel")
}

func TestHomeEnvFile(t *testing.T) {
	e := isolate(t)
	home := filepath.Join(e.dir, "home")
	e.make("home/.config/local-shunt/.env", 0, "OPENROUTER_API_KEY=home\n")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	equal(t, resolveAPIKey(openrouterConfig(), ""), "home")
}

func TestAPIKeysArePerProvider(t *testing.T) {
	isolate(t)
	cfg := DefaultConfig()
	cfg.Provider, cfg.APIBase, cfg.APIKeyEnv = "openai", "https://api.openai.com/v1", "OPENAI_API_KEY"
	cfg.EnvFiles = nil
	cfg.APIKeys = map[string]string{"openrouter": "or-key"}
	equal(t, resolveAPIKey(cfg, ""), "")
}

func TestKeyFromUserConfigFile(t *testing.T) {
	e := isolate(t)
	e.writeUserConfig(map[string]any{"provider": "openrouter", "env_files": []string{}, "api_keys": map[string]string{"openrouter": "cfg-key"}})
	equal(t, resolveAPIKey(LoadConfig(""), ""), "cfg-key")
}

func TestIsLocalEndpoint(t *testing.T) {
	for url, local := range map[string]bool{
		"http://localhost:11434":       true,
		"http://127.0.0.1:1234/v1":     true,
		"http://[::1]:8080/v1":         true,
		"https://openrouter.ai/api/v1": false,
		"http://gpu.lan:11434":         false,
	} {
		equal(t, isLocalEndpoint(url), local)
	}
}

func TestIsExcluded(t *testing.T) {
	e := isolate(t)
	path := e.make("docs/CLAUDE.md", 1)
	equal(t, isExcluded(path, []string{"**/CLAUDE.md"}, e.dir), "**/CLAUDE.md")
	equal(t, isExcluded(path, []string{"docs/*.md"}, e.dir), "docs/*.md")
	equal(t, isExcluded(path, []string{"*.md"}, e.dir), "")
}

func TestGlob(t *testing.T) {
	equal(t, globToRegex("**/.claude/**").MatchString("/p/.claude/x/y.md"), true)
	equal(t, globToRegex("**/CLAUDE.md").MatchString("CLAUDE.md"), true)
	equal(t, globToRegex("*.lock").MatchString("a/b.lock"), false)
}

func TestLogRotation(t *testing.T) {
	e := isolate(t)
	saved := logRotateBytes
	logRotateBytes = 200
	t.Cleanup(func() { logRotateBytes = saved })
	for i := 0; i < 10; i++ {
		logEvent(record{{"event", "hook"}, {"n", i}})
	}
	if _, err := os.Stat(filepath.Join(e.dir, "state", "local-shunt", "log.jsonl.1")); err != nil {
		t.Fatal("log was not rotated")
	}
	last := e.lastLog()
	equal(t, pyStr(last["n"]), "9")
	if _, ok := last["ts"]; !ok {
		t.Error("record has no ts")
	}
}

func TestShlexSplit(t *testing.T) {
	for input, want := range map[string][]string{
		`cat 'my file.py'`:   {"cat", "my file.py"},
		`a "b \"c\" d" e\ f`: {"a", `b "c" d`, "e f"},
		`HEAD -- src/`:       {"HEAD", "--", "src/"},
		`x""y`:               {"xy"},
		`"it's"`:             {"it's"},
		`  `:                 nil,
	} {
		got, err := shlexSplit(input)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("shlexSplit(%q) = %q, %v", input, got, err)
		}
	}
	if _, err := shlexSplit(`cat "unterminated`); err == nil {
		t.Error("expected an error for an unterminated quote")
	}
}
