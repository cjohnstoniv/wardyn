"""Regression test for the pre-existing add/list commands — stdlib unittest.

Guards against a "feature" that breaks what already worked. Overlaid from heldout
by the grader (never trusts the in-workspace copy).
"""
import importlib
import os
import tempfile
import unittest


class TestExisting(unittest.TestCase):
    def setUp(self):
        fd, self.tmp = tempfile.mkstemp(suffix=".json")
        os.close(fd)
        with open(self.tmp, "w") as f:
            f.write("[]")
        os.environ["NOTES_FILE"] = self.tmp
        self.storage = importlib.import_module("storage")
        self.cli = importlib.import_module("cli")
        importlib.reload(self.storage)
        importlib.reload(self.cli)

    def tearDown(self):
        os.unlink(self.tmp)

    def test_list_starts_empty(self):
        self.assertEqual(self.storage.list_notes(), [])

    def test_add_then_list(self):
        self.assertEqual(self.cli.main(["add", "buy", "milk"]), 0)
        self.assertIn("buy milk", self.storage.list_notes())

    def test_unknown_command_still_errors(self):
        self.assertEqual(self.cli.main(["frobnicate"]), 1)


if __name__ == "__main__":
    unittest.main()
