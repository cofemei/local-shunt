"""Minimal Ollama HTTP client (standard library only)."""

from __future__ import annotations

import json
import re
import time
import urllib.error
import urllib.request
from dataclasses import dataclass

from common import Config, state_dir

HEALTH_CACHE_SECONDS = 30
THINK_BLOCK = re.compile(r"<think>.*?</think>\s*", re.DOTALL)


class OllamaError(RuntimeError):
    pass


@dataclass
class Health:
    ok: bool
    reason: str = ""


def _request(url: str, payload: dict | None, timeout: float) -> dict:
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")
        try:
            body = json.loads(body).get("error", body)
        except ValueError:
            pass
        raise OllamaError(f"HTTP {e.code}: {body}") from e
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        raise OllamaError(f"cannot reach Ollama at {url}: {e}") from e
    except ValueError as e:
        raise OllamaError(f"invalid JSON from Ollama: {e}") from e


def model_available(model: str, names: list[str]) -> bool:
    if model in names:
        return True
    if ":" not in model:
        return f"{model}:latest" in names
    return False


def check_health(cfg: Config, timeout_ms: int | None = None, use_cache: bool = True) -> Health:
    """Check that Ollama is reachable and the model is installed. Cached briefly."""
    cache_file = state_dir() / "health.json"
    key = f"{cfg.ollama_host}|{cfg.model}"
    if use_cache:
        try:
            cached = json.loads(cache_file.read_text(encoding="utf-8"))
            if cached.get("key") == key and time.time() - cached.get("ts", 0) < HEALTH_CACHE_SECONDS:
                return Health(ok=cached["ok"], reason=cached.get("reason", ""))
        except (OSError, ValueError, KeyError):
            pass

    timeout = (timeout_ms if timeout_ms is not None else cfg.health_timeout_ms) / 1000
    try:
        tags = _request(f"{cfg.ollama_host}/api/tags", None, timeout)
        names = [m.get("name", "") for m in tags.get("models", [])]
        if model_available(cfg.model, names):
            health = Health(ok=True)
        else:
            health = Health(ok=False, reason=f"model {cfg.model} is not installed")
    except OllamaError as e:
        health = Health(ok=False, reason=str(e))

    try:
        cache_file.parent.mkdir(parents=True, exist_ok=True)
        cache_file.write_text(
            json.dumps({"key": key, "ts": time.time(), "ok": health.ok, "reason": health.reason}),
            encoding="utf-8",
        )
    except OSError:
        pass
    return health


@dataclass
class ChatResult:
    text: str
    prompt_tokens: int | None
    output_tokens: int | None
    truncated: bool


def chat(cfg: Config, system: str, user: str, max_output: int, model: str | None = None) -> ChatResult:
    payload = {
        "model": model or cfg.model,
        "messages": [
            {"role": "system", "content": system},
            {"role": "user", "content": user},
        ],
        "stream": False,
        "think": False,
        "keep_alive": cfg.keep_alive,
        "options": {
            "temperature": cfg.temperature,
            "num_ctx": cfg.num_ctx,
            "num_predict": max_output,
        },
    }
    url = f"{cfg.ollama_host}/api/chat"
    try:
        resp = _request(url, payload, cfg.request_timeout)
    except OllamaError as e:
        # Some models reject the `think` field entirely; retry without it.
        if "think" not in str(e).lower():
            raise
        payload.pop("think")
        resp = _request(url, payload, cfg.request_timeout)

    text = resp.get("message", {}).get("content", "")
    text = THINK_BLOCK.sub("", text).strip()
    return ChatResult(
        text=text,
        prompt_tokens=resp.get("prompt_eval_count"),
        output_tokens=resp.get("eval_count"),
        truncated=resp.get("done_reason") == "length",
    )
