"""Tests for configuration layering, API key resolution and logging."""

from __future__ import annotations

import json
import os
import unittest
from unittest import mock

from helpers import IsolatedTestCase

import common
from common import (
    Config,
    apply_overrides,
    config_problem,
    is_excluded,
    is_local_endpoint,
    load_config,
    log_event,
    parse_dotenv,
    resolve_api_key,
)


class LoadConfigTest(IsolatedTestCase):
    def test_defaults(self):
        cfg = load_config()
        self.assertEqual((cfg.provider, cfg.model, cfg.min_lines), ("ollama", "qwen2.5-coder:7b", 350))
        self.assertEqual(cfg.ollama_host, "http://localhost:11434")
        self.assertIsNone(config_problem(cfg))

    def test_layers_user_project_env(self):
        self.write_user_config({"min_lines": 100, "model": "user-model", "num_ctx": 8192})
        self.write_project_config({"min_lines": 200, "exclude": ["**/gen/**"]})
        os.environ["LOCAL_SHUNT_MIN_LINES"] = "300"
        cfg = load_config()
        self.assertEqual(cfg.min_lines, 300)
        self.assertEqual(cfg.model, "user-model")
        self.assertEqual(cfg.num_ctx, 8192)
        self.assertEqual(cfg.exclude, ["**/gen/**"])

    def test_project_config_cannot_redirect_data_or_keys(self):
        self.write_user_config({"provider": "ollama"})
        self.write_project_config({
            "provider": "openai-compatible",
            "api_base": "https://attacker.example/v1",
            "api_key_env": "AWS_SECRET_ACCESS_KEY",
            "env_files": ["/etc/passwd"],
            "api_keys": {"openrouter": "x"},
            "ollama_host": "http://attacker.example:11434",
            "extra_body": {"model": "expensive"},
            "min_lines": 123,
        })
        cfg = load_config()
        self.assertEqual(cfg.provider, "ollama")
        self.assertEqual(cfg.api_base, "")
        self.assertEqual(cfg.api_key_env, "")
        self.assertEqual(cfg.env_files, ["~/.config/local-shunt/.env"])
        self.assertEqual(cfg.api_keys, {})
        self.assertEqual(cfg.ollama_host, "http://localhost:11434")
        self.assertEqual(cfg.extra_body, {})
        self.assertEqual(cfg.min_lines, 123)

    def test_malformed_values_ignored(self):
        self.write_user_config({"min_lines": "many", "exclude": "not-a-list", "intercept_bash": "no"})
        cfg = load_config()
        self.assertEqual(cfg.min_lines, 350)
        self.assertEqual(cfg.exclude, Config().exclude)
        self.assertFalse(cfg.intercept_bash)

    def test_invalid_json_file_ignored(self):
        path = self.dir / "config" / "local-shunt" / "config.json"
        path.parent.mkdir(parents=True)
        path.write_text("{not json")
        self.assertEqual(load_config().min_lines, 350)

    def test_disable_env(self):
        os.environ["LOCAL_SHUNT_DISABLE"] = "1"
        self.assertFalse(load_config().enabled)

    def test_ollama_host_normalized(self):
        for raw, expected in (
            ("127.0.0.1", "http://127.0.0.1:11434"),
            ("0.0.0.0:11434", "http://127.0.0.1:11434"),
            ("https://gpu.lan:8443/", "https://gpu.lan:8443"),
        ):
            os.environ["OLLAMA_HOST"] = raw
            self.assertEqual(load_config().ollama_host, expected, raw)

    def test_provider_presets(self):
        os.environ["LOCAL_SHUNT_PROVIDER"] = "openrouter"
        cfg = load_config()
        self.assertEqual((cfg.api_base, cfg.api_key_env), ("https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"))
        os.environ["LOCAL_SHUNT_PROVIDER"] = "OpenAI"
        cfg = load_config()
        self.assertEqual((cfg.provider, cfg.api_base, cfg.api_key_env), ("openai", "https://api.openai.com/v1", "OPENAI_API_KEY"))

    def test_user_config_can_override_preset_base(self):
        self.write_user_config({"provider": "openrouter", "api_base": "https://proxy.example/api/v1/"})
        self.assertEqual(load_config().api_base, "https://proxy.example/api/v1")

    def test_apply_overrides(self):
        self.write_user_config({"provider": "openai-compatible", "api_base": "http://127.0.0.1:1234/v1"})
        cfg = apply_overrides(load_config(), provider="openrouter", model="m")
        self.assertEqual((cfg.provider, cfg.api_base, cfg.api_key_env, cfg.model),
                         ("openrouter", "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY", "m"))
        cfg = apply_overrides(load_config(), provider="openai-compatible")
        self.assertEqual(cfg.api_base, "http://127.0.0.1:1234/v1")


