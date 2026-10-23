import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import manage
import remove
import service_config


class RemovalTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.units = self.root / "systemd"
        self.units.mkdir()
        self.sudoers = self.root / "sudoers"
        self.sudoers.mkdir()
        self.binary = self.root / "mcs-bootloader"
        self.binary.write_bytes(b"\x7fELFtest-only-synthetic-binary")
        self.binary.chmod(0o755)
        self.env = self.root / ".env"
        self.env.write_text("MC_RCON_PASSWORD='private-synthetic-test-only'\nMC_CONFIG="
                            + json.dumps(str(self.env)) + "\nMC_BINARY=" + json.dumps(str(self.binary)) + "\n")
        self.env.chmod(0o600)
        self.cfg = {**manage.settings({}), "MC_CONFIG": str(self.env), "MC_BINARY": str(self.binary)}
        self.gateway = self.units / self.cfg["MC_GATEWAY_SERVICE"]
        self.backend = self.units / self.cfg["MC_BACKEND_SERVICE"]
        self.gateway.write_text(f"[Service]\nExecStart={self.binary} serve --config {self.env} --json\n")
        self.backend.write_text(f"[Service]\nExecStart={self.binary} backend-start --config {self.env}\n"
                                f"ExecStop={self.binary} backend-stop --config {self.env} --pid=${{MAINPID}}\n")
        for file in (self.gateway, self.backend):
            file.chmod(0o644)
        for patcher in (
            mock.patch.object(remove.os, "geteuid", return_value=0),
            mock.patch.object(service_config, "_root_owned", return_value=True),
            mock.patch.object(service_config, "SYSTEMD_DIRECTORY", self.units),
            mock.patch.object(remove, "SUDOERS_DIRECTORY", self.sudoers),
            mock.patch.object(remove, "JOURNAL_DIRECTORY", self.units),
            mock.patch.dict(remove.MANAGE_DEFAULTS, {"MC_BINARY": str(self.binary)}),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        inspection = mock.patch.object(service_config, "inspect_service", return_value={
            "FragmentPath": str(self.gateway), "NeedDaemonReload": "no"})
        self.inspection = inspection.start()
        self.addCleanup(inspection.stop)
        def property_values(unit, names):
            if unit == self.cfg["MC_GATEWAY_SERVICE"]:
                return {"ActiveState": "active", "UnitFileState": "enabled"}
            return {"Id": self.cfg["MC_BACKEND_SERVICE"], "LoadState": "loaded",
                    "FragmentPath": str(self.backend), "NeedDaemonReload": "no"}
        properties = mock.patch.object(remove, "unit_properties", side_effect=property_values)
        self.properties = properties.start()
        self.addCleanup(properties.stop)
        stopped = mock.patch.object(remove, "backend_stopped")
        self.stopped = stopped.start()
        self.addCleanup(stopped.stop)
        command = mock.patch.object(service_config, "run", side_effect=AssertionError("unexpected system mutation"))
        self.command = command.start()
        self.addCleanup(command.stop)

    def test_default_plan_reads_but_changes_nothing(self):
        before = {path: path.read_bytes() for path in (self.env, self.binary, self.gateway, self.backend)}
        result = remove.remove(self.env)
        self.assertFalse(result["applied"])
        self.assertIn(str(self.env), result["preserved"])
        self.assertNotIn(str(self.env), result["files"])
        self.command.assert_not_called()
        self.assertEqual(before, {path: path.read_bytes() for path in before})

    def test_unknown_unit_dropin_prevents_removal_before_stop(self):
        overrides = self.units / (self.cfg["MC_GATEWAY_SERVICE"] + ".d")
        overrides.mkdir()
        custom = overrides / "operator.conf"
        custom.write_text("[Service]\nEnvironment=IMPORTANT=1\n")
        with self.assertRaisesRegex(RuntimeError, "operator overrides"):
            remove.remove(self.env, yes=True)
        self.assertTrue(custom.exists())
        self.command.assert_not_called()

    def test_unknown_sudo_rule_is_not_deleted(self):
        rule = self.sudoers / "mcs-bootloader"
        rule.write_text("minecraft ALL=(root) ALL\n")
        rule.chmod(0o440)
        with self.assertRaisesRegex(RuntimeError, "unrecognized edits"):
            remove.remove(self.env, yes=True)
        self.assertTrue(rule.exists())
        self.command.assert_not_called()

    def test_backend_other_command_is_not_treated_as_owned(self):
        self.backend.write_text("[Service]\nExecStart=/usr/bin/sleep infinity\n")
        with self.assertRaisesRegex(RuntimeError, "recognized bootloader"):
            remove.remove(self.env, yes=True)
        self.command.assert_not_called()

    def test_removal_preserves_world_env_and_backups(self):
        world = self.root / "world"
        world.mkdir()
        (world / "level.dat").write_bytes(b"valuable-test-world")
        backup = self.root / "backup.tar.zst"
        backup.write_bytes(b"valuable-test-backup")
        config = self.env.read_bytes()
        self.command.side_effect = None
        self.command.return_value = ""
        result = remove.remove(self.env, yes=True)
        self.assertTrue(result["applied"])
        self.assertFalse(self.binary.exists())
        self.assertFalse(self.gateway.exists())
        self.assertFalse(self.backend.exists())
        self.assertEqual(self.env.read_bytes(), config)
        self.assertEqual((world / "level.dat").read_bytes(), b"valuable-test-world")
        self.assertEqual(backup.read_bytes(), b"valuable-test-backup")
        self.assertEqual(self.command.call_args_list, [
            mock.call([service_config.SYSTEMCTL, "stop", self.cfg["MC_GATEWAY_SERVICE"]]),
            mock.call([service_config.SYSTEMCTL, "disable", self.cfg["MC_GATEWAY_SERVICE"]]),
            mock.call([service_config.SYSTEMCTL, "daemon-reload"]),
        ])
        self.assertEqual(self.stopped.call_count, 3)

    def test_wake_racing_preflight_restores_the_running_gateway(self):
        before = {path: path.read_bytes() for path in (self.env, self.binary, self.gateway, self.backend)}
        self.command.side_effect = None
        self.command.return_value = ""
        self.stopped.side_effect = [None, None, RuntimeError("Minecraft started during preflight")]
        with self.assertRaisesRegex(RuntimeError, "were restored"):
            remove.remove(self.env, yes=True)
        self.assertEqual(self.command.call_args_list, [
            mock.call([service_config.SYSTEMCTL, "stop", self.cfg["MC_GATEWAY_SERVICE"]]),
            mock.call([service_config.SYSTEMCTL, "start", self.cfg["MC_GATEWAY_SERVICE"]]),
        ])
        self.assertEqual(before, {path: path.read_bytes() for path in before})

    def test_failed_reload_restores_files_and_gateway_enable_and_active_state(self):
        before = {path: path.read_bytes() for path in (self.env, self.binary, self.gateway, self.backend)}
        count = 0
        def command(argv):
            nonlocal count
            if argv == [service_config.SYSTEMCTL, "daemon-reload"]:
                count += 1
                if count == 1:
                    raise RuntimeError("systemd reload failed")
            return ""
        self.command.side_effect = command
        with mock.patch.object(manage.os, "fchown"):
            with self.assertRaisesRegex(RuntimeError, "were restored"):
                remove.remove(self.env, yes=True)
        self.assertEqual(before, {path: path.read_bytes() for path in before})
        self.assertEqual(self.command.call_args_list[-2:], [
            mock.call([service_config.SYSTEMCTL, "enable", self.cfg["MC_GATEWAY_SERVICE"]]),
            mock.call([service_config.SYSTEMCTL, "start", self.cfg["MC_GATEWAY_SERVICE"]]),
        ])


class StopRequirementTests(unittest.TestCase):
    def test_active_unknown_or_transitioning_backend_refuses_without_signal(self):
        cfg = manage.settings({})
        for state, pid in (("active", "123"), ("activating", "0"), ("deactivating", "123"),
                           ("", "0"), ("failed", "123")):
            with self.subTest(state=state, pid=pid), \
                    mock.patch.object(remove, "unit_properties", return_value={"ActiveState": state, "MainPID": pid}), \
                    mock.patch.object(remove.pwd, "getpwnam") as account:
                with self.assertRaisesRegex(RuntimeError, "Minecraft must be stopped"):
                    remove.backend_stopped(cfg)
                account.assert_not_called()


if __name__ == "__main__":
    unittest.main()
