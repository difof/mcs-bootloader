#!/usr/bin/env python3
"""Embedded operator helpers. Treat .env and arguments as data, never shell code.

The Go CLI supplies explicit paths and extracts these helpers privately.
Configuration reload and console commands are handled natively by Go through
the control socket. Binary deployment is atomic; restart remains explicit.
"""

import argparse
import contextlib
import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import signal
import stat
import subprocess
import sys
import tempfile

from env_config import (
    MANAGE_DEFAULTS, RUNTIME_DEFAULTS, MAX_ENV_BYTES, parse_env, redacted_values,
)

ROOT = Path(__file__).resolve().parent.parent
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
    if data == old:
        return False
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
    return True


def private_env(path):
    """Read one explicit source, including a canonical-file symlink."""
    resolved = path.resolve(strict=True)
    descriptor = os.open(resolved, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) in (0o600, 0o640),
                ".env must be a regular private file with 0600 or 0640 permissions")
        raw = stream.read(MAX_ENV_BYTES + 1)
    return resolved, parse_env(raw)


def embedded_canonical(env_path):
    path, values = private_env(env_path)
    cfg = settings(values)
    require(path == Path(cfg["MC_CONFIG"]).resolve(strict=True),
            "this command requires the canonical installed MC_CONFIG file; select it with --env")
    return path, values, cfg


def embedded_system(argv, *, user=None):
    require(os.geteuid() == 0, "this operator command requires root; run it with sudo")
    if user:
        argv = ["/usr/sbin/runuser", "-u", user, "--"] + list(argv)
    return run(argv, capture=True)


