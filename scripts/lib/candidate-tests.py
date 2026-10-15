#!/usr/bin/env python3
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# candidate-tests.py — the two test lists scripts/lane-preflight.sh --candidate builds by grep, never
# by hand, so a new guard or Postgres-gated test is picked up without anyone editing a list.
#
#   candidate-tests.py guards        every Go test whose file reads the repository tree: ci.yml's
#                                    "guard packages" pattern (a docs/threatmodel/ui/.github path or
#                                    a .md file) plus the go/parser, go/ast and directory-walking
#                                    tests that pattern misses.
#   candidate-tests.py pg DIR...     in each package directory, the Test functions that reach a
#                                    WARDYN_TEST_PG read, directly or through any helper they call.
#
# Output: one "<dir>\t<Test1|Test2|...>" line per package, ready for `go test -run '^(...)$' ./<dir>`.
# Only tagless sources matter: a test behind a build tag is simply not matched by `go test` under
# default tags.
import re
import subprocess
import sys
from collections import defaultdict

CI_GUARD = re.compile(r'"(\.\./)*(docs|threatmodel|ui|\.github)[/"]|CHANGELOG\.md|RELEASING\.md|AGENTS\.md|\.md["`]')
TREE_READ = re.compile(r'"go/(parser|ast)"|filepath\.WalkDir|filepath\.Walk\(|os\.ReadDir')
FUNC = re.compile(r'^func\s+(?:\([^)]*\)\s*)?(\w+)')


def go_files(pattern):
    out = subprocess.run(["git", "ls-files", "--cached", "--others", "--exclude-standard", "--", pattern],
                         check=True, capture_output=True, text=True).stdout
    return sorted(set(out.split()))


def read(path):
    try:
        with open(path, encoding="utf-8") as f:
            return f.read()
    except OSError:
        return ""  # listed but deleted in the working tree


def split_funcs(text):
    """Top-level funcs as (name, body); a body runs to the first line that is exactly "}"."""
    funcs, cur, name = [], [], None
    for line in text.splitlines():
        m = FUNC.match(line)
        if m and name is None:
            name, cur = m.group(1), [line]
            if line.rstrip().endswith("}"):
                funcs.append((name, line))
                name, cur = None, []
            continue
        if name is not None:
            cur.append(line)
            if line == "}":
                funcs.append((name, "\n".join(cur)))
                name, cur = None, []
    return funcs


def test_names(text):
    return [n for n, _ in split_funcs(text) if n.startswith("Test") and n != "TestMain"]


def emit(by_dir):
    for d in sorted(by_dir):
        if by_dir[d]:
            print(d + "\t" + "|".join(sorted(set(by_dir[d]))))


def guards():
    by_dir = defaultdict(list)
    for path in go_files("*_test.go"):
        text = read(path)
        if CI_GUARD.search(text) or TREE_READ.search(text):
            by_dir[path.rpartition("/")[0] or "."].extend(test_names(text))
    emit(by_dir)


def pg(dirs):
    by_dir = defaultdict(list)
    for d in dirs:
        d = d.removeprefix("./").rstrip("/") or "."
        funcs = []
        for path in go_files(("" if d == "." else d + "/") + "*.go"):
            if path.rpartition("/")[0] == ("" if d == "." else d):
                funcs.extend(split_funcs(read(path)))
        idents = [(n, body, set(re.findall(r"\w+", body))) for n, body in funcs]
        hot = {n for n, body, _ in idents if "WARDYN_TEST_PG" in body}
        while True:
            more = {n for n, _, ids in idents if n not in hot and ids & hot}
            if not more:
                break
            hot |= more
        by_dir[d] = [n for n in hot if n.startswith("Test") and n != "TestMain"]
    emit(by_dir)


if __name__ == "__main__":
    if len(sys.argv) >= 2 and sys.argv[1] == "guards":
        guards()
    elif len(sys.argv) >= 3 and sys.argv[1] == "pg":
        pg(sys.argv[2:])
    else:
        sys.exit("usage: candidate-tests.py guards | pg DIR...")
