from pathlib import Path
import json
import shutil
import subprocess
import tempfile
import unittest

import install


class InstallerValidationTests(unittest.TestCase):
    def env_file(self, directory, additions="", password="  private $() `literal` #= café 🚀  "):
        path = Path(directory) / ".env"
        text = ("# preserve my comments\nMC_RCON_PASSWORD=" + json.dumps(password, ensure_ascii=False)
                + "\nMC_SERVER_ARGFILE=@libraries/loader/unix_args.txt\n" + additions)
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
            path = self.env_file(temporary, "MC_SERVICE_USER=compute\nMC_SERVER_DIRECTORY=/home/compute/minecraft/server\n")
            original = path.read_bytes()
            settings, raw, metadata = install.installation_settings(path)
            self.assertEqual(raw, original)
            self.assertEqual(settings["MC_RCON_PASSWORD"], "  private $() `literal` #= café 🚀  ")
            self.assertEqual(settings["MC_SERVICE_USER"], "compute")
            self.assertEqual(settings["MC_CONFIG"], "/etc/mc-gateway/.env")
            self.assertEqual(settings["MC_JVM_ARGFILE"], "@user_jvm_args.txt")
            self.assertEqual(metadata.st_uid, path.stat().st_uid)
            self.assertNotIn("MC_RCON_PASSWORD_FILE", settings)

    def test_installer_requires_password_private_permissions_and_fixed_topology(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = self.env_file(temporary)
            path.chmod(0o644)
            with self.assertRaisesRegex(RuntimeError, "private"):
                install.installation_settings(path)
            path.chmod(0o600)
            path.write_text("MC_SERVER_ARGFILE=@libraries/loader/unix_args.txt\n")
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
            self.assertFalse(list(base.glob(".mc-gateway-*")))

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

    def test_existing_argfiles_are_required_inside_the_server(self):
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            server = base / "server"
            server.mkdir()
            (server / "args.txt").write_text("unchanged loader arguments\n")
            self.assertEqual(install.server_argument_file(server, "@args.txt", "MC_SERVER_ARGFILE"), server / "args.txt")
            (base / "outside.txt").write_text("not server arguments\n")
            (server / "escape.txt").symlink_to(base / "outside.txt")
            for argument in ("args.txt", "@@args.txt", "@../outside.txt", "@/absolute", "@escape.txt"):
                with self.subTest(argument=argument), self.assertRaises(RuntimeError):
                    install.server_argument_file(server, argument, "MC_SERVER_ARGFILE")

    def test_systemd_working_directory_is_unquoted_with_literal_specifiers(self):
        self.assertEqual(install.systemd_working_directory("/srv/example/minecraft/server"),
                         "/srv/example/minecraft/server")
        self.assertEqual(install.systemd_working_directory("/srv/minecraft/100%"),
                         "/srv/minecraft/100%%")
        for invalid in ("relative/world", '"/srv/world"', "/srv/a b", "/srv/a\\b",
                        "/srv/a'b", "/srv/world\nExecStart=/evil", "/srv/a\x7f"):
            with self.subTest(path=invalid), self.assertRaises(RuntimeError):
                install.systemd_working_directory(invalid)

    @unittest.skipUnless(shutil.which("systemd-analyze"), "requires systemd's actual unit parser")
    def test_rendered_working_directory_passes_systemd_unit_verification(self):
        with tempfile.TemporaryDirectory() as temporary:
            unit = Path(temporary) / "mc-gateway-working-directory-test.service"
            for path in ("/srv/example/minecraft/server", "/srv/minecraft/100%"):
                unit.write_text("[Service]\nType=oneshot\nExecStart=/usr/bin/true\nWorkingDirectory="
                                + install.systemd_working_directory(path) + "\n")
                result = subprocess.run([shutil.which("systemd-analyze"), "verify", str(unit)],
                                        capture_output=True, text=True, timeout=20)
                self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
