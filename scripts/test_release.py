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

    def test_notes_render_subjects_as_text_and_explain_generation(self):
        commit = "a" * 40
        metadata = {"commit": commit, "go_toolchain": "go version go1.24.6 linux/amd64"}
        notes = release.release_notes("v0.4.0", [(commit, "[click](bad) <script> `code`")],
                                      metadata, "owner/repository", "v0.3.0")
        self.assertTrue(notes.startswith("## Changelog\n"))
        self.assertIn("compare/v0.3.0...v0.4.0", notes)
        self.assertIn("Generated automatically from Git commit subjects", notes)
        self.assertIn(r"\[click\]\(bad\) &lt;script&gt; \`code\`", notes)
        self.assertIn(f"https://github.com/owner/repository/commit/{commit}", notes)
        for forbidden in ("## Highlights", "## Downloads", "## Install or update", "<script>"):
            self.assertNotIn(forbidden, notes)
        first = release.release_notes("v0.3.0", [(commit, "First release")], metadata, "owner/repository")
        self.assertIn("/commits/v0.3.0", first)
        self.assertIn("complete history through `v0.3.0`", first)

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
