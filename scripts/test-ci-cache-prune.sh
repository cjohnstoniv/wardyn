#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-ci-cache-prune.sh — ci.yml's go-cache-prune job deletes Go cache
# entries, so its selection must delete only what this run superseded: an
# older entry of a family this run saved (old key shapes included), never this
# run's entry, never a newer run's, never a family this run did not save. The
# shared module cache (gomod-) counts as saved only when go-modules saved it.
#
# Daemon-free, network-free: it runs the jq program scripts/ci-go-cache-prune.jq against a
# fixed listing. CI_JQ overrides the program read, for a mutation check.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
prog_file="${CI_JQ:-$ROOT/scripts/ci-go-cache-prune.jq}"

fail() { echo "FAIL: $*" >&2; exit 1; }

[ -s "$prog_file" ] || fail "no go-cache-prune jq program at $prog_file"

# Both prunes (ci.yml's on a push to main, release-branch-checks.yml's on a release branch) must read the tested program.
for wf in ci.yml release-branch-checks.yml; do
  n="$(git -C "$ROOT" grep -c 'ci-go-cache-prune.jq' -- ".github/workflows/$wf" | cut -d: -f2 || true)"
  [ "${n:-0}" = 1 ] || fail "$wf must read scripts/ci-go-cache-prune.jq exactly once, found ${n:-0}"
done

h="$(printf 'e%.0s' $(seq 64))"
o="$(printf '0%.0s' $(seq 64))"
listing="$(cat <<EOF
{"id":1,"key":"go-unit-$h-20261018","created_at":"2026-10-18T01:00:00Z"}
{"id":2,"key":"go-unit-Linux-$h-111","created_at":"2026-10-19T01:00:00Z"}
{"id":3,"key":"go-unit-Linux-$h-222","created_at":"2026-10-19T02:00:00Z"}
{"id":4,"key":"go-unit-Linux-$h-333","created_at":"2026-10-19T03:00:00Z"}
{"id":5,"key":"go-test-pg-api-1-Linux-$h-111","created_at":"2026-10-19T01:00:00Z"}
{"id":6,"key":"go-test-pg-api-2-Linux-$h-111","created_at":"2026-10-19T01:00:00Z"}
{"id":7,"key":"go-test-pg-api-2-Linux-$h-222","created_at":"2026-10-19T02:00:00Z"}
{"id":8,"key":"go-docker-Linux-$h-111","created_at":"2026-10-19T01:00:00Z"}
{"id":9,"key":"go-unit-Linux-$h-1222","created_at":"2026-10-19T00:30:00Z"}
{"id":10,"key":"gomod-Linux-$o","created_at":"2026-10-18T05:00:00Z"}
{"id":11,"key":"gomod-Linux-$h","created_at":"2026-10-19T02:00:00Z"}
{"id":12,"key":"gotools-Linux-$o","created_at":"2026-10-18T05:00:00Z"}
{"id":13,"key":"gotools-Linux-$h","created_at":"2026-10-19T02:00:00Z"}
{"id":14,"key":"go-docker-Linux-$h-2223","created_at":"2026-10-19T04:00:00Z"}
EOF
)"
# Run 222 saved go-unit and go-test-pg-api-2. It deletes the older go-unit
# entries (the per-day key, run 111, and run 1222, whose id only ends in 222)
# and the older api-2 entry; it keeps its own, run 333's newer go-unit, and the
# api-1 and docker families, which it did not save.
got="$(printf '%s\n' "$listing" | jq -rs --arg r "-222" --arg mod "" -f "$prog_file" | cut -f1 | sort -n | paste -sd' ')"
[ "$got" = "1 2 6 9" ] || fail "run 222 would delete ids [$got], want [1 2 6 9]"

# Run 222 also saved the module cache: the older gomod entry goes too.
got="$(printf '%s\n' "$listing" | jq -rs --arg r "-222" --arg mod "gomod-Linux-$h" -f "$prog_file" | cut -f1 | sort -n | paste -sd' ')"
[ "$got" = "1 2 6 9 10" ] || fail "run 222 saving gomod would delete ids [$got], want [1 2 6 9 10]"

# The gate tools' entries (gotools-, saved by go-gate-tools) are never marked as
# superseded: ids 12 and 13 are in the listing and in neither result above.

# A run that saved nothing deletes nothing.
got="$(printf '%s\n' "$listing" | jq -rs --arg r "-999" --arg mod "" -f "$prog_file")"
[ -z "$got" ] || fail "a run that saved no cache would delete: $got"

echo "test-ci-cache-prune: ok"
