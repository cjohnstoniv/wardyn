#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-ci-candidate-marker.sh — ci-candidate-marker.sh decides whether a train head stages images, so it must say
# `true` only for a head commit whose message has a line that is exactly `Candidate: v<V>` with V the tree's own
# version, and must say `false` for every near miss. Drives the script on throwaway repositories; no network, no daemon.
set -euo pipefail

# Throwaway repos get no background git: a commit can leave detached auto-maintenance writing under .git when the
# EXIT trap runs rm -rf (#3328).
export GIT_CONFIG_COUNT=3 GIT_CONFIG_KEY_0=maintenance.auto GIT_CONFIG_VALUE_0=false GIT_CONFIG_KEY_1=gc.auto GIT_CONFIG_VALUE_1=0 GIT_CONFIG_KEY_2=gc.autoDetach GIT_CONFIG_VALUE_2=false
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/ci-candidate-marker.sh"
fail() { echo "FAIL: $*" >&2; exit 1; }

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
git -C "$tmp" init -q
mkdir -p "$tmp/internal/version" "$tmp/scripts"
printf 'package version\n\nconst Version = "0.10.4"\n' >"$tmp/internal/version/version.go"
cp "$SCRIPT" "$tmp/scripts/ci-candidate-marker.sh"
git -C "$tmp" add -A

# commit <message>: an empty commit on top, so the head's message is the only thing that changes.
commit() { git -C "$tmp" commit -q --allow-empty --no-verify -m "$1"; }
# run: prints "<stdout> <rc>" and leaves stderr in $tmp/err.
run() { local out rc=0; out="$(cd "$tmp" && bash scripts/ci-candidate-marker.sh 2>"$tmp/err")" || rc=$?; echo "$out $rc"; }

git -C "$tmp" commit -q --no-verify -m "the tree"
commit "$(printf 'release commit\n\nCandidate: v0.10.4\n')"
[ "$(run)" = "true 0" ] || fail "an exact marker line: got [$(run)], want [true 0]"

commit "$(printf 'next commit\n')"
[ "$(run)" = "false 0" ] || fail "the marker only on the parent commit: got [$(run)], want [false 0]"

commit "$(printf 'release commit\n\nCandidate: v0.10.3\n')"
[ "$(run)" = "false 0" ] || fail "a marker naming another version: got [$(run)], want [false 0]"
grep -q '::warning::' "$tmp/err" || fail "a marker naming another version must warn on stderr"

commit "$(printf 'release commit\n\n  Candidate: v0.10.4\n')"
[ "$(run)" = "false 0" ] || fail "an indented marker: got [$(run)], want [false 0]"

commit "$(printf 'release commit\n\nCandidate: v0.10.4 please\n')"
[ "$(run)" = "false 0" ] || fail "a marker with trailing words: got [$(run)], want [false 0]"

commit "$(printf 'release commit\n\nCandidate: v0.10.4\n')"
git -C "$tmp" rm -q internal/version/version.go
got="$(run)"
[ "$got" = " 1" ] || fail "version.go absent: got [$got], want exit 1"
grep -q '::error::' "$tmp/err" || fail "version.go absent must print ::error:: on stderr"

echo "test-ci-candidate-marker: ok"
