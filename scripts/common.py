"""Shared helpers: configuration, path matching, file inspection, logging."""

from __future__ import annotations

import json
import os
import re
import time
from dataclasses import dataclass, field, fields
from datetime import datetime, timezone
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parent.parent
SHUNT_SCRIPT = PLUGIN_ROOT / "scripts" / "shunt.py"

DEFAULT_EXCLUDE = [
    "**/CLAUDE.md",
    "**/SKILL.md",
    "**/.claude/**",
    "**/.env*",
    "**/*.lock",
]

# Extensions the Read tool renders natively (images, PDFs, notebooks).
NATIVE_EXTENSIONS = {
    ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".tif", ".tiff",
    ".pdf", ".ipynb",
}

# Files larger than this are not line-counted in the hook (keeps it fast).
LINE_COUNT_BYTE_CAP = 64 * 1024 * 1024


LOG_ROTATE_BYTES = 10 * 1024 * 1024

# Provider presets: default API base and the environment variable holding the key.
PROVIDERS = {
    "ollama": {"api_base": None, "api_key_env": ""},
    "openai-compatible": {"api_base": None, "api_key_env": ""},
    "openrouter": {"api_base": "https://openrouter.ai/api/v1", "api_key_env": "OPENROUTER_API_KEY"},
    "openai": {"api_base": "https://api.openai.com/v1", "api_key_env": "OPENAI_API_KEY"},
}


@dataclass
class Config:
    enabled: bool = True
    provider: str = "ollama"
    api_base: str = ""  # OpenAI-compatible base URL, e.g. https://openrouter.ai/api/v1
    api_key_env: str = ""  # name of the environment variable holding the API key
    # Dotenv files searched for api_key_env; relative paths resolve against the project.
    env_files: list[str] = field(default_factory=lambda: ["~/.config/local-shunt/.env"])
    api_keys: dict[str, str] = field(default_factory=dict, repr=False)  # provider -> key
    max_retries: int = 3
    disable_reasoning: bool = True  # ask thinking models to skip reasoning (saves output budget)
    extra_body: dict = field(default_factory=dict)  # merged into OpenAI-compatible request bodies
    ollama_host: str = "http://localhost:11434"
    model: str = "qwen2.5-coder:7b"
    min_lines: int = 350
    min_bytes: int = 32768
    max_file_bytes: int = 2_000_000
    num_ctx: int = 32768
    max_output: int = 1024
    temperature: float = 0.1
    keep_alive: str = "15m"
    request_timeout: int = 300
    health_timeout_ms: int = 300
    intercept_bash: bool = True
    exclude: list[str] = field(default_factory=lambda: list(DEFAULT_EXCLUDE))


# Settings that decide where file contents and API keys are sent. A project config
# may come from an untrusted repository, so these are read only from the user
# config file and the environment.
TRUSTED_KEYS = {"provider", "api_base", "api_key_env", "env_files", "api_keys", "ollama_host", "extra_body"}

ENV_MAP = {
    "provider": "LOCAL_SHUNT_PROVIDER",
    "api_base": "LOCAL_SHUNT_API_BASE",
    "api_key_env": "LOCAL_SHUNT_API_KEY_ENV",
    "ollama_host": "OLLAMA_HOST",
    "model": "LOCAL_SHUNT_MODEL",
    "min_lines": "LOCAL_SHUNT_MIN_LINES",
    "min_bytes": "LOCAL_SHUNT_MIN_BYTES",
    "num_ctx": "LOCAL_SHUNT_NUM_CTX",
}


def project_dir(cwd: str | None = None) -> Path:
    return Path(os.environ.get("CLAUDE_PROJECT_DIR") or cwd or os.getcwd())


def _read_json(path: Path) -> dict:
    try:
        with path.open(encoding="utf-8") as f:
            data = json.load(f)
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def _coerce(name: str, value, default):
    if isinstance(default, bool):
        if isinstance(value, str):
            return value.strip().lower() in ("1", "true", "yes", "on")
        return bool(value)
    if isinstance(default, int):
        return int(value)
    if isinstance(default, float):
        return float(value)
    if isinstance(default, dict):
        if not isinstance(value, dict):
            raise ValueError(f"{name} must be an object")
        return {str(k): (str(v) if name == "api_keys" else v) for k, v in value.items()}
    if isinstance(default, list):
        if not isinstance(value, list):
            raise ValueError(f"{name} must be a list")
        return [str(v) for v in value]
    return str(value)


