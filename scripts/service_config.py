#!/usr/bin/env python3
"""Apply the owned gateway runtime drop-in without restarting either service."""

import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile

from env_config import MAX_ENV_BYTES, parse_env
from manage import settings


SYSTEMD_DIRECTORY = Path("/etc/systemd/system")
DROP_IN_NAME = "mcs-bootloader-runtime.conf"
DROP_IN = (
    "# Managed by mcs-bootloader scripts/service_config.py.\n"
    "# Put operator overrides in a separate drop-in.\n"
    "[Service]\n"
    "ProtectHome=read-only\n"
    "TimeoutStopSec=infinity\n"
).encode()
SYSTEMCTL = "/usr/bin/systemctl"
BUSCTL = "/usr/bin/busctl"


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(argv):
    """Only fixed inspection commands and daemon-reload are issued below."""
    try:
        result = subprocess.run(argv, capture_output=True, text=True,
                                check=False, timeout=30)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RuntimeError("systemd inspection/reload command could not finish") from error
    require(result.returncode == 0, "systemd inspection/reload command failed")
    require(len(result.stdout) <= 256 * 1024, "systemd inspection output exceeds its safety limit")
    # Do not include raw unit/argv output in errors: unrelated services may have
    # credentials in their command line, and this helper must reject them quietly.
    return result.stdout


def _root_owned(info):
    return info.st_uid == 0


def secure_directory(path):
    """Reject symlinks and writable parents before any privileged file write."""
    require(path.is_absolute(), "service directories must be absolute")
    for directory in (path, *path.parents):
        info = directory.lstat()
        require(stat.S_ISDIR(info.st_mode) and _root_owned(info),
                "service directory and its parents must be real root-owned directories")
        writable = bool(info.st_mode & 0o022)
        # A sticky root-owned ancestor such as /tmp cannot replace our
        # root-owned child. The actual destination itself must be private to root.
        sticky_ancestor = directory != path and bool(info.st_mode & stat.S_ISVTX)
        require(not writable or sticky_ancestor,
                "service directory and its parents must not permit untrusted writes")


def read_regular(path, *, private=False, owned=False, limit=1024 * 1024):
    descriptor = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
    with os.fdopen(descriptor, "rb") as stream:
        info = os.fstat(stream.fileno())
        require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1,
                "service/configuration files must be regular files without symlinks or hardlinks")
        if private:
            require(stat.S_IMODE(info.st_mode) in (0o600, 0o640),
                    ".env must have private 0600 or 0640 permissions")
        if owned:
            require(_root_owned(info) and not info.st_mode & 0o022,
                    "installed service files must be root-owned without group/other writes")
        raw = stream.read(limit + 1)
        require(len(raw) <= limit, "service/configuration file exceeds its safety limit")
        after = os.fstat(stream.fileno())
    current = path.lstat()
    identity = lambda st: (st.st_dev, st.st_ino, st.st_mode, st.st_size, st.st_mtime_ns, st.st_ctime_ns)
    require(identity(info) == identity(after) == identity(current) and len(raw) == current.st_size,
            "service/configuration file changed during validation")
    return raw, info


