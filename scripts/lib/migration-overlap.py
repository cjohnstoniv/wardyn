#!/usr/bin/env python3
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# migration-overlap.py BANDS BASE_REF [--line=REF ...] -- FILE...
# migration-overlap.py --selftest
#
# The table-overlap pass of scripts/check-migration-numbers.sh, over the band migrations a branch adds (FILE, in the
# working tree). Band files on two lines can reach a database in either order, whatever the follows column says: a
# file can land in a band after a following line has started, and that line never carries it. An older-band file
# already on the base runs first wherever the base goes, so it is never a conflict. So:
#   - a new file must not touch a table that a file of a newer such band, on the base or on another line, alters;
#   - a new file must not alter a table that a file of an older such band touches while that file is on another line
#     and not yet on the base. It waits for that file's forward-port, after which every database runs the two in one
#     order.
# An older-band file already on the base is never a conflict for a new newer-band file. New files are not compared with
# each other: they arrive together, so every database runs them in lexical order.
#
# alter: CREATE, ALTER or DROP TABLE; CREATE INDEX ... ON; CREATE or DROP TRIGGER ... ON; CREATE, ALTER or DROP POLICY
# ... ON; and, as the objects fn:NAME, view:NAME and type:NAME, CREATE [OR REPLACE] or ALTER or DROP of a FUNCTION,
# PROCEDURE, VIEW or TYPE. touch: an alter, or INSERT INTO, UPDATE ... SET, DELETE FROM, TRUNCATE. Comments are removed first; text in
# dollar quotes (DO blocks, function bodies) is read like any other, and a table named only through dynamic SQL
# (EXECUTE format(...)) is not seen. A name loses its schema and quotes and is lower-cased, so it can only over-match.
# Exit 0 no conflict, 1 a conflict (each printed), 2 usage.
import os
import re
import subprocess
import sys

MIGRATIONS = "internal/db/migrations"
IDENT = r'(?:"[^"]+"|\w+)'
NAME = r"(?:ONLY\s+)?(" + IDENT + r"(?:\s*\.\s*" + IDENT + r")?)"
ALTER = [(pre, re.compile(p, re.I)) for pre, p in (
    ("", r"\bCREATE\s+(?:(?:GLOBAL|LOCAL)\s+)?(?:(?:TEMP|TEMPORARY|UNLOGGED)\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?" + NAME),
    ("", r"\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?" + NAME),
    ("", r"\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?" + NAME),
    ("", r"\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(?:" + IDENT + r"\s+)?ON\s+" + NAME),
    ("", r"\b(?:CREATE\s+(?:OR\s+REPLACE\s+)?(?:CONSTRAINT\s+)?|DROP\s+)TRIGGER\s+(?:IF\s+EXISTS\s+)?" + IDENT
     + r"\s+(?:[^;]*?\s)?ON\s+" + NAME),
    ("", r"\b(?:CREATE|ALTER|DROP)\s+POLICY\s+(?:IF\s+EXISTS\s+)?" + IDENT + r"\s+ON\s+" + NAME),
    ("fn:", r"\bCREATE\s+(?:OR\s+REPLACE\s+)?(?:FUNCTION|PROCEDURE)\s+" + NAME),
    ("fn:", r"\b(?:ALTER|DROP)\s+(?:FUNCTION|PROCEDURE)\s+(?:IF\s+EXISTS\s+)?" + NAME),
    ("view:", r"\bCREATE\s+(?:OR\s+REPLACE\s+)?VIEW\s+" + NAME),
    ("view:", r"\b(?:ALTER|DROP)\s+VIEW\s+(?:IF\s+EXISTS\s+)?" + NAME),
    ("type:", r"\b(?:CREATE|ALTER|DROP)\s+TYPE\s+(?:IF\s+EXISTS\s+)?" + NAME),
)]
DML = [re.compile(p, re.I) for p in (
    r"\bINSERT\s+INTO\s+" + NAME,
    r"\bUPDATE\s+" + NAME + r"(?:\s+(?:AS\s+)?\w+)?\s+SET\b",
    r"\bDELETE\s+FROM\s+" + NAME,
    r"\bTRUNCATE\s+(?:TABLE\s+)?" + NAME,
)]
COMMENT = re.compile(r"--[^\n]*|/\*.*?\*/", re.S)
KEYWORDS = {"only", "if", "on", "set", "table", "not", "exists"}


def norm(name):
    return re.split(r"\s*\.\s*", name)[-1].strip('"').lower()


def tables(sql):
    """(alters, touches): the objects sql alters (tables, and fn:/view:/type: names), and those it alters or writes rows of."""
    text = COMMENT.sub(" ", sql)
    alters = {pre + norm(m.group(1)) for pre, r in ALTER for m in r.finditer(text)} - KEYWORDS
    return alters, alters | ({norm(m.group(1)) for r in DML for m in r.finditer(text)} - KEYWORDS)


def read_bands(path):
    rows = []
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.rstrip("\n")
            if line and not line.startswith("#"):
                first, last, open_, _branches, follows, _tags = line.split("\t")
                rows.append((int(first, 10), int(last, 10), 0 if follows == "-" else int(follows, 10), open_ == "yes"))
    return rows