def _normalize_host(host: str) -> str:
    host = host.strip().rstrip("/")
    if not re.match(r"^https?://", host):
        # OLLAMA_HOST is often set as "0.0.0.0:11434" or "127.0.0.1".
        host = "http://" + host
    if not re.search(r":\d+$", host.split("//", 1)[1]):
        host += ":11434"
    return host.replace("//0.0.0.0", "//127.0.0.1")


def load_config(cwd: str | None = None) -> Config:
    """Merge defaults < user config < project config < environment."""
    cfg = Config()
    defaults = {f.name: getattr(cfg, f.name) for f in fields(Config)}

    config_home = Path(os.environ.get("XDG_CONFIG_HOME") or Path.home() / ".config")
    user_layer = _read_json(config_home / "local-shunt" / "config.json")
    project_layer = _read_json(project_dir(cwd) / ".claude" / "local-shunt.json")
    for key in TRUSTED_KEYS:
        project_layer.pop(key, None)
    layers = [user_layer, project_layer]
    env_layer = {key: os.environ[var] for key, var in ENV_MAP.items() if os.environ.get(var)}
    if os.environ.get("LOCAL_SHUNT_DISABLE") == "1":
        env_layer["enabled"] = False
    layers.append(env_layer)

    for layer in layers:
        for key, value in layer.items():
            if key not in defaults:
                continue
            try:
                setattr(cfg, key, _coerce(key, value, defaults[key]))
            except (TypeError, ValueError):
                pass  # Ignore malformed values; keep the previous layer.

    cfg.ollama_host = _normalize_host(cfg.ollama_host)
    cfg.provider = cfg.provider.strip().lower()
    preset = PROVIDERS.get(cfg.provider)
    if preset is not None:
        if not cfg.api_base and preset["api_base"]:
            cfg.api_base = preset["api_base"]
        if not cfg.api_key_env:
            cfg.api_key_env = preset["api_key_env"]
    cfg.api_base = cfg.api_base.strip().rstrip("/")
    return cfg


def parse_dotenv(text: str) -> dict[str, str]:
    """Parse KEY=VALUE lines. Supports `export`, quotes and trailing comments."""
    values = {}
    for raw in text.splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        if line.startswith("export "):
            line = line[len("export "):].lstrip()
        key, value = line.split("=", 1)
        key, value = key.strip(), value.strip()
        if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
            continue
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
            value = value[1:-1]
        else:
            value = re.split(r"\s+#", value, maxsplit=1)[0].strip()
        values[key] = value
    return values


def resolve_api_key(cfg: Config, cwd: str | None = None) -> str | None:
    """Find the API key: environment variable, then env_files, then api_keys in the user config."""
    if cfg.api_key_env:
        if os.environ.get(cfg.api_key_env):
            return os.environ[cfg.api_key_env]
        for name in cfg.env_files:
            path = Path(os.path.expanduser(name))
            if not path.is_absolute():
                path = project_dir(cwd) / path
            try:
                value = parse_dotenv(path.read_text(encoding="utf-8")).get(cfg.api_key_env)
            except (OSError, UnicodeDecodeError):
                continue
            if value:
                return value
    return cfg.api_keys.get(cfg.provider) or None


def config_problem(cfg: Config, cwd: str | None = None) -> str | None:
    """Return a description of a configuration error, or None."""
    if cfg.provider not in PROVIDERS:
        return f"unknown provider {cfg.provider!r} (expected one of: {', '.join(PROVIDERS)})"
    if cfg.provider != "ollama" and not cfg.api_base:
        return f"provider {cfg.provider} needs api_base"
    if cfg.api_key_env and not resolve_api_key(cfg, cwd):
        return (
            f"API key not found: set {cfg.api_key_env}, add it to one of {cfg.env_files}, "
            f"or set api_keys.{cfg.provider} in ~/.config/local-shunt/config.json"
        )
    return None


