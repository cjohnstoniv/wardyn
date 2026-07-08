"""Notes CLI: dispatches add/list sub-commands to storage.py."""
import sys

import storage


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    if not argv:
        print("usage: cli.py <add|list> ...")
        return 1
    cmd, rest = argv[0], argv[1:]
    if cmd == "add":
        storage.add_note(" ".join(rest))
        return 0
    if cmd == "list":
        for note in storage.list_notes():
            print(note)
        return 0
    print("unknown command: %s" % cmd)
    return 1


if __name__ == "__main__":
    sys.exit(main())
