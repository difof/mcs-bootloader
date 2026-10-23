from pathlib import Path
import json
import tempfile
import unittest

import install


class InstallerValidationTests(unittest.TestCase):
    def env_file(self, directory, additions="", password="  private $() `literal` #= café 🚀  "):
        path = Path(directory) / ".env"
        text = ("# preserve my comments\nMC_RCON_PASSWORD=" + json.dumps(password, ensure_ascii=False)
                + '\nMC_LAUNCH_ARGS=\'["@user_jvm_args.txt","@libraries/loader/unix_args.txt","nogui"]\'\n' + additions)
        path.write_text(text)
        path.chmod(0o600)
        return path

    def test_properties_preserve_authentication_world_and_comments(self):
        original = (b"# valuable world\r\nonline-mode=false\r\nwhite-list=false\r\n"
                    b"level-name=world\r\nserver-ip=\r\nserver-port=25565\r\n"
                    b"enable-rcon=false\r\nrcon.password=\r\nmotd=Friends")
        result = install.updated_properties(original, "a" * 64)
        for line in (b"# valuable world\r\n", b"online-mode=false\r\n",
                     b"white-list=false\r\n", b"level-name=world\r\n", b"motd=Friends\r\n"):
            self.assertIn(line, result)
        for line in (b"server-ip=127.0.0.1\r\n", b"server-port=25566\r\n",
                     b"enable-rcon=true\r\n", b"rcon.port=25575\r\n",
                     b"broadcast-rcon-to-ops=false\r\n"):
            self.assertIn(line, result)
        self.assertIn(b"rcon.password=" + b"a" * 64 + b"\r\n", result)

    def test_duplicate_target_keys_are_all_replaced(self):
        result = install.updated_properties(b"server-port=1\nserver-port : 2\nother=unchanged\n", "safe")
        self.assertEqual(result.count(b"server-port=25566\n"), 2)
        self.assertIn(b"other=unchanged\n", result)

    def test_continuations_require_review(self):
        with self.assertRaisesRegex(RuntimeError, "continued property"):
            install.updated_properties(b"motd=first\\\nsecond\n", "safe")

    def test_password_uses_java_escapes_without_expansion_or_trimming(self):
        password = "  literal\\tab\t#:= café 🚀  "
        expected = b"\\  literal\\\\tab\\t#:= caf\\u00E9 \\uD83D\\uDE80  "
        self.assertEqual(install.java_property_value(password), expected)
        result = install.updated_properties(b"online-mode=true\nrcon.password=old\n", password)
        self.assertIn(b"rcon.password=" + expected + b"\n", result)
        self.assertIn(b"online-mode=true\n", result)

    def test_ambiguous_credentials_are_refused(self):
        for original in (b"rcon.password=a\nrcon.password=b\n", b"rcon\\.password=old\n"):
            with self.subTest(original=original), self.assertRaises(RuntimeError):
                install.updated_properties(original, "private-test-only")

    def test_installation_settings_use_one_private_env_and_preserve_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = self.env_file(temporary, "MC_SERVICE_USER=gamesvc\nMC_SERVER_DIRECTORY=/srv/test-minecraft\n")
            original = path.read_bytes()
            settings, raw, metadata = install.installation_settings(path)
            self.assertEqual(raw, original)
            self.assertEqual(settings["MC_RCON_PASSWORD"], "  private $() `literal` #= café 🚀  ")
            self.assertEqual(settings["MC_SERVICE_USER"], "gamesvc")
            self.assertEqual(settings["MC_CONFIG"], "/etc/mcs-bootloader/.env")
            self.assertEqual(settings["MC_LAUNCH_EXECUTABLE"], "/usr/bin/java")
            self.assertEqual(metadata.st_uid, path.stat().st_uid)
            self.assertNotIn("MC_RCON_PASSWORD_FILE", settings)
            self.assertNotIn("MC_BACKUP_DIRECTORY", settings)
            self.assertEqual(settings["MC_IDLE_TIMEOUT"], "15m")

    def test_installer_requires_password_private_permissions_and_fixed_topology(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = self.env_file(temporary)
            path.chmod(0o644)
            with self.assertRaisesRegex(RuntimeError, "private"):
                install.installation_settings(path)
            path.chmod(0o600)
            path.write_text("MC_LAUNCH_ARGS='[]'\n")
            with self.assertRaisesRegex(RuntimeError, "MC_RCON_PASSWORD"):
                install.installation_settings(path)
            for password in ("short", "x" * 4097, "replace-with-a-random-private-password"):
                path = self.env_file(temporary, password=password)
                with self.subTest(password_length=len(password)), self.assertRaises(RuntimeError) as caught:
                    install.installation_settings(path)
                self.assertNotIn(password, str(caught.exception))
            secret = "DO_NOT_ECHO_SETTING_VALUE_0123456789"
            path = self.env_file(temporary, "MC_CONFIG=" + secret + "\n")
            with self.assertRaisesRegex(RuntimeError, "MC_CONFIG") as caught:
                install.installation_settings(path)
            self.assertNotIn(secret, str(caught.exception))

    def test_installation_env_link_and_restore_keep_one_canonical_copy(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            source = self.env_file(base)
            settings, raw, metadata = install.installation_settings(source)
            canonical = base / "canonical.env"
            canonical.write_bytes(raw)
            canonical.chmod(0o640)
            install.link_shared_env(source, canonical, raw, metadata)
            self.assertTrue(source.is_symlink())
            self.assertEqual(source.resolve(), canonical.resolve())
            self.assertEqual(source.read_bytes(), raw)
            self.assertEqual(source.lstat().st_uid, metadata.st_uid)
            with self.assertRaisesRegex(RuntimeError, "not a symlink"):
                install.installation_settings(source)
            # This is the installer's pre-activation recovery operation. The
            # canonical file must not be overwritten by following the link.
            install.write_atomic(source, raw, 0o600, metadata.st_uid, metadata.st_gid)
            self.assertFalse(source.is_symlink())
            self.assertEqual(source.read_bytes(), raw)
            self.assertEqual(canonical.read_bytes(), raw)
            self.assertEqual(source.stat().st_mode & 0o777, 0o600)
            self.assertFalse(list(base.glob(".mcs-bootloader-*")))

    def test_installation_env_link_refuses_concurrent_operator_edits(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = self.env_file(temporary)
            _, raw, metadata = install.installation_settings(source)
            changed = raw + b"MC_IDLE_TIMEOUT=2h\n"
            source.write_bytes(changed)
            with self.assertRaisesRegex(RuntimeError, "changed during validation"):
                install.link_shared_env(source, Path(temporary) / "canonical.env", raw, metadata)
            self.assertFalse(source.is_symlink())
            self.assertEqual(source.read_bytes(), changed)

    def test_generic_executable_may_be_absolute_or_relative_and_contain_spaces(self):
        with tempfile.TemporaryDirectory() as temporary:
            server = Path(temporary)
            executable = server / "custom launcher"
            executable.write_text("#!/bin/sh\nexec true\n")
            executable.chmod(0o755)
            self.assertEqual(install.launch_executable(server, "./custom launcher"), executable)
            self.assertEqual(install.launch_executable(server, str(executable)), executable)
            executable.chmod(0o644)
            with self.assertRaisesRegex(RuntimeError, "executable"):
                install.launch_executable(server, "./custom launcher")

    def test_configurable_ports_update_only_network_properties(self):
        settings = {**install.RUNTIME_DEFAULTS, "MC_LISTEN": ":28000",
                    "MC_BACKEND_ADDRESS": "127.0.0.2:28001", "MC_RCON_ADDRESS": "127.0.0.2:28002"}
        self.assertEqual(install.network_settings(settings), (("", 28000), ("127.0.0.2", 28001), ("127.0.0.2", 28002)))
        result = install.updated_properties(b"level-name=custom-world\nonline-mode=true\n", "private",
                                            settings["MC_BACKEND_ADDRESS"], settings["MC_RCON_ADDRESS"])
        self.assertIn(b"server-ip=127.0.0.2\n", result)
        self.assertIn(b"server-port=28001\n", result)
        self.assertIn(b"rcon.port=28002\n", result)
        self.assertIn(b"level-name=custom-world\nonline-mode=true\n", result)
        for invalid in ({"MC_RCON_ADDRESS": "0.0.0.0:28002"}, {"MC_RCON_ADDRESS": "127.0.0.1:28002"},
                        {"MC_RCON_ADDRESS": "127.0.0.2:28001"}, {"MC_BACKEND_ADDRESS": "localhost:28001"}):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                install.network_settings({**settings, **invalid})

    def test_server_directory_is_validated_without_reading_world_contents(self):
        with tempfile.TemporaryDirectory() as temporary:
            server = Path(temporary) / "server"
            server.mkdir()
            world = server / "custom world"
            world.mkdir()
            data = b"valuable synthetic world data"
            (world / "level.dat").write_bytes(data)
            self.assertEqual(install.server_directory({"MC_SERVER_DIRECTORY": str(server)}), server)
            self.assertEqual((world / "level.dat").read_bytes(), data)
            for path in ("relative", "/", str(world / "level.dat")):
                with self.subTest(path=path), self.assertRaises(RuntimeError):
                    install.server_directory({"MC_SERVER_DIRECTORY": path})
            with self.assertRaises(FileNotFoundError):
                install.server_directory({"MC_SERVER_DIRECTORY": str(server / "missing")})


if __name__ == "__main__":
    unittest.main()
