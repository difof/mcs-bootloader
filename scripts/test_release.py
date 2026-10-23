import hashlib
import json
from pathlib import Path
import struct
import tempfile
import unittest
from unittest import mock

import release


class ReleaseTests(unittest.TestCase):
    def test_only_stable_semver_is_accepted(self):
        for version in ("v0.0.0", "v0.2.1", "v10.20.300"):
            self.assertEqual(release.checked_version(version), version)
        for version in ("dev", "0.2.1", "v01.2.1", "v1.02.1", "v1.2.01", "v1.2.3-rc.1", "v1.2.3+build", "v1.2.3\n", "v1.2.3;echo bad"):
            with self.subTest(version=version), self.assertRaises(ValueError):
                release.checked_version(version)

    def make_repository(self, root):
        release.run(["git", "init", "--initial-branch=master"], root=root)
        release.run(["git", "config", "user.name", "Release test"], root=root)
        release.run(["git", "config", "user.email", "release-test@example.invalid"], root=root)

    def commit(self, root, subject):
        release.run(["git", "commit", "--allow-empty", "-m", subject], root=root)
        return release.run(["git", "rev-parse", "HEAD"], root=root)

    def test_first_changelog_uses_complete_committed_history(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.make_repository(root)
            first = self.commit(root, "Initial gateway")
            second = self.commit(root, "Literal `configuration` and <details>")
            release.run(["git", "tag", "v0.3.0"], root=root)
            self.assertEqual(release.git_changelog("v0.3.0", root),
                             (None, [(first, "Initial gateway"),
                                     (second, "Literal `configuration` and <details>")]))

    def test_changelog_selects_only_previous_reachable_stable_version(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.make_repository(root)
            first = self.commit(root, "Initial gateway")
            release.run(["git", "tag", "v0.2.0"], root=root)
            self.commit(root, "Published work")
            release.run(["git", "tag", "-a", "v0.3.0", "-m", "Published release"], root=root)
            expected = self.commit(root, "New telemetry")
            for tag in ("v0.4.0", "v0.3.9-rc.1", "v0.03.9", "v99.0.0"):
                release.run(["git", "tag", tag], root=root)
            release.run(["git", "checkout", "-b", "unmerged", first], root=root)
            self.commit(root, "Unmerged work")
            release.run(["git", "tag", "v0.3.9"], root=root)
            release.run(["git", "checkout", "master"], root=root)
            self.assertEqual(release.git_changelog("v0.4.0", root),
                             ("v0.3.0", [(expected, "New telemetry")]))

    def test_changelog_rejects_empty_range_and_shallow_history(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "source"
            root.mkdir()
            self.make_repository(root)
            self.commit(root, "Published work")
            release.run(["git", "tag", "v0.3.0"], root=root)
            with self.assertRaisesRegex(ValueError, "no new commits"):
                release.git_changelog("v0.4.0", root)
            self.commit(root, "New work")
            clone = Path(temporary) / "shallow"
            release.run(["git", "clone", "--depth=1", root.as_uri(), str(clone)], root=root)
            with self.assertRaisesRegex(ValueError, "full Git history"):
                release.git_changelog("v0.4.0", clone)

    def test_notes_render_subjects_without_removed_boilerplate(self):
        commit = "a" * 40
        metadata = {"commit": commit, "go_toolchain": "go version go1.24.6 linux/amd64"}
        notes = release.release_notes("v0.4.0", [(commit, "[click](bad) <script> `code`")],
                                      metadata, "owner/repository", "v0.3.0")
        self.assertTrue(notes.startswith("## Changelog\n"))
        self.assertIn("compare/v0.3.0...v0.4.0", notes)
        self.assertIn(r"\[click\]\(bad\) &lt;script&gt; \`code\`", notes)
        self.assertIn(f"https://github.com/owner/repository/commit/{commit}", notes)
        for forbidden in ("## Highlights", "## Downloads", "## Install or update", "<script>",
                          "Generated automatically", "There is no manually maintained changelog file",
                          "highest earlier stable version", "Automated tests run on amd64"):
            self.assertNotIn(forbidden, notes)
        first = release.release_notes("v0.3.0", [(commit, "First release")], metadata, "owner/repository")
        self.assertIn("/commits/v0.3.0", first)

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
            self.assertNotIn("CHANGELOG.md", release.package_files(root, names + ["CHANGELOG.md"]))
            with self.assertRaises(ValueError):
                release.package_files(root, names + [".env"])
            with self.assertRaises(ValueError):
                release.package_files(root, names + [".mcs-bootloader-installation.json"])
            (root / "README.md").unlink()
            (root / "README.md").symlink_to(root / ".env")
            with self.assertRaises(ValueError):
                release.package_files(root, names)

    def test_package_contains_only_two_executables_metadata_and_checksums(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.make_repository(root)
            for name in release.REQUIRED_FILES:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("fixture source")
            (root / ".gitignore").write_text("/dist/\n")
            release.run(["git", "add", "."], root=root)
            self.commit(root, "Ship standalone operator commands")
            original_run = release.run
            def fake_build(argv, *, root=root, env=None):
                if argv[:2] == ["go", "version"]:
                    return "go version go1.24.6 linux/amd64"
                if argv[:2] == ["go", "build"]:
                    header = bytearray(20)
                    header[:6] = b"\x7fELF\x02\x01"
                    struct.pack_into("<H", header, 18, release.ARCHITECTURES[env["GOARCH"]])
                    Path(argv[argv.index("-o") + 1]).write_bytes(header)
                    self.assertEqual(env["CGO_ENABLED"], "0")
                    self.assertEqual(env["GOOS"], "linux")
                    return ""
                return original_run(argv, root=root, env=env)
            with mock.patch.object(release, "run", side_effect=fake_build):
                release.package("v0.5.0", root)
            destination = root / "dist/v0.5.0"
            names = {"mcs-bootloader_v0.5.0_linux_amd64", "mcs-bootloader_v0.5.0_linux_arm64",
                     "build-info.json", "SHA256SUMS", "RELEASE_NOTES.md"}
            self.assertEqual({p.name for p in destination.iterdir()}, names)
            checksums = (destination / "SHA256SUMS").read_text().splitlines()
            self.assertEqual(len(checksums), 3)
            for line in checksums:
                digest, name = line.split("  ")
                self.assertEqual(digest, hashlib.sha256((destination / name).read_bytes()).hexdigest())
            for architecture in release.ARCHITECTURES:
                binary = destination / f"mcs-bootloader_v0.5.0_linux_{architecture}"
                release.validate_binary(binary, architecture)
                self.assertEqual(binary.stat().st_mode & 0o777, 0o755)
            info = json.loads((destination / "build-info.json").read_text())
            self.assertEqual(info["distribution"], "standalone-binary")
            self.assertEqual(info["operator_assets"], "embedded")


if __name__ == "__main__":
    unittest.main()
