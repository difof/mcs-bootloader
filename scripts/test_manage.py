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


if __name__ == "__main__":
    unittest.main()
