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
import ipaddress
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

from env_config import MANAGE_DEFAULTS, RUNTIME_DEFAULTS, launch_arguments, read_env


ROOT = Path(__file__).resolve().parent.parent
FIXED_SETTINGS = {
    "MC_BACKEND_SERVICE": "mcs-bootloader-backend.service",
    "MC_STATE_DIRECTORY": "/var/lib/mcs-bootloader", "MC_ADMIN_SOCKET": "/run/mcs-bootloader/control.sock",
    "MC_BINARY": "/opt/mcs-bootloader/bin/mcs-bootloader", "MC_CONFIG": "/etc/mcs-bootloader/.env",
    "MC_GATEWAY_SERVICE": "mcs-bootloader.service", "MC_LOG_NAMESPACE": "mcs-bootloader",
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
    launch_arguments(settings["MC_LAUNCH_ARGS"])
    network_settings(settings)
    for key, value in FIXED_SETTINGS.items():
        require(settings[key] == value, f"{key} must match the bundled installation topology")
    return settings, raw, metadata


def link_shared_env(env_path, canonical, original, metadata):
    """Replace the validated source with a link only if it is still unchanged."""
    current = env_path.lstat()
    require(stat.S_ISREG(current.st_mode) and current.st_ino == metadata.st_ino
            and current.st_dev == metadata.st_dev and env_path.read_bytes() == original,
            ".env changed during validation; refusing to replace the operator's edits")
    temporary = env_path.parent / (".mcs-bootloader-env-" + str(os.getpid()))
    require(not os.path.lexists(temporary), "temporary configuration link already exists")
    try:
        temporary.symlink_to(canonical)
        os.lchown(temporary, metadata.st_uid, metadata.st_gid)
        os.replace(temporary, env_path)
    finally:
        if os.path.lexists(temporary):
            temporary.unlink()


def launch_executable(server, executable):
    """Resolve one executable path; relative paths use the server directory."""
    path = Path(safe_text(executable))
    if not path.is_absolute():
        path = server / path
    path = path.resolve(strict=True)
    require(path.is_file() and path.stat().st_mode & 0o111,
            "MC_LAUNCH_EXECUTABLE must name an existing executable file")
    return path


def split_address(value, key):
    match = re.fullmatch(r"(?:\[([^\]]+)\]|([^:]*)):([0-9]+)", safe_text(value))
    require(match is not None, f"{key} must be a host:port address; bracket IPv6 addresses")
    host, port = match.group(1) or match.group(2) or "", int(match.group(3))
    require(1 <= port <= 65535, f"{key} must have a valid TCP port")
    return host, port


def network_settings(settings):
    public = split_address(settings["MC_LISTEN"], "MC_LISTEN")
    backend = split_address(settings["MC_BACKEND_ADDRESS"], "MC_BACKEND_ADDRESS")
    rcon = split_address(settings["MC_RCON_ADDRESS"], "MC_RCON_ADDRESS")
    for key, (host, _) in (("MC_BACKEND_ADDRESS", backend), ("MC_RCON_ADDRESS", rcon)):
        try:
            address = ipaddress.ip_address(host)
        except ValueError:
            raise RuntimeError(f"{key} must use a numeric loopback address") from None
        require(address.is_loopback, f"{key} must use a numeric loopback address")
    require(ipaddress.ip_address(backend[0]) == ipaddress.ip_address(rcon[0]),
            "backend and RCON must use the same loopback host because Minecraft shares server-ip")
    require(len({public[1], backend[1], rcon[1]}) == 3,
            "public, backend, and RCON TCP ports must be different")
    return public, backend, rcon


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


def updated_properties(original, password, backend_address="127.0.0.1:25566", rcon_address="127.0.0.1:25575"):
    """Preserve unrelated lines byte-for-byte, including authentication settings."""
    host, game_port = split_address(backend_address, "MC_BACKEND_ADDRESS")
    _, rcon_port = split_address(rcon_address, "MC_RCON_ADDRESS")
    changes = {
        b"server-ip": host.encode("ascii"), b"server-port": str(game_port).encode("ascii"),
        b"enable-rcon": b"true", b"rcon.port": str(rcon_port).encode("ascii"),
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
    fd, temporary = tempfile.mkstemp(prefix=".mcs-bootloader-", dir=path.parent)
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


def stopped_and_ports_free(uid, settings=None, server=None):
    settings = settings or RUNTIME_DEFAULTS
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
                if server is not None and int(process.name) != os.getpid():
                    try:
                        cwd = Path(os.readlink(process / "cwd")).resolve()
                    except FileNotFoundError:
                        cwd = None
                    require(cwd is None or not cwd.is_relative_to(server),
                            f"a process still uses the server directory (PID {process.name}); stop the server and its launcher first")
        except (FileNotFoundError, ProcessLookupError):
            continue
    ports = [address[1] for address in network_settings(settings)]
    for family, address in ((socket.AF_INET, "0.0.0.0"), (socket.AF_INET6, "::")):
        for port in ports:
            try:
                with socket.socket(family, socket.SOCK_STREAM) as listener:
                    if family == socket.AF_INET6:
                        listener.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
                    listener.bind((address, port))
            except OSError as exc:
                if family == socket.AF_INET6 and exc.errno in (errno.EAFNOSUPPORT, errno.EADDRNOTAVAIL):
                    continue
                raise RuntimeError(f"required TCP port {port} is unavailable on {address}: {exc}") from exc


def sha256_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def backup_paths(settings):
    server = Path(safe_text(settings["MC_SERVER_DIRECTORY"]))
    backup = Path(safe_text(settings["MC_BACKUP_DIRECTORY"]))
    require(server.is_absolute() and backup.is_absolute(), "server and backup paths must be absolute")
    server, backup = server.resolve(strict=True), backup.resolve()
    require(server.is_dir() and server != Path("/"), "server path must be an existing non-root directory")
    require(not backup.is_relative_to(server) and not server.is_relative_to(backup),
            "backup directory must be separate from the Minecraft server directory")
    return server, backup


def verify_backup(backup, server, original):
    """Check the completed full-server archive produced by task backup."""
    archive = backup / "server-before-bootloader.tar.zst"
    manifest_path = backup / "backup.json"
    require(archive.is_file() and not archive.is_symlink() and archive.stat().st_size > 0,
            "full-server backup is missing; run task backup with Minecraft and its launcher stopped")
    require(manifest_path.is_file() and not manifest_path.is_symlink(), "backup completion manifest is missing")
    manifest = json.loads(manifest_path.read_text())
    require(manifest.get("format") == 1 and manifest.get("server_directory") == str(server),
            "backup belongs to a different server directory or format")
    require((backup / "server.properties").read_bytes() == original
            and manifest.get("properties_sha256") == hashlib.sha256(original).hexdigest(),
            "backup properties differ from the stopped server; create a fresh backup in a new directory")
    require(sha256_file(archive) == manifest.get("archive_sha256"), "backup checksum does not match its completion manifest")
    run(["/usr/bin/zstd", "--test", "--quiet", str(archive)], timeout=3600)
    names = run(["/usr/bin/tar", "--zstd", "--list", "--file", str(archive)], timeout=3600).splitlines()
    require("./server.properties" in names, "full-server backup does not contain server.properties")
    return archive


def create_backup(env_path):
    """Snapshot the entire stopped server, including custom world directory names."""
    require(os.geteuid() == 0, "backup requires sudo")
    settings, _, env_metadata = installation_settings(env_path.resolve(strict=True))
    account = pwd.getpwnam(safe_text(settings["MC_SERVICE_USER"]))
    require(account.pw_uid != 0, "the Minecraft account must not be root")
    server, backup = backup_paths(settings)
    props = server / "server.properties"
    require(props.is_file() and not props.is_symlink(), "server.properties must be an ordinary file")
    require(props.stat().st_uid == account.pw_uid, "server.properties is not owned by the selected service account")
    stopped_and_ports_free(account.pw_uid, settings, server)
    # Tar stores symlinks as links. Refuse external targets so this cannot be
    # mistaken for a complete snapshot when a world is mounted via a symlink.
    for directory, dirs, files in os.walk(server, followlinks=False):
        for name in dirs + files:
            path = Path(directory) / name
            if path.is_symlink():
                require(path.resolve().is_relative_to(server),
                        "automatic backup does not support symlinks outside the server directory; use a server root containing the real data")
    require(not backup.exists() or (backup.is_dir() and not any(backup.iterdir())),
            "backup directory must be absent or empty; existing backups are never overwritten")
    backup.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chown(backup, env_metadata.st_uid, env_metadata.st_gid)
    os.chmod(backup, 0o700)
    original = props.read_bytes()
    archive = backup / "server-before-bootloader.tar.zst"
    temporary = backup / ".server-backup.partial"
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    os.close(descriptor)
    try:
        print("Creating a full-server backup; Minecraft and its old launcher must stay stopped.", flush=True)
        run(["/usr/bin/tar", "--zstd", "--create", "--file", str(temporary), "--directory", str(server), "."], timeout=3600)
        stopped_and_ports_free(account.pw_uid, settings, server)
        require(props.read_bytes() == original, "server.properties changed during backup")
        run(["/usr/bin/zstd", "--test", "--quiet", str(temporary)], timeout=3600)
        names = run(["/usr/bin/tar", "--zstd", "--list", "--file", str(temporary)], timeout=3600).splitlines()
        require("./server.properties" in names, "backup archive is incomplete")
        os.chown(temporary, env_metadata.st_uid, env_metadata.st_gid)
        os.replace(temporary, archive)
        write_atomic(backup / "server.properties", original, 0o600, env_metadata.st_uid, env_metadata.st_gid)
        manifest = {"format": 1, "server_directory": str(server),
                    "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                    "archive_sha256": sha256_file(archive),
                    "properties_sha256": hashlib.sha256(original).hexdigest()}
        write_atomic(backup / "backup.json", (json.dumps(manifest, indent=2) + "\n").encode(),
                     0o600, env_metadata.st_uid, env_metadata.st_gid)
        print(f"Verified full-server backup: {archive}")
    finally:
        temporary.unlink(missing_ok=True)


def install(env_path, report_path):
    require(os.geteuid() == 0, "installer must run as root")
    settings, original_env, env_metadata = installation_settings(env_path)
    user = safe_text(settings["MC_SERVICE_USER"])
    require(re.fullmatch(r"[a-z_][a-z0-9_-]*\$?", user) is not None, "unsupported account name")
    account = pwd.getpwnam(user)
    require(account.pw_uid != 0, "the gateway must not run as root")
    group = grp.getgrgid(account.pw_gid).gr_name
    require(re.fullmatch(r"[a-z_][a-z0-9_-]*\$?", group) is not None, "unsupported group name")
    server, backup = backup_paths(settings)
    executable = launch_executable(server, settings["MC_LAUNCH_EXECUTABLE"])
    props = server / "server.properties"
    require(props.is_file() and not props.is_symlink(), "server.properties must be an ordinary file")
    metadata = props.stat()
    original = props.read_bytes()
    require(metadata.st_uid == account.pw_uid, "server.properties is not owned by the selected service account")
    require(not os.path.lexists(report_path), "installation report already exists; this is a first-install operation")
    require(report_path.parent.is_dir(), "installation report directory must already exist")
    for directory in ("/opt", "/etc", "/etc/systemd/system", "/etc/sudoers.d", "/var/lib"):
        st = Path(directory).stat()
        require(st.st_uid == 0 and not st.st_mode & 0o022, f"unsafe installation parent permissions: {directory}")
    roots = [Path("/opt/mcs-bootloader"), Path("/etc/mcs-bootloader"), Path("/var/lib/mcs-bootloader")]
    targets = [Path("/etc/systemd/system/mcs-bootloader.service"),
               Path("/etc/systemd/system/mcs-bootloader-backend.service"),
               Path("/etc/systemd/journald@mcs-bootloader.conf"), Path("/etc/sudoers.d/mcs-bootloader")]
    for path in roots + targets + [Path("/run/mcs-bootloader")]:
        require(not os.path.lexists(path), f"refusing existing installation path: {path}")
    for unit in ("mcs-bootloader.service", "mcs-bootloader-backend.service"):
        state = run(["/usr/bin/systemctl", "show", unit, "--property=LoadState", "--value"], allowed=(0, 1))
        require(state == "not-found", f"refusing existing or uninspectable unit: {unit} ({state})")
    stopped_and_ports_free(account.pw_uid, settings, server)
    verify_backup(backup, server, original)
    for path, permission in ((server, "-w"), (executable, "-x")):
        run(["/usr/sbin/runuser", "-u", user, "--", "/usr/bin/test", permission, str(path)])
    password = settings["MC_RCON_PASSWORD"]
    replacement = updated_properties(original, password, settings["MC_BACKEND_ADDRESS"], settings["MC_RCON_ADDRESS"])
    binary = (ROOT / "bin/mcs-bootloader").read_bytes()
    require(binary.startswith(b"\x7fELF"), "bin/mcs-bootloader is not a built Linux executable")
    created, directories, changed_props, reloaded, linked_env = [], [], False, False, False
    report = {"created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "status": "validating",
              "user": user, "server_directory": str(server), "backup_directory": str(backup),
              "configuration": "/etc/mcs-bootloader/.env", "project_env": str(env_path),
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
        add_file(roots[0] / "bin/mcs-bootloader", binary, 0o755)
        add_file(roots[1] / ".env", original_env, 0o640, account.pw_gid, env_metadata.st_uid)
        for destination in targets[:2]:
            unit = (ROOT / "deploy" / destination.name).read_text()
            require(unit.count("User=minecraft\n") == 1 and unit.count("Group=minecraft\n") == 1,
                    "service template account markers changed; review installer")
            unit = unit.replace("User=minecraft\n", f"User={user}\n").replace("Group=minecraft\n", f"Group={group}\n")
            add_file(destination, unit.encode())
        add_file(targets[2], (ROOT / "deploy/journald@mcs-bootloader.conf").read_bytes())
        sudoers = f"{user} ALL=(root) NOPASSWD: /usr/bin/systemctl start mcs-bootloader-backend.service\n".encode()
        with tempfile.TemporaryDirectory(prefix="mcs-bootloader-sudoers-") as temporary:
            staged = Path(temporary) / "rule"
            staged.write_bytes(sudoers)
            staged.chmod(0o440)
            run(["/usr/sbin/visudo", "-cf", str(staged)])
        add_file(targets[3], sudoers, 0o440)
        report["phase"] = "validate installed units and configuration"
        run(["/usr/bin/systemd-analyze", "verify", str(targets[0]), str(targets[1])])
        run(["/usr/sbin/runuser", "-u", user, "--", str(roots[0] / "bin/mcs-bootloader"),
             "check", "--config", str(roots[1] / ".env")])
        report["phase"] = "update stopped server properties"
        stopped_and_ports_free(account.pw_uid, settings, server)
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
        stopped_and_ports_free(account.pw_uid, settings, server)
        report["status"], report["phase"] = "validated", "before activation"
        save_report()
        report["phase"] = "activate gateway"
        run(["/usr/bin/systemctl", "enable", "--now", "mcs-bootloader.service"])
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
    parser.add_argument("--backup", action="store_true", help="create and verify a full backup of the stopped server")
    args = parser.parse_args()
    try:
        if args.backup:
            create_backup(args.env.absolute())
        else:
            install(args.env.absolute(), args.report.absolute())
    except Exception as failure:
        print(f"mcs-bootloader install: {failure}", file=sys.stderr)
        sys.exit(1)
