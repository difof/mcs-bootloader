"""Remove a verified bootloader installation without deleting Minecraft data.

Removal requires an explicit --yes after a read-only plan. Unknown unit
overrides and modified sudo/journal configuration are refused, never guessed.
The canonical .env, state, journals, backups and native server settings remain.
"""

import fcntl
import json
import os
from pathlib import Path
import pwd
import stat

import service_config
from env_config import MANAGE_DEFAULTS, RUNTIME_DEFAULTS, read_env
from manage import atomic_write, settings

SUDOERS_DIRECTORY = Path("/etc/sudoers.d")
JOURNAL_DIRECTORY = Path("/etc/systemd")

def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def unit_properties(unit, names):
    raw = service_config.run([service_config.SYSTEMCTL, "show", unit,
                              *["--property=" + name for name in names]])
    properties = {}
    for line in raw.splitlines():
        name, separator, value = line.partition("=")
        require(separator and name in names and name not in properties,
                "systemd returned an invalid service description")
        properties[name] = value
    require(set(properties) == set(names), "systemd returned an incomplete service description")
    return properties


def backend_stopped(cfg):
    state = unit_properties(cfg["MC_BACKEND_SERVICE"], ("ActiveState", "MainPID"))
    require(state["ActiveState"] in ("inactive", "failed") and state["MainPID"] == "0",
            "Minecraft must be stopped before removal; run mcs-bootloader stop and wait for saving to finish")
    # An old tmux/restart launcher may be outside the unit. Refuse any Java
    # belonging to this service account; no process is signaled here.
    uid = pwd.getpwnam(cfg["MC_SERVICE_USER"]).pw_uid
    for process in Path("/proc").iterdir():
        if not process.name.isdigit():
            continue
        try:
            if process.stat().st_uid == uid:
                name = (process / "comm").read_text().strip()
                executable = Path(os.readlink(process / "exe")).name
                require(name != "java" and executable != "java" and name != "startserver.sh",
                        "a Java process or restart launcher still runs as the Minecraft account; removal was refused")
        except (FileNotFoundError, ProcessLookupError):
            continue


def backend_fragment(cfg):
    names = ("Id", "LoadState", "FragmentPath", "NeedDaemonReload")
    properties = unit_properties(cfg["MC_BACKEND_SERVICE"], names)
    require(properties["Id"] == cfg["MC_BACKEND_SERVICE"] and properties["LoadState"] == "loaded",
            "configured backend is missing, masked, or an alias")
    require(properties["NeedDaemonReload"] == "no", "backend unit has pending edits; review and reload them before removal")
    path = Path(properties["FragmentPath"])
    require(path.is_absolute(), "backend unit has no installed fragment")
    service_config.secure_directory(path.parent)
    raw, _ = service_config.read_regular(path, owned=True)
    # Exact generated command lines make an unrelated configured service fail
    # closed. Additional administrative Exec*= directives are also refused.
    commands = [line.strip() for line in raw.decode().splitlines()
                if line.strip().startswith("Exec")]
    expected = [f"ExecStart={cfg['MC_BINARY']} backend-start --config {cfg['MC_CONFIG']}",
                f"ExecStop={cfg['MC_BINARY']} backend-stop --config {cfg['MC_CONFIG']} --pid=${{MAINPID}}"]
    require(commands == expected, "backend service is not a recognized bootloader unit")
    return path


def plan(env_path):
    require(os.geteuid() == 0, "removal inspection requires root")
    canonical = Path(env_path).resolve(strict=True)
    service_config.read_regular(canonical, private=True, limit=65536)
    values = {**RUNTIME_DEFAULTS, **MANAGE_DEFAULTS, **read_env(canonical)}
    cfg = settings(values)
    require(canonical == Path(cfg["MC_CONFIG"]).resolve(strict=True),
            "removal requires the canonical installed .env")
    require(Path(cfg["MC_BINARY"]) == Path(MANAGE_DEFAULTS["MC_BINARY"]),
            "automatic removal supports the bundled /opt/mcs-bootloader installation only")
    before = service_config.inspect_service(cfg)
    require(before["NeedDaemonReload"] == "no", "gateway unit has pending edits; review and reload them before removal")
    gateway = Path(before["FragmentPath"])
    backend = backend_fragment(cfg)
    expected_parent = service_config.SYSTEMD_DIRECTORY
    require(gateway.parent == expected_parent and backend.parent == expected_parent,
            "automatic removal requires units directly installed in /etc/systemd/system")
    backend_stopped(cfg)
    files = [gateway, backend]
    for unit in (cfg["MC_GATEWAY_SERVICE"], cfg["MC_BACKEND_SERVICE"]):
        directory = expected_parent / (unit + ".d")
        if os.path.lexists(directory):
            service_config.secure_directory(directory)
            for path in directory.iterdir():
                require(unit == cfg["MC_GATEWAY_SERVICE"] and path.name == service_config.DROP_IN_NAME,
                        "service has operator overrides; move/review them before automatic removal")
                raw, _ = service_config.read_regular(path, owned=True)
                require(raw == service_config.DROP_IN, "gateway runtime drop-in has unrecognized edits")
                files.append(path)

    sudoers = SUDOERS_DIRECTORY / "mcs-bootloader"
    if os.path.lexists(sudoers):
        service_config.secure_directory(sudoers.parent)
        raw, _ = service_config.read_regular(sudoers, owned=True)
        expected = f"{cfg['MC_SERVICE_USER']} ALL=(root) NOPASSWD: /usr/bin/systemctl start {cfg['MC_BACKEND_SERVICE']}\n".encode()
        require(raw == expected, "bootloader sudo rule contains unrecognized edits")
        files.append(sudoers)

    journal = JOURNAL_DIRECTORY / ("journald@" + cfg["MC_LOG_NAMESPACE"] + ".conf")
    if os.path.lexists(journal):
        service_config.secure_directory(journal.parent)
        raw, _ = service_config.read_regular(journal, owned=True)
        template = (Path(__file__).resolve().parent.parent / "deploy/journald@mcs-bootloader.conf").read_bytes()
        require(raw == template, "journal configuration contains unrecognized edits")
        files.append(journal)

    binary = Path(cfg["MC_BINARY"])
    files.append(binary)
    previous = binary.with_name(binary.name + ".previous")
    if os.path.lexists(previous):
        raw, _ = service_config.read_regular(previous, owned=True, limit=128 * 1024 * 1024)
        require(raw.startswith(b"\x7fELF"), "previous executable is not a recognized binary")
        files.append(previous)
    records = []
    for path in files:
        raw, metadata = service_config.read_regular(path, owned=True, limit=128 * 1024 * 1024)
        records.append((path, raw, metadata))
    return cfg, records, {"ok": True, "action": "remove", "applied": False,
                         "files": [str(path) for path in files],
                         "preserved": [str(canonical), values["MC_SERVER_DIRECTORY"],
                                       values["MC_BACKUP_DIRECTORY"], values["MC_STATE_DIRECTORY"],
                                       "Minecraft network settings", "system journal history"],
                         "message": "Removal plan only. Run remove --yes to remove these files after Minecraft is stopped."}


