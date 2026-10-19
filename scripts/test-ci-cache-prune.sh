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
# Daemon-free, network-free: it runs the jq program ci.yml carries against a
# fixed listing. CI_YML overrides the workflow read, for a mutation check.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CI_YML="${CI_YML:-$ROOT/.github/workflows/ci.yml}"

fail() { echo "FAIL: $*" >&2; exit 1; }

# The program is the single-quoted jq argument after `--arg mod "$MOD_KEY" '`.
prog="$(awk '
  /jq -rs --arg r "-\$RUN_ID" --arg mod "\$MOD_KEY" '\''$/ { on = 1; next }
  on && /^ *\| "\\\(\.id\)\\t\\\(\.key\)"'\''\)"$/ { print "| \"\\(.id)\\t\\(.key)\""; exit }
  on { print }
' "$CI_YML")"
[ -n "$prog" ] || fail "no go-cache-prune jq program found in $CI_YML"

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
EOF
)"
# Run 222 saved go-unit and go-test-pg-api-2. It deletes the older go-unit
# entries (the per-day key, run 111, and run 1222, whose id only ends in 222)
# and the older api-2 entry; it keeps its own, run 333's newer go-unit, and the
# api-1 and docker families, which it did not save.
got="$(printf '%s\n' "$listing" | jq -rs --arg r "-222" --arg mod "" "$prog" | cut -f1 | sort -n | paste -sd' ')"
[ "$got" = "1 2 6 9" ] || fail "run 222 would delete ids [$got], want [1 2 6 9]"

# Run 222 also saved the module cache: the older gomod entry goes too.
got="$(printf '%s\n' "$listing" | jq -rs --arg r "-222" --arg mod "gomod-Linux-$h" "$prog" | cut -f1 | sort -n | paste -sd' ')"
[ "$got" = "1 2 6 9 10" ] || fail "run 222 saving gomod would delete ids [$got], want [1 2 6 9 10]"

# A run that saved nothing deletes nothing.
got="$(printf '%s\n' "$listing" | jq -rs --arg r "-999" --arg mod "" "$prog")"
[ -z "$got" ] || fail "a run that saved no cache would delete: $got"

echo "test-ci-cache-prune: ok"
