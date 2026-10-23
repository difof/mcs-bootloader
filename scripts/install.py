#!/usr/bin/env python3
"""First-install only. Validate a stopped, backed-up server before activating the gateway.

Pre-activation failures restore properties and remove only this invocation's new
files. Activation failures retain the validated installation for inspection: a
backend might already be starting, so automatic world/config rollback is unsafe.
"""

import argparse
import datetime
import errno
import grp
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import socket
import stat
import subprocess
import sys
import tempfile

from env_config import MANAGE_DEFAULTS, RUNTIME_DEFAULTS, read_env


ROOT = Path(__file__).resolve().parent.parent
FIXED_SETTINGS = {
    "MC_LISTEN": ":25565", "MC_BACKEND_ADDRESS": "127.0.0.1:25566",
    "MC_RCON_ADDRESS": "127.0.0.1:25575", "MC_BACKEND_SERVICE": "mc-gateway-backend.service",
    "MC_STATE_DIRECTORY": "/var/lib/mc-gateway", "MC_ADMIN_SOCKET": "/run/mc-gateway/control.sock",
    "MC_BINARY": "/opt/mc-gateway/bin/mc-gateway", "MC_CONFIG": "/etc/mc-gateway/.env",
    "MC_GATEWAY_SERVICE": "mc-gateway.service", "MC_LOG_NAMESPACE": "mc-gateway",
}


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(argv, timeout=120, allowed=(0,)):
    result = subprocess.run(argv, capture_output=True, text=True, timeout=timeout,
                            env={**os.environ, "PATH": "/usr/sbin:/usr/bin:/sbin:/bin"})
    require(result.returncode in allowed,
            f"{argv[0]} failed ({result.returncode}): {(result.stderr or result.stdout).strip()[:2000]}")
    return result.stdout.strip()


def safe_text(value):
    require(isinstance(value, str) and value and not any(c in value for c in "\n\r\0"),
            "settings must be nonempty strings without line breaks or NULs")
    return value


def installation_settings(env_path):
    """Read the one private source without exposing values in diagnostics."""
    metadata = env_path.lstat()
    require(stat.S_ISREG(metadata.st_mode), "first-install .env must be a regular file, not a symlink")
    require(stat.S_IMODE(metadata.st_mode) in (0o600, 0o640),
            "first-install .env requires private 0600 or 0640 permissions")
    raw = env_path.read_bytes()
    settings = {**RUNTIME_DEFAULTS, **MANAGE_DEFAULTS, **read_env(env_path)}
    require(env_path.read_bytes() == raw, ".env changed while being read; retry validation")
    password = settings.get("MC_RCON_PASSWORD")
    require(isinstance(password, str) and 16 <= len(password.encode("utf-8")) <= 4096
            and not any(c in password for c in "\r\n\0"),
            "MC_RCON_PASSWORD must be supplied in .env as 16-4096 UTF-8 bytes without line breaks or NUL")
    require(password != "replace-with-a-random-private-password",
            "replace the MC_RCON_PASSWORD example placeholder before installation")
    require("MC_SERVER_ARGFILE" in settings, "MC_SERVER_ARGFILE is required in .env")
    for key, value in FIXED_SETTINGS.items():
        require(settings[key] == value, f"{key} must match the bundled installation topology")
    return settings, raw, metadata


def link_shared_env(env_path, canonical, original, metadata):
    """Replace the validated source with a link only if it is still unchanged."""
    current = env_path.lstat()
    require(stat.S_ISREG(current.st_mode) and current.st_ino == metadata.st_ino
            and current.st_dev == metadata.st_dev and env_path.read_bytes() == original,
            ".env changed during validation; refusing to replace the operator's edits")
    temporary = env_path.parent / (".mc-gateway-env-" + str(os.getpid()))
    require(not os.path.lexists(temporary), "temporary configuration link already exists")
    try:
        temporary.symlink_to(canonical)
        os.lchown(temporary, metadata.st_uid, metadata.st_gid)
        os.replace(temporary, env_path)
    finally:
        if os.path.lexists(temporary):
            temporary.unlink()


