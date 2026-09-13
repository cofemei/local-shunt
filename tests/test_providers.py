"""Tests for the Ollama and OpenAI-compatible providers against a fake server."""

from __future__ import annotations

import os
import unittest
from unittest import mock

from helpers import FakeLLMServer, IsolatedTestCase

import providers
from common import Config
from providers import (
    LLMError,
    ModelOutputError,
    OllamaProvider,
    OpenAICompatibleProvider,
    check_health,
    get_provider,
)


def chat_response(content="ok", finish="stop", **message):
    return 200, {
        "choices": [{"message": {"role": "assistant", "content": content, **message}, "finish_reason": finish}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 2},
    }, None


class ProviderTestCase(IsolatedTestCase):
    def setUp(self):
        super().setUp()
        patcher = mock.patch.object(providers, "BACKOFF_SCALE", 0)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.server = FakeLLMServer().__enter__()
        self.addCleanup(self.server.__exit__)

    def ollama(self, **kwargs) -> Config:
        return Config(provider="ollama", ollama_host=self.server.url, model="fake", **kwargs)

    def compat(self, provider="openai-compatible", **kwargs) -> Config:
        kwargs.setdefault("api_key_env", "")
        kwargs.setdefault("model", "fake-model")
        return Config(provider=provider, api_base=self.server.url + "/v1", **kwargs)


class OllamaProviderTest(ProviderTestCase):
    def test_chat_payload_and_result(self):
        self.server.reply = lambda body: "<think>hmm</think>\nanswer"
        result = get_provider(self.ollama(num_ctx=4096, temperature=0.2)).chat("sys", "user", 77)
        self.assertEqual(result.text, "answer")
        self.assertEqual((result.prompt_tokens, result.output_tokens, result.truncated), (100, 20, False))
        body = self.server.chat_requests()[0]["body"]
        self.assertIs(body["think"], False)
        self.assertFalse(body["stream"])
        self.assertEqual(body["options"], {"temperature": 0.2, "num_ctx": 4096, "num_predict": 77})
        self.assertEqual([m["role"] for m in body["messages"]], ["system", "user"])

    def test_retries_without_think_when_rejected(self):
        def handler(method, path, body, headers):
            if path == "/api/chat" and "think" in body:
                return 400, {"error": "model does not support thinking"}, None
            return self.server.default(method, path, body, headers)
        self.server.handler = handler
        get_provider(self.ollama()).chat("s", "u", 10)
        self.assertEqual(["think" in r["body"] for r in self.server.chat_requests()], [True, False])

    def test_truncation_flag(self):
        self.server.handler = lambda m, p, b, h: (200, {"message": {"content": "x"}, "done_reason": "length"}, None)
        self.assertTrue(get_provider(self.ollama()).chat("s", "u", 10).truncated)

    def test_local_endpoint_is_not_retried(self):
        self.server.handler = lambda m, p, b, h: (503, {"error": "busy"}, None)
        with self.assertRaises(LLMError):
            get_provider(self.ollama()).chat("s", "u", 10)
        self.assertEqual(len(self.server.chat_requests()), 1)


