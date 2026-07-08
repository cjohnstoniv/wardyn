"""HIDDEN acceptance test for issue #42 — stdlib unittest, no network.

This file is NOT present in the workspace; the grader overlays it. It encodes the
exact expected behaviour from the issue, including the unicode and dash-collapse
edge cases.
"""
import unittest

from textutil import slugify


class TestSlugify(unittest.TestCase):
    def test_basic(self):
        self.assertEqual(slugify("Hello World"), "hello-world")

    def test_accents_folded(self):
        self.assertEqual(slugify("Café Münchraum"), "cafe-munchraum")

    def test_unicode_no_crash(self):
        self.assertEqual(slugify("Ünïcödé"), "unicode")

    def test_collapse_separators(self):
        self.assertEqual(slugify("a  b"), "a-b")

    def test_trim_and_collapse_dashes(self):
        self.assertEqual(slugify("--Hey--There--"), "hey-there")

    def test_trailing_punct(self):
        self.assertEqual(slugify("Wow!!!"), "wow")

    def test_empty(self):
        self.assertEqual(slugify(""), "")


if __name__ == "__main__":
    unittest.main()
