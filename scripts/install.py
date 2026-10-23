#!/usr/bin/env python3
"""First-install only. Validate a stopped server before activating the gateway.

Pre-activation failures restore properties and remove only this invocation's new
files. Activation failures retain the validated installation for inspection: a
backend might already be starting, so automatic world/config rollback is unsafe.
"""

from dataclasses import dataclass
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


def server_directory(settings):
    server = Path(safe_text(settings["MC_SERVER_DIRECTORY"]))
    require(server.is_absolute(), "MC_SERVER_DIRECTORY must be an absolute path")
    server = server.resolve(strict=True)
    require(server.is_dir() and server != Path("/"), "server path must be an existing non-root directory")
    return server


@dataclass(frozen=True)
class OriginalFile:
    path: Path
    data: bytes
    metadata: os.stat_result

    def restore(self):
        info = self.metadata
        write_atomic(self.path, self.data, stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid)
        os.utime(self.path, ns=(info.st_atime_ns, info.st_mtime_ns))


@dataclass(frozen=True)
class InstallationPlan:
    settings: dict
    source_env: OriginalFile
    properties: OriginalFile
    report_path: Path
    user: str
    account: pwd.struct_passwd
    group: str
    server: Path
    roots: list
    targets: list
    replacement: bytes
    binary: bytes


def installation_account(settings):
    user = safe_text(settings["MC_SERVICE_USER"])
    require(re.fullmatch(r"[a-z_][a-z0-9_-]*\$?", user) is not None, "unsupported account name")
    account = pwd.getpwnam(user)
    require(account.pw_uid != 0, "the gateway must not run as root")
    group = grp.getgrgid(account.pw_gid).gr_name
    require(re.fullmatch(r"[a-z_][a-z0-9_-]*\$?", group) is not None, "unsupported group name")
    return user, account, group


def original_properties(server, account):
    props = server / "server.properties"
    require(props.is_file() and not props.is_symlink(), "server.properties must be an ordinary file")
    metadata = props.stat()
    original = props.read_bytes()
    require(metadata.st_uid == account.pw_uid, "server.properties is not owned by the selected service account")
    return OriginalFile(props, original, metadata)


def installation_paths(report_path):
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
    return roots, targets


def prepare_installation(env_path, report_path, binary_path):
    require(os.geteuid() == 0, "installer must run as root")
    settings, raw, metadata = installation_settings(env_path)
    source_env = OriginalFile(env_path, raw, metadata)
    user, account, group = installation_account(settings)
    server = server_directory(settings)
    executable = launch_executable(server, settings["MC_LAUNCH_EXECUTABLE"])
    properties = original_properties(server, account)
    roots, targets = installation_paths(report_path)
    stopped_and_ports_free(account.pw_uid, settings, server)
    for path, permission in ((server, "-w"), (executable, "-x")):
        run(["/usr/sbin/runuser", "-u", user, "--", "/usr/bin/test", permission, str(path)])
    replacement = updated_properties(properties.data, settings["MC_RCON_PASSWORD"],
                                     settings["MC_BACKEND_ADDRESS"], settings["MC_RCON_ADDRESS"])
    binary = Path(binary_path).read_bytes()
    require(binary.startswith(b"\x7fELF"), "the installation source is not a Linux executable")
    return InstallationPlan(settings, source_env, properties, report_path, user, account,
                            group, server, roots, targets, replacement, binary)


def installation_report(plan):
    metadata = plan.properties.metadata
    return {"created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "status": "validating",
            "user": plan.user, "server_directory": str(plan.server),
            "configuration": "/etc/mcs-bootloader/.env", "project_env": str(plan.source_env.path),
            "operator_uid": plan.source_env.metadata.st_uid,
            "original_properties": {"sha256": hashlib.sha256(plan.properties.data).hexdigest(),
                                    "uid": metadata.st_uid, "gid": metadata.st_gid,
                                    "mode": oct(stat.S_IMODE(metadata.st_mode)),
                                    "atime_ns": metadata.st_atime_ns, "mtime_ns": metadata.st_mtime_ns},
            "binary_sha256": hashlib.sha256(plan.binary).hexdigest(), "phase": "install files"}