def band_of(name, rows):
    m = re.match(r"([0-9]{4})_", name)
    if m:
        p = int(m.group(1), 10)
        for i, (first, last, _follows, _open) in enumerate(rows):
            if first <= p <= last:
                return i
    return None


def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout


def main(argv):
    if argv == ["--selftest"]:
        return selftest()
    if "--" not in argv or argv.index("--") < 2:
        print("usage: migration-overlap.py BANDS BASE_REF [--line=REF ...] -- FILE...", file=sys.stderr)
        return 2
    cut = argv.index("--")
    rows, base_ref = read_bands(argv[0]), argv[1]
    lines = [a[len("--line="):] for a in argv[2:cut]]
    new = argv[cut + 1:]
    new_names = {os.path.basename(f) for f in new}
    on_base, others = set(), {}  # others: name -> the ref it is read from, for every band file this branch does not add
    for i, ref in enumerate([base_ref, *lines]):
        for path in git("ls-tree", "--name-only", "-r", ref, "--", MIGRATIONS).split():
            name = os.path.basename(path)
            if band_of(name, rows) is None or name in new_names:
                continue
            if i == 0:
                on_base.add(name)
            others.setdefault(name, ref)
    bad = 0
    for f in new:
        name = os.path.basename(f)
        x = band_of(name, rows)
        if x is None:
            continue
        with open(f, encoding="utf-8") as fh:
            f_alters, f_touches = tables(fh.read())
        for other, ref in sorted(others.items()):
            y = band_of(other, rows)
            if y == x:
                continue
            o_alters, o_touches = tables(git("show", f"{ref}:{MIGRATIONS}/{other}"))
            hit = f_touches & o_alters if y > x else (f_alters & o_touches if other not in on_base else set())
            if hit and y > x:
                print(f"migration-overlap: {f} touches {', '.join(sorted(hit))}, which {other} ({ref}) alters. A database "
                      f"can run {name} after {other} (a forward-port reaches databases that already ran the newer band), "
                      f"so the result would depend on the order. Leave the tables the newer line altered alone.",
                      file=sys.stderr)
                bad += 1
            elif hit:
                print(f"migration-overlap: {f} alters {', '.join(sorted(hit))}, which {other} ({ref}, not yet on the "
                      f"base) touches. Add it once {other}'s forward-port is on the base, so every database runs the "
                      f"two in one order.", file=sys.stderr)
                bad += 1
    return 1 if bad else 0


SELFTEST = [
    ("ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS x text;", {"workspaces"}, {"workspaces"}),
    ("-- ALTER TABLE ghosts ADD COLUMN y text;\n/* DROP TABLE ghosts; */ SELECT 1;", set(), set()),
    ('ALTER TABLE ONLY "public"."workspaces" ADD COLUMN z text;', {"workspaces"}, {"workspaces"}),
    ("CREATE UNIQUE INDEX IF NOT EXISTS i ON ONLY agent_runs (id);", {"agent_runs"}, {"agent_runs"}),
    ("CREATE TRIGGER t BEFORE UPDATE OR DELETE ON audit_events FOR EACH ROW EXECUTE FUNCTION f();",
     {"audit_events"}, {"audit_events"}),
    ("DROP TRIGGER IF EXISTS t ON audit_events;", {"audit_events"}, {"audit_events"}),
    ("UPDATE agent_runs SET repo = repo;", set(), {"agent_runs"}),
    ("INSERT INTO k (a) VALUES (1) ON CONFLICT (a) DO UPDATE SET a = 2;", set(), {"k"}),
    ("DELETE FROM secrets WHERE false; TRUNCATE TABLE scratch;", set(), {"secrets", "scratch"}),
    ("DO $$ BEGIN ALTER TABLE runners ADD COLUMN IF NOT EXISTS q int; END $$;", {"runners"}, {"runners"}),
    ("CREATE TABLE IF NOT EXISTS fresh (id int);", {"fresh"}, {"fresh"}),
    ("CREATE POLICY p ON grants USING (true);", {"grants"}, {"grants"}),
    ("CREATE OR REPLACE FUNCTION public.audit_chain(a int) RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql;\n"
     "DROP FUNCTION IF EXISTS old_fn(int); ALTER PROCEDURE p() RENAME TO q;",
     {"fn:audit_chain", "fn:old_fn", "fn:p"}, {"fn:audit_chain", "fn:old_fn", "fn:p"}),
    ("CREATE OR REPLACE VIEW v AS SELECT 1; ALTER VIEW IF EXISTS w RENAME TO x; DROP VIEW v2;",
     {"view:v", "view:w", "view:v2"}, {"view:v", "view:w", "view:v2"}),
    ("CREATE TYPE mood AS ENUM ('a'); ALTER TYPE mood ADD VALUE 'b'; DROP TYPE IF EXISTS t2;",
     {"type:mood", "type:t2"}, {"type:mood", "type:t2"}),
]


def selftest():
    bad = 0
    for sql, want_alters, want_touches in SELFTEST:
        got = tables(sql)
        if got != (want_alters, want_touches):
            print(f"migration-overlap selftest: {sql!r}: alters {sorted(got[0])} touches {sorted(got[1])}, want "
                  f"alters {sorted(want_alters)} touches {sorted(want_touches)}", file=sys.stderr)
            bad += 1
    print("migration-overlap selftest: " + ("OK" if not bad else f"{bad} case(s) failed"))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