def endpoint(cfg: Config) -> str:
    return cfg.ollama_host if cfg.provider == "ollama" else cfg.api_base


def is_local_endpoint(url: str) -> bool:
    hostname = url.split("//", 1)[-1].split("/", 1)[0]
    if hostname.startswith("["):
        hostname = hostname[1:].split("]", 1)[0]
    else:
        hostname = hostname.rsplit(":", 1)[0]
    return hostname in ("localhost", "127.0.0.1", "::1")


def glob_to_regex(pattern: str) -> re.Pattern:
    """Translate a glob with `**` support into a regex matching whole paths."""
    out = []
    i = 0
    while i < len(pattern):
        if pattern.startswith("**/", i):
            out.append("(?:.*/)?")
            i += 3
        elif pattern.startswith("**", i):
            out.append(".*")
            i += 2
        elif pattern[i] == "*":
            out.append("[^/]*")
            i += 1
        elif pattern[i] == "?":
            out.append("[^/]")
            i += 1
        else:
            out.append(re.escape(pattern[i]))
            i += 1
    return re.compile("^" + "".join(out) + "$")


def is_excluded(path: Path, patterns: list[str], base: Path) -> str | None:
    """Return the first matching pattern, or None."""
    candidates = [path.as_posix()]
    try:
        candidates.append(path.relative_to(base).as_posix())
    except ValueError:
        pass
    for pattern in patterns:
        regex = glob_to_regex(pattern)
        if any(regex.match(c) for c in candidates):
            return pattern
    return None


@dataclass
class FileInfo:
    path: Path
    bytes: int
    lines: int | None  # None when the file was too large to count
    binary: bool


def inspect_file(path: Path) -> FileInfo | None:
    """Return size, line count and binary flag, or None if unreadable."""
    try:
        if not path.is_file():
            return None
        size = path.stat().st_size
        with path.open("rb") as f:
            head = f.read(8192)
            binary = b"\0" in head
            lines = None
            if not binary and size <= LINE_COUNT_BYTE_CAP:
                lines = head.count(b"\n")
                last = head
                while chunk := f.read(1 << 20):
                    lines += chunk.count(b"\n")
                    last = chunk
                if last and not last.endswith(b"\n"):
                    lines += 1
        return FileInfo(path=path, bytes=size, lines=lines, binary=binary)
    except OSError:
        return None


def estimate_tokens(text: str) -> int:
    """Rough token estimate: ASCII characters / 3, other characters count as 1 each."""
    non_ascii = sum(1 for c in text if ord(c) > 127)
    return (len(text) - non_ascii) // 3 + non_ascii


def session_id() -> str | None:
    """Claude Code session ID inherited from the environment (Bash tool or MCP server)."""
    return os.environ.get("CLAUDE_CODE_SESSION_ID") or os.environ.get("CLAUDE_SESSION_ID") or None


def state_dir() -> Path:
    base = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local" / "state")
    return base / "local-shunt"


def log_event(record: dict) -> None:
    """Append one JSON line to the log. Never raises."""
    try:
        directory = state_dir()
        directory.mkdir(parents=True, exist_ok=True)
        record = {"ts": datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds"), **record}
        log_file = directory / "log.jsonl"
        if log_file.exists() and log_file.stat().st_size > LOG_ROTATE_BYTES:
            log_file.replace(directory / "log.jsonl.1")
        with log_file.open("a", encoding="utf-8") as f:
            f.write(json.dumps(record, ensure_ascii=False) + "\n")
    except Exception:
        pass


def now_ms() -> int:
    return int(time.monotonic() * 1000)


def apply_overrides(cfg: Config, provider: str | None = None, model: str | None = None) -> Config:
    """Apply command-line overrides. A new provider resets its preset endpoint and key variable."""
    if provider:
        cfg.provider = provider.strip().lower()
        preset = PROVIDERS.get(cfg.provider, {})
        if preset.get("api_base"):
            cfg.api_base = preset["api_base"]
        if preset.get("api_key_env") is not None and cfg.provider in ("openrouter", "openai"):
            cfg.api_key_env = preset["api_key_env"]
    if model:
        cfg.model = model
    return cfg
