"""Read gateway .env settings as data, without a shell or environment expansion.

``read_env`` returns only settings explicitly present in the file. Integer
settings become Python ints; all other settings remain strings. ``render_config``
overlays these settings on installed JSON, so omitted settings retain their
installed values. Callers can merge ``MANAGE_DEFAULTS`` for task-only options.

Supported syntax is KEY=value, optional ``export``, comments, single-quoted
literal strings, and JSON double-quoted strings. A physical line always contains
one assignment. JSON ``\\n`` escapes are allowed only in player-facing messages.
"""

from collections.abc import Mapping
import json
from pathlib import Path
import re


MAX_ENV_BYTES = 64 * 1024
MAX_INTEGER = (1 << 63) - 1

CONFIG_KEYS = {
    "MC_LISTEN": "listen",
    "MC_BACKEND_ADDRESS": "backend_address",
    "MC_RCON_ADDRESS": "rcon_address",
    "MC_RCON_PASSWORD_FILE": "rcon_password_file",
    "MC_BACKEND_SERVICE": "backend_service",
    "MC_STATE_DIRECTORY": "state_directory",
    "MC_ADMIN_SOCKET": "admin_socket",
    "MC_PROTOCOL": "protocol",
    "MC_VERSION": "version",
    "MC_MAX_PLAYERS": "max_players",
    "MC_MAX_CONNECTIONS": "max_connections",
    "MC_IDLE_TIMEOUT": "idle_timeout",
    "MC_POLL_INTERVAL": "poll_interval",
    "MC_OPERATION_TIMEOUT": "operation_timeout",
    "MC_STARTUP_TIMEOUT": "startup_timeout",
    "MC_SHUTDOWN_WARN_AFTER": "shutdown_warn_after",
    "MC_WAKE_COOLDOWN": "wake_cooldown",
    "MC_HANDSHAKE_TIMEOUT": "handshake_timeout",
    "MC_DIAL_TIMEOUT": "dial_timeout",
    "MC_BOOT_MESSAGE": "boot_message",
    "MC_SLEEPING_MOTD": "sleeping_motd",
    "MC_STARTING_MOTD": "starting_motd",
    "MC_FAILED_MOTD": "failed_motd",
    "MC_LOG_LEVEL": "log_level",
}

MANAGE_DEFAULTS = {
    "MC_BINARY": "/opt/mc-gateway/bin/mc-gateway",
    "MC_CONFIG": "/etc/mc-gateway/config.json",
    "MC_SERVICE_USER": "minecraft",
    "MC_GATEWAY_SERVICE": "mc-gateway.service",
    "MC_LOG_NAMESPACE": "mc-gateway",
    "MC_LOG_LINES": 100,
    "MC_COMMAND_TIMEOUT": "30s",
}

INTEGER_KEYS = frozenset({
    "MC_PROTOCOL", "MC_MAX_PLAYERS", "MC_MAX_CONNECTIONS", "MC_LOG_LINES",
})
MESSAGE_KEYS = frozenset({
    "MC_BOOT_MESSAGE", "MC_SLEEPING_MOTD", "MC_STARTING_MOTD", "MC_FAILED_MOTD",
})
SUPPORTED_KEYS = frozenset(CONFIG_KEYS) | frozenset(MANAGE_DEFAULTS)
_ASSIGNMENT = re.compile(r"([A-Za-z_][A-Za-z0-9_]*)[ \t]*=[ \t]*(.*)")


class EnvConfigError(ValueError):
    """A configuration error whose message never includes a setting's value."""


def _error(message, key=None, line=None):
    context = []
    if line is not None:
        context.append(f"line {line}")
    if key is not None:
        context.append(key)
    prefix = ": ".join(context)
    raise EnvConfigError(f"{prefix}: {message}" if prefix else message)


def _validate_key(key, line=None):
    if not isinstance(key, str):
        _error("setting names must be strings", line=line)
    if key not in SUPPORTED_KEYS:
        # Invalid names can contain arbitrary data; never print them.
        if re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key):
            _error("unknown setting", key=key, line=line)
        _error("invalid setting name", line=line)


def _validate_value(key, value, line=None):
    if key in INTEGER_KEYS:
        if isinstance(value, str):
            if not re.fullmatch(r"[0-9]+", value):
                _error("must be a positive decimal integer", key, line)
            # Bound before int(), including Python's decimal conversion limit.
            digits = value.lstrip("0") or "0"
            if len(digits) > 19:
                _error("integer is out of range", key, line)
            value = int(digits)
        if type(value) is not int or not 1 <= value <= MAX_INTEGER:
            _error("must be a positive integer within the signed 64-bit range", key, line)
        return value

    if not isinstance(value, str):
        _error("must be a string", key, line)
    if not value.strip():
        _error("must not be empty", key, line)
    if "\x00" in value or "\r" in value:
        _error("must not contain NUL or carriage return", key, line)
    if "\n" in value and key not in MESSAGE_KEYS:
        _error("line breaks are allowed only in messages", key, line)
    if any(ord(char) < 32 and char not in "\n\t" for char in value):
        _error("contains an unsupported control character", key, line)
    try:
        value.encode("utf-8")
    except UnicodeEncodeError:
        _error("must contain valid Unicode", key, line)
    return value