class ConfigProblemTest(IsolatedTestCase):
    def test_unknown_provider(self):
        self.assertIn("unknown provider", config_problem(Config(provider="bogus")))

    def test_openai_compatible_needs_base(self):
        self.assertIn("api_base", config_problem(Config(provider="openai-compatible")))

    def test_missing_key(self):
        os.environ["LOCAL_SHUNT_PROVIDER"] = "openrouter"
        problem = config_problem(load_config())
        self.assertIn("OPENROUTER_API_KEY", problem)
        self.assertIn("api_keys.openrouter", problem)

    def test_openai_compatible_without_key_is_fine(self):
        self.assertIsNone(config_problem(Config(provider="openai-compatible", api_base="http://127.0.0.1:1234/v1")))


class ApiKeyTest(IsolatedTestCase):
    def cfg(self, **kwargs) -> Config:
        return Config(provider="openrouter", api_base="https://openrouter.ai/api/v1", api_key_env="OPENROUTER_API_KEY", **kwargs)

    def test_parse_dotenv(self):
        text = (
            "# comment\n"
            "export A=1\n"
            "B = two words # trailing comment\n"
            "C='quoted # not a comment'\n"
            'D="double"\n'
            "E=\n"
            "not a line\n"
            "1BAD=x\n"
            "URL=https://x/#frag\n"
        )
        self.assertEqual(parse_dotenv(text), {
            "A": "1", "B": "two words", "C": "quoted # not a comment", "D": "double", "E": "", "URL": "https://x/#frag",
        })

    def test_priority_env_then_file_then_user_config(self):
        env_file = self.make("secrets/.env", content="OPENROUTER_API_KEY=from-file\n")
        cfg = self.cfg(env_files=[str(env_file)], api_keys={"openrouter": "from-config"})
        os.environ["OPENROUTER_API_KEY"] = "from-env"
        self.assertEqual(resolve_api_key(cfg), "from-env")
        del os.environ["OPENROUTER_API_KEY"]
        self.assertEqual(resolve_api_key(cfg), "from-file")
        env_file.unlink()
        self.assertEqual(resolve_api_key(cfg), "from-config")

    def test_relative_env_file_resolves_against_project(self):
        self.make("apps/api/.env", content="OPENROUTER_API_KEY=rel\n")
        self.assertEqual(resolve_api_key(self.cfg(env_files=["apps/api/.env"])), "rel")

    def test_home_env_file(self):
        home = self.dir / "home"
        (home / ".config" / "local-shunt").mkdir(parents=True)
        (home / ".config" / "local-shunt" / ".env").write_text("OPENROUTER_API_KEY=home\n")
        os.environ["HOME"], saved = str(home), os.environ.get("HOME")
        try:
            self.assertEqual(resolve_api_key(self.cfg()), "home")
        finally:
            os.environ["HOME"] = saved

    def test_api_keys_are_per_provider(self):
        cfg = Config(provider="openai", api_base="https://api.openai.com/v1", api_key_env="OPENAI_API_KEY",
                     env_files=[], api_keys={"openrouter": "or-key"})
        self.assertIsNone(resolve_api_key(cfg))

    def test_key_from_user_config_file(self):
        self.write_user_config({"provider": "openrouter", "env_files": [], "api_keys": {"openrouter": "cfg-key"}})
        cfg = load_config()
        self.assertEqual(resolve_api_key(cfg), "cfg-key")
        self.assertNotIn("cfg-key", repr(cfg))


class HelpersTest(IsolatedTestCase):
    def test_is_local_endpoint(self):
        for url, local in (
            ("http://localhost:11434", True),
            ("http://127.0.0.1:1234/v1", True),
            ("http://[::1]:8080/v1", True),
            ("https://openrouter.ai/api/v1", False),
            ("http://gpu.lan:11434", False),
        ):
            self.assertEqual(is_local_endpoint(url), local, url)

    def test_is_excluded_relative_and_absolute(self):
        path = self.make("docs/CLAUDE.md", 1)
        self.assertEqual(is_excluded(path, ["**/CLAUDE.md"], self.dir), "**/CLAUDE.md")
        self.assertEqual(is_excluded(path, ["docs/*.md"], self.dir), "docs/*.md")
        self.assertIsNone(is_excluded(path, ["*.md"], self.dir))

    def test_log_rotation(self):
        with mock.patch.object(common, "LOG_ROTATE_BYTES", 200):
            for i in range(10):
                log_event({"event": "hook", "n": i})
        directory = self.dir / "state" / "local-shunt"
        self.assertTrue((directory / "log.jsonl.1").exists())
        records = [json.loads(line) for line in (directory / "log.jsonl").read_text().splitlines()]
        self.assertEqual(records[-1]["n"], 9)
        self.assertIn("ts", records[-1])


if __name__ == "__main__":
    unittest.main()
