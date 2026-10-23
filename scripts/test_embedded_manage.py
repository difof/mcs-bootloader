import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import env_config
import manage


class EmbeddedOperatorTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.path = self.root / ".env"
        self.executable = self.root / "downloaded-bootloader"
        self.executable.write_bytes(b"\x7fELFsynthetic")

    def invoke(self, action, arguments=()):
        return manage.embedded_operate(action, list(arguments), self.path, self.executable)

    def test_init_is_private_no_overwrite_and_show_redacts_without_installed_config(self):
        result = self.invoke("init")
        self.assertTrue(result["ok"])
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        original = self.path.read_bytes()
        values = env_config.read_env(self.path)
        self.assertNotEqual(values["MC_RCON_PASSWORD"], "replace-with-a-random-private-password")
        self.assertNotIn(values["MC_RCON_PASSWORD"], json.dumps(result))
        result = self.invoke("config-show")
        self.assertEqual(result["settings"]["MC_RCON_PASSWORD"], "[REDACTED]")
        self.assertNotIn(values["MC_RCON_PASSWORD"], json.dumps(result))
        with self.assertRaisesRegex(RuntimeError, "already exists"):
            self.invoke("init")
        self.assertEqual(self.path.read_bytes(), original)

    def test_deploy_installs_the_running_download_not_a_source_tree_path(self):
        destination = self.root / "installed"
        destination.write_bytes(b"\x7fELFprevious")
        self.path.write_text("MC_RCON_PASSWORD='synthetic-test-only-password'\nMC_CONFIG="
                             + json.dumps(str(self.path)) + "\nMC_BINARY=" + json.dumps(str(destination)) + "\n")
        self.path.chmod(0o600)
        import service_config
        with mock.patch.object(manage.os, "geteuid", return_value=0), \
                mock.patch.object(service_config, "secure_directory"), \
                mock.patch.object(service_config, "read_regular", return_value=(destination.read_bytes(), destination.stat())), \
                mock.patch.object(service_config, "preflight"), \
                mock.patch.object(service_config, "configure", return_value=True), \
                mock.patch.object(manage, "replace_installed") as replace:
            result = self.invoke("deploy")
        self.assertTrue(result["changed"])
        self.assertEqual(replace.call_args.args[:2], (self.executable, destination))
        self.assertEqual(replace.call_args.kwargs["config"], str(self.path))

    def test_same_binary_deploy_preserves_previous_without_replacement(self):
        self.path.write_text("MC_RCON_PASSWORD='synthetic-test-only-password'\nMC_CONFIG="
                             + json.dumps(str(self.path)) + "\nMC_BINARY=" + json.dumps(str(self.executable)) + "\n")
        self.path.chmod(0o600)
        import service_config
        with mock.patch.object(manage.os, "geteuid", return_value=0), \
                mock.patch.object(service_config, "secure_directory"), \
                mock.patch.object(service_config, "read_regular", return_value=(self.executable.read_bytes(), self.executable.stat())), \
                mock.patch.object(service_config, "preflight"), \
                mock.patch.object(service_config, "configure", return_value=True) as configure, \
                mock.patch.object(manage, "replace_installed") as replace:
            self.assertFalse(self.invoke("deploy")["changed"])
        replace.assert_not_called()
        configure.assert_called_once_with(self.path)

    def test_deploy_rejects_known_service_conflict_before_copying_binary(self):
        canonical, _ = self.installed_config()
        import service_config
        with mock.patch.object(manage.os, "geteuid", return_value=0), \
                mock.patch.object(service_config, "secure_directory"), \
                mock.patch.object(service_config, "read_regular", return_value=(b"old", self.executable.stat())), \
                mock.patch.object(service_config, "preflight", side_effect=RuntimeError("unknown service override")), \
                mock.patch.object(manage, "replace_installed") as replace:
            with self.assertRaisesRegex(RuntimeError, "unknown service override"):
                self.invoke("deploy")
        replace.assert_not_called()

    def test_deploy_reports_partial_service_refresh_failure_explicitly(self):
        self.installed_config()
        import service_config
        with mock.patch.object(manage.os, "geteuid", return_value=0), \
                mock.patch.object(service_config, "secure_directory"), \
                mock.patch.object(service_config, "read_regular", return_value=(b"old", self.executable.stat())), \
                mock.patch.object(service_config, "preflight"), \
                mock.patch.object(service_config, "configure", side_effect=RuntimeError("reload failed")), \
                mock.patch.object(manage, "replace_installed") as replace:
            with self.assertRaisesRegex(RuntimeError, "Executable installed, but service settings") as caught:
                self.invoke("deploy")
        replace.assert_called_once()
        self.assertIn("not restarted", str(caught.exception))
        self.assertIn(".previous", str(caught.exception))

    def test_install_passes_exact_download_and_report_without_source_dependency(self):
        import install
        with mock.patch.object(manage.os, "geteuid", return_value=0), \
                mock.patch.object(install, "install", return_value={"status": "installed"}) as installer:
            self.invoke("install", ["--report", str(self.root / "report.json")])
        installer.assert_called_once_with(self.path, self.root / "report.json", binary_path=self.executable)

    def test_remove_defaults_to_plan_and_requires_explicit_yes(self):
        import remove
        with mock.patch.object(remove, "remove", return_value={"ok": True}) as remover:
            self.invoke("remove")
            remover.assert_called_with(self.path, yes=False)
            self.invoke("remove", ["--yes"])
            remover.assert_called_with(self.path, yes=True)
            with self.assertRaisesRegex(RuntimeError, "accepts"):
                self.invoke("remove", ["--purge-world"])

    def installed_config(self):
        canonical = self.root / "canonical.env"
        values = {"MC_RCON_PASSWORD": "SYNTHETIC_NEVER_EXPOSE_SECRET_1234",
                  "MC_CONFIG": str(canonical), "MC_BINARY": str(self.executable),
                  "MC_SERVICE_USER": "gamesvc", "MC_ADMIN_SOCKET": "/run/example/control.sock"}
        canonical.write_text(env_config.serialize_env(values))
        canonical.chmod(0o640)
        self.path.symlink_to(canonical)
        return canonical, values

    def test_installed_operations_refuse_a_separate_config_copy(self):
        canonical, _ = self.installed_config()
        self.path.unlink()
        self.path.write_bytes(canonical.read_bytes())
        self.path.chmod(0o600)
        with mock.patch.object(manage.subprocess, "run") as process:
            for action in ("deploy", "gateway-start", "gateway-stop", "gateway-restart", "gateway-configure"):
                with self.subTest(action=action), self.assertRaisesRegex(RuntimeError, "canonical installed"):
                    self.invoke(action)
            process.assert_not_called()

    def test_edit_resolves_link_and_can_repair_invalid_syntax(self):
        canonical, _ = self.installed_config()
        canonical.write_text("broken syntax to repair\n")
        terminal = self.root / "terminal"
        terminal.write_bytes(b"")
        original_open = open

        def open_file(path, *args, **kwargs):
            return original_open(terminal if path == "/dev/tty" else path, *args, **kwargs)

        with mock.patch.dict(os.environ, {"EDITOR": "editor --wait", "VISUAL": ""}), \
                mock.patch("builtins.open", side_effect=open_file), \
                mock.patch.object(manage.os, "umask") as umask, \
                mock.patch.object(manage.subprocess, "Popen") as editor:
            editor.return_value.wait.return_value = 0
            result = self.invoke("config-edit")
        self.assertEqual(editor.call_args.args[0], ["editor", "--wait", str(canonical)])
        self.assertIs(editor.call_args.kwargs["stdout"], editor.call_args.kwargs["stderr"])
        self.assertNotIn("shell", editor.call_args.kwargs)
        self.assertEqual(result["path"], str(canonical))
        self.assertTrue(self.path.is_symlink())
        umask.assert_called_once_with(0o027)

    def test_link_is_idempotent_and_refuses_existing_copies(self):
        canonical = self.root / "canonical.env"
        canonical.write_text("unchanged private data\n")
        canonical.chmod(0o600)
        with mock.patch.dict(manage.MANAGE_DEFAULTS, {"MC_CONFIG": str(canonical)}):
            self.assertTrue(self.invoke("config-link")["changed"])
            self.assertFalse(self.invoke("config-link")["changed"])
            self.path.unlink()
            self.path.write_text("operator copy\n")
            with self.assertRaisesRegex(RuntimeError, "refusing to overwrite"):
                self.invoke("config-link")
        self.assertEqual(self.path.read_text(), "operator copy\n")
        self.assertEqual(canonical.read_text(), "unchanged private data\n")

    def test_active_restart_preflights_before_systemctl_restart(self):
        canonical, values = self.installed_config()
        observed = []

        def command(argv, **options):
            observed.append(argv)
            return subprocess.CompletedProcess(argv, 0, "active\n" if "show" in argv else "", "")

        with mock.patch.object(manage.os, "geteuid", return_value=0), \
                mock.patch.object(manage.subprocess, "run", side_effect=command):
            self.invoke("gateway-restart")
        self.assertEqual(observed, [
            ["/usr/bin/systemctl", "show", "mcs-bootloader.service", "--property=ActiveState", "--value"],
            ["/usr/sbin/runuser", "-u", "gamesvc", "--", str(self.executable), "restart-check",
             "--socket", values["MC_ADMIN_SOCKET"], "--config", str(canonical)],
            ["/usr/bin/systemctl", "restart", "mcs-bootloader.service"],
        ])
        self.assertNotIn(values["MC_RCON_PASSWORD"], repr(observed))

    def test_failed_restart_preflight_preserves_running_gateway(self):
        self.installed_config()
        observed = []

        def command(argv, **options):
            observed.append(argv)
            if "show" in argv:
                return subprocess.CompletedProcess(argv, 0, "active\n", "")
            return subprocess.CompletedProcess(argv, 1, "", "candidate configuration rejected")

        with mock.patch.object(manage.os, "geteuid", return_value=0), \
                mock.patch.object(manage.subprocess, "run", side_effect=command):
            with self.assertRaisesRegex(RuntimeError, "candidate configuration rejected"):
                self.invoke("gateway-restart")
        self.assertEqual(len(observed), 2)
        self.assertFalse(any("restart" in argv for argv in observed))

    def test_inactive_restart_validates_and_unknown_state_refuses(self):
        canonical, _ = self.installed_config()
        for state in ("inactive", "failed", "deactivating", ""):
            observed = []
            def command(argv, **options):
                observed.append(argv)
                return subprocess.CompletedProcess(argv, 0, state + "\n" if "show" in argv else "", "")
            with self.subTest(state=state), mock.patch.object(manage.os, "geteuid", return_value=0), \
                    mock.patch.object(manage.subprocess, "run", side_effect=command):
                if state in ("inactive", "failed"):
                    self.invoke("gateway-restart")
                    self.assertEqual(observed[1][-3:], ["check", "--config", str(canonical)])
                    self.assertEqual(observed[2], ["/usr/bin/systemctl", "restart", "mcs-bootloader.service"])
                else:
                    with self.assertRaisesRegex(RuntimeError, "state is unknown"):
                        self.invoke("gateway-restart")
                    self.assertEqual(len(observed), 1)

    def test_gateway_configure_uses_resolved_canonical_and_does_not_restart(self):
        canonical, _ = self.installed_config()
        import service_config
        with mock.patch.object(manage.os, "geteuid", return_value=0), \
                mock.patch.object(service_config, "configure", return_value=True) as configure, \
                mock.patch.object(manage.subprocess, "run") as process:
            self.invoke("gateway-configure")
        configure.assert_called_once_with(canonical)
        process.assert_not_called()

    def test_fifo_configuration_is_rejected_without_blocking(self):
        os.mkfifo(self.path, 0o600)
        with self.assertRaisesRegex(RuntimeError, "regular private"):
            self.invoke("config-show")


if __name__ == "__main__":
    unittest.main()