def validate_values(values):
    """Return a typed copy of known settings, without injecting defaults.

    This checks the data format, not the complete runtime configuration. The Go
    configuration check remains authoritative for addresses, durations, paths,
    service names, protocol support, and relationships between settings.
    """
    if not isinstance(values, Mapping):
        _error("settings must be a mapping")
    normalized = {}
    for key, value in values.items():
        _validate_key(key)
        normalized[key] = _validate_value(key, value)
    return normalized


def _check_tail(tail, key, line):
    if not tail or not tail.strip(" \t"):
        return
    if tail[0] in " \t" and tail.lstrip(" \t").startswith("#"):
        return
    _error("unexpected text after quoted value", key, line)


def _parse_value(text, key, line):
    if text.startswith("'"):
        end = text.find("'", 1)
        if end < 0:
            _error("unterminated single-quoted value; multiline values are not supported", key, line)
        _check_tail(text[end + 1:], key, line)
        return text[1:end]
    if text.startswith('"'):
        try:
            value, end = json.JSONDecoder().raw_decode(text)
        except json.JSONDecodeError:
            _error("invalid JSON-quoted string; multiline values are not supported", key, line)
        _check_tail(text[end:], key, line)
        return value
    if text.startswith("#"):
        return ""
    return re.split(r"[ \t]+#", text, maxsplit=1)[0].rstrip(" \t")


def read_env(path):
    """Read at most 64 KiB of UTF-8 settings; never execute or expand values."""
    with Path(path).open("rb") as stream:
        raw = stream.read(MAX_ENV_BYTES + 1)
    if len(raw) > MAX_ENV_BYTES:
        _error("environment file exceeds 64 KiB")
    invalid_offsets = [raw.find(char) for char in (b"\r", b"\x00") if char in raw]
    if invalid_offsets:
        line = raw[:min(invalid_offsets)].count(b"\n") + 1
        _error("NUL and carriage return are not allowed; use LF line endings", line=line)
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        _error("environment file must be UTF-8", line=raw[:exc.start].count(b"\n") + 1)

    values = {}
    for number, text in enumerate(text.split("\n"), start=1):
        text = text.strip(" \t")
        if not text or text.startswith("#"):
            continue
        if text.startswith("export ") or text.startswith("export\t"):
            text = text[6:].lstrip(" \t")
        match = _ASSIGNMENT.fullmatch(text)
        if match is None:
            _error("expected KEY=value", line=number)
        key, literal = match.groups()
        _validate_key(key, number)
        if key in values:
            _error("duplicate setting", key, number)
        value = _parse_value(literal, key, number)
        values[key] = _validate_value(key, value, number)
    return values


def render_config(values, base_dict):
    """Overlay supplied gateway settings on a copy of installed JSON settings."""
    if not isinstance(base_dict, Mapping):
        _error("base configuration must be a mapping")
    result = dict(base_dict)
    for key, value in validate_values(values).items():
        if key in CONFIG_KEYS:
            result[CONFIG_KEYS[key]] = value
    return result


def values_from_config(base_dict, *, include_manage_defaults=True):
    """Map installed JSON to .env values, optionally adding task defaults.

    No runtime defaults are invented: absent JSON fields remain absent. Unknown
    JSON fields are rejected so a future configuration cannot be silently lost.
    """
    if not isinstance(base_dict, Mapping):
        _error("base configuration must be a mapping")
    reverse = {field: key for key, field in CONFIG_KEYS.items()}
    if any(field not in reverse for field in base_dict):
        _error("base configuration contains an unsupported field")
    values = dict(MANAGE_DEFAULTS) if include_manage_defaults else {}
    values.update({reverse[field]: value for field, value in base_dict.items()})
    return validate_values(values)


def serialize_env(values):
    """Serialize values for safe, lossless read_env round trips.

    Strings use JSON quoting, which preserves literal dollar signs, backticks,
    quotes, backslashes, hashes, Unicode, and escaped message line breaks.
    """
    values = validate_values(values)
    lines = []
    for key in (*CONFIG_KEYS, *MANAGE_DEFAULTS):
        if key in values:
            encoded = json.dumps(values[key], ensure_ascii=False)
            lines.append(f"{key}={encoded}\n")
    result = "".join(lines)
    if len(result.encode("utf-8")) > MAX_ENV_BYTES:
        _error("serialized environment file exceeds 64 KiB")
    return result
