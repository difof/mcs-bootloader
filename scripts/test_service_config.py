import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
from unittest import mock

import env_config
import service_config as service


class GatewayServiceConfigTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.systemd = self.root / "etc/systemd/system"
        self.systemd.mkdir(parents=True)
        self.unit = "friends-frontdoor.service"
        self.fragment = self.systemd / self.unit
        self.fragment.write_text("[Service]\nUser=custom-player\nLogNamespace=custom-logs\n")
        self.fragment.chmod(0o644)
        self.binary = self.root / "opt/custom executable"
        self.binary.parent.mkdir()
        self.binary.write_bytes(b"synthetic installed binary")
        self.binary.chmod(0o755)
        self.env = self.root / "canonical config.env"
        self.env.write_text(env_config.serialize_env({
            "MC_CONFIG": str(self.env), "MC_BINARY": str(self.binary),
            "MC_GATEWAY_SERVICE": self.unit, "MC_SERVICE_USER": "gamesvc",
            "MC_RCON_PASSWORD": "SYNTHETIC_SECRET_NEVER_PRINTED",
        }))
        self.env.chmod(0o640)
        self.drop_directory = self.systemd / (self.unit + ".d")
        self.target = self.drop_directory / service.DROP_IN_NAME
        self.applied = False
        self.pending = False
        self.loaded = True
        self.reload_failures = []
        self.conflicting_override = False
        self.exec_argv = [str(self.binary), "serve", "--config", str(self.env)]
        self.commands = []
        for patcher in (
            mock.patch.object(service.os, "geteuid", return_value=0),
            # Run real temporary-filesystem checks while modeling ownership as
            # root on CI, where the test runner is intentionally unprivileged.
            mock.patch.object(service, "_root_owned", side_effect=lambda info: info.st_uid in (0, os.getuid())),
            mock.patch.object(service.os, "fchown"),
            mock.patch.object(service.subprocess, "run", side_effect=self.command),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)

    def command(self, argv, **kwargs):
        self.commands.append(argv)
        self.assertEqual(kwargs, {"capture_output": True, "text": True, "check": False, "timeout": 30})
        if argv[:2] == [service.SYSTEMCTL, "show"]:
            self.assertEqual(argv[2], self.unit)
            values = {
                "Id": self.unit, "LoadState": "loaded" if self.loaded else "not-found",
                "FragmentPath": str(self.fragment), "NeedDaemonReload": "yes" if self.pending else "no",
                "ProtectHome": "read-only" if self.applied else "yes",
                "TimeoutStopUSec": "infinity" if self.applied else "15s",
            }
            output = "".join(key + "=" + value + "\n" for key, value in values.items())
        elif argv == [service.SYSTEMCTL, "daemon-reload"]:
            if self.reload_failures and self.reload_failures.pop(0):
                return subprocess.CompletedProcess(argv, 1, "", "private diagnostic must not be echoed")
            self.applied = self.target.exists() and not self.conflicting_override
            self.pending = False
            output = ""
        elif argv[:4] == [service.BUSCTL, "--system", "--json=short", "call"]:
            output = json.dumps({"type": "o", "data": ["/org/freedesktop/systemd1/unit/friends_2eservice"]})
        elif argv[:4] == [service.BUSCTL, "--system", "--json=short", "get-property"]:
            self.assertEqual(argv[-1], "ExecStart")
            output = json.dumps({"type": "a(sasbttttuii)", "data": [[str(self.binary), self.exec_argv, False, 0, 0, 0, 0, 0, 0, 0]]})
        else:
            self.fail("unexpected process or service mutation: " + str(argv))
        return subprocess.CompletedProcess(argv, 0, output, "")

    def configure(self):
        return service.configure(self.env, systemd_directory=self.systemd)

    def install_owned(self, mode=0o644):
        self.drop_directory.mkdir()
        self.target.write_bytes(service.DROP_IN)
        self.target.chmod(mode)

    def reloads(self):
        return self.commands.count([service.SYSTEMCTL, "daemon-reload"])

    def test_migration_only_adds_owned_override_and_is_idempotent(self):
        fragment, env = self.fragment.read_bytes(), self.env.read_bytes()
        self.assertTrue(self.configure())
        self.assertEqual(self.target.read_bytes(), service.DROP_IN)
        self.assertEqual(stat.S_IMODE(self.target.stat().st_mode), 0o644)
        self.assertEqual(self.fragment.read_bytes(), fragment)
        self.assertEqual(self.env.read_bytes(), env)
        self.assertEqual(self.reloads(), 1)
        self.assertFalse(self.configure())
        self.assertEqual(self.reloads(), 1)
        self.assertEqual(list(self.drop_directory.iterdir()), [self.target])

    def test_actual_argv_must_match_without_flattening_or_shell_parsing(self):
        # The printable joined text can look identical, but this is one argument.
        self.exec_argv = [str(self.binary), "serve --config " + str(self.env)]
        with self.assertRaisesRegex(RuntimeError, "does not execute"):
            self.configure()
        self.assertFalse(self.drop_directory.exists())
        self.assertEqual(self.reloads(), 0)

    def test_json_logging_flag_in_new_service_template_is_accepted(self):
        self.exec_argv += ["--json"]
        self.assertTrue(self.configure())
        self.assertEqual(self.reloads(), 1)

    def test_missing_or_pending_unit_is_refused_before_writing(self):
        self.loaded = False
        with self.assertRaisesRegex(RuntimeError, "missing"):
            self.configure()
        self.loaded, self.pending = True, True
        with self.assertRaisesRegex(RuntimeError, "unapplied edits"):
            self.configure()
        self.assertFalse(self.drop_directory.exists())
        self.assertEqual(self.reloads(), 0)

    def test_unknown_fixed_name_dropin_is_not_overwritten(self):
        self.install_owned()
        operator = b"[Service]\nEnvironment=OPERATOR_SETTING\n"
        self.target.write_bytes(operator)
        with self.assertRaisesRegex(RuntimeError, "unknown content"):
            self.configure()
        self.assertEqual(self.target.read_bytes(), operator)
        self.assertEqual(self.reloads(), 0)

    def test_symlink_directory_and_symlink_file_are_refused(self):
        outside = self.root / "outside"
        outside.mkdir()
        self.drop_directory.symlink_to(outside, target_is_directory=True)
        with self.assertRaisesRegex(RuntimeError, "real root-owned"):
            self.configure()
        self.assertEqual(list(outside.iterdir()), [])
        self.drop_directory.unlink()
        self.drop_directory.mkdir()
        destination = outside / "operator.conf"
        destination.write_bytes(service.DROP_IN)
        self.target.symlink_to(destination)
        with self.assertRaises(OSError):
            self.configure()
        self.assertTrue(self.target.is_symlink())
        self.assertEqual(destination.read_bytes(), service.DROP_IN)
        self.assertEqual(self.reloads(), 0)

    def test_group_writable_paths_and_hardlinked_target_are_refused(self):
        self.install_owned()
        self.drop_directory.chmod(0o775)
        with self.assertRaisesRegex(RuntimeError, "untrusted writes"):
            self.configure()
        self.drop_directory.chmod(0o755)
        self.target.chmod(0o664)
        with self.assertRaisesRegex(RuntimeError, "group/other writes"):
            self.configure()
        self.target.chmod(0o644)
        os.link(self.target, self.root / "hardlink.conf")
        with self.assertRaisesRegex(RuntimeError, "hardlinks"):
            self.configure()
        self.assertEqual(self.reloads(), 0)

    def test_requires_privilege_root_ownership_and_private_canonical_env(self):
        with mock.patch.object(service.os, "geteuid", return_value=1000):
            with self.assertRaisesRegex(RuntimeError, "requires sudo"):
                self.configure()
        with mock.patch.object(service, "_root_owned", return_value=False):
            with self.assertRaisesRegex(RuntimeError, "root-owned"):
                self.configure()
        self.env.chmod(0o644)
        with self.assertRaisesRegex(RuntimeError, "private"):
            self.configure()
        self.env.chmod(0o640)
        duplicate = self.root / "duplicate.env"
        duplicate.write_bytes(self.env.read_bytes())
        duplicate.chmod(0o640)
        with self.assertRaisesRegex(RuntimeError, "canonical MC_CONFIG"):
            service.configure(duplicate, systemd_directory=self.systemd)
        self.assertFalse(self.drop_directory.exists())

    def test_failed_reload_restores_absence_and_reloads_previous_state(self):
        self.reload_failures = [True, False]
        with self.assertRaisesRegex(RuntimeError, "previous drop-in was restored") as caught:
            self.configure()
        self.assertFalse(self.target.exists())
        self.assertFalse(self.drop_directory.exists())
        self.assertFalse(self.applied)
        self.assertEqual(self.reloads(), 2)
        self.assertNotIn("private diagnostic", str(caught.exception))

    def test_failed_reload_restores_existing_content_and_permissions(self):
        self.install_owned(mode=0o600)
        self.reload_failures = [True, False]
        with self.assertRaisesRegex(RuntimeError, "previous drop-in was restored"):
            self.configure()
        self.assertEqual(self.target.read_bytes(), service.DROP_IN)
        self.assertEqual(stat.S_IMODE(self.target.stat().st_mode), 0o600)
        self.assertEqual(list(self.drop_directory.iterdir()), [self.target])
        self.assertEqual(self.reloads(), 2)

    def test_later_operator_override_is_reported_and_owned_change_rolled_back(self):
        self.conflicting_override = True
        with self.assertRaisesRegex(RuntimeError, "another drop-in"):
            self.configure()
        self.assertFalse(self.target.exists())
        self.assertEqual(self.reloads(), 2)

    def test_failed_rollback_reload_retains_backup_and_reports_recovery(self):
        self.install_owned(mode=0o600)
        self.reload_failures = [True, True]
        with self.assertRaisesRegex(RuntimeError, "operator attention.*saved drop-in"):
            self.configure()
        self.assertEqual(stat.S_IMODE(self.target.stat().st_mode), 0o600)
        backups = list(self.drop_directory.glob(".mcs-bootloader-runtime-backup-*"))
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].read_bytes(), service.DROP_IN)

    def test_failed_atomic_write_preserves_absent_target(self):
        with mock.patch.object(service.os, "replace", side_effect=OSError("synthetic write failure")):
            with self.assertRaisesRegex(RuntimeError, "previous drop-in was restored"):
                self.configure()
        self.assertFalse(self.target.exists())
        self.assertFalse(self.drop_directory.exists())


if __name__ == "__main__":
    unittest.main()
