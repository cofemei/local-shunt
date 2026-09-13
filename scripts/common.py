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


@dataclass
class Config:
    enabled: bool = True
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


ENV_MAP = {
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
    layers = [
        _read_json(config_home / "local-shunt" / "config.json"),
        _read_json(project_dir(cwd) / ".claude" / "local-shunt.json"),
    ]
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
    return cfg


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


def state_dir() -> Path:
    base = Path(os.environ.get("XDG_STATE_HOME") or Path.home() / ".local" / "state")
    return base / "local-shunt"


def log_event(record: dict) -> None:
    """Append one JSON line to the log. Never raises."""
    try:
        directory = state_dir()
        directory.mkdir(parents=True, exist_ok=True)
        record = {"ts": datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds"), **record}
        with (directory / "log.jsonl").open("a", encoding="utf-8") as f:
            f.write(json.dumps(record, ensure_ascii=False) + "\n")
    except Exception:
        pass


def now_ms() -> int:
    return int(time.monotonic() * 1000)