def inspect_service(cfg):
    unit = cfg["MC_GATEWAY_SERVICE"]
    names = ("Id", "LoadState", "FragmentPath", "NeedDaemonReload", "ProtectHome", "TimeoutStopUSec")
    text = run([SYSTEMCTL, "show", unit, *["--property=" + name for name in names]])
    properties = {}
    for line in text.splitlines():
        key, separator, value = line.partition("=")
        require(separator and key in names and key not in properties,
                "systemd returned an unexpected service description")
        properties[key] = value
    require(set(properties) == set(names), "systemd service description is incomplete")
    require(properties["Id"] == unit and properties["LoadState"] == "loaded",
            "configured gateway unit is missing, masked, or an alias")
    fragment = Path(properties["FragmentPath"])
    require(fragment.is_absolute(), "gateway unit must have an installed file")
    secure_directory(fragment.parent)
    read_regular(fragment, owned=True)
    binary = Path(cfg["MC_BINARY"])
    secure_directory(binary.parent)
    read_regular(binary, owned=True, limit=128 * 1024 * 1024)

    # systemctl's printable ExecStart joins argv with spaces. Inspect the typed
    # D-Bus array instead, so a quoted argument cannot impersonate several args.
    try:
        unit_reply = json.loads(run([BUSCTL, "--system", "--json=short", "call",
                                    "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                                    "org.freedesktop.systemd1.Manager", "GetUnit", "s", unit]))
        require(unit_reply.get("type") == "o" and isinstance(unit_reply.get("data"), list)
                and len(unit_reply["data"]) == 1 and isinstance(unit_reply["data"][0], str)
                and unit_reply["data"][0].startswith("/org/freedesktop/systemd1/unit/"),
                "systemd returned an invalid gateway object")
        reply = json.loads(run([BUSCTL, "--system", "--json=short", "get-property",
                               "org.freedesktop.systemd1", unit_reply["data"][0],
                               "org.freedesktop.systemd1.Service", "ExecStart"]))
        commands = reply.get("data")
        expected = [cfg["MC_BINARY"], "serve", "--config", cfg["MC_CONFIG"]]
        require(reply.get("type") == "a(sasbttttuii)" and isinstance(commands, list)
                and len(commands) == 1 and isinstance(commands[0], list) and len(commands[0]) == 10
                and commands[0][0] == cfg["MC_BINARY"] and commands[0][1] == expected,
                "configured unit does not execute the installed mcs-bootloader with its canonical .env")
    except (ValueError, TypeError, AttributeError, KeyError) as error:
        raise RuntimeError("systemd returned an invalid gateway execution description") from error
    return properties


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def atomic_write(path, data, *, mode=0o644, gid=0):
    descriptor, name = tempfile.mkstemp(prefix=".mcs-bootloader-runtime-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            os.fchown(stream.fileno(), 0, gid)
            os.fchmod(stream.fileno(), mode)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        sync_directory(path.parent)
    finally:
        Path(name).unlink(missing_ok=True)


def configured(properties):
    return properties["ProtectHome"] == "read-only" and properties["TimeoutStopUSec"] == "infinity"


def configure(env_path, *, systemd_directory=SYSTEMD_DIRECTORY):
    require(os.geteuid() == 0, "gateway service configuration requires sudo")
    env_path = Path(env_path)
    require(env_path.is_absolute(), "--env must name the absolute canonical .env")
    raw, _ = read_regular(env_path, private=True, limit=MAX_ENV_BYTES)
    cfg = settings(parse_env(raw))
    require(env_path.resolve(strict=True) == Path(cfg["MC_CONFIG"]).resolve(strict=True),
            "--env must name the canonical MC_CONFIG file")
    before = inspect_service(cfg)
    require(before["NeedDaemonReload"] == "no",
            "unit definitions already have unapplied edits; review and reload those before gateway:configure")
    secure_directory(systemd_directory)
    directory = systemd_directory / (cfg["MC_GATEWAY_SERVICE"] + ".d")
    created_directory = False
    try:
        directory.mkdir(mode=0o755)
        created_directory = True
    except FileExistsError:
        pass
    try:
        secure_directory(directory)
        descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        with contextlib.ExitStack() as lock:
            lock.callback(os.close, descriptor)
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise RuntimeError("gateway service configuration is already in progress") from error
            path = directory / DROP_IN_NAME
            try:
                previous, metadata = read_regular(path, owned=True)
            except FileNotFoundError:
                previous, metadata = None, None
            require(previous is None or previous == DROP_IN,
                    "the fixed gateway drop-in already exists with unknown content; it was not overwritten")
            if previous == DROP_IN and stat.S_IMODE(metadata.st_mode) == 0o644 and configured(before):
                return False
            # Keep the previous bytes/permissions until both the reload and the
            # resulting effective settings have been verified. No base unit or
            # operator drop-in is ever rewritten.
            backup = None
            if previous is not None:
                fd, name = tempfile.mkstemp(prefix=".mcs-bootloader-runtime-backup-", dir=directory)
                backup = Path(name)
                with os.fdopen(fd, "wb") as stream:
                    os.fchmod(stream.fileno(), 0o600)
                    stream.write(previous)
                    stream.flush()
                    os.fsync(stream.fileno())
            try:
                atomic_write(path, DROP_IN)
                run([SYSTEMCTL, "daemon-reload"])
                after = inspect_service(cfg)
                require(after["FragmentPath"] == before["FragmentPath"] and configured(after)
                        and after["NeedDaemonReload"] == "no",
                        "gateway runtime settings were not applied; another drop-in may override them")
            except Exception as original:
                try:
                    try:
                        present, _ = read_regular(path, owned=True)
                    except FileNotFoundError:
                        present = None
                    require(present == DROP_IN or previous is None and present is None,
                            "gateway drop-in changed concurrently; automatic rollback was refused")
                    if previous is None:
                        path.unlink(missing_ok=True)
                        sync_directory(directory)
                    else:
                        atomic_write(path, previous, mode=stat.S_IMODE(metadata.st_mode), gid=metadata.st_gid)
                    run([SYSTEMCTL, "daemon-reload"])
                except Exception as recovery:
                    raise RuntimeError("gateway configuration failed and rollback/reload needs operator attention"
                                       + (f"; saved drop-in: {backup}" if backup else "")) from recovery
                if backup is not None:
                    backup.unlink()
                raise RuntimeError("gateway configuration failed; the previous drop-in was restored: " + str(original)) from original
            if backup is not None:
                backup.unlink()
            return True
    finally:
        if created_directory:
            try:
                directory.rmdir()  # only removes the new directory after a failed, fully rolled-back write
            except OSError:
                pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--env", required=True, type=Path)
    args = parser.parse_args()
    changed = configure(args.env)
    print("Gateway service settings configured; neither service was restarted." if changed
          else "Gateway service settings are already configured; no changes made.")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError) as error:
        print(f"mcs-bootloader gateway configuration: {error}", file=sys.stderr)
        sys.exit(1)
