#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-forward-port-check.sh — pins forward-port-check.sh in a throwaway repo:
# a forward-port that carries only the release delta passes; one that drops a
# main-only line (a prose line or a bullet) fails and names it; budget files
# are exempt; a duplicate CHANGELOG heading fails; --prev-tag picks the right
# tag. Daemon-free, network-free.
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
commit base
git tag v1.2.2
git tag v1.3.0-rc.1
git tag v1.2.3-rc.1

printf '# Notes\n\nshared line\nmain-only line\n- main-only bullet\n' >NOTES.md
commit "main-only content"
MAIN="$(git rev-parse HEAD)"

# good: release delta only (a CHANGELOG entry and a budget change)
git checkout -q -b good "$MAIN"
printf '# Changelog\n\n## [Unreleased]\n\n## [1.2.3]\n\n- new entry\n\n## [1.2.2]\n\n- old entry\n' >CHANGELOG.md
printf 'budget 11\n' >scripts/doc-form.d/A.budget
commit good
GOOD="$(git rev-parse HEAD)"

# bad: a forward-port that rebuilt NOTES.md from the release branch
git checkout -q -b bad "$MAIN"
printf '# Notes\n\nshared line\n' >NOTES.md
commit bad
BAD="$(git rev-parse HEAD)"

# dup: the same CHANGELOG version heading twice
git checkout -q -b dup "$GOOD"
printf '# Changelog\n\n## [1.2.3]\n\n- a\n\n## [1.2.3]\n\n- b\n\n## [1.2.2]\n\n- old entry\n' >CHANGELOG.md
commit dup
DUP="$(git rev-parse HEAD)"

out="$TMP/out"
run() { "$CHECK" "$@" >"$out" 2>&1; }

run v1.2.2 "$MAIN" "$GOOD" || fail "good forward-port failed: $(cat "$out")"
grep -q 'PASS' "$out" || fail "good forward-port printed no PASS"

rc=0; run v1.2.2 "$MAIN" "$BAD" || rc=$?
[ "$rc" = 1 ] || fail "bad forward-port exited $rc, want 1"
grep -qxF 'NOTES.md: main-only line' "$out" || fail "main-only line not named: $(cat "$out")"
grep -qxF 'NOTES.md: - main-only bullet' "$out" || fail "main-only bullet not named: $(cat "$out")"

git checkout -q -b budget "$MAIN"
printf 'budget 9\n' >scripts/doc-form.d/A.budget
commit budget
run v1.2.2 "$MAIN" "$(git rev-parse HEAD)" || fail "a budget-only change failed: $(cat "$out")"

rc=0; run v1.2.2 "$MAIN" "$DUP" || rc=$?
[ "$rc" = 1 ] || fail "duplicate heading exited $rc, want 1"
grep -q '## \[1.2.3\]' "$out" || fail "duplicate heading not named: $(cat "$out")"

[ "$("$CHECK" --prev-tag 1.2.3)" = v1.2.2 ] || fail "--prev-tag 1.2.3 is not v1.2.2 (an rc is skipped for a final)"
[ "$("$CHECK" --prev-tag 1.2.3-rc.2)" = v1.2.3-rc.1 ] || fail "--prev-tag 1.2.3-rc.2 is not v1.2.3-rc.1"
[ -z "$("$CHECK" --prev-tag 1.2.2)" ] || fail "--prev-tag 1.2.2 should print nothing"

echo "test-forward-port-check: ok"
