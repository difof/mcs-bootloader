#!/usr/bin/env python3
"""Task entry points. Treat .env and command arguments as data, never shell code.

The project .env is a symlink to the daemon's one private configuration file.
Configuration reload and console commands go through the existing Unix socket.
Binary deployment is atomic; gateway restart remains an explicit operation.
"""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import stat
import subprocess
import sys
import tempfile

from env_config import (
    MANAGE_DEFAULTS, RUNTIME_DEFAULTS, read_env, redacted_values,
)

ROOT = Path(__file__).resolve().parent.parent
ENV_FILE = ROOT / ".env"
SERVICE_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.@:-]*\.service\Z")
ACCOUNT_NAME = re.compile(r"[a-z_][a-z0-9_-]*\$?\Z")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def task_arguments(proc_root=Path("/proc"), parent_pid=None):
    """Recover original CLI args before Task's recursive template expansion.

    Task 3.44 templates both CLI_ARGS and CLI_ARGS_LIST, including literal
    Minecraft text such as {{.FOO}}. On this Linux/systemd project the direct
    parent is Task, so its original NUL-delimited argv is the reliable source.
    If that contract changes, fail instead of sending guessed command text.
    """
    parent = proc_root / str(os.getppid() if parent_pid is None else parent_pid)
    executable = Path(os.readlink(parent / "exe")).name.removesuffix(" (deleted)")
    require(executable in ("task", "go-task"),
            "cannot recover original arguments: invoke task command -- <Minecraft command>")
    with (parent / "cmdline").open("rb") as stream:
        raw = stream.read(2 * 1024 * 1024 + 1)
    require(len(raw) <= 2 * 1024 * 1024 and raw.endswith(b"\0"),
            "invoking Task argument list is incomplete or too large")
    argv = raw[:-1].split(b"\0")
    try:
        separator = argv.index(b"--")
    except ValueError:
        return []
    return [os.fsdecode(item) for item in argv[separator + 1:]]


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
    result = {**MANAGE_DEFAULTS,
              "MC_ADMIN_SOCKET": RUNTIME_DEFAULTS["MC_ADMIN_SOCKET"],
              "MC_BACKEND_SERVICE": RUNTIME_DEFAULTS["MC_BACKEND_SERVICE"]}
    result.update({key: value for key, value in values.items() if key in result})
    for name in ("MC_BINARY", "MC_CONFIG", "MC_ADMIN_SOCKET"):
        require(Path(result[name]).is_absolute() and Path(result[name]) != Path("/"),
                f"{name} must be an absolute non-root path")
    require(ACCOUNT_NAME.fullmatch(result["MC_SERVICE_USER"]),
            "MC_SERVICE_USER must be an existing non-root service account")
    require(result["MC_SERVICE_USER"] != "root", "MC_SERVICE_USER must not be root")
    for name in ("MC_GATEWAY_SERVICE", "MC_BACKEND_SERVICE"):
        require(SERVICE_NAME.fullmatch(result[name]), f"{name} must be a systemd service name")
    require(re.fullmatch(r"[A-Za-z0-9_.-]+", result["MC_LOG_NAMESPACE"]),
            "MC_LOG_NAMESPACE contains unsupported characters")
    require(1 <= int(result["MC_LOG_LINES"]) <= 100000,
            "MC_LOG_LINES must be between 1 and 100000")
    return result


def require_canonical_config(cfg):
    """Fail clearly if an editor replaced the project symlink with a copy."""
    require(ENV_FILE.resolve(strict=True) == Path(cfg["MC_CONFIG"]).resolve(strict=True),
            "project .env is not the canonical MC_CONFIG file; move the duplicate aside "
            "and restore the symlink with task config:init before applying settings")


def atomic_write(path, data, *, mode, uid, gid):
    descriptor, name = tempfile.mkstemp(prefix=".mcs-bootloader-", dir=path.parent)
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


def replace_installed(source, destination, expected_hash, *, config):
    """Privileged final stage; verify again before touching the installed file."""
    require(os.geteuid() == 0, "installation requires sudo")
    require(source.is_absolute() and destination.is_absolute(), "installation paths must be absolute")
    lock_path = destination.with_name("." + destination.name + ".lock")
    descriptor = os.open(lock_path, os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        _replace_locked(source, destination, expected_hash, config=config)


def _replace_locked(source, destination, expected_hash, *, config):
    info = destination.lstat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o022,
            "installed destination must be a root-owned regular file without group/other writes")
    old = destination.read_bytes()
    require(hashlib.sha256(old).hexdigest() == expected_hash,
            "installed file changed during validation; retry after reviewing the new file")
    data = source.read_bytes()
    require(data.startswith(b"\x7fELF"), "candidate is not an ELF executable")
    require(config and Path(config).is_absolute(), "installed configuration path must be absolute")
    # Validate the exact executable bytes that will be installed, not a mutable
    # build output. The credential is read from .env, never passed in argv.
    descriptor, name = tempfile.mkstemp(prefix=".mcs-bootloader-check-", dir=destination.parent)
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
    canonical = Path(MANAGE_DEFAULTS["MC_CONFIG"])
    require(canonical.is_file(), "canonical .env is missing; complete task install first")
    if ENV_FILE.is_symlink() and ENV_FILE.resolve() == canonical.resolve():
        print("Project .env already points to the canonical configuration.")
        return
    require(not os.path.lexists(ENV_FILE),
            ".env already exists; refusing to overwrite it or create another configuration copy")
    ENV_FILE.symlink_to(canonical)
    print("Linked project .env to the canonical configuration; no settings were copied.")


