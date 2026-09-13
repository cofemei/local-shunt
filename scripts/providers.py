"""LLM providers: Ollama native API and OpenAI-compatible APIs (OpenRouter, OpenAI,
LM Studio, llama.cpp, vLLM, ...). Standard library only."""

from __future__ import annotations

import json
import re
import time
import urllib.error
import urllib.request
from dataclasses import dataclass

from common import Config, config_problem, endpoint, is_local_endpoint, resolve_api_key, state_dir

LOCAL_HEALTH_TTL = 30
REMOTE_HEALTH_TTL = 600
RETRY_STATUSES = {408, 409, 429, 500, 502, 503, 504}
THINK_BLOCK = re.compile(r"<think>.*?</think>\s*", re.DOTALL)


class LLMError(RuntimeError):
    pass


@dataclass
class Health:
    ok: bool
    reason: str = ""


@dataclass
class ChatResult:
    text: str
    prompt_tokens: int | None
    output_tokens: int | None
    truncated: bool
    cost: float | None = None


class ModelOutputError(LLMError):
    """The request succeeded but the output is unusable; retrying will not help."""


class HTTPStatusError(LLMError):
    def __init__(self, status: int, message: str, retry_after: float | None = None):
        super().__init__(f"HTTP {status}: {message}")
        self.status = status
        self.retry_after = retry_after


def _error_message(body: str) -> str:
    try:
        data = json.loads(body)
    except ValueError:
        return body.strip()[:500]
    error = data.get("error", data) if isinstance(data, dict) else data
    if isinstance(error, dict):
        message = error.get("message") or json.dumps(error)
        # OpenRouter puts the upstream provider's explanation in metadata.
        metadata = error.get("metadata") if isinstance(error.get("metadata"), dict) else {}
        detail = metadata.get("raw") or metadata.get("reason")
        if detail:
            message = f"{message} ({metadata.get('provider_name') or 'upstream'}: {str(detail)[:300]})"
        error = message
    return str(error)[:500]


def http_json(url: str, payload: dict | None, timeout: float, headers: dict | None = None) -> dict:
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json", **(headers or {})})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            result = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        retry_after = e.headers.get("Retry-After") if e.headers else None
        try:
            retry_after = float(retry_after) if retry_after else None
        except ValueError:
            retry_after = None
        raise HTTPStatusError(e.code, _error_message(e.read().decode("utf-8", "replace")), retry_after) from e
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        # Strip the query string; it never carries secrets here, but keep messages short.
        raise LLMError(f"cannot reach {url.split('?', 1)[0]}: {getattr(e, 'reason', e)}") from e
    except ValueError as e:
        raise LLMError(f"invalid JSON from {url}: {e}") from e
    if isinstance(result, dict) and result.get("error"):
        raise LLMError(f"API error: {_error_message(json.dumps(result))}")
    return result


# Tests set this to 0 to avoid sleeping.
BACKOFF_SCALE = 1.0


def backoff(attempt: int) -> float:
    """Delay before retry number attempt+1: 2, 4, 8 ... seconds, capped at 30."""
    return min(2 ** (attempt + 1), 30) * BACKOFF_SCALE


class Provider:
    name = ""

    def __init__(self, cfg: Config, cwd: str | None = None):
        self.cfg = cfg
        self.cwd = cwd
        self.base = endpoint(cfg)
        self.is_local = is_local_endpoint(self.base)

    # Subclasses implement these two.
    def list_models(self, timeout: float) -> list[str] | None: ...
    def _chat_once(self, system: str, user: str, max_output: int) -> ChatResult: ...

    def chat(self, system: str, user: str, max_output: int) -> ChatResult:
        attempts = 1 if self.is_local else 1 + max(0, self.cfg.max_retries)
        for attempt in range(attempts):
            try:
                result = self._chat_once(system, user, max_output)
                result.text = THINK_BLOCK.sub("", result.text or "").strip()
                return result
            except HTTPStatusError as e:
                if e.status not in RETRY_STATUSES or attempt == attempts - 1:
                    raise
                delay = e.retry_after * BACKOFF_SCALE if e.retry_after is not None and e.retry_after <= 60 else backoff(attempt)
            except LLMError as e:
                if isinstance(e, ModelOutputError) or attempt == attempts - 1:
                    raise
                delay = backoff(attempt)
            time.sleep(delay)
        raise LLMError("unreachable")


class OllamaProvider(Provider):
    name = "ollama"

    def list_models(self, timeout: float) -> list[str] | None:
        tags = http_json(f"{self.base}/api/tags", None, timeout)
        return [m.get("name", "") for m in tags.get("models", [])]

    def _chat_once(self, system: str, user: str, max_output: int) -> ChatResult:
        cfg = self.cfg
        payload = {
            "model": cfg.model,
            "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}],
            "stream": False,
            "think": False,
            "keep_alive": cfg.keep_alive,
            "options": {"temperature": cfg.temperature, "num_ctx": cfg.num_ctx, "num_predict": max_output},
        }
        url = f"{self.base}/api/chat"
        try:
            resp = http_json(url, payload, cfg.request_timeout)
        except HTTPStatusError as e:
            # Some models reject the `think` field entirely; retry without it.
            if "think" not in str(e).lower():
                raise
            payload.pop("think")
            resp = http_json(url, payload, cfg.request_timeout)
        return ChatResult(
            text=resp.get("message", {}).get("content") or "",
            prompt_tokens=resp.get("prompt_eval_count"),
            output_tokens=resp.get("eval_count"),
            truncated=resp.get("done_reason") == "length",
        )


