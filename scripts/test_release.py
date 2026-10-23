import io
from pathlib import Path
import tarfile
import tempfile
import unittest

import release


class ReleaseTests(unittest.TestCase):
    def test_only_stable_semver_is_accepted(self):
        for version in ("v0.0.0", "v0.2.1", "v10.20.300"):
            self.assertEqual(release.checked_version(version), version)
        for version in ("dev", "0.2.1", "v01.2.1", "v1.02.1", "v1.2.01", "v1.2.3-rc.1", "v1.2.3+build", "v1.2.3\n", "v1.2.3;echo bad"):
            with self.subTest(version=version), self.assertRaises(ValueError):
                release.checked_version(version)

    def test_changelog_selects_exact_version_then_unreleased(self):
        text = "# Changelog\n\n## Unreleased\n\n- Future work.\n\n## [0.3.0] - 2026-10-23\n\n### Changed\n\n- Released work.\n\n## v0.2.0\n\n- Older work.\n"
        self.assertEqual(release.changelog_section(text, "v0.3.0"), "### Changed\n\n- Released work.")
        self.assertEqual(release.changelog_section(text, "v0.4.0"), "- Future work.")
        with self.assertRaises(ValueError):
            release.changelog_section("# Changelog\n\n## Unreleased\n", "v1.0.0")

    def test_manifest_excludes_untracked_and_rejects_secret_or_symlink(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in release.REQUIRED_FILES:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("public source")
            (root / ".env").write_text("private credential")
            names = list(release.REQUIRED_FILES)
            self.assertNotIn(".env", release.package_files(root, names))
            with self.assertRaises(ValueError):
                release.package_files(root, names + [".env"])
            (root / "README.md").unlink()
            (root / "README.md").symlink_to(root / ".env")
            with self.assertRaises(ValueError):
                release.package_files(root, names)

    def test_archive_is_reproducible_and_normalizes_modes_and_owners(self):
        files = [("bin/mcs-bootloader", b"test binary", True), (".env.example", b"sample only", False)]
        with tempfile.TemporaryDirectory() as temporary:
            first, second = Path(temporary) / "a.tar.gz", Path(temporary) / "b.tar.gz"
            release.write_archive(first, "mcs-bootloader_v0.3.0_linux_amd64", files, 1000000000)
            release.write_archive(second, "mcs-bootloader_v0.3.0_linux_amd64", list(reversed(files)), 1000000000)
            self.assertEqual(first.read_bytes(), second.read_bytes())
            with tarfile.open(fileobj=io.BytesIO(first.read_bytes()), mode="r:gz") as archive:
                members = archive.getmembers()
                self.assertEqual(len(members), 2)
                for member in members:
                    self.assertTrue(member.isfile())
                    self.assertEqual((member.uid, member.gid, member.mtime), (0, 0, 1000000000))
                    self.assertEqual(member.mode, 0o755 if "/bin/" in member.name else 0o644)


if __name__ == "__main__":
    unittest.main()
