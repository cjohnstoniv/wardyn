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