def new_configuration():
    """Create the first private .env without echoing or exporting credentials."""
    require(not os.path.lexists(ENV_FILE), ".env already exists; use task config:edit")
    example = (ROOT / ".env.example").read_text()
    placeholder = "replace-with-a-random-private-password"
    require(example.count(placeholder) == 1, ".env.example credential marker changed; review the example")
    data = example.replace(placeholder, secrets.token_urlsafe(32))
    descriptor = os.open(ENV_FILE, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as stream:
        stream.write(data)
    print("Created private .env with a random RCON password. Run task config:edit to set your server paths and account.")


def preflight_gateway_restart(cfg):
    """Keep the working daemon alive if the candidate cannot safely adopt Java."""
    state = sudo(["systemctl", "show", cfg["MC_GATEWAY_SERVICE"],
                  "--property=ActiveState", "--value"], capture=True).strip()
    command = [cfg["MC_BINARY"]]
    if state in ("active", "activating", "reloading"):
        command += ["restart-check", "--socket", cfg["MC_ADMIN_SOCKET"],
                    "--config", cfg["MC_CONFIG"]]
    else:
        require(state in ("inactive", "failed"),
                "gateway state is stopping or unknown; wait for it to settle before restarting")
        # There is no running socket to consult. The daemon also verifies its
        # credential before adopting a running Minecraft process at startup.
        command += ["check", "--config", cfg["MC_CONFIG"]]
    sudo(command, capture=True, user=cfg["MC_SERVICE_USER"])


def operate(action, arguments):
    if action == "config-new":
        require(not arguments, "config:new takes no arguments")
        new_configuration()
        return
    if action == "config-init":
        require(not arguments, "config:init takes no arguments")
        initialize()
        return
    if action == "config-edit":
        require(not arguments, "config:edit takes no arguments")
        require(ENV_FILE.exists(), "run task config:init first")
        editor = shlex.split(os.environ.get("VISUAL") or os.environ.get("EDITOR") or "nano")
        require(editor, "EDITOR is empty")
        # Give editors the real filename: their atomic saves must replace the
        # canonical file rather than silently replace the project symlink.
        # Its parent has the service group and setgid bit, so replacement files
        # retain service-account access without opening the operator's home directory.
        os.umask(0o027)
        os.execvp(editor[0], editor + [str(ENV_FILE.resolve(strict=True))])
    require(ENV_FILE.exists(), "missing .env; run task config:init first")
    values = read_env(ENV_FILE)
    cfg = settings(values)
    require_canonical_config(cfg)
    command_prefix = [cfg["MC_BINARY"]]

    if action in ("config-show", "config-check", "config-apply", "reload"):
        require(not arguments, "configuration tasks take no arguments")
        if action == "config-show":
            print(json.dumps(redacted_values(values), indent=2, ensure_ascii=False))
            print("These are .env settings with defaults; task config:apply reloads supported changes live.")
            return
        if action == "config-check":
            sudo(command_prefix + ["check", "--config", cfg["MC_CONFIG"]],
                 capture=True, user=cfg["MC_SERVICE_USER"])
            print(".env is valid and readable by the service account. No files or services changed.")
            return
        sudo(command_prefix + ["reload", "--socket", cfg["MC_ADMIN_SOCKET"],
                               "--config", cfg["MC_CONFIG"]], user=cfg["MC_SERVICE_USER"])
        print("Configuration reload completed through the control socket. No service restart was requested.")
        return

    if action == "deploy":
        require(not arguments, "deploy takes no arguments")
        source = ROOT / "bin/mcs-bootloader"
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
        unit = cfg["MC_BACKEND_SERVICE"] if server else cfg["MC_GATEWAY_SERVICE"]
        argv = ["sudo", "-n", "--", "journalctl", "--namespace=" + cfg["MC_LOG_NAMESPACE"],
                "-u", unit, "-n", str(cfg["MC_LOG_LINES"]), "-o", "cat"]
        argv += ["-f"] if follow else ["--no-pager"]
        os.execvp(argv[0], argv + arguments)

    if action in ("status", "config-live", "start", "stop", "command", "version"):
        if action == "command":
            argv = command_prefix + ["command", "--socket", cfg["MC_ADMIN_SOCKET"],
                                     "--timeout", str(cfg["MC_COMMAND_TIMEOUT"]), "--"] + arguments
        else:
            require(not arguments, f"{action} takes no arguments")
            operation = {"start": "wake", "stop": "sleep", "config-live": "config"}.get(action, action)
            argv = command_prefix + [operation]
            if operation != "version":
                argv += ["--socket", cfg["MC_ADMIN_SOCKET"]]
        # Tasks run as the service user, not as root; RCON stays local/private.
        full = ["sudo", "-n", "-u", cfg["MC_SERVICE_USER"], "--"] + argv
        os.execvp(full[0], full)

    if action in ("gateway-start", "gateway-stop", "gateway-restart"):
        require(not arguments, f"{action} takes no arguments")
        verb = action.removeprefix("gateway-")
        if verb == "restart":
            preflight_gateway_restart(cfg)
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
    if args.action == "command" and arguments == ["--task-args"]:
        arguments = task_arguments()
    if args.action == "_deploy":
        require(len(arguments) == 4, "invalid install arguments")
        source, destination, digest = arguments[:3]
        replace_installed(Path(source), Path(destination), digest, config=arguments[3])
        return
    operate(args.action, arguments)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError) as error:
        print(f"mcs-bootloader tasks: {error}", file=sys.stderr)
        sys.exit(1)