class InstallationTransaction:
    """Track only this invocation's tool files and reversible pre-activation edits."""

    def __init__(self, plan):
        self.plan = plan
        self.report = installation_report(plan)
        self.created = []
        self.directories = []
        self.changed_props = False
        self.reloaded = False
        self.linked_env = False

    def save_report(self):
        path = self.plan.report_path
        write_atomic(path, (json.dumps(self.report, indent=2) + "\n").encode(), 0o600,
                     path.parent.stat().st_uid, path.parent.stat().st_gid)

    def add_file(self, path, data, mode=0o644, gid=0, uid=0):
        require(not os.path.lexists(path), f"installation path appeared concurrently: {path}")
        write_atomic(path, data, mode, uid, gid)
        self.created.append(path)

    def create_directories(self):
        plan = self.plan
        for path, mode, uid, gid in ((plan.roots[0], 0o755, 0, 0), (plan.roots[0] / "bin", 0o755, 0, 0),
                                     (plan.roots[1], 0o2750, plan.source_env.metadata.st_uid, plan.account.pw_gid),
                                     (plan.roots[2], 0o700, plan.account.pw_uid, plan.account.pw_gid)):
            path.mkdir(mode=mode)
            self.directories.append(path)
            os.chown(path, uid, gid)
            os.chmod(path, mode)

    def install_service_files(self):
        plan = self.plan
        for destination in plan.targets[:2]:
            unit = (ROOT / "deploy" / destination.name).read_text()
            require(unit.count("User=minecraft\n") == 1 and unit.count("Group=minecraft\n") == 1,
                    "service template account markers changed; review installer")
            unit = unit.replace("User=minecraft\n", f"User={plan.user}\n").replace("Group=minecraft\n", f"Group={plan.group}\n")
            self.add_file(destination, unit.encode())
        self.add_file(plan.targets[2], (ROOT / "deploy/journald@mcs-bootloader.conf").read_bytes())
        sudoers = f"{plan.user} ALL=(root) NOPASSWD: /usr/bin/systemctl start mcs-bootloader-backend.service\n".encode()
        with tempfile.TemporaryDirectory(prefix="mcs-bootloader-sudoers-") as temporary:
            staged = Path(temporary) / "rule"
            staged.write_bytes(sudoers)
            staged.chmod(0o440)
            run(["/usr/sbin/visudo", "-cf", str(staged)])
        self.add_file(plan.targets[3], sudoers, 0o440)

    def install_files(self):
        plan = self.plan
        self.create_directories()
        self.add_file(plan.roots[0] / "bin/mcs-bootloader", plan.binary, 0o755)
        self.add_file(plan.roots[1] / ".env", plan.source_env.data, 0o640,
                      plan.account.pw_gid, plan.source_env.metadata.st_uid)
        self.install_service_files()

    def validate_installed_files(self):
        plan = self.plan
        self.report["phase"] = "validate installed units and configuration"
        run(["/usr/bin/systemd-analyze", "verify", str(plan.targets[0]), str(plan.targets[1])])
        run(["/usr/sbin/runuser", "-u", plan.user, "--", str(plan.roots[0] / "bin/mcs-bootloader"),
             "check", "--config", str(plan.roots[1] / ".env")])

    def update_properties(self):
        plan = self.plan
        self.report["phase"] = "update stopped server properties"
        stopped_and_ports_free(plan.account.pw_uid, plan.settings, plan.server)
        require(plan.properties.path.read_bytes() == plan.properties.data, "server.properties changed during validation")
        metadata = plan.properties.metadata
        write_atomic(plan.properties.path, plan.replacement, 0o640, metadata.st_uid, metadata.st_gid)
        self.changed_props = True
        self.report["properties_sha256"] = hashlib.sha256(plan.replacement).hexdigest()
        self.report["installed_files"] = [str(path) for path in self.created]

    def link_configuration(self):
        plan = self.plan
        self.report["phase"] = "link shared configuration"
        link_shared_env(plan.source_env.path, plan.roots[1] / ".env",
                        plan.source_env.data, plan.source_env.metadata)
        self.linked_env = True

    def activate(self):
        plan = self.plan
        self.report["phase"] = "reload validated units"
        run(["/usr/bin/systemctl", "daemon-reload"])
        self.reloaded = True
        stopped_and_ports_free(plan.account.pw_uid, plan.settings, plan.server)
        self.report["status"], self.report["phase"] = "validated", "before activation"
        self.save_report()
        # A failed enable/start may already have started Minecraft. Keep the
        # validated configuration from this point onward, including report errors.
        self.report["phase"] = "activate gateway"
        run(["/usr/bin/systemctl", "enable", "--now", "mcs-bootloader.service"])
        self.report["status"], self.report["phase"] = "installed", "complete"
        self.save_report()

    def restore_original(self, original, label, errors):
        try:
            original.restore()
        except Exception as rollback:
            errors.append(f"{label}: {rollback}")

    def rollback(self):
        errors = []
        if self.linked_env:
            self.restore_original(self.plan.source_env, "restore project .env", errors)
        if self.changed_props:
            self.restore_original(self.plan.properties, "properties", errors)
        for path in reversed(self.created):
            try:
                path.unlink()
            except OSError as rollback:
                errors.append(f"remove {path}: {rollback}")
        for path in reversed(self.directories):
            try:
                path.rmdir()
            except OSError as rollback:
                errors.append(f"remove empty {path}: {rollback}")
        if self.reloaded:
            try:
                run(["/usr/bin/systemctl", "daemon-reload"])
            except Exception as rollback:
                errors.append(f"reload after rollback: {rollback}")
        return errors

    def fail(self, error):
        phase = self.report["phase"]
        if phase != "activate gateway" and self.report["status"] != "installed":
            notes = self.rollback()
        else:
            notes = ["activation began; validated files retained for safe operator inspection"]
        self.report.update(status="failed", phase=phase, error=str(error), rollback_errors=notes)
        try:
            self.save_report()
        except Exception as report_error:
            notes.append(f"report: {report_error}")
        raise RuntimeError(f"installation failed during {phase}: {error}; rollback notes: {notes}") from error

    def run(self):
        try:
            self.install_files()
            self.validate_installed_files()
            self.update_properties()
            self.link_configuration()
            self.activate()
            result = {"status": "installed", "gateway": "enabled", "backend": "not explicitly started or enabled",
                      "report": str(self.plan.report_path), "configuration": str(self.plan.roots[1] / ".env")}
            print(json.dumps(result))
            return result
        except Exception as error:
            self.fail(error)


def install(env_path, report_path, *, binary_path):
    plan = prepare_installation(env_path, report_path, binary_path)
    return InstallationTransaction(plan).run()