def embedded_operate(action, arguments, env_path, executable):
    """Source-independent dispatcher called by the binary's isolated helper."""
    env_path, executable = Path(env_path).absolute(), Path(executable).absolute()
    result = {"ok": True, "action": action}
    if action in ("init", "config-new"):
        require(not arguments, "init accepts one output path through --env")
        require(not os.path.lexists(env_path), ".env already exists; use config edit")
        require(env_path.parent.is_dir(), "configuration parent directory does not exist")
        example = (ROOT / ".env.example").read_text()
        placeholder = "replace-with-a-random-private-password"
        require(example.count(placeholder) == 1, "embedded credential marker is invalid")
        data = example.replace(placeholder, secrets.token_urlsafe(32))
        descriptor = os.open(env_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        result.update(path=str(env_path), message="Created private .env with a random RCON password. Configure your Minecraft paths and service account before installation.")
        return result

    if action == "config-link":
        require(not arguments, "config link takes no extra arguments")
        canonical = Path(MANAGE_DEFAULTS["MC_CONFIG"])
        require(canonical.is_file(), "installed canonical .env is missing")
        if env_path.is_symlink() and env_path.resolve() == canonical.resolve():
            result.update(path=str(env_path), changed=False, message="Configuration link is already correct.")
            return result
        require(not os.path.lexists(env_path), "refusing to overwrite an existing configuration file or link")
        env_path.symlink_to(canonical)
        result.update(path=str(env_path), changed=True, message="Linked the installed canonical .env; no configuration was copied.")
        return result

    if action == "config-edit":
        require(not arguments, "config edit takes no extra arguments")
        source = env_path.resolve(strict=True)
        require(source.is_file(), "configuration must be a regular file")
        editor = shlex.split(os.environ.get("VISUAL") or os.environ.get("EDITOR") or "nano")
        require(editor, "EDITOR is empty")
        # Terminal output stays on the terminal, preserving the CLI's JSON
        # stdout contract. Resolve links before editors perform atomic saves.
        os.umask(0o027)
        try:
            with open("/dev/tty", "r+b", buffering=0) as terminal:
                edited = subprocess.Popen(editor + [str(source)], stdin=terminal,
                                          stdout=terminal, stderr=terminal)
                try:
                    returncode = edited.wait()
                except BaseException:
                    # In an interactive foreground group only Python receives
                    # the CLI's explicit cancel signal. Close its editor too.
                    edited.terminate()
                    try:
                        edited.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        edited.kill()
                        edited.wait()
                    raise
        except OSError as error:
            raise RuntimeError("config edit needs an interactive terminal and an installed EDITOR") from error
        require(returncode == 0, "editor exited unsuccessfully")
        result.update(path=str(source), message="Configuration edited. Use config check, then config reload to apply supported settings.")
        return result

    if action == "config-show":
        require(not arguments, "config show takes no extra arguments")
        source, values = private_env(env_path)
        result.update(path=str(source), settings=redacted_values(values))
        return result

    if action == "install":
        import install
        require(os.geteuid() == 0, "installation requires root")
        parser = argparse.ArgumentParser(add_help=False, exit_on_error=False)
        parser.add_argument("--report", type=Path, default=env_path.parent / ".mcs-bootloader-installation.json")
        parsed, unknown = parser.parse_known_args(arguments)
        require(not unknown, "install supports only --report PATH")
        with contextlib.redirect_stdout(io.StringIO()):
            data = install.install(env_path, parsed.report.absolute(), binary_path=executable)
        result.update(data)
        result["message"] = "Gateway installed and enabled; Minecraft starts when a player connects or you run start."
        return result

    if action == "remove":
        import remove
        require(arguments in ([], ["--dry-run"], ["--yes"]), "remove accepts --dry-run or --yes")
        return remove.remove(env_path, yes=arguments == ["--yes"])

    require(not arguments, "this operator command takes no extra arguments")
    canonical, values, cfg = embedded_canonical(env_path)
    require(os.geteuid() == 0, "this operator command requires root")
    if action == "deploy":
        require(executable.is_file(), "the running executable is unavailable")
        destination = Path(cfg["MC_BINARY"])
        # Destination and parents are validated before the privileged replace.
        import service_config
        service_config.secure_directory(destination.parent)
        installed, _ = service_config.read_regular(destination, owned=True, limit=128 * 1024 * 1024)
        # Reject pending/foreign unit edits before copying a candidate binary.
        service_config.preflight(canonical)
        if executable.read_bytes() == installed:
            configured = service_config.configure(canonical)
            result.update(changed=False, service_changed=configured, binary=str(destination),
                          message="This executable is already installed; service settings were checked and the previous rollback binary was preserved.")
            return result
        replace_installed(executable, destination, hashlib.sha256(installed).hexdigest(), config=str(canonical))
        try:
            configured = service_config.configure(canonical)
        except Exception as error:
            raise RuntimeError("Executable installed, but service settings could not be refreshed; "
                               "the running gateway and Minecraft were not restarted. "
                               f"Previous executable: {destination}.previous. Review gateway configure before restarting: {error}") from error
        result.update(changed=True, service_changed=configured, binary=str(destination), previous=str(destination) + ".previous",
                      message="Executable installed atomically and service settings refreshed. Run gateway restart to use it; Minecraft stays running.")
        return result
    if action == "gateway-configure":
        import service_config
        changed = service_config.configure(canonical)
        result.update(changed=changed, message="Gateway service settings configured; no service was restarted." if changed else "Gateway service settings are already configured.")
        return result
    if action in ("gateway-start", "gateway-stop", "gateway-restart"):
        verb = action.removeprefix("gateway-")
        if verb in ("start", "restart"):
            state = embedded_system(["/usr/bin/systemctl", "show", cfg["MC_GATEWAY_SERVICE"],
                                     "--property=ActiveState", "--value"]).strip()
            if state in ("active", "activating", "reloading"):
                check = [cfg["MC_BINARY"], "restart-check", "--socket", cfg["MC_ADMIN_SOCKET"],
                         "--config", str(canonical)]
            else:
                require(state in ("inactive", "failed"), "gateway is stopping or its state is unknown; wait before starting it")
                check = [cfg["MC_BINARY"], "check", "--config", str(canonical)]
            embedded_system(check, user=cfg["MC_SERVICE_USER"])
        embedded_system(["/usr/bin/systemctl", verb, cfg["MC_GATEWAY_SERVICE"]])
        result.update(service=cfg["MC_GATEWAY_SERVICE"],
                      message=f"Gateway {verb} completed. Minecraft was left running if it was already up.")
        return result
    raise RuntimeError("unknown embedded operator action")


def embedded_main():
    parser = argparse.ArgumentParser(add_help=False, exit_on_error=False)
    parser.add_argument("--embedded", action="store_true")
    parser.add_argument("--env", required=True)
    parser.add_argument("--executable", required=True)
    parser.add_argument("action")
    parser.add_argument("arguments", nargs=argparse.REMAINDER)

    def interrupted(signum, frame):
        # Allow install's exception handler to restore transaction state.
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        raise RuntimeError("operator command interrupted")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        args = parser.parse_args()
        result = embedded_operate(args.action, args.arguments, Path(args.env), Path(args.executable))
        print(json.dumps(result, ensure_ascii=False))
    except Exception as error:
        print(json.dumps({"ok": False, "error": str(error)}, ensure_ascii=False))
        sys.exit(1)


if __name__ == "__main__":
    embedded_main()
