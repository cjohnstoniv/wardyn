#!/usr/bin/env python3
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
"""Split one Go test binary's top-level tests into n `-test.run` slices, and check a split run.

  test_slices.py split <n>
      Reads one top-level name per line on stdin (the binary's `-test.list=.*`) and prints n patterns, one per line.
      Each is ^(p1|p2|...) over name prefixes, so it stays a few kilobytes however many tests there are: a list of
      whole names passes the kernel's 128 KiB limit on one argument. The prefixes are prefix-free, so a name matches
      exactly one; the same input always gives the same output.
  test_slices.py check <all.list> <skipped.list> <sel_1.list>,<out_1.json> [<sel_i.list>,<out_i.json>...]
      all.list: every top-level name. skipped.list: names quarantined as whole tests. sel_i.list: what the binary's
      own matcher selects for slice i (`-test.list=<pattern i>`). out_i.json: slice i's test2json stream.
      Exit 1 when a name is selected by no slice or by two, when a selected name (not quarantined, not a benchmark)
      has no pass, fail or skip in its own slice, or when a slice has an outcome for a name it did not select.
"""
import json
import re
import sys

NAME = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")


def groups(names, n):
    """Prefix groups: key -> members. A key is a prefix followed by "." (a longer name) or a whole name with "$"."""
    target = max(1, -(-len(names) // (4 * n)))
    out = {}

    def place(members, k):
        by = {}
        for name in members:
            key = name[:k] + "." if len(name) > k else name + "$"
            by.setdefault(key, []).append(name)
        for key, sub in by.items():
            if len(sub) > target and key.endswith("."):
                place(sub, k + 1)
            else:
                out[key] = sub

    place(sorted(set(names)), 6)
    return out


def split(n):
    names = [line.strip() for line in sys.stdin if line.strip()]
    if n < 1 or not names:
        sys.exit("test_slices.py: need n >= 1 and at least one test name")
    for name in names:
        if not NAME.fullmatch(name):
            sys.exit(f"test_slices.py: not a top-level test name: {name!r}")
    g = groups(names, n)
    if len(g) < n:
        sys.exit(f"test_slices.py: {len(g)} prefix groups cannot fill {n} slices")
    keys = [[] for _ in range(n)]
    sizes = [0] * n
    for key in sorted(g, key=lambda k: (-len(g[k]), k)):
        i = min(range(n), key=lambda j: (sizes[j], j))
        keys[i].append(key)
        sizes[i] += len(g[key])
    for ks in keys:
        print("^(" + "|".join(sorted(ks)) + ")")


def read_names(path):
    with open(path) as f:
        return [line.strip() for line in f if line.strip()]


def outcomes(path):
    seen = set()
    with open(path) as f:
        for line in f:
            try:
                ev = json.loads(line)
            except ValueError:
                continue
            test = ev.get("Test", "")
            if test and "/" not in test and ev.get("Action") in ("pass", "fail", "skip"):
                seen.add(test)
    return seen


def check(argv):
    listed = set(read_names(argv[0]))
    skipped = set(read_names(argv[1]))
    slices = [pair.split(",", 1) for pair in argv[2:]]
    bad = []
    owner = {}
    for i, (sel_path, _) in enumerate(slices, 1):
        for name in read_names(sel_path):
            if name not in listed:
                bad.append(f"slice {i} selects {name}, which the binary does not list")
            elif name in owner:
                bad.append(f"{name} is selected by slices {owner[name]} and {i}")
            else:
                owner[name] = i
    for name in sorted(listed - owner.keys()):
        bad.append(f"{name} is selected by no slice")
    ran = 0
    for i, (sel_path, out_path) in enumerate(slices, 1):
        sel = set(read_names(sel_path))
        got = outcomes(out_path)
        want = {t for t in sel if t not in skipped and not t.startswith("Benchmark")}
        for name in sorted(want - got):
            bad.append(f"{name} is selected by slice {i} but has no outcome in it (dropped, or the binary exited early)")
        for name in sorted(got - sel):
            bad.append(f"slice {i} ran {name}, which it did not select")
        ran += len(want & got)
    if bad:
        for b in bad:
            print(">> SPLIT: " + b, file=sys.stderr)
        sys.exit(1)
    print(f">> split: {len(slices)} slices, {len(listed)} names listed, {ran} ran once, "
          f"{len(listed & skipped)} quarantined")


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "split" and sys.argv[2].isdigit():
        split(int(sys.argv[2]))
    elif len(sys.argv) >= 5 and sys.argv[1] == "check":
        check(sys.argv[2:])
    else:
        sys.exit(__doc__)