def remove(env_path, *, yes=False):
    cfg, records, result = plan(env_path)
    if not yes:
        return result
    binary = Path(cfg["MC_BINARY"])
    lock_path = binary.with_name("." + binary.name + ".lock")
    descriptor = os.open(lock_path, os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as lock:
        info = os.fstat(lock.fileno())
        require(stat.S_ISREG(info.st_mode) and service_config._root_owned(info) and not info.st_mode & 0o077,
                "unsafe installation lock")
        fcntl.flock(lock, fcntl.LOCK_EX)
        # Revalidate the complete plan after acquiring the deployment lock.
        cfg, records, result = plan(env_path)
        previous = unit_properties(cfg["MC_GATEWAY_SERVICE"], ("ActiveState", "UnitFileState"))
        require(previous["ActiveState"] in ("active", "activating", "inactive", "failed"),
                "gateway state is transitioning or unknown; retry removal after it settles")
        require(previous["UnitFileState"] in ("enabled", "enabled-runtime", "disabled"),
                "gateway has an unsupported enable state; review it before removal")
        removed, stopped, disabled = [], False, False
        try:
            stopped = True
            service_config.run([service_config.SYSTEMCTL, "stop", cfg["MC_GATEWAY_SERVICE"]])
            # A wake may have started while preflight ran. Restore the gateway
            # on failure so its private stop/control path remains available.
            backend_stopped(cfg)
            disabled = True
            service_config.run([service_config.SYSTEMCTL, "disable", cfg["MC_GATEWAY_SERVICE"]])
            for path, raw, metadata in records:
                current, info = service_config.read_regular(path, owned=True, limit=128 * 1024 * 1024)
                require(current == raw and info.st_ino == metadata.st_ino and info.st_dev == metadata.st_dev,
                        "an installation file changed during removal; removal was rolled back")
                path.unlink()
                removed.append((path, raw, metadata))
            service_config.run([service_config.SYSTEMCTL, "daemon-reload"])
        except Exception as original:
            recovery_errors = []
            for path, raw, metadata in reversed(removed):
                try:
                    require(not os.path.lexists(path), "restore destination appeared")
                    atomic_write(path, raw, mode=stat.S_IMODE(metadata.st_mode), uid=metadata.st_uid, gid=metadata.st_gid)
                except Exception as error:
                    recovery_errors.append(f"restore {path}: {error}")
            if removed:
                try:
                    service_config.run([service_config.SYSTEMCTL, "daemon-reload"])
                except Exception as error:
                    recovery_errors.append(f"reload restored units: {error}")
            if disabled and previous["UnitFileState"] in ("enabled", "enabled-runtime"):
                try:
                    argv = [service_config.SYSTEMCTL, "enable"]
                    if previous["UnitFileState"] == "enabled-runtime":
                        argv.append("--runtime")
                    service_config.run(argv + [cfg["MC_GATEWAY_SERVICE"]])
                except Exception as error:
                    recovery_errors.append(f"restore gateway enable state: {error}")
            if stopped and previous["ActiveState"] in ("active", "activating"):
                try:
                    service_config.run([service_config.SYSTEMCTL, "start", cfg["MC_GATEWAY_SERVICE"]])
                except Exception as error:
                    recovery_errors.append(f"restore running gateway: {error}")
            if recovery_errors:
                raise RuntimeError(f"removal failed: {original}; recovery needs operator attention: {recovery_errors}") from original
            raise RuntimeError(f"removal failed; prior gateway service and removed files were restored: {original}") from original
    # Only known empty installation directories can disappear. Runtime state
    # and config are intentionally retained; no recursive delete is used.
    for unit in (cfg["MC_GATEWAY_SERVICE"], cfg["MC_BACKEND_SERVICE"]):
        directory = service_config.SYSTEMD_DIRECTORY / (unit + ".d")
        try:
            directory.rmdir()
        except (FileNotFoundError, OSError):
            pass
    result.update(applied=True, message="Gateway and owned installation files removed. Minecraft data, configuration, backups and native network settings were preserved.")
    return result
