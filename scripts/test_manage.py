import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import manage


CURRENT = {
    "listen": ":25565",
    "backend_address": "127.0.0.1:25566",
    "rcon_address": "127.0.0.1:25575",
    "rcon_password_file": "/etc/minecraft-gateway/rcon.password",
    "backend_service": "minecraft-backend.service",
    "state_directory": "/var/lib/minecraft-gateway",
    "admin_socket": "/run/minecraft-gateway/control.sock",
    "idle_timeout": "1h",
}


def encoded(value):
    return (json.dumps(value, indent=2) + "\n").encode()


def fingerprint(path):
    info = path.stat()
    return (path.read_bytes(), stat.S_IMODE(info.st_mode), info.st_uid,
            info.st_gid, info.st_mtime_ns)


class ExecRequested(Exception):
    """Stand in for execvp, which does not return on a successful launch."""

    def __init__(self, program, argv):
        self.program = program
        self.argv = argv


class InstallationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.destination = self.root / "config.json"
        self.previous = self.root / "config.json.previous"
        self.source = self.root / "candidate.json"
        self.destination.write_bytes(encoded(CURRENT))
        self.destination.chmod(0o600)
        self.previous.write_bytes(b"earlier backup must survive failed checks\n")
        self.previous.chmod(0o600)
        self.source.write_bytes(encoded({**CURRENT, "idle_timeout": "2h"}))
        self.digest = hashlib.sha256(self.destination.read_bytes()).hexdigest()

        # Exercise the real file replacement and modes in a temporary directory.
        # Mock only the privilege boundary so these tests also run as a normal
        # developer account; fchown arguments are checked separately below.
        original_lstat = Path.lstat
        destination = self.destination

        def installed_metadata(path):
            info = original_lstat(path)
            if path == destination:
                fields = list(info)
                fields[4] = 0
                return os.stat_result(fields)
            return info

        for patcher in (
            mock.patch.object(manage.os, "geteuid", return_value=0),
            mock.patch.object(Path, "lstat", installed_metadata),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        owner_patch = mock.patch.object(manage.os, "fchown")
        self.fchown = owner_patch.start()
        self.addCleanup(owner_patch.stop)
        subprocess_patch = mock.patch.object(
            manage.subprocess, "run",
            side_effect=AssertionError("unexpected external process in installation test"),
        )
        self.process = subprocess_patch.start()
        self.addCleanup(subprocess_patch.stop)
        exec_patch = mock.patch.object(
            manage.os, "execvp", side_effect=AssertionError("unexpected exec in installation test"),
        )
        self.execute = exec_patch.start()
        self.addCleanup(exec_patch.stop)

    def apply(self, **options):
        manage.replace_installed(self.source, self.destination, self.digest,
                                 kind="config", binary="/opt/minecraft-gateway/bin/mc-gateway",
                                 **options)

    def test_invalid_configuration_keeps_current_and_backup_unchanged(self):
        cases = (
            ("malformed JSON", b"{invalid JSON", False),
            ("topology migration", encoded({**CURRENT, "rcon_address": "127.0.0.1:29999"}), False),
            ("runtime validation failure", encoded({**CURRENT, "idle_timeout": "nonsense"}), True),
        )
        before = fingerprint(self.destination), fingerprint(self.previous)
        for name, candidate, should_validate in cases:
            with self.subTest(name=name):
                self.source.write_bytes(candidate)
                self.process.reset_mock()
                self.process.side_effect = None
                self.process.return_value = subprocess.CompletedProcess(
                    [], 1, stdout="", stderr="invalid idle timeout",
                )
                with self.assertRaises((ValueError, RuntimeError)):
                    self.apply()
                self.assertEqual(before, (fingerprint(self.destination), fingerprint(self.previous)))
                self.assertEqual(self.process.call_count, int(should_validate))
                self.fchown.assert_not_called()
                self.execute.assert_not_called()

    def test_apply_installs_exact_validated_bytes_atomically_and_preserves_ownership(self):
        original = self.destination.read_bytes()
        original_owner = self.destination.stat().st_uid, self.destination.stat().st_gid
        # Deliberately retain noncanonical JSON whitespace: the saved bytes must
        # be the exact candidate that passed the final authoritative check.
        candidate = b" \n" + encoded({**CURRENT, "idle_timeout": "2h"}) + b"\t\n"
        self.source.write_bytes(candidate)
        validated_paths = []

        def validate(argv, **options):
            self.assertEqual(argv[:3], ["/opt/minecraft-gateway/bin/mc-gateway", "check", "--config"])
            self.assertEqual(options, {"text": True, "capture_output": True, "check": False})
            staged = Path(argv[3])
            self.assertNotEqual(staged, self.source)
            self.assertEqual(staged.read_bytes(), candidate)
            validated_paths.append(staged)
            # The mutable input can change after validation has started. The
            # installed bytes must still be the validated snapshot above.
            self.source.write_bytes(encoded({**CURRENT, "idle_timeout": "9h"}))
            return subprocess.CompletedProcess(argv, 0, stdout="configuration valid\n", stderr="")

        self.process.side_effect = validate
        real_replace = os.replace
        with mock.patch.object(manage.os, "replace", wraps=real_replace) as replace:
            self.apply()
        self.assertEqual(self.destination.read_bytes(), candidate)
        self.assertEqual(self.previous.read_bytes(), original)
        for path in (self.destination, self.previous):
            info = path.stat()
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o640)
            self.assertEqual((info.st_uid, info.st_gid), original_owner)
        self.assertEqual(self.fchown.call_count, 2)
        for call in self.fchown.call_args_list:
            self.assertEqual(call.args[1:], (0, original_owner[1]))
        self.assertEqual([Path(call.args[1]) for call in replace.call_args_list],
                         [self.previous, self.destination])
        for call in replace.call_args_list:
            self.assertEqual(Path(call.args[0]).parent, self.destination.parent)
        self.assertEqual(len(validated_paths), 1)
        self.assertFalse(validated_paths[0].exists())
        self.execute.assert_not_called()

    def test_expected_hash_mismatch_aborts_before_validation_or_writes(self):
        before = fingerprint(self.destination), fingerprint(self.previous)
        with self.assertRaisesRegex(RuntimeError, "changed during validation"):
            manage.replace_installed(self.source, self.destination, "0" * 64,
                                     kind="config", binary="/opt/minecraft-gateway/bin/mc-gateway")
        self.assertEqual(before, (fingerprint(self.destination), fingerprint(self.previous)))
        self.process.assert_not_called()
        self.fchown.assert_not_called()
        self.execute.assert_not_called()

    def test_binary_install_only_replaces_files_and_does_not_operate_services(self):
        old = b"\x7fELFexisting executable bytes"
        new = b"\x7fELFnew executable bytes"
        self.destination.write_bytes(old)
        self.source.write_bytes(new)
        checked = []

        def validate(argv, **options):
            snapshot = Path(argv[0])
            self.assertNotEqual(snapshot, self.source)
            self.assertEqual(snapshot.read_bytes(), new)
            self.assertEqual(stat.S_IMODE(snapshot.stat().st_mode), 0o700)
            self.assertEqual(argv[1:], ["check", "--config", "/etc/minecraft-gateway/config.json"])
            self.assertEqual(options, {"text": True, "capture_output": True, "check": False})
            checked.append(snapshot)
            self.source.write_bytes(b"\x7fELFchanged after validation started")
            return subprocess.CompletedProcess(argv, 0, stdout="configuration valid\n", stderr="")

        self.process.side_effect = validate
        manage.replace_installed(self.source, self.destination, hashlib.sha256(old).hexdigest(),
                                 kind="binary", config="/etc/minecraft-gateway/config.json")
        self.assertEqual(self.destination.read_bytes(), new)
        self.assertEqual(self.previous.read_bytes(), old)
        self.assertEqual(stat.S_IMODE(self.destination.stat().st_mode), 0o755)
        self.assertEqual(stat.S_IMODE(self.previous.stat().st_mode), 0o755)
        self.assertEqual(self.process.call_count, 1)
        self.assertEqual(len(checked), 1)
        self.assertFalse(checked[0].exists())
        self.execute.assert_not_called()


class OperatorTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.env_file = self.root / ".env"
        self.env_file.write_text("# test settings are supplied by the read_env fixture\n")
        self.values = {
            "MC_BINARY": str(self.root / "installed executable"),
            "MC_CONFIG": str(self.root / "installed settings.json"),
            "MC_SERVICE_USER": "compute",
            "MC_COMMAND_TIMEOUT": "45s",
            # Until explicitly applied, an edited .env socket must not divert
            # operator requests away from the installed daemon's socket.
            "MC_ADMIN_SOCKET": "/run/proposed-but-not-installed/control.sock",
        }
        for patcher in (
            mock.patch.object(manage, "ROOT", self.root),
            mock.patch.object(manage, "ENV_FILE", self.env_file),
            mock.patch.object(manage, "read_env", return_value=self.values),
            mock.patch.object(manage, "installed_config", return_value=(dict(CURRENT), encoded(CURRENT))),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        process_patch = mock.patch.object(
            manage.subprocess, "run",
            side_effect=AssertionError("unexpected external process in operator test"),
        )
        self.process = process_patch.start()
        self.addCleanup(process_patch.stop)
        def exec_requested(program, argv):
            raise ExecRequested(program, argv)

        execute_patch = mock.patch.object(manage.os, "execvp", side_effect=exec_requested)
        self.execute = execute_patch.start()
        self.addCleanup(execute_patch.stop)

    def test_fixed_installed_topology_cannot_drift_through_env_apply(self):
        replacements = {
            "MC_BACKEND_ADDRESS": "127.0.0.1:29998",
            "MC_RCON_ADDRESS": "127.0.0.1:29999",
            "MC_RCON_PASSWORD_FILE": "/etc/different/rcon.password",
            "MC_BACKEND_SERVICE": "different-backend.service",
            "MC_STATE_DIRECTORY": "/var/lib/different-gateway",
            "MC_ADMIN_SOCKET": "/run/different-gateway/control.sock",
        }
        for name, value in replacements.items():
            with self.subTest(setting=name), self.assertRaisesRegex(RuntimeError, "cannot migrate"):
                manage.candidate_config({name: value}, CURRENT)
        proposed = manage.candidate_config({"MC_IDLE_TIMEOUT": "2h"}, CURRENT)
        self.assertEqual(proposed, {**CURRENT, "idle_timeout": "2h"})
        self.assertEqual(CURRENT["idle_timeout"], "1h")

    def test_start_stop_status_and_version_run_as_service_user(self):
        for action, operation in (("start", "wake"), ("stop", "sleep"),
                                  ("status", "status"), ("version", "version")):
            with self.subTest(action=action):
                with self.assertRaises(ExecRequested) as launched:
                    manage.operate(action, [])
                expected = ["sudo", "-n", "-u", "compute", "--", self.values["MC_BINARY"], operation]
                if operation != "version":
                    expected += ["--socket", CURRENT["admin_socket"]]
                self.assertEqual(launched.exception.program, "sudo")
                self.assertEqual(launched.exception.argv, expected)
        self.process.assert_not_called()

    def test_command_forwards_literal_arguments_with_config_timeout_and_separator(self):
        sentinel = self.root / "shell-must-not-run"
        text = json.dumps({"text": f"quotes, $HOME, $(touch {sentinel}), `id`, ; and spaces"})
        arguments = ["tellraw", "@a", text, "--not-a-gateway-flag"]
        with self.assertRaises(ExecRequested) as launched:
            manage.operate("command", arguments)
        self.assertEqual(launched.exception.program, "sudo")
        self.assertEqual(launched.exception.argv, [
            "sudo", "-n", "-u", "compute", "--", self.values["MC_BINARY"],
            "command", "--config", self.values["MC_CONFIG"], "--timeout", "45s", "--",
        ] + arguments)
        self.assertFalse(sentinel.exists())
        self.process.assert_not_called()

    def test_deploy_validates_and_installs_without_start_stop_or_restart(self):
        source = self.root / "bin" / "mc-gateway"
        source.parent.mkdir()
        source.write_bytes(b"\x7fELFnew candidate")
        destination = Path(self.values["MC_BINARY"])
        destination.write_bytes(b"\x7fELFcurrently installed")
        config = Path(self.values["MC_CONFIG"])
        config.write_bytes(encoded(CURRENT))
        before = fingerprint(destination), fingerprint(config)
        digest = hashlib.sha256(destination.read_bytes()).hexdigest()
        observed = []

        def subprocess_result(argv, **options):
            observed.append(argv)
            self.assertNotIn("shell", options)
            stdout = digest + "  " + str(destination) + "\n" if "sha256sum" in argv else ""
            return subprocess.CompletedProcess(argv, 0, stdout=stdout, stderr="")

        self.process.side_effect = subprocess_result
        with contextlib.redirect_stdout(io.StringIO()) as output:
            manage.operate("deploy", [])
        prefix = ["sudo", "-n", "--"]
        self.assertEqual(observed, [
            prefix + [str(source), "check", "--config", str(config)],
            prefix + ["sha256sum", "--", str(destination)],
            prefix + [sys.executable, str(Path(manage.__file__).resolve()), "_deploy",
                      str(source), str(destination), digest, str(config)],
        ])
        self.assertEqual(before, (fingerprint(destination), fingerprint(config)))
        self.assertIn("left running", output.getvalue())
        self.execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
