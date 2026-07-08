"""Text utilities."""


def slugify(text):
    """Turn a title into a URL slug.

    BUG (issue #42): encode('ascii') raises UnicodeEncodeError on any non-ASCII
    input, and replacing each separator char individually leaves doubled dashes.
    """
    text = text.lower()
    text = text.encode("ascii").decode("ascii")  # crashes on non-ASCII
    out = ""
    for ch in text:
        out += ch if ch.isalnum() else "-"
    return out.strip("-")
