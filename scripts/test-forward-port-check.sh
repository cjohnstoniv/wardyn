#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-forward-port-check.sh — pins forward-port-check.sh in a throwaway repo:
# a forward-port that carries only the release delta passes; one that drops a
# main-only line (a prose line, a bullet, a budget line, a glob-named file) or
# reverts main's binary fails and names it; <V>.allow lists the removals a port
# makes on purpose (a stale, reasonless or other-version entry does not count);
# a port that edits the script or another version's allow file is refused; a
# duplicate CHANGELOG heading fails; --prev-tag picks the right baseline. The
# last block replays four real forward-ports from this repo's history (skipped
# with a printed SKIP when a clone lacks those merges or tags).
set -euo pipefail
# Throwaway repos get no background git: auto-maintenance can write under .git while the EXIT trap runs rm -rf.
export GIT_CONFIG_COUNT=3 GIT_CONFIG_KEY_0=maintenance.auto GIT_CONFIG_VALUE_0=false GIT_CONFIG_KEY_1=gc.auto GIT_CONFIG_VALUE_1=0 GIT_CONFIG_KEY_2=gc.autoDetach GIT_CONFIG_VALUE_2=false
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT/scripts/forward-port-check.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

cd "$TMP"
git init -q -b main .
git config user.name t; git config user.email t@example.invalid; git config commit.gpgsign false
mkdir -p scripts/doc-form.d
commit() { git add -A && git commit -q -m "$1"; }

printf '# Notes\n\nshared line\n' >NOTES.md
printf '# Changelog\n\n## [Unreleased]\n\n## [1.2.2]\n\n- old entry\n' >CHANGELOG.md
printf 'budget 10\n' >scripts/doc-form.d/A.budget
printf 'shared\n' >g1.md
printf 'shared\n' >'g[1].md'
printf '\0one' >bin.dat
commit base
BASEC="$(git rev-parse HEAD)"
git tag v1.2.2
git tag v1.3.0-rc.1
git tag v1.2.3-rc.1

# a release branch cut from the base, for the X.Y.0 branch-point cases
git checkout -q -b rel
printf 'r1\n' >rel.txt; commit r1; git tag v1.4.0-rc.1
printf 'r2\n' >rel.txt; commit r2; git tag v1.4.0-rc.2
printf 'r3\n' >rel.txt; commit r3; git tag v1.4.0-rc.3
printf 'r4\n' >rel.txt; commit r4; git tag v1.4.0
git checkout -q main

printf '# Notes\n\nshared line\nmain-only line\n- main-only bullet\n' >NOTES.md
printf 'budget 12\n' >scripts/doc-form.d/A.budget
printf 'shared\nmain-only g\n' >g1.md
printf '\0two' >bin.dat
commit "main-only content"
MAIN="$(git rev-parse HEAD)"

# good: release delta only (a CHANGELOG entry)
git checkout -q -b good "$MAIN"
printf '# Changelog\n\n## [Unreleased]\n\n## [1.2.3]\n\n- new entry\n\n## [1.2.2]\n\n- old entry\n' >CHANGELOG.md
commit good
GOOD="$(git rev-parse HEAD)"
branch() { git checkout -q -b "$1" "${2:-$MAIN}"; }

# bad: a forward-port that rebuilt NOTES.md from the release branch
branch bad
printf '# Notes\n\nshared line\n' >NOTES.md
commit bad
BAD="$(git rev-parse HEAD)"

branch dup "$GOOD"
printf '# Changelog\n\n## [1.2.3]\n\n- a\n\n## [1.2.3]\n\n- b\n\n## [1.2.2]\n\n- old entry\n' >CHANGELOG.md
commit dup
DUP="$(git rev-parse HEAD)"

branch nohead "$GOOD"
printf 'nothing to see\n' >CHANGELOG.md
commit nohead
NOHEAD="$(git rev-parse HEAD)"

branch budget
printf 'budget 13\n' >scripts/doc-form.d/A.budget
commit budget
BUDGET="$(git rev-parse HEAD)"

