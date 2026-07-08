#!/bin/sh
# ORACLE solution for github-issue-fix.
#
# Runs INSIDE the sandbox as the agent, cwd = the mounted workspace ($PWD).
# Writes a correct slugify(): NFKD-normalise then drop non-ASCII (folds accents),
# lowercase, replace every run of non-alphanumerics with a single dash, and trim
# leading/trailing dashes. Stdlib only (unicodedata + re). POSIX sh, no network.
set -u

cat > textutil.py <<'PY'
"""Text utilities."""
import re
import unicodedata


def slugify(text):
    """Turn a title into a URL slug (issue #42 spec).

    Folds accented characters to ASCII, lowercases, collapses every run of
    non-alphanumeric characters into a single dash, and trims edge dashes.
    Never raises on non-ASCII input.
    """
    text = unicodedata.normalize("NFKD", text)
    text = text.encode("ascii", "ignore").decode("ascii")
    text = text.lower()
    text = re.sub(r"[^a-z0-9]+", "-", text)
    return text.strip("-")
PY

echo "solution.sh: rewrote slugify() to fold unicode and collapse separators" >&2
