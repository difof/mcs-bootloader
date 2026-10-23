import os
from pathlib import Path
import tempfile
import unittest

from manage import task_arguments


class TaskArgumentsTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.parent = self.root / "123"
        self.parent.mkdir()
        (self.parent / "exe").symlink_to("/usr/local/bin/task")

    def read(self, argv):
        (self.parent / "cmdline").write_bytes(b"\0".join(os.fsencode(item) for item in argv) + b"\0")
        return task_arguments(self.root, 123)

    def test_original_arguments_preserve_templates_json_and_shell_text(self):
        args = ["tellraw", "@a", '{"text":"{{.FOO}} and $(touch absent) `literal`; &"}', "", "--"]
        self.assertEqual(self.read(["task", "--taskfile", "Taskfile.yml", "command", "--", *args]), args)

    def test_missing_separator_means_no_command_arguments(self):
        self.assertEqual(self.read(["task", "command"]), [])
        self.assertEqual(self.read(["task", "command", "--"]), [])

    def test_unexpected_parent_or_incomplete_argv_fails_closed(self):
        (self.parent / "cmdline").write_bytes(b"task\0command\0--\0list")
        with self.assertRaisesRegex(RuntimeError, "incomplete"):
            task_arguments(self.root, 123)
        (self.parent / "exe").unlink()
        (self.parent / "exe").symlink_to("/bin/bash")
        with self.assertRaisesRegex(RuntimeError, "invoke task command"):
            task_arguments(self.root, 123)


if __name__ == "__main__":
    unittest.main()