class OpenAICompatibleProviderTest(ProviderTestCase):
    def test_payload_headers_and_usage(self):
        os.environ["MY_KEY"] = "secret-123"
        self.server.handler = lambda m, p, b, h: (200, {
            "choices": [{"message": {"content": "hello"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 5, "completion_tokens": 1, "cost": 0.0025},
        }, None)
        llm = get_provider(self.compat(api_key_env="MY_KEY", temperature=0.0))
        self.assertIsInstance(llm, OpenAICompatibleProvider)
        result = llm.chat("sys", "user", 50)
        self.assertEqual((result.text, result.prompt_tokens, result.output_tokens, result.cost), ("hello", 5, 1, 0.0025))
        request = self.server.chat_requests()[0]
        self.assertEqual(request["headers"]["Authorization"], "Bearer secret-123")
        self.assertNotIn("X-Title", request["headers"])
        body = request["body"]
        self.assertEqual((body["model"], body["max_tokens"], body["temperature"]), ("fake-model", 50, 0.0))
        self.assertEqual(body["reasoning_effort"], "none")

    def test_no_authorization_without_key(self):
        get_provider(self.compat()).chat("s", "u", 10)
        self.assertNotIn("Authorization", self.server.chat_requests()[0]["headers"])

    def test_openrouter_fields(self):
        os.environ["OPENROUTER_API_KEY"] = "or"
        self.server.handler = lambda m, p, b, h: chat_response()
        get_provider(self.compat("openrouter", api_key_env="OPENROUTER_API_KEY")).chat("s", "u", 10)
        request = self.server.chat_requests()[0]
        self.assertEqual(request["headers"]["X-Title"], "local-shunt")
        self.assertEqual(request["body"]["reasoning"], {"effort": "none"})
        self.assertNotIn("reasoning_effort", request["body"])

    def test_reasoning_field_rejected_then_dropped(self):
        def handler(method, path, body, headers):
            if "reasoning_effort" in body:
                return 400, {"error": {"message": "Unrecognized request argument: reasoning_effort"}}, None
            return chat_response("fine")
        self.server.handler = handler
        llm = get_provider(self.compat())
        self.assertEqual(llm.chat("s", "u", 10).text, "fine")
        llm.chat("s", "u", 10)  # later calls skip the field immediately
        self.assertEqual(["reasoning_effort" in r["body"] for r in self.server.chat_requests()], [True, False, False])

    def test_disable_reasoning_off(self):
        self.server.handler = lambda m, p, b, h: chat_response()
        get_provider(self.compat(disable_reasoning=False)).chat("s", "u", 10)
        self.assertNotIn("reasoning_effort", self.server.chat_requests()[0]["body"])

    def test_extra_body_merged(self):
        self.server.handler = lambda m, p, b, h: chat_response()
        get_provider(self.compat(extra_body={"top_p": 0.5, "provider": {"sort": "price"}})).chat("s", "u", 10)
        body = self.server.chat_requests()[0]["body"]
        self.assertEqual((body["top_p"], body["provider"]), (0.5, {"sort": "price"}))

    def test_remote_retries_on_429_then_succeeds(self):
        responses = [(429, {"error": "rate limited"}, {"Retry-After": "1"}), (502, {"error": "bad gateway"}, None), chat_response("third")]
        self.server.handler = lambda m, p, b, h: responses.pop(0)
        llm = get_provider(self.compat(max_retries=3))
        llm.is_local = False
        self.assertEqual(llm.chat("s", "u", 10).text, "third")
        self.assertEqual(len(self.server.chat_requests()), 3)

    def test_remote_gives_up_after_max_retries(self):
        self.server.handler = lambda m, p, b, h: (429, {"error": "rate limited"}, None)
        llm = get_provider(self.compat(max_retries=2))
        llm.is_local = False
        with self.assertRaisesRegex(LLMError, "HTTP 429: rate limited"):
            llm.chat("s", "u", 10)
        self.assertEqual(len(self.server.chat_requests()), 3)

    def test_client_errors_are_not_retried(self):
        self.server.handler = lambda m, p, b, h: (401, {"error": {"message": "invalid key"}}, None)
        llm = get_provider(self.compat(disable_reasoning=False))
        llm.is_local = False
        with self.assertRaisesRegex(LLMError, "HTTP 401: invalid key"):
            llm.chat("s", "u", 10)
        self.assertEqual(len(self.server.chat_requests()), 1)

    def test_reasoning_used_whole_budget(self):
        self.server.handler = lambda m, p, b, h: chat_response(None, finish="length", reasoning="long thoughts")
        llm = get_provider(self.compat())
        llm.is_local = False
        with self.assertRaises(ModelOutputError):
            llm.chat("s", "u", 10)
        self.assertEqual(len(self.server.chat_requests()), 1)

    def test_error_inside_200_body(self):
        self.server.handler = lambda m, p, b, h: (200, {"error": {"message": "upstream failed", "code": 502}}, None)
        with self.assertRaisesRegex(LLMError, "upstream failed"):
            get_provider(self.compat()).chat("s", "u", 10)

    def test_no_choices(self):
        self.server.handler = lambda m, p, b, h: (200, {"choices": []}, None)
        with self.assertRaises(ModelOutputError):
            get_provider(self.compat()).chat("s", "u", 10)


class GetProviderTest(IsolatedTestCase):
    def test_types_and_config_errors(self):
        self.assertIsInstance(get_provider(Config()), OllamaProvider)
        with self.assertRaisesRegex(LLMError, "API key not found"):
            get_provider(Config(provider="openrouter", api_base="https://openrouter.ai/api/v1", api_key_env="OPENROUTER_API_KEY"))


class HealthTest(ProviderTestCase):
    def test_ollama_model_present(self):
        self.assertTrue(check_health(self.ollama(), use_cache=False).ok)  # "fake" matches "fake:latest"

    def test_ollama_model_missing(self):
        health = check_health(Config(ollama_host=self.server.url, model="other:7b"), use_cache=False)
        self.assertFalse(health.ok)
        self.assertIn("other:7b", health.reason)

    def test_unreachable(self):
        health = check_health(Config(ollama_host="http://127.0.0.1:9", model="x"), use_cache=False)
        self.assertFalse(health.ok)

    def test_result_is_cached(self):
        cfg = self.ollama()
        self.assertTrue(check_health(cfg).ok)
        self.server.handler = lambda m, p, b, h: (500, {"error": "down"}, None)
        self.assertTrue(check_health(cfg).ok)
        self.assertFalse(check_health(cfg, use_cache=False).ok)

    def test_compat_models_listing(self):
        self.assertTrue(check_health(self.compat(), use_cache=False).ok)
        self.assertFalse(check_health(self.compat(model="missing"), use_cache=False).ok)

    def test_compat_without_models_endpoint_is_assumed_ok(self):
        self.server.handler = lambda m, p, b, h: (404, {"error": "no such route"}, None)
        self.assertTrue(check_health(self.compat(model="anything"), use_cache=False).ok)

    def test_remote_not_probed_unless_asked(self):
        os.environ["OPENROUTER_API_KEY"] = "k"
        cfg = Config(provider="openrouter", api_base="http://10.255.255.1/v1", api_key_env="OPENROUTER_API_KEY", model="m")
        health = check_health(cfg)
        self.assertEqual((health.ok, health.reason), (True, "remote endpoint not probed"))

    def test_remote_probe_result_is_cached_for_hook(self):
        os.environ["OPENROUTER_API_KEY"] = "k"
        cfg = Config(provider="openrouter", api_base="https://openrouter.example/v1", api_key_env="OPENROUTER_API_KEY", model="m")
        with mock.patch.object(OpenAICompatibleProvider, "list_models", return_value=["other"]):
            self.assertFalse(check_health(cfg, use_cache=False, probe_remote=True).ok)
        self.assertFalse(check_health(cfg).ok)  # hook sees the cached failure without probing

    def test_missing_key_reported(self):
        health = check_health(Config(provider="openrouter", api_base="https://openrouter.ai/api/v1", api_key_env="OPENROUTER_API_KEY"))
        self.assertFalse(health.ok)
        self.assertIn("OPENROUTER_API_KEY", health.reason)


if __name__ == "__main__":
    unittest.main()
