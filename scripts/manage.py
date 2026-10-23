#!/usr/bin/env python3
"""Task entry points. Treat .env and command arguments as data, never shell code.

Configuration and binary writes are atomic and deliberately do not restart the
gateway. Restart is a separate, explicit operator action because it disconnects
proxied clients even though Java remains running.
"""

import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import stat
import subprocess
import sys
import tempfile

from env_config import (
    MANAGE_DEFAULTS, read_env, render_config, serialize_env, values_from_config,
)

ROOT = Path(__file__).resolve().parent.parent
ENV_FILE = ROOT / ".env"
FIXED_FIELDS = {
    "backend_address", "rcon_address", "rcon_password_file", "backend_service",
    "state_directory", "admin_socket",
}
SERVICE_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.@:-]*\.service\Z")
ACCOUNT_NAME = re.compile(r"[a-z_][a-z0-9_-]*\$?\Z")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(argv, *, capture=False):
    result = subprocess.run([str(arg) for arg in argv], text=True,
                            capture_output=capture, check=False)
    if result.returncode:
        detail = (result.stderr or result.stdout or "").strip() if capture else ""
        raise RuntimeError(f"command failed (exit {result.returncode})" +
                           (f": {detail[:2000]}" if detail else ""))
    return result.stdout if capture else ""


def sudo(argv, *, capture=False, user=None):
    prefix = ["sudo", "-n"]
    if user:
        prefix += ["-u", user]
    return run(prefix + ["--"] + list(argv), capture=capture)


def settings(values):
    result = {**MANAGE_DEFAULTS}
    result.update({key: value for key, value in values.items() if key in result})
    for name in ("MC_BINARY", "MC_CONFIG"):
        require(Path(result[name]).is_absolute(), f"{name} must be an absolute path")
    require(ACCOUNT_NAME.fullmatch(result["MC_SERVICE_USER"]),
            "MC_SERVICE_USER must be an existing non-root service account")
    require(result["MC_SERVICE_USER"] != "root", "MC_SERVICE_USER must not be root")
    require(SERVICE_NAME.fullmatch(result["MC_GATEWAY_SERVICE"]),
            "MC_GATEWAY_SERVICE must be a systemd service name")
    require(re.fullmatch(r"[A-Za-z0-9_.-]+", result["MC_LOG_NAMESPACE"]),
            "MC_LOG_NAMESPACE contains unsupported characters")
    require(1 <= int(result["MC_LOG_LINES"]) <= 100000,
            "MC_LOG_LINES must be between 1 and 100000")
    return result


def installed_config(cfg):
    raw = sudo(["cat", "--", cfg["MC_CONFIG"]], capture=True)
    require(len(raw.encode()) <= 65536, "installed configuration exceeds 64 KiB")
    value = json.loads(raw)
    require(isinstance(value, dict), "installed configuration must be a JSON object")
    return value, raw.encode()


def candidate_config(values, current):
    candidate = render_config(values, current)
    changed = sorted(key for key in FIXED_FIELDS if candidate.get(key) != current.get(key))
    require(not changed,
            "these settings are tied to installed units, sudoers, or Minecraft listeners: " +
            ", ".join(changed) +
            "; configuration apply cannot migrate that installation")
    return candidate


@contextlib.contextmanager
def temporary_config(candidate):
    directory = ROOT / ".local"
    directory.mkdir(mode=0o700, exist_ok=True)
    descriptor, name = tempfile.mkstemp(prefix="config-check-", suffix=".json", dir=directory)
    try:
        with os.fdopen(descriptor, "w") as stream:
            json.dump(candidate, stream, indent=2)
            stream.write("\n")
        yield Path(name)
    finally:
        Path(name).unlink(missing_ok=True)


