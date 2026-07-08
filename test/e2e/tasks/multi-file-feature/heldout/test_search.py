"""Acceptance test for the new `search` feature — stdlib unittest, no network.

Fails in the seed workspace (search_notes / the search command do not exist yet)
and passes once the feature spans storage.py + cli.py. Grader overlays this
pristine copy so a deleted/weakened in-workspace copy cannot help an agent.
"""
import importlib
import os
import tempfile
import unittest


class TestSearch(unittest.TestCase):
    def setUp(self):
        fd, self.tmp = tempfile.mkstemp(suffix=".json")
        os.close(fd)
        with open(self.tmp, "w") as f:
            f.write("[]")
        os.environ["NOTES_FILE"] = self.tmp
        # Import fresh each test so the modules pick up env / new attributes.
        self.storage = importlib.import_module("storage")
        self.cli = importlib.import_module("cli")
        importlib.reload(self.storage)
        importlib.reload(self.cli)
        self.storage.add_note("Buy Milk")
        self.storage.add_note("call Bob")
        self.storage.add_note("milk the cow")

    def tearDown(self):
        os.unlink(self.tmp)

    def test_search_case_insensitive(self):
        res = self.storage.search_notes("MILK")
        self.assertEqual(sorted(res), sorted(["Buy Milk", "milk the cow"]))

    def test_search_substring(self):
        self.assertEqual(self.storage.search_notes("bob"), ["call Bob"])

    def test_search_no_match(self):
        self.assertEqual(self.storage.search_notes("zzz"), [])

    def test_search_command_wired(self):
        # The CLI must recognise the search sub-command (returns 0, not "unknown").
        self.assertEqual(self.cli.main(["search", "cow"]), 0)


if __name__ == "__main__":
    unittest.main()
