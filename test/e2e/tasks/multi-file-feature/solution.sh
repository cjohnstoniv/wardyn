#!/bin/sh
# ORACLE solution for multi-file-feature.
#
# Runs INSIDE the sandbox as the agent, cwd = the mounted workspace ($PWD).
# Adds the `search` feature across BOTH files by rewriting them via heredocs
# (deterministic; preserves add/list). POSIX sh, no network.
set -u

cat > storage.py <<'PY'
"""JSON-backed note storage.

The notes file path is read lazily from $NOTES_FILE (default notes.json) so tests
can point it at a temp file.
"""
import json
import os


def _path():
    return os.environ.get("NOTES_FILE", "notes.json")


def _load():
    path = _path()
    if not os.path.exists(path):
        return []
    with open(path) as f:
        return json.load(f)


def _save(notes):
    with open(_path(), "w") as f:
        json.dump(notes, f)


def add_note(text):
    notes = _load()
    notes.append(text)
    _save(notes)
    return len(notes)


def list_notes():
    return _load()


def search_notes(term):
    """Return notes containing ``term`` as a case-insensitive substring."""
    needle = term.lower()
    return [n for n in list_notes() if needle in n.lower()]
PY

cat > cli.py <<'PY'
"""Notes CLI: dispatches add/list/search sub-commands to storage.py."""
import sys

import storage


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv:
        print("usage: cli.py <add|list|search> ...")
        return 1
    cmd, rest = argv[0], argv[1:]
    if cmd == "add":
        storage.add_note(" ".join(rest))
        return 0
    if cmd == "list":
        for note in storage.list_notes():
            print(note)
        return 0
    if cmd == "search":
        for note in storage.search_notes(" ".join(rest)):
            print(note)
        return 0
    print("unknown command: %s" % cmd)
    return 1


if __name__ == "__main__":
    sys.exit(main())
PY

echo "solution.sh: added search feature to storage.py + cli.py" >&2