def atomic_write(path, data, *, mode, uid, gid):
    descriptor, name = tempfile.mkstemp(prefix=".mc-gateway-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            os.fchmod(stream.fileno(), mode)
            os.fchown(stream.fileno(), uid, gid)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        Path(name).unlink(missing_ok=True)


def replace_installed(source, destination, expected_hash, *, kind, binary=None, config=None):
    """Privileged final stage; verify again before touching the installed file."""
    require(os.geteuid() == 0, "installation requires sudo")
    require(source.is_absolute() and destination.is_absolute(), "installation paths must be absolute")
    lock_path = destination.with_name("." + destination.name + ".lock")
    descriptor = os.open(lock_path, os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        _replace_locked(source, destination, expected_hash, kind=kind, binary=binary, config=config)


def _replace_locked(source, destination, expected_hash, *, kind, binary, config):
    info = destination.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o022,
            "installed destination must be a root-owned regular file without group/other writes")
    old = destination.read_bytes()
    require(hashlib.sha256(old).hexdigest() == expected_hash,
            "installed file changed during validation; retry after reviewing the new file")
    data = source.read_bytes()
    require(data, "candidate file is empty")
    if kind == "config":
        require(len(data) <= 65536, "candidate configuration exceeds 64 KiB")
        before, after = json.loads(old), json.loads(data)
        require(isinstance(after, dict), "candidate configuration must be a JSON object")
        require(all(before.get(key) == after.get(key) for key in FIXED_FIELDS),
                "candidate would change the installed backend topology")
        require(binary and Path(binary).is_absolute(), "validation binary must be absolute")
        # Validate the exact bytes that will be installed, not a mutable source.
        descriptor, name = tempfile.mkstemp(prefix=".mc-gateway-check-", dir=destination.parent)
        try:
            with os.fdopen(descriptor, "wb") as stream:
                stream.write(data)
            run([binary, "check", "--config", name], capture=True)
        finally:
            Path(name).unlink(missing_ok=True)
        mode = 0o640
    else:
        require(kind == "binary" and data.startswith(b"\x7fELF"), "candidate is not an ELF executable")
        require(config and Path(config).is_absolute(), "installed configuration path must be absolute")
        descriptor, name = tempfile.mkstemp(prefix=".mc-gateway-check-", dir=destination.parent)
        try:
            with os.fdopen(descriptor, "wb") as stream:
                os.fchmod(stream.fileno(), 0o700)
                stream.write(data)
            run([name, "check", "--config", config], capture=True)
        finally:
            Path(name).unlink(missing_ok=True)
        mode = 0o755
    # A failed validation leaves both current and previous files unchanged.
    atomic_write(destination.with_name(destination.name + ".previous"), old,
                 mode=mode, uid=info.st_uid, gid=info.st_gid)
    atomic_write(destination, data, mode=mode, uid=info.st_uid, gid=info.st_gid)


def initialize():
    require(not ENV_FILE.exists(), ".env already exists; use task config:edit")
    cfg = settings({})
    current, _ = installed_config(cfg)
    values = values_from_config(current)
    service_user = sudo(["systemctl", "show", cfg["MC_GATEWAY_SERVICE"],
                         "--property=User", "--value"], capture=True).strip()
    require(service_user, "installed gateway service does not declare a User")
    values["MC_SERVICE_USER"] = service_user
    settings(values)
    data = ("# Local operator and gateway settings. Never commit this file.\n"
            "# task config:check; task config:apply; task gateway:restart\n" +
            serialize_env(values))
    descriptor = os.open(ENV_FILE, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w") as stream:
        stream.write(data)
    print("Created .env from the installed settings (mode 0600).")


def operate(action, arguments):
    if action == "config-init":
        require(not arguments, "config:init takes no arguments")
        initialize()
        return
    if action == "config-edit":
        require(not arguments, "config:edit takes no arguments")
        require(ENV_FILE.exists(), "run task config:init first")
        editor = shlex.split(os.environ.get("VISUAL") or os.environ.get("EDITOR") or "nano")
        require(editor, "EDITOR is empty")
        os.execvp(editor[0], editor + [str(ENV_FILE)])
    require(ENV_FILE.exists(), "missing .env; run task config:init first")
    values = read_env(ENV_FILE)
    cfg = settings(values)
    current, raw = installed_config(cfg)
    command_prefix = [cfg["MC_BINARY"]]

    if action in ("config-show", "config-check", "config-apply"):
        require(not arguments, "configuration tasks take no arguments")
        candidate = candidate_config(values, current)
        if action == "config-show":
            print(json.dumps(candidate, indent=2))
            print("These are proposed settings; apply saves them for the next gateway start.")
            return
        with temporary_config(candidate) as path:
            sudo(command_prefix + ["check", "--config", str(path)], capture=True)
            if action == "config-check":
                print(".env and RCON credential file permissions are valid. No files or services changed.")
                return
            if candidate == current:
                print("Installed configuration already matches .env. No files or services changed.")
                return
            sudo([sys.executable, str(Path(__file__).resolve()), "_apply", str(path),
                  cfg["MC_CONFIG"], hashlib.sha256(raw).hexdigest(), cfg["MC_BINARY"]])
        print(f"Configuration saved atomically; previous copy is {cfg['MC_CONFIG']}.previous.")
        print("The running gateway keeps its current settings until task gateway:restart.")
        print("Gateway restart disconnects clients; Java stays running. No restart was performed.")
        return

    if action == "deploy":
        require(not arguments, "deploy takes no arguments")
        source = ROOT / "bin/mc-gateway"
        require(source.is_file(), "build the binary with task build first")
        sudo([str(source), "check", "--config", cfg["MC_CONFIG"]], capture=True)
        old_hash = sudo(["sha256sum", "--", cfg["MC_BINARY"]], capture=True).split()[0]
        sudo([sys.executable, str(Path(__file__).resolve()), "_deploy", str(source),
              cfg["MC_BINARY"], old_hash, cfg["MC_CONFIG"]])
        print("Installed the new executable atomically. Operator commands use it immediately.")
        print("The running gateway keeps its current executable until task gateway:restart.")
        print("Gateway and Minecraft processes were left running.")
        return

    if action.startswith("logs"):
        server = "server" in action
        follow = action.endswith("follow")
        unit = current["backend_service"] if server else cfg["MC_GATEWAY_SERVICE"]
        argv = ["sudo", "-n", "--", "journalctl", "--namespace=" + cfg["MC_LOG_NAMESPACE"],
                "-u", unit, "-n", str(cfg["MC_LOG_LINES"]), "-o", "cat"]
        argv += ["-f"] if follow else ["--no-pager"]
        os.execvp(argv[0], argv + arguments)

    if action in ("status", "start", "stop", "command", "version"):
        if action == "command":
            argv = command_prefix + ["command", "--config", cfg["MC_CONFIG"],
                                     "--timeout", str(cfg["MC_COMMAND_TIMEOUT"]), "--"] + arguments
        else:
            require(not arguments, f"{action} takes no arguments")
            operation = {"start": "wake", "stop": "sleep"}.get(action, action)
            argv = command_prefix + [operation]
            if operation != "version":
                argv += ["--socket", current["admin_socket"]]
        # Tasks run as the service user, not as root; RCON stays local/private.
        full = ["sudo", "-n", "-u", cfg["MC_SERVICE_USER"], "--"] + argv
        os.execvp(full[0], full)

    if action in ("gateway-start", "gateway-stop", "gateway-restart"):
        require(not arguments, f"{action} takes no arguments")
        verb = action.removeprefix("gateway-")
        if verb in ("stop", "restart"):
            print(f"Explicit gateway {verb}: connected clients disconnect; Java stays running.", flush=True)
        sudo(["systemctl", verb, cfg["MC_GATEWAY_SERVICE"]])
        return
    raise RuntimeError("unknown management action")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action")
    parser.add_argument("arguments", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    arguments = args.arguments
    if arguments[:1] == ["--"]:
        arguments = arguments[1:]
    if args.action in ("_apply", "_deploy"):
        require(len(arguments) == 4, "invalid install arguments")
        source, destination, digest = arguments[:3]
        replace_installed(Path(source), Path(destination), digest,
                          kind="config" if args.action == "_apply" else "binary",
                          binary=arguments[3] if args.action == "_apply" else None,
                          config=arguments[3] if args.action == "_deploy" else None)
        return
    operate(args.action, arguments)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError) as error:
        print(f"mc-gateway tasks: {error}", file=sys.stderr)
        sys.exit(1)
