#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-ci-pick-dispatched-run.sh — train-suites waits on the run this pick
# returns, so it must return the newest run on the train head's sha started by
# this dispatch, never a run on another sha or one from before the dispatch.
#
# Daemon-free, network-free: canned `gh run list` JSON on stdin.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pick() { "$ROOT/scripts/ci-pick-dispatched-run.sh" "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

runs='[
{"databaseId":10,"headSha":"aaa","createdAt":"2026-10-19T09:59:00Z"},
{"databaseId":11,"headSha":"aaa","createdAt":"2026-10-19T10:00:05Z"},
{"databaseId":12,"headSha":"aaa","createdAt":"2026-10-19T10:00:40Z"},
{"databaseId":13,"headSha":"bbb","createdAt":"2026-10-19T10:01:00Z"}
]'
since=2026-10-19T10:00:00Z

got="$(pick aaa "$since" <<<"$runs")"
[ "$got" = 12 ] || fail "newest matching run: got [$got], want 12"

got="$(pick bbb "$since" <<<"$runs")"
[ "$got" = 13 ] || fail "the other sha's run: got [$got], want 13"

# No run on the sha at all.
got="$(pick ccc "$since" <<<"$runs")"
[ -z "$got" ] || fail "no run on the sha: got [$got], want none"

# A run on the sha that predates the dispatch is not this dispatch's run.
got="$(pick aaa 2026-10-19T10:02:00Z <<<"$runs")"
[ -z "$got" ] || fail "older runs only: got [$got], want none"

got="$(pick aaa "$since" <<<'[]')"
[ -z "$got" ] || fail "no runs listed: got [$got], want none"

echo "test-ci-pick-dispatched-run: ok"
