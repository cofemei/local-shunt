"""Tests for the MCP server, run as a subprocess over stdio."""

from __future__ import annotations

import json
import subprocess
import sys
import unittest

from helpers import SCRIPTS, FakeLLMServer, IsolatedTestCase


class McpServerTest(IsolatedTestCase):
    def setUp(self):
        super().setUp()
        self.server = FakeLLMServer().__enter__()
        self.addCleanup(self.server.__exit__)

    def start(self, **env: str) -> subprocess.Popen:
        env.setdefault("OLLAMA_HOST", self.server.url)
        env.setdefault("LOCAL_SHUNT_MODEL", "fake")
        proc = subprocess.Popen(
            [sys.executable, str(SCRIPTS / "mcp_server.py")],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, env=self.isolated_env(**env), cwd="/",
        )
        self.addCleanup(self.stop, proc)
        return proc

    @staticmethod
    def stop(proc: subprocess.Popen) -> None:
        if proc.poll() is None:
            proc.stdin.close()
            proc.wait(timeout=10)
        proc.stdout.close()
        proc.stderr.close()

    @staticmethod
    def send(proc: subprocess.Popen, message: dict | str) -> None:
        proc.stdin.write((message if isinstance(message, str) else json.dumps(message)) + "\n")
        proc.stdin.flush()

    def request(self, proc: subprocess.Popen, msg_id: int, method: str, params: dict | None = None) -> dict:
        self.send(proc, {"jsonrpc": "2.0", "id": msg_id, "method": method, "params": params or {}})
        response = json.loads(proc.stdout.readline())
        self.assertEqual(response["id"], msg_id)
        return response

    def initialized(self, **env: str) -> subprocess.Popen:
        proc = self.start(**env)
        init = self.request(proc, 1, "initialize", {
            "protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "test", "version": "0"},
        })
        self.assertEqual(init["result"]["protocolVersion"], "2025-06-18")
        self.assertEqual(init["result"]["serverInfo"]["name"], "local-shunt")
        self.assertIn("tools", init["result"]["capabilities"])
        self.send(proc, {"jsonrpc": "2.0", "method": "notifications/initialized"})
        return proc

    def call(self, proc, msg_id, name, arguments) -> dict:
        return self.request(proc, msg_id, "tools/call", {"name": name, "arguments": arguments})["result"]

    def test_tools_list(self):
        proc = self.initialized()
        tools = {t["name"]: t for t in self.request(proc, 2, "tools/list")["result"]["tools"]}
        self.assertEqual(set(tools), {"shunt_read", "shunt_write", "shunt_stats"})
        self.assertEqual(tools["shunt_read"]["inputSchema"]["required"], ["files", "question"])

    def test_ping_and_unknown_method(self):
        proc = self.initialized()
        self.assertEqual(self.request(proc, 2, "ping")["result"], {})
        self.assertEqual(self.request(proc, 3, "resources/list")["error"]["code"], -32601)

    def test_parse_error_keeps_server_alive(self):
        proc = self.initialized()
        self.send(proc, "{broken")
        self.assertEqual(json.loads(proc.stdout.readline())["error"]["code"], -32700)
        self.assertEqual(self.request(proc, 2, "ping")["result"], {})

    def test_shunt_read(self):
        path = self.make("big.py", 400)
        proc = self.initialized()
        result = self.call(proc, 2, "shunt_read", {"files": [str(path)], "question": "Where is value_2?"})
        self.assertFalse(result["isError"])
        text = result["content"][0]["text"]
        self.assertIn("(400 lines)", text)
        self.assertIn("### Answer", text)
        self.assertEqual(len(self.server.chat_requests()), 1)

    def test_relative_paths_use_project_dir(self):
        self.make("rel.py", 10)
        proc = self.initialized()
        result = self.call(proc, 2, "shunt_read", {"files": ["rel.py"], "question": "q"})
        self.assertFalse(result["isError"], result)

    def test_shunt_read_errors_are_tool_errors(self):
        proc = self.initialized()
        missing = self.call(proc, 2, "shunt_read", {"files": ["/nope.py"], "question": "q"})
        self.assertTrue(missing["isError"])
        self.assertIn("not found", missing["content"][0]["text"])
        bad_args = self.call(proc, 3, "shunt_read", {"files": ["/a.py"], "question": 42})
        self.assertIn("argument question must be a valid string", bad_args["content"][0]["text"])
        missing_arg = self.call(proc, 4, "shunt_read", {"files": ["/a.py"]})
        self.assertIn("missing required argument: question", missing_arg["content"][0]["text"])
        extra = self.call(proc, 5, "shunt_read", {"files": ["/a.py"], "question": "q", "bogus": 1})
        self.assertIn("unknown argument", extra["content"][0]["text"])

    def test_provider_failure_is_tool_error(self):
        path = self.make("a.py", 10)
        proc = self.initialized(OLLAMA_HOST="http://127.0.0.1:9")
        result = self.call(proc, 2, "shunt_read", {"files": [str(path)], "question": "q"})
        self.assertTrue(result["isError"])
        self.assertIn("Fall back", result["content"][0]["text"])

    def test_provider_and_model_override(self):
        path = self.make("a.py", 10)
        proc = self.initialized(LOCAL_SHUNT_API_BASE=self.server.url + "/v1")
        result = self.call(proc, 2, "shunt_read", {
            "files": [str(path)], "question": "q", "provider": "openai-compatible", "model": "fake-model",
        })
        self.assertFalse(result["isError"], result)
        self.assertEqual(self.server.chat_requests()[0]["path"], "/v1/chat/completions")
        bad = self.call(proc, 3, "shunt_read", {"files": [str(path)], "question": "q", "provider": "nope"})
        self.assertTrue(bad["isError"])

    def test_shunt_write_and_stats(self):
        self.server.reply = lambda body: "x = 1"
        out = self.dir / "gen" / "out.py"
        proc = self.initialized()
        result = self.call(proc, 2, "shunt_write", {"out": str(out), "spec": "a constant"})
        self.assertFalse(result["isError"], result)
        self.assertEqual(out.read_text(), "x = 1\n")
        again = self.call(proc, 3, "shunt_write", {"out": str(out), "spec": "a constant"})
        self.assertTrue(again["isError"])
        stats = self.call(proc, 4, "shunt_stats", {})
        self.assertIn("Delegated writes", stats["content"][0]["text"])

    def test_lenient_argument_types(self):
        path = self.make("a.py", 10)
        proc = self.initialized()
        for msg_id, files in ((2, json.dumps([str(path)])), (3, str(path))):
            result = self.call(proc, msg_id, "shunt_read", {"files": files, "question": "q", "max_output": "64"})
            self.assertFalse(result["isError"], result)
        self.assertEqual(self.server.chat_requests()[-1]["body"]["options"]["num_predict"], 64)

    def test_unknown_tool(self):
        proc = self.initialized()
        self.assertEqual(self.request(proc, 2, "tools/call", {"name": "nope", "arguments": {}})["error"]["code"], -32602)


if __name__ == "__main__":
    unittest.main()
