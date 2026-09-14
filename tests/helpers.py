"""Test helpers: environment isolation and a fake LLM HTTP server."""

from __future__ import annotations

import json
import os
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SCRIPTS = ROOT / "scripts"
sys.path.insert(0, str(SCRIPTS))

ISOLATED_VARS = (
    "CLAUDE_PROJECT_DIR", "CLAUDE_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CONFIG_DIR",
    "XDG_STATE_HOME", "XDG_CONFIG_HOME",
    "OLLAMA_HOST", "OPENROUTER_API_KEY", "OPENAI_API_KEY", "LOCAL_SHUNT_DISABLE",
    "LOCAL_SHUNT_PROVIDER", "LOCAL_SHUNT_API_BASE", "LOCAL_SHUNT_API_KEY_ENV",
    "LOCAL_SHUNT_MODEL", "LOCAL_SHUNT_MIN_LINES", "LOCAL_SHUNT_MIN_BYTES", "LOCAL_SHUNT_NUM_CTX",
)

ANSWER = (
    "### Answer\n- `value_2` is set (L2).\n\n"
    "### Relevant locations\n- `L2-L3`: values\n\n"
    "### Not covered or uncertain\n- None"
)


class IsolatedTestCase(unittest.TestCase):
    """Runs each test with a private project, config and state directory."""

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self._tmp.name)
        self._saved_env = {k: os.environ.get(k) for k in ISOLATED_VARS}
        for key in ISOLATED_VARS:
            os.environ.pop(key, None)
        os.environ["CLAUDE_PROJECT_DIR"] = str(self.dir)
        os.environ["XDG_STATE_HOME"] = str(self.dir / "state")
        os.environ["XDG_CONFIG_HOME"] = str(self.dir / "config")
        self._saved_cwd = os.getcwd()
        os.chdir(self.dir)

    def tearDown(self):
        os.chdir(self._saved_cwd)
        for key, value in self._saved_env.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value
        self._tmp.cleanup()

    def make(self, name: str, lines: int = 0, content: bytes | str | None = None) -> Path:
        path = self.dir / name
        path.parent.mkdir(parents=True, exist_ok=True)
        if content is None:
            content = "".join(f"value_{i} = {i}\n" for i in range(1, lines + 1))
        if isinstance(content, str):
            content = content.encode()
        path.write_bytes(content)
        return path

    def write_user_config(self, data: dict) -> None:
        path = self.dir / "config" / "local-shunt" / "config.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(data))

    def write_project_config(self, data: dict) -> None:
        path = self.dir / ".claude" / "local-shunt.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(data))

    def log_records(self) -> list[dict]:
        path = self.dir / "state" / "local-shunt" / "log.jsonl"
        if not path.exists():
            return []
        return [json.loads(line) for line in path.read_text().splitlines()]

    def isolated_env(self, **extra: str) -> dict:
        """Environment for subprocesses: the current isolated variables plus extras."""
        env = {k: v for k, v in os.environ.items() if k not in ISOLATED_VARS}
        env.update({k: os.environ[k] for k in ISOLATED_VARS if k in os.environ})
        env.update(extra)
        return env


class FakeLLMServer:
    """Serves Ollama (/api/...) and OpenAI-compatible (/v1/...) endpoints.

    Set `handler` to a function (method, path, body, headers) -> (status, dict, headers)
    or None to use the default behavior. Every request is recorded in `requests`.
    """

    def __init__(self):
        self.requests: list[dict] = []
        self.handler = None
        self.reply = lambda body: ANSWER
        server = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def _serve(self, method):
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length) if length else b""
                body = json.loads(raw) if raw else None
                record = {"method": method, "path": self.path, "body": body, "headers": dict(self.headers)}
                server.requests.append(record)
                status, data, headers = (server.handler or server.default)(method, self.path, body, record["headers"])
                payload = json.dumps(data).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                for key, value in (headers or {}).items():
                    self.send_header(key, value)
                self.end_headers()
                self.wfile.write(payload)

            def do_GET(self):
                self._serve("GET")

            def do_POST(self):
                self._serve("POST")

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        self.thread = threading.Thread(target=self.httpd.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *exc):
        self.httpd.shutdown()
        self.httpd.server_close()

    def default(self, method, path, body, headers):
        if path == "/api/tags":
            return 200, {"models": [{"name": "fake:latest"}]}, None
        if path == "/api/chat":
            return 200, {
                "message": {"role": "assistant", "content": self.reply(body)},
                "done_reason": "stop", "prompt_eval_count": 100, "eval_count": 20,
            }, None
        if path == "/v1/models":
            return 200, {"data": [{"id": "fake-model"}]}, None
        if path == "/v1/chat/completions":
            return 200, {
                "choices": [{"message": {"role": "assistant", "content": self.reply(body)}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 100, "completion_tokens": 20, "cost": 0.001},
            }, None
        return 404, {"error": "not found"}, None

    def chat_requests(self) -> list[dict]:
        return [r for r in self.requests if r["path"] in ("/api/chat", "/v1/chat/completions")]