def server_argument_file(server, argument, key):
    """Resolve exactly one existing Java @file, without shell parsing."""
    argument = safe_text(argument)
    require(argument.startswith("@") and len(argument) > 1 and not argument.startswith("@@"),
            f"{key} must be an @argument-file reference")
    relative = Path(argument[1:])
    require(not relative.is_absolute() and ".." not in relative.parts,
            f"{key} must refer to a file within the server directory")
    path = (server / relative).resolve(strict=True)
    require(path.is_relative_to(server) and path.is_file(),
            f"{key} must refer to an existing file within the server directory")
    return path


def java_property_value(value):
    """Encode one Java Properties value without changing credential bytes."""
    encoded = []
    for index, char in enumerate(value):
        if char == "\\":
            encoded.append("\\\\")
        elif char in "\t\r\n\f":
            encoded.append({"\t": "\\t", "\r": "\\r", "\n": "\\n", "\f": "\\f"}[char])
        elif char == " " and index == 0:
            encoded.append("\\ ")
        elif 32 <= ord(char) <= 126:
            encoded.append(char)
        else:
            units = char.encode("utf-16-be")
            encoded.extend(f"\\u{int.from_bytes(units[i:i+2], 'big'):04X}"
                           for i in range(0, len(units), 2))
    return "".join(encoded).encode("ascii")


def systemd_working_directory(value):
    """Render a scalar path directive, whose outer quotes are literal characters.

    Keep the supported path representation conservative; reject ambiguous unit
    syntax instead of inventing shell-style quoting. Escape literal specifiers.
    """
    value = safe_text(value)
    require(value.startswith("/") and not any(
        c.isspace() or ord(c) < 32 or ord(c) == 127 or c in "\\\"'" for c in value),
        "WorkingDirectory must be absolute and contain no whitespace, quotes, backslashes, or control characters")
    return value.replace("%", "%%")


def updated_properties(original, password):
    """Preserve unrelated lines byte-for-byte, including authentication settings."""
    changes = {
        b"server-ip": b"127.0.0.1", b"server-port": b"25566",
        b"enable-rcon": b"true", b"rcon.port": b"25575",
        b"rcon.password": java_property_value(password), b"broadcast-rcon-to-ops": b"false",
    }
    lines, seen = [], set()
    newline = b"\r\n" if b"\r\n" in original else b"\n"
    for line in original.splitlines(keepends=True):
        body = line.rstrip(b"\r\n")
        require((len(body) - len(body.rstrip(b"\\"))) % 2 == 0,
                "continued property lines require manual review; nothing was changed")
        match = re.match(rb"^[ \t\f]*([^#! \t\f:=]+)", body)
        key = match.group(1) if match else None
        require(key is None or b"\\" not in key,
                "escaped property keys require manual review; nothing was changed")
        if key in changes:
            require(key != b"rcon.password" or key not in seen,
                    "duplicate rcon.password properties require manual review; nothing was changed")
            ending = line[len(body):]
            lines.append(key + b"=" + changes[key] + ending)
            seen.add(key)
        else:
            lines.append(line)
    result = b"".join(lines)
    missing = [key for key in changes if key not in seen]
    if missing and result and not result.endswith((b"\n", b"\r")):
        result += newline
    return result + b"".join(key + b"=" + changes[key] + newline for key in missing)


