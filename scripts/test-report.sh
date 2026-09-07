#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Run a Go test suite and emit detailed reports under test/reports/go/<suite>/:
#   - test-output.json  : raw go test -json event stream
#   - cover.out         : coverage profile
#   - coverage.html     : HTML coverage (go tool cover)
#   - coverage-func.txt : per-func coverage + total
#
# Usage:
#   scripts/test-report.sh <suite-name> [go test args/pkgs...]
# Examples:
#   scripts/test-report.sh unit ./...
#   scripts/test-report.sh docker -tags docker ./internal/runner/...
#
# Honors env: GOFLAGS, WARDYN_TEST_PG, WARDYN_TEST_DOCKER (passed through to go test).
# Exit code mirrors the test run (non-zero if any test failed).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SUITE="${1:?usage: test-report.sh <suite> [go test args...]}"
shift
PKGS=("$@")
if [ ${#PKGS[@]} -eq 0 ]; then PKGS=("./..."); fi

OUT="$ROOT/test/reports/go/$SUITE"
mkdir -p "$OUT"

echo ">> running suite '$SUITE': go test -json ${PKGS[*]}"
# -coverprofile with atomic mode; capture the JSON stream to a file.
go test -json -covermode=atomic -coverprofile="$OUT/cover.out" "${PKGS[@]}" \
  > "$OUT/test-output.json"
GO_EXIT=$?

# Coverage artifacts (best-effort; cover.out may be absent if build failed).
if [ -s "$OUT/cover.out" ]; then
  go tool cover -func="$OUT/cover.out" > "$OUT/coverage-func.txt" 2>/dev/null
  go tool cover -html="$OUT/cover.out" -o "$OUT/coverage.html" 2>/dev/null
  TOTAL=$(grep -E "^total:" "$OUT/coverage-func.txt" | awk '{print $NF}')
  echo ">> coverage total: ${TOTAL:-n/a}"
fi

# ── the skip floor ───────────────────────────────────────────────────────────
# A test that SKIPS produces `--- SKIP` -> `ok` -> exit 0, and everything above
# grades on the exit code alone. So a probe that quietly stopped running looked
# exactly like a probe that passed, and the invariant it proves - audit_events is
# append-only, and the boot check says so honestly - was unfalsifiable from CI's
# own output. The probes now derive fail-not-skip from the connection, but that
# derivation is itself only observable here: if it ever stops holding, this is
# the step that says so.
#
# NAMED, not "no skips anywhere": a suite legitimately skips what its lane cannot
# provide (no Docker, no cluster). The floor covers the tests whose whole purpose
# is to be falsifiable, matched by NAME so a rename cannot quietly empty the set -
# an empty match is itself a failure.
# The DEFAULT floor applies to the pg suite, and only when the lane actually
# declared a database: a run with WARDYN_TEST_PG unset has no substrate, and
# skipping what the environment genuinely cannot provide is the one sanctioned
# skip. An explicitly-set regex is honoured either way, because then somebody
# asserted the lane can satisfy it.
REQUIRE_PASS="${WARDYN_TEST_REPORT_REQUIRE_PASS:-}"
if [ -z "$REQUIRE_PASS" ] && [ "$SUITE" = "pg" ] && [ -n "${WARDYN_TEST_PG:-}" ]; then
  REQUIRE_PASS='^TestPG_ProbeF11_'
fi
if [ -n "$REQUIRE_PASS" ] && [ -s "$OUT/test-output.json" ]; then
  # go test -json emits one event per line; a top-level test's outcome is the
  # event whose Test is the bare name (subtests carry a "/"). Extracted with
  # grep/sed so this needs no jq on the runner.
  names() {
    grep -o "\"Action\":\"$1\",\"Package\":\"[^\"]*\",\"Test\":\"[^\"/]*\"" "$OUT/test-output.json" \
      | sed 's/.*"Test":"//; s/"$//' | grep -E "$REQUIRE_PASS" | sort -u
  }
  PASSED="$(names pass)"
  SKIPPED="$(names skip)"
  FAILED="$(names fail)"
  if [ -z "$PASSED$SKIPPED$FAILED" ]; then
    echo ">> SKIP FLOOR: no test matching /$REQUIRE_PASS/ ran in suite '$SUITE'." >&2
    echo ">> Those probes are the falsifiable proof of the append-only invariant; a set that matches nothing" >&2
    echo ">> is a rename that silently removed the floor, not a suite with nothing to check." >&2
    GO_EXIT=1
  elif [ -n "$SKIPPED" ]; then
    echo ">> SKIP FLOOR: these probes SKIPPED on a lane that declared its substrate:" >&2
    echo "$SKIPPED" | sed 's/^/>>   /' >&2
    echo ">> A skip here reports \`ok\` and exit 0 while proving nothing. Give the lane a CREATE ROLE-capable" >&2
    echo ">> role over a URL-form DSN, or set WARDYN_TEST_PG_SUPERUSER=1 to assert it." >&2
    GO_EXIT=1
  else
    echo ">> skip floor: $(echo "$PASSED" | wc -l | tr -d ' ') probe(s) matching /$REQUIRE_PASS/ passed"
  fi
fi

echo ">> reports in $OUT"
exit $GO_EXIT
