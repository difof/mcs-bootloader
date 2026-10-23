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

import env_config
import manage


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
        self.destination = self.root / "mcs-bootloader"
        self.previous = self.root / "mcs-bootloader.previous"
        self.source = self.root / "candidate"
        self.config = self.root / ".env"
        self.config.write_text("MC_RCON_PASSWORD='synthetic-private-password'\n")
        self.config.chmod(0o640)
        self.destination.write_bytes(b"\x7fELFexisting executable bytes")
        self.destination.chmod(0o755)
        self.previous.write_bytes(b"earlier backup must survive failed checks\n")
        self.previous.chmod(0o755)
        self.source.write_bytes(b"\x7fELFnew executable bytes")
        self.digest = hashlib.sha256(self.destination.read_bytes()).hexdigest()

        # Exercise real replacement and modes in a temporary directory. Mock
        # only the privilege boundary so tests also run as a developer account.
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

    def deploy(self):
        manage.replace_installed(self.source, self.destination, self.digest,
                                 config=str(self.config))

    def test_invalid_executable_keeps_current_and_backup_unchanged(self):
        before = fingerprint(self.destination), fingerprint(self.previous)
        for candidate in (b"", b"not an executable"):
            with self.subTest(candidate=candidate):
                self.source.write_bytes(candidate)
                with self.assertRaisesRegex(RuntimeError, "ELF"):
                    self.deploy()
                self.assertEqual(before, (fingerprint(self.destination), fingerprint(self.previous)))
        self.process.assert_not_called()
        self.fchown.assert_not_called()

    def test_runtime_validation_failure_keeps_current_and_backup_unchanged(self):
        before = fingerprint(self.destination), fingerprint(self.previous)
        self.process.side_effect = None
        self.process.return_value = subprocess.CompletedProcess(
            [], 1, stdout="", stderr="invalid idle timeout",
        )
        with self.assertRaisesRegex(RuntimeError, "invalid idle timeout"):
            self.deploy()
        self.assertEqual(before, (fingerprint(self.destination), fingerprint(self.previous)))
        self.assertFalse(list(self.root.glob(".mcs-bootloader-check-*")))
        self.fchown.assert_not_called()

    def test_expected_hash_mismatch_aborts_before_validation_or_writes(self):
        before = fingerprint(self.destination), fingerprint(self.previous)
        with self.assertRaisesRegex(RuntimeError, "changed during validation"):
            manage.replace_installed(self.source, self.destination, "0" * 64,
                                     config=str(self.config))
        self.assertEqual(before, (fingerprint(self.destination), fingerprint(self.previous)))
        self.process.assert_not_called()
        self.fchown.assert_not_called()

    def test_binary_install_uses_exact_validated_bytes_and_preserves_ownership(self):
        old, new = self.destination.read_bytes(), self.source.read_bytes()
        original_owner = self.destination.stat().st_uid, self.destination.stat().st_gid
        config_before = fingerprint(self.config)
        checked = []

        def validate(argv, **options):
            snapshot = Path(argv[0])
            self.assertNotEqual(snapshot, self.source)
            self.assertEqual(snapshot.read_bytes(), new)
            self.assertEqual(stat.S_IMODE(snapshot.stat().st_mode), 0o700)
            self.assertEqual(argv[1:], ["check", "--config", str(self.config)])
            self.assertEqual(options, {"text": True, "capture_output": True, "check": False})
            checked.append(snapshot)
            self.source.write_bytes(b"\x7fELFchanged after validation started")
            return subprocess.CompletedProcess(argv, 0, stdout="configuration valid\n", stderr="")

        self.process.side_effect = validate
        real_replace = os.replace
        with mock.patch.object(manage.os, "replace", wraps=real_replace) as replace:
            self.deploy()
        self.assertEqual(self.destination.read_bytes(), new)
        self.assertEqual(self.previous.read_bytes(), old)
        self.assertEqual(fingerprint(self.config), config_before)
        for path in (self.destination, self.previous):
            info = path.stat()
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o755)
            self.assertEqual((info.st_uid, info.st_gid), original_owner)
        self.assertEqual(self.fchown.call_count, 2)
        for call in self.fchown.call_args_list:
            self.assertEqual(call.args[1:], (0, original_owner[1]))
        self.assertEqual([Path(call.args[1]) for call in replace.call_args_list],
                         [self.previous, self.destination])
        self.assertEqual(self.process.call_count, 1)
        self.assertEqual(len(checked), 1)
        self.assertFalse(checked[0].exists())


class OperatorTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.root = self.directory / "project"
        self.root.mkdir()
        canonical_directory = self.directory / "canonical"
        canonical_directory.mkdir()
        self.canonical = canonical_directory / ".env"
        self.env_file = self.root / ".env"
        self.env_file.symlink_to(self.canonical)
        self.secret = "SYNTHETIC_DO_NOT_PRINT_OR_PASS_IN_ARGV"
        self.values = {
            "MC_BINARY": str(self.directory / "installed executable"),
            "MC_CONFIG": str(self.canonical),
            "MC_SERVICE_USER": "gamesvc",
            "MC_COMMAND_TIMEOUT": "45s",
            "MC_ADMIN_SOCKET": "/run/mcs-bootloader/control.sock",
            "MC_BACKEND_SERVICE": "minecraft-backend.service",
            "MC_RCON_PASSWORD": self.secret,
            "MC_IDLE_TIMEOUT": "2h",
        }
        self.canonical.write_text(env_config.serialize_env(self.values))
        self.canonical.chmod(0o640)
        for patcher in (
            mock.patch.object(manage, "ROOT", self.root),
            mock.patch.object(manage, "ENV_FILE", self.env_file),
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

    def test_start_stop_status_and_version_run_as_service_user(self):
        for action, operation in (("start", "wake"), ("stop", "sleep"),
                                  ("status", "status"), ("config-live", "config"),
                                  ("version", "version")):
            with self.subTest(action=action):
                with self.assertRaises(ExecRequested) as launched:
                    manage.operate(action, [])
                expected = ["sudo", "-n", "-u", "gamesvc", "--", self.values["MC_BINARY"], operation]
                if operation != "version":
                    expected += ["--socket", self.values["MC_ADMIN_SOCKET"]]
                self.assertEqual(launched.exception.program, "sudo")
                self.assertEqual(launched.exception.argv, expected)
        self.process.assert_not_called()

    def test_command_forwards_literal_arguments_to_socket_without_credentials(self):
        sentinel = self.root / "shell-must-not-run"
        text = json.dumps({"text": f"{{{{.FOO}}}}, $HOME, $(touch {sentinel}), `id`, ; and spaces"})
        arguments = ["tellraw", "@a", text, "--not-a-gateway-flag"]
        with self.assertRaises(ExecRequested) as launched:
            manage.operate("command", arguments)
        self.assertEqual(launched.exception.program, "sudo")
        self.assertEqual(launched.exception.argv, [
            "sudo", "-n", "-u", "gamesvc", "--", self.values["MC_BINARY"],
            "command", "--socket", self.values["MC_ADMIN_SOCKET"], "--timeout", "45s", "--",
        ] + arguments)
        self.assertNotIn(self.secret, " ".join(launched.exception.argv))
        self.assertFalse(sentinel.exists())
        self.process.assert_not_called()

    def test_check_and_apply_use_canonical_env_without_copies_or_restarts(self):
        before = fingerprint(self.canonical)
        self.process.side_effect = None
        self.process.return_value = subprocess.CompletedProcess([], 0, stdout="valid\n", stderr="")
        for action in ("config-check", "config-apply", "reload"):
            with self.subTest(action=action), contextlib.redirect_stdout(io.StringIO()):
                self.process.reset_mock()
                manage.operate(action, [])
                argv = ["sudo", "-n", "-u", "gamesvc", "--", self.values["MC_BINARY"]]
                if action == "config-check":
                    argv += ["check", "--config", str(self.canonical)]
                else:
                    argv += ["reload", "--socket", self.values["MC_ADMIN_SOCKET"],
                             "--config", str(self.canonical)]
                self.process.assert_called_once_with(
                    argv, text=True, capture_output=action == "config-check", check=False,
                )
                self.assertNotIn(self.secret, " ".join(argv))
                self.assertEqual(fingerprint(self.canonical), before)
                self.assertEqual(list(self.root.iterdir()), [self.env_file])
        self.execute.assert_not_called()

    def test_failed_reload_preserves_file_and_does_not_claim_success(self):
        before = fingerprint(self.canonical)
        self.process.side_effect = None
        self.process.return_value = subprocess.CompletedProcess([], 1, stdout="", stderr="reload rejected")
        with contextlib.redirect_stdout(io.StringIO()) as output:
            with self.assertRaisesRegex(RuntimeError, "command failed"):
                manage.operate("config-apply", [])
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(fingerprint(self.canonical), before)
        self.assertEqual(self.process.call_count, 1)

    def test_show_redacts_password_and_displays_defaults_without_external_reads(self):
        with contextlib.redirect_stdout(io.StringIO()) as output:
            manage.operate("config-show", [])
        displayed, _ = json.JSONDecoder().raw_decode(output.getvalue())
        self.assertEqual(displayed["MC_RCON_PASSWORD"], "[REDACTED]")
        self.assertEqual(displayed["MC_IDLE_TIMEOUT"], "2h")
        self.assertEqual(displayed["MC_POLL_INTERVAL"], "15s")
        self.assertNotIn(self.secret, output.getvalue())
        self.process.assert_not_called()

    def test_split_project_and_canonical_files_fail_before_any_action(self):
        self.env_file.unlink()
        self.env_file.write_text(env_config.serialize_env(self.values))
        for action in ("config-show", "config-check", "config-apply", "command", "deploy"):
            with self.subTest(action=action), self.assertRaisesRegex(RuntimeError, "not the canonical"):
                manage.operate(action, [])
        self.process.assert_not_called()
        self.execute.assert_not_called()

    def test_edit_targets_real_file_even_with_invalid_syntax_and_sets_private_umask(self):
        self.canonical.write_text("broken dotenv syntax, repair me\n")
        with mock.patch.dict(os.environ, {"EDITOR": "editor --wait", "VISUAL": ""}), \
                mock.patch.object(manage.os, "umask") as umask:
            with self.assertRaises(ExecRequested) as launched:
                manage.operate("config-edit", [])
        self.assertEqual(launched.exception.argv, ["editor", "--wait", str(self.canonical)])
        self.assertTrue(self.env_file.is_symlink())
        umask.assert_called_once_with(0o027)
        self.process.assert_not_called()

    def test_init_links_existing_canonical_file_without_overwriting_copies(self):
        before = fingerprint(self.canonical)
        self.env_file.unlink()
        with mock.patch.dict(manage.MANAGE_DEFAULTS, {"MC_CONFIG": str(self.canonical)}), \
                contextlib.redirect_stdout(io.StringIO()):
            manage.initialize()
            self.assertTrue(self.env_file.is_symlink())
            self.assertEqual(self.env_file.resolve(), self.canonical)
            manage.initialize()  # already-correct link is harmless
            self.env_file.unlink()
            self.env_file.write_text("local copy must survive\n")
            with self.assertRaisesRegex(RuntimeError, "refusing to overwrite"):
                manage.initialize()
        self.assertEqual(fingerprint(self.canonical), before)
        self.assertEqual(self.env_file.read_text(), "local copy must survive\n")
        self.process.assert_not_called()

    def test_server_logs_use_env_backend_unit_without_reading_an_extra_config(self):
        with self.assertRaises(ExecRequested) as launched:
            manage.operate("logs-server-follow", [])
        self.assertEqual(launched.exception.argv, [
            "sudo", "-n", "--", "journalctl", "--namespace=mcs-bootloader",
            "-u", "minecraft-backend.service", "-n", "100", "-o", "cat", "-f",
        ])
        self.process.assert_not_called()

    def test_active_gateway_restart_checks_candidate_before_stopping_old_daemon(self):
        observed = []

        def result(argv, **options):
            observed.append(argv)
            return subprocess.CompletedProcess(
                argv, 0, stdout="active\n" if "show" in argv else "", stderr="",
            )

        self.process.side_effect = result
        with contextlib.redirect_stdout(io.StringIO()):
            manage.operate("gateway-restart", [])
        self.assertEqual(observed, [
            ["sudo", "-n", "--", "systemctl", "show", "mcs-bootloader.service",
             "--property=ActiveState", "--value"],
            ["sudo", "-n", "-u", "gamesvc", "--", self.values["MC_BINARY"],
             "restart-check", "--socket", self.values["MC_ADMIN_SOCKET"],
             "--config", str(self.canonical)],
            ["sudo", "-n", "--", "systemctl", "restart", "mcs-bootloader.service"],
        ])
        self.assertNotIn(self.secret, str(observed))

    def test_failed_restart_preflight_preserves_the_running_gateway(self):
        observed = []

        def result(argv, **options):
            observed.append(argv)
            if "show" in argv:
                return subprocess.CompletedProcess(argv, 0, stdout="active\n", stderr="")
            self.assertIn("restart-check", argv)
            return subprocess.CompletedProcess(
                argv, 1, stdout="", stderr="apply MC_RCON_PASSWORD while Minecraft is stopped before restarting",
            )

        self.process.side_effect = result
        with contextlib.redirect_stdout(io.StringIO()) as output:
            with self.assertRaisesRegex(RuntimeError, "MC_RCON_PASSWORD"):
                manage.operate("gateway-restart", [])
        self.assertEqual(len(observed), 2)
        self.assertFalse(any("restart" in argv for argv in observed))
        self.assertEqual(output.getvalue(), "")

    def test_inactive_restart_validates_file_and_unknown_state_cannot_restart(self):
        for state in ("inactive", "failed", "deactivating", ""):
            observed = []

            def result(argv, **options):
                observed.append(argv)
                return subprocess.CompletedProcess(
                    argv, 0, stdout=state + "\n" if "show" in argv else "", stderr="",
                )

            self.process.side_effect = result
            with self.subTest(state=state), contextlib.redirect_stdout(io.StringIO()):
                if state in ("inactive", "failed"):
                    manage.operate("gateway-restart", [])
                    self.assertEqual(observed[1], [
                        "sudo", "-n", "-u", "gamesvc", "--", self.values["MC_BINARY"],
                        "check", "--config", str(self.canonical),
                    ])
                    self.assertEqual(observed[2], [
                        "sudo", "-n", "--", "systemctl", "restart", "mcs-bootloader.service",
                    ])
                else:
                    with self.assertRaisesRegex(RuntimeError, "stopping or unknown"):
                        manage.operate("gateway-restart", [])
                    self.assertEqual(len(observed), 1)

    def test_deploy_validates_and_installs_without_start_stop_or_restart(self):
        source = self.root / "bin" / "mcs-bootloader"
        source.parent.mkdir()
        source.write_bytes(b"\x7fELFnew candidate")
        destination = Path(self.values["MC_BINARY"])
        destination.write_bytes(b"\x7fELFcurrently installed")
        before = fingerprint(destination), fingerprint(self.canonical)
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
            prefix + [str(source), "check", "--config", str(self.canonical)],
            prefix + ["sha256sum", "--", str(destination)],
            prefix + [sys.executable, str(Path(manage.__file__).resolve()), "_deploy",
                      str(source), str(destination), digest, str(self.canonical)],
        ])
        self.assertEqual(before, (fingerprint(destination), fingerprint(self.canonical)))
        self.assertIn("left running", output.getvalue())
        self.execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
