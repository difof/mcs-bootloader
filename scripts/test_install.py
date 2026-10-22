from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import install


class InstallerValidationTests(unittest.TestCase):
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
