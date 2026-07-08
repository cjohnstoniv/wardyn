"""Text utilities."""
import re


def slugify(text):
    # Naive: doesn't fold accents to ASCII and doesn't collapse separator runs.
    # Doesn't crash, but still fails the issue spec (accents + doubled dashes).
    return re.sub(r"[^a-z0-9]", "-", text.lower()).strip("-")