def write_atomic(path, data, mode, uid=0, gid=0):
    fd, temporary = tempfile.mkstemp(prefix=".mc-gateway-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            os.fchmod(stream.fileno(), mode)
            os.fchown(stream.fileno(), uid, gid)
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.lexists(temporary):
            os.unlink(temporary)


def stopped_and_ports_free(uid):
    for process in Path("/proc").iterdir():
        if not process.name.isdigit():
            continue
        try:
            if process.stat().st_uid == uid:
                name = (process / "comm").read_text().strip()
                executable = Path(os.readlink(process / "exe")).name
                require(name != "java" and executable != "java",
                        f"Java is still running for the service account (PID {process.name})")
                require(name != "startserver.sh", f"a restart launcher is still running (PID {process.name})")
        except (FileNotFoundError, ProcessLookupError):
            continue
    for family, address in ((socket.AF_INET, "0.0.0.0"), (socket.AF_INET6, "::")):
        for port in (25565, 25566, 25575):
            try:
                with socket.socket(family, socket.SOCK_STREAM) as listener:
                    if family == socket.AF_INET6:
                        listener.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
                    listener.bind((address, port))
            except OSError as exc:
                if family == socket.AF_INET6 and exc.errno in (errno.EAFNOSUPPORT, errno.EADDRNOTAVAIL):
                    continue
                raise RuntimeError(f"required TCP port {port} is unavailable on {address}: {exc}") from exc


def install(env_path, report_path):
    require(os.geteuid() == 0, "installer must run as root")
    settings, original_env, env_metadata = installation_settings(env_path)
    user = safe_text(settings["MC_SERVICE_USER"])
    require(re.fullmatch(r"[a-z_][a-z0-9_-]*\$?", user) is not None, "unsupported account name")
    account = pwd.getpwnam(user)
    require(account.pw_uid != 0, "the gateway must not run as root")
    group = grp.getgrgid(account.pw_gid).gr_name
    require(re.fullmatch(r"[a-z_][a-z0-9_-]*\$?", group) is not None, "unsupported group name")
    server_input = Path(safe_text(settings["MC_SERVER_DIRECTORY"]))
    backup_input = Path(safe_text(settings["MC_BACKUP_DIRECTORY"]))
    require(server_input.is_absolute() and backup_input.is_absolute(), "server and backup paths must be absolute")
    server, backup = server_input.resolve(strict=True), backup_input.resolve(strict=True)
    require(server.is_dir() and backup.is_dir(), "server and backup paths must be directories")
    require(not backup.is_relative_to(server), "backup directory must be outside the Minecraft server directory")
    argfile = server_argument_file(server, settings["MC_SERVER_ARGFILE"], "MC_SERVER_ARGFILE")
    jvm_argfile = server_argument_file(server, settings["MC_JVM_ARGFILE"], "MC_JVM_ARGFILE")
    java = Path(safe_text(settings["MC_JAVA_BINARY"]))
    require(java.is_absolute() and java.is_file(), "MC_JAVA_BINARY must be an existing absolute executable path")
    props, archive = server / "server.properties", backup / "world-before-gateway.tar.zst"
    require(props.is_file() and not props.is_symlink(), "server.properties must be an ordinary file")
    metadata = props.stat()
    original = props.read_bytes()
    require(metadata.st_uid == account.pw_uid, "server.properties is not owned by the selected service account")
    require((backup / "server.properties").read_bytes() == original, "backup properties differ from the stopped server")
    require(archive.is_file() and archive.stat().st_size > 0, "world backup is missing or empty")
    require(not os.path.lexists(report_path), "installation report already exists; this is a first-install operation")
    require(report_path.parent.is_dir(), "installation report directory must already exist")
    for directory in ("/opt", "/etc", "/etc/systemd/system", "/etc/sudoers.d", "/var/lib"):
        st = Path(directory).stat()
        require(st.st_uid == 0 and not st.st_mode & 0o022, f"unsafe installation parent permissions: {directory}")
    roots = [Path("/opt/mc-gateway"), Path("/etc/mc-gateway"), Path("/var/lib/mc-gateway")]
    targets = [Path("/etc/systemd/system/mc-gateway.service"),
               Path("/etc/systemd/system/mc-gateway-backend.service"),
               Path("/etc/systemd/journald@mc-gateway.conf"), Path("/etc/sudoers.d/mc-gateway")]
    for path in roots + targets + [Path("/run/mc-gateway")]:
        require(not os.path.lexists(path), f"refusing existing installation path: {path}")
    for unit in ("mc-gateway.service", "mc-gateway-backend.service"):
        state = run(["/usr/bin/systemctl", "show", unit, "--property=LoadState", "--value"], allowed=(0, 1))
        require(state == "not-found", f"refusing existing or uninspectable unit: {unit} ({state})")
    stopped_and_ports_free(account.pw_uid)
    run(["/usr/bin/zstd", "--test", "--quiet", str(archive)])
    require("world/level.dat" in run(["/usr/bin/tar", "--zstd", "--list", "--file", str(archive),
                                    "--wildcards", "--no-anchored", "world/level.dat"]),
            "world backup does not contain level.dat")
    for path, permission in ((server, "-w"), (argfile, "-r"), (jvm_argfile, "-r"), (java, "-x")):
        run(["/usr/sbin/runuser", "-u", user, "--", "/usr/bin/test", permission, str(path)])
    password = settings["MC_RCON_PASSWORD"]
    replacement = updated_properties(original, password)
    binary = (ROOT / "bin/mc-gateway").read_bytes()
    require(binary.startswith(b"\x7fELF"), "bin/mc-gateway is not a built Linux executable")
    created, directories, changed_props, reloaded, linked_env = [], [], False, False, False
    report = {"created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "status": "validating",
              "user": user, "server_directory": str(server), "backup_directory": str(backup),
              "configuration": "/etc/mc-gateway/.env", "project_env": str(env_path),
              "operator_uid": env_metadata.st_uid,
              "original_properties": {"sha256": hashlib.sha256(original).hexdigest(),
                                      "uid": metadata.st_uid, "gid": metadata.st_gid,
                                      "mode": oct(stat.S_IMODE(metadata.st_mode)),
                                      "atime_ns": metadata.st_atime_ns, "mtime_ns": metadata.st_mtime_ns},
              "binary_sha256": hashlib.sha256(binary).hexdigest(), "phase": "install files"}

    def save_report():
        write_atomic(report_path, (json.dumps(report, indent=2) + "\n").encode(), 0o600,
                     report_path.parent.stat().st_uid, report_path.parent.stat().st_gid)

    def add_file(path, data, mode=0o644, gid=0, uid=0):
        require(not os.path.lexists(path), f"installation path appeared concurrently: {path}")
        write_atomic(path, data, mode, uid, gid)
        created.append(path)

    try:
        for path, mode, uid, gid in ((roots[0], 0o755, 0, 0), (roots[0] / "bin", 0o755, 0, 0),
                                     (roots[1], 0o2750, env_metadata.st_uid, account.pw_gid),
                                     (roots[2], 0o700, account.pw_uid, account.pw_gid)):
            path.mkdir(mode=mode)
            directories.append(path)
            os.chown(path, uid, gid)
            os.chmod(path, mode)
        add_file(roots[0] / "bin/mc-gateway", binary, 0o755)
        add_file(roots[1] / ".env", original_env, 0o640, account.pw_gid, env_metadata.st_uid)
        for destination in targets[:2]:
            unit = (ROOT / "deploy" / destination.name).read_text()
            require(unit.count("User=minecraft\n") == 1 and unit.count("Group=minecraft\n") == 1,
                    "service template account markers changed; review installer")
            unit = unit.replace("User=minecraft\n", f"User={user}\n").replace("Group=minecraft\n", f"Group={group}\n")
            add_file(destination, unit.encode())
        add_file(targets[2], (ROOT / "deploy/journald@mc-gateway.conf").read_bytes())
        sudoers = f"{user} ALL=(root) NOPASSWD: /usr/bin/systemctl start mc-gateway-backend.service\n".encode()
        with tempfile.TemporaryDirectory(prefix="mc-gateway-sudoers-") as temporary:
            staged = Path(temporary) / "rule"
            staged.write_bytes(sudoers)
            staged.chmod(0o440)
            run(["/usr/sbin/visudo", "-cf", str(staged)])
        add_file(targets[3], sudoers, 0o440)
        report["phase"] = "validate installed units and configuration"
        run(["/usr/bin/systemd-analyze", "verify", str(targets[0]), str(targets[1])])
        run(["/usr/sbin/runuser", "-u", user, "--", str(roots[0] / "bin/mc-gateway"),
             "check", "--config", str(roots[1] / ".env")])
        report["phase"] = "update stopped server properties"
        stopped_and_ports_free(account.pw_uid)
        require(props.read_bytes() == original, "server.properties changed during validation")
        write_atomic(props, replacement, 0o640, metadata.st_uid, metadata.st_gid)
        changed_props = True
        report["properties_sha256"] = hashlib.sha256(replacement).hexdigest()
        report["installed_files"] = [str(path) for path in created]
        report["phase"] = "link shared configuration"
        link_shared_env(env_path, roots[1] / ".env", original_env, env_metadata)
        linked_env = True
        report["phase"] = "reload validated units"
        run(["/usr/bin/systemctl", "daemon-reload"])
        reloaded = True
        stopped_and_ports_free(account.pw_uid)
        report["status"], report["phase"] = "validated", "before activation"
        save_report()
        report["phase"] = "activate gateway"
        run(["/usr/bin/systemctl", "enable", "--now", "mc-gateway.service"])
        report["status"], report["phase"] = "installed", "complete"
        save_report()
        print(json.dumps({"status": "installed", "gateway": "enabled", "backend": "not explicitly started or enabled",
                          "report": str(report_path)}))
    except Exception as exc:
        failure_phase = report["phase"]
        rollback_errors = []
        if failure_phase != "activate gateway" and report["status"] != "installed":
            if linked_env:
                try:
                    write_atomic(env_path, original_env, stat.S_IMODE(env_metadata.st_mode),
                                 env_metadata.st_uid, env_metadata.st_gid)
                    os.utime(env_path, ns=(env_metadata.st_atime_ns, env_metadata.st_mtime_ns))
                except Exception as rollback:
                    rollback_errors.append(f"restore project .env: {rollback}")
            if changed_props:
                try:
                    write_atomic(props, original, stat.S_IMODE(metadata.st_mode), metadata.st_uid, metadata.st_gid)
                    os.utime(props, ns=(metadata.st_atime_ns, metadata.st_mtime_ns))
                except Exception as rollback:
                    rollback_errors.append(f"properties: {rollback}")
            for path in reversed(created):
                try:
                    path.unlink()
                except OSError as rollback:
                    rollback_errors.append(f"remove {path}: {rollback}")
            for path in reversed(directories):
                try:
                    path.rmdir()
                except OSError as rollback:
                    rollback_errors.append(f"remove empty {path}: {rollback}")
            if reloaded:
                try:
                    run(["/usr/bin/systemctl", "daemon-reload"])
                except Exception as rollback:
                    rollback_errors.append(f"reload after rollback: {rollback}")
        else:
            rollback_errors.append("activation began; validated files retained for safe operator inspection")
        report.update(status="failed", phase=failure_phase, error=str(exc), rollback_errors=rollback_errors)
        try:
            save_report()
        except Exception as report_error:
            rollback_errors.append(f"report: {report_error}")
        raise RuntimeError(f"installation failed during {failure_phase}: {exc}; rollback notes: {rollback_errors}") from exc


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--env", type=Path, required=True)
    parser.add_argument("--report", type=Path, default=Path(".local/installation.json"))
    args = parser.parse_args()
    try:
        install(args.env.absolute(), args.report.absolute())
    except Exception as failure:
        print(f"mc-gateway install: {failure}", file=sys.stderr)
        sys.exit(1)