branch binrev
printf '\0one' >bin.dat
commit binrev
BINREV="$(git rev-parse HEAD)"

branch binnew
printf '\0three' >bin.dat
commit binnew
BINNEW="$(git rev-parse HEAD)"

branch glob
printf 'shared\n' >g1.md
printf 'changed\n' >'g[1].md'
commit glob
GLOB="$(git rev-parse HEAD)"

branch touchscript
mkdir -p scripts; printf '#!/bin/sh\n' >scripts/forward-port-check.sh
commit touchscript
TOUCHSCRIPT="$(git rev-parse HEAD)"

branch touchother "$GOOD"
mkdir -p scripts/forward-port-check.d; printf '# other\n' >scripts/forward-port-check.d/1.2.4.allow
commit touchother
TOUCHOTHER="$(git rev-parse HEAD)"

branch ownallow "$BUDGET"
mkdir -p scripts/forward-port-check.d
printf 'scripts/doc-form.d/A.budget\tbudget 12\tbudget: main 12 replaced by the release value 13\n' >scripts/forward-port-check.d/1.2.3.allow
commit ownallow
OWNALLOW="$(git rev-parse HEAD)"
git checkout -q main

out="$TMP/out"
run() { "$CHECK" "$@" >"$out" 2>&1; }
want_rc() {  # want_rc N what -- args...
  local want="$1" what="$2" rc=0; shift 3
  run "$@" || rc=$?
  [ "$rc" = "$want" ] || fail "$what exited $rc, want $want: $(cat "$out")"
}
allow() {  # allow V <file contents on stdin>: the working-tree allow file the check reads
  mkdir -p "$TMP/scripts/forward-port-check.d"; cat >"$TMP/scripts/forward-port-check.d/$1.allow"
}
noallow() { rm -f "$TMP"/scripts/forward-port-check.d/*.allow; }
BUD=$'scripts/doc-form.d/A.budget\tbudget 12\tbudget: main 12 replaced by the release value 13'

want_rc 0 "good forward-port" -- v1.2.2 "$MAIN" "$GOOD"
grep -q 'PASS' "$out" || fail "good forward-port printed no PASS"
want_rc 0 "good forward-port with a version and no allow file" -- --version 1.2.3 v1.2.2 "$MAIN" "$GOOD"

want_rc 1 "bad forward-port" -- v1.2.2 "$MAIN" "$BAD"
grep -qxF 'NOTES.md: main-only line' "$out" || fail "main-only line not named: $(cat "$out")"
grep -qxF 'NOTES.md: - main-only bullet' "$out" || fail "main-only bullet not named: $(cat "$out")"
want_rc 1 "bad forward-port with a version and no allow file" -- --version 1.2.3 v1.2.2 "$MAIN" "$BAD"

want_rc 1 "duplicate heading" -- v1.2.2 "$MAIN" "$DUP"
grep -q '## \[1.2.3\]' "$out" || fail "duplicate heading not named: $(cat "$out")"
want_rc 0 "a CHANGELOG with no heading" -- v1.2.2 "$MAIN" "$NOHEAD"
grep -q "no '## \[' heading" "$out" || fail "no-heading CHANGELOG was silent: $(cat "$out")"

# a budget removal is main-only content like any other: it must be listed
want_rc 1 "unlisted budget removal" -- --version 1.2.3 v1.2.2 "$MAIN" "$BUDGET"
grep -qxF 'scripts/doc-form.d/A.budget: budget 12' "$out" || fail "budget line not named: $(cat "$out")"
printf '%s\n' "$BUD" | allow 1.2.3
want_rc 0 "listed budget removal" -- --version 1.2.3 v1.2.2 "$MAIN" "$BUDGET"
want_rc 1 "listed budget removal without --version" -- v1.2.2 "$MAIN" "$BUDGET"
printf '# comment\n\n%s\n' "$BUD" | allow 1.2.3
want_rc 0 "comments and blank lines in the allow file" -- --version 1.2.3 v1.2.2 "$MAIN" "$BUDGET"
printf '%s\n' "$BUD" | allow 1.2.4
noallow; printf '%s\n' "$BUD" | allow 1.2.4
want_rc 1 "another version's allow file" -- --version 1.2.3 v1.2.2 "$MAIN" "$BUDGET"

# the bad port's lines, listed with a reason, pass; a stale entry beside them fails
printf 'NOTES.md\tmain-only line\trebuilt from the release branch\nNOTES.md\t- main-only bullet\trebuilt from the release branch\n' | allow 1.2.3
want_rc 0 "listed prose removals" -- --version 1.2.3 v1.2.2 "$MAIN" "$BAD"
printf 'NOTES.md\tmain-only line\ta\nNOTES.md\t- main-only bullet\ta\nNOTES.md\tshared line\tthis port keeps it\n' | allow 1.2.3
want_rc 1 "stale allow entry" -- --version 1.2.3 v1.2.2 "$MAIN" "$BAD"
grep -q 'stale allow entry.*NOTES.md: shared line' "$out" || fail "stale entry not named: $(cat "$out")"
printf 'NOTES.md\tmain-only line\ta\n' | allow 1.2.3
want_rc 1 "one of two removals listed" -- --version 1.2.3 v1.2.2 "$MAIN" "$BAD"
grep -qxF 'NOTES.md: - main-only bullet' "$out" || fail "unlisted bullet not named: $(cat "$out")"

for r in '' ' ' '-' 'n/a' ' N/A ' 'N/a'; do
  printf 'scripts/doc-form.d/A.budget\tbudget 12\t%s\n' "$r" | allow 1.2.3
  want_rc 1 "reason '$r'" -- --version 1.2.3 v1.2.2 "$MAIN" "$BUDGET"
  grep -q 'give a real reason' "$out" || fail "reason '$r' not refused by name: $(cat "$out")"
done
printf 'scripts/doc-form.d/A.budget\tbudget 12\n' | allow 1.2.3
want_rc 1 "an entry with no reason field" -- --version 1.2.3 v1.2.2 "$MAIN" "$BUDGET"
noallow

# a port may not edit the check or the exceptions of any version but its own
want_rc 1 "a port that edits the script" -- --version 1.2.3 v1.2.2 "$MAIN" "$TOUCHSCRIPT"
grep -q 'may not change scripts/forward-port-check.sh' "$out" || fail "script edit not named: $(cat "$out")"
want_rc 1 "a port that edits another version's allow file" -- --version 1.2.3 v1.2.2 "$MAIN" "$TOUCHOTHER"
grep -q 'may not change scripts/forward-port-check.d/1.2.4.allow' "$out" || fail "other allow file not named: $(cat "$out")"
want_rc 1 "a port that edits an allow file with no version given" -- v1.2.2 "$MAIN" "$OWNALLOW"
git checkout -q ownallow   # CI runs the check in a checkout of the port, so its own allow file is on disk
want_rc 0 "a port that carries its own allow file" -- --version 1.2.3 v1.2.2 "$MAIN" "$OWNALLOW"
git checkout -q main

# binary: main's content reverted to the previous release's fails; any other change does not
want_rc 1 "a reverted binary" -- v1.2.2 "$MAIN" "$BINREV"
grep -qxF 'bin.dat: <binary reverted to PREV>' "$out" || fail "reverted binary not named: $(cat "$out")"
printf 'bin.dat\t<binary>\tthe release rebuilt it from the same source\n' | allow 1.2.3
want_rc 0 "a listed reverted binary" -- --version 1.2.3 v1.2.2 "$MAIN" "$BINREV"
noallow
want_rc 0 "a binary changed to something new" -- v1.2.2 "$MAIN" "$BINNEW"

# a filename with glob characters is a literal path
want_rc 1 "a glob-named file beside a plain one" -- v1.2.2 "$MAIN" "$GLOB"
[ "$(grep -c ': main-only g' "$out")" = 1 ] || fail "glob output wrong: $(cat "$out")"
grep -qxF 'g1.md: main-only g' "$out" || fail "glob output wrong: $(cat "$out")"
grep -q '^g\[1\]\.md:' "$out" && fail "g[1].md matched g1.md: $(cat "$out")"

# a failing git diff is not read as "nothing removed"
mkdir "$TMP/shim"
printf '#!/bin/sh\ncase " $* " in *" -U0 "*) exit 128 ;; esac\nexec %s "$@"\n' "$(command -v git)" >"$TMP/shim/git"
chmod +x "$TMP/shim/git"
rc=0; PATH="$TMP/shim:$PATH" "$CHECK" v1.2.2 "$MAIN" "$BAD" >"$out" 2>&1 || rc=$?
[ "$rc" != 0 ] || fail "a failing git diff passed: $(cat "$out")"
grep -q PASS "$out" && fail "a failing git diff printed PASS: $(cat "$out")"

# --prev-tag
[ "$("$CHECK" --prev-tag 1.2.3)" = v1.2.2 ] || fail "--prev-tag 1.2.3 is not v1.2.2 (an rc is skipped for a final)"
[ "$("$CHECK" --prev-tag 1.2.3-rc.2)" = v1.2.3-rc.1 ] || fail "--prev-tag 1.2.3-rc.2 is not v1.2.3-rc.1"
[ -z "$("$CHECK" --prev-tag 1.2.2)" ] || fail "--prev-tag 1.2.2 should print nothing"
[ "$("$CHECK" --prev-tag 1.4.0 main)" = "$BASEC" ] || fail "--prev-tag 1.4.0 is not the branch point"
[ "$("$CHECK" --prev-tag 1.4.0-rc.1 main)" = "$BASEC" ] || fail "--prev-tag 1.4.0-rc.1 is not the branch point"
[ "$("$CHECK" --prev-tag 1.4.0-rc.2)" = v1.4.0-rc.1 ] || fail "--prev-tag 1.4.0-rc.2 is not v1.4.0-rc.1"
[ "$("$CHECK" --prev-tag 1.4.0-rc.3)" = v1.4.0-rc.2 ] || fail "--prev-tag 1.4.0-rc.3 is not v1.4.0-rc.2"
[ "$("$CHECK" --prev-tag 1.4.1)" = v1.4.0 ] || fail "--prev-tag 1.4.1 is not v1.4.0"
rc=0; "$CHECK" --prev-tag 1.4.0-rc.5 >"$out" 2>&1 || rc=$?
[ "$rc" != 0 ] || fail "--prev-tag 1.4.0-rc.5 named a candidate that was never cut: $(cat "$out")"
rc=0; "$CHECK" --prev-tag 1.5.0 main >"$out" 2>&1 || rc=$?
[ "$rc" != 0 ] || fail "--prev-tag 1.5.0 with no tag to cut from printed: $(cat "$out")"

# Four real forward-ports: each fails with no allow file and passes with the one fixture under
# scripts/test-forward-port-check.d, whose entries each carry the reason read from the release commits.
cd "$ROOT"
mainref=""
for r in origin/main main; do git rev-parse -q --verify "$r" >/dev/null 2>&1 && { mainref="$r"; break; }; done
for v in 0.9.4 0.9.3 0.8.6 0.8.2; do
  M=""; [ -z "$mainref" ] || M="$(git log "$mainref" --merges --format=%H -1 --grep="chore/forward-port-v\?$v" 2>/dev/null || true)"
  prev="$("$CHECK" --prev-tag "$v" 2>/dev/null || true)"
  if [ -z "$M" ] || [ -z "$prev" ] || ! git rev-parse -q --verify "$M^2" >/dev/null 2>&1; then
    echo "SKIP: real forward-port $v (this clone lacks its merge or the tag below it)"; continue
  fi
  b="$(git merge-base "$M^1" "$M^2")"
  want_rc 1 "real forward-port $v with no allow file" -- "$prev" "$b" "$M^2"
  want_rc 0 "real forward-port $v with its allow file" -- --version "$v" --allow-dir "$ROOT/scripts/test-forward-port-check.d" "$prev" "$b" "$M^2"
done

echo "test-forward-port-check: ok"
