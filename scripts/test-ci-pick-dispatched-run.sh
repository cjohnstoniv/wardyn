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
{"databaseId":10,"headSha":"aaa","createdAt":"2026-10-19T09:59:00Z","displayTitle":"Nightly (full-stack e2e) [ci-7-1]"},
{"databaseId":11,"headSha":"aaa","createdAt":"2026-10-19T10:00:05Z","displayTitle":"Nightly (full-stack e2e) [ci-7-1]"},
{"databaseId":12,"headSha":"aaa","createdAt":"2026-10-19T10:00:40Z","displayTitle":"Nightly (full-stack e2e) [ci-7-1]"},
{"databaseId":13,"headSha":"bbb","createdAt":"2026-10-19T10:01:00Z","displayTitle":"Nightly (full-stack e2e) [ci-8-1]"},
{"databaseId":14,"headSha":"aaa","createdAt":"2026-10-19T10:00:50Z","displayTitle":"Nightly (full-stack e2e)"},
{"databaseId":15,"headSha":"aaa","createdAt":"2026-10-19T10:00:55Z","displayTitle":"Nightly (full-stack e2e) [ci-7-10]"}
]'
since=2026-10-19T10:00:00Z

got="$(pick aaa "$since" ci-7-1 <<<"$runs")"
[ "$got" = 12 ] || fail "newest run of this dispatch: got [$got], want 12 (not 14 or 15, the unrelated runs on the same commit)"

got="$(pick bbb "$since" ci-8-1 <<<"$runs")"
[ "$got" = 13 ] || fail "the other sha's run: got [$got], want 13"

# A dispatch id that no run's title carries picks nothing, though runs on the sha are in the window.
got="$(pick aaa "$since" ci-9-1 <<<"$runs")"
[ -z "$got" ] || fail "an id no title carries: got [$got], want none"

# A prefix of another run's id is not that run: ci-7-1 never matches ci-7-10's title.
got="$(pick aaa "$since" ci-7-10 <<<"$runs")"
[ "$got" = 15 ] || fail "ci-7-10's own run: got [$got], want 15"

# No run on the sha at all.
got="$(pick ccc "$since" ci-7-1 <<<"$runs")"
[ -z "$got" ] || fail "no run on the sha: got [$got], want none"

# A run on the sha that predates the dispatch is not this dispatch's run.
got="$(pick aaa 2026-10-19T10:02:00Z ci-7-1 <<<"$runs")"
[ -z "$got" ] || fail "older runs only: got [$got], want none"

got="$(pick aaa "$since" ci-7-1 <<<'[]')"
[ -z "$got" ] || fail "no runs listed: got [$got], want none"

# The dispatch id is required.
if pick aaa "$since" <<<"$runs" >/dev/null 2>&1; then fail "a call without the dispatch id must exit non-zero"; fi

echo "test-ci-pick-dispatched-run: ok"