class OpenAICompatibleProvider(Provider):
    name = "openai-compatible"

    def __init__(self, cfg: Config, cwd: str | None = None):
        super().__init__(cfg, cwd)
        self.send_reasoning_off = cfg.disable_reasoning

    def headers(self) -> dict:
        headers = {}
        key = resolve_api_key(self.cfg, self.cwd)
        if key:
            headers["Authorization"] = f"Bearer {key}"
        if self.cfg.provider == "openrouter":
            headers["X-Title"] = "local-shunt"
        return headers

    def list_models(self, timeout: float) -> list[str] | None:
        try:
            data = http_json(f"{self.base}/models", None, timeout, self.headers())
        except HTTPStatusError as e:
            if e.status in (404, 405):
                return None  # server has no model listing; cannot verify
            raise
        return [m.get("id", "") for m in data.get("data", []) if isinstance(m, dict)]

    def reasoning_off_fields(self) -> dict:
        if self.cfg.provider == "openrouter":
            return {"reasoning": {"effort": "none"}}
        return {"reasoning_effort": "none"}

    def _chat_once(self, system: str, user: str, max_output: int) -> ChatResult:
        cfg = self.cfg
        payload = {
            "model": cfg.model,
            "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}],
            "temperature": cfg.temperature,
            "max_tokens": max_output,
            "stream": False,
        }
        if self.send_reasoning_off:
            payload.update(self.reasoning_off_fields())
        payload.update(cfg.extra_body)
        url = f"{self.base}/chat/completions"
        try:
            resp = http_json(url, payload, cfg.request_timeout, self.headers())
        except HTTPStatusError as e:
            # Models without adjustable reasoning may reject the field; retry without it.
            if not (self.send_reasoning_off and e.status in (400, 422)):
                raise
            self.send_reasoning_off = False
            for key in self.reasoning_off_fields():
                if key not in cfg.extra_body:
                    payload.pop(key, None)
            resp = http_json(url, payload, cfg.request_timeout, self.headers())

        choices = resp.get("choices") or []
        if not choices:
            raise ModelOutputError("API returned no choices")
        choice = choices[0]
        message = choice.get("message") or {}
        usage = resp.get("usage") or {}
        text = message.get("content") or ""
        truncated = choice.get("finish_reason") == "length"
        if truncated and not text.strip() and (message.get("reasoning") or message.get("reasoning_content")):
            raise ModelOutputError(
                "the model used the whole output budget on reasoning and returned no answer; "
                "choose a non-reasoning model or raise --max-output"
            )
        return ChatResult(
            text=text,
            prompt_tokens=usage.get("prompt_tokens"),
            output_tokens=usage.get("completion_tokens"),
            truncated=truncated,
            cost=usage.get("cost") if isinstance(usage.get("cost"), (int, float)) else None,
        )


def get_provider(cfg: Config, cwd: str | None = None) -> Provider:
    problem = config_problem(cfg, cwd)
    if problem:
        raise LLMError(problem)
    if cfg.provider == "ollama":
        return OllamaProvider(cfg, cwd)
    return OpenAICompatibleProvider(cfg, cwd)


def model_available(model: str, names: list[str]) -> bool:
    if model in names:
        return True
    if ":" not in model:
        return f"{model}:latest" in names
    return False


def check_health(
    cfg: Config,
    timeout_ms: int | None = None,
    use_cache: bool = True,
    probe_remote: bool = False,
    cwd: str | None = None,
) -> Health:
    """Check that the provider is configured, reachable and has the model.

    Local endpoints are probed over HTTP. Remote endpoints are probed only when
    probe_remote is True (SessionStart); the hook relies on the cached result or,
    without one, on the configuration check alone, so it never waits on the network.
    """
    problem = config_problem(cfg, cwd)
    if problem:
        return Health(ok=False, reason=problem)

    base = endpoint(cfg)
    local = is_local_endpoint(base)
    ttl = LOCAL_HEALTH_TTL if local else REMOTE_HEALTH_TTL
    cache_file = state_dir() / "health.json"
    key = f"{cfg.provider}|{base}|{cfg.model}"
    if use_cache:
        try:
            cached = json.loads(cache_file.read_text(encoding="utf-8"))
            if cached.get("key") == key and time.time() - cached.get("ts", 0) < ttl:
                return Health(ok=cached["ok"], reason=cached.get("reason", ""))
        except (OSError, ValueError, KeyError):
            pass
    if not local and not probe_remote:
        return Health(ok=True, reason="remote endpoint not probed")

    timeout = (timeout_ms if timeout_ms is not None else cfg.health_timeout_ms) / 1000
    provider = get_provider(cfg, cwd)
    try:
        names = provider.list_models(timeout)
        if names is None or model_available(cfg.model, names):
            health = Health(ok=True)
        else:
            health = Health(ok=False, reason=f"model {cfg.model} is not available from {cfg.provider}")
    except LLMError as e:
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
