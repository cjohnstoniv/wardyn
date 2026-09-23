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
# -coverprofile with atomic mode; -coverpkg=./... so coverage counts calls
# from any package in the module, not just calls from within the same
# package as the covered code (module-wide instrumentation regardless of
# which PKGS are under test). Capture the JSON stream to a file.
go test -json -covermode=atomic -coverprofile="$OUT/cover.out" -coverpkg=./... "${PKGS[@]}" \
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
# exactly like a probe that passed, and the invariant it proves - e.g. that
# audit_events is append-only, or that a sandbox really has no default route -
# was unfalsifiable from CI's own output.
#
# NAMED, not "no skips anywhere": a suite legitimately skips what its lane
# cannot provide (no Docker, no cluster). The floor covers the tests whose
# whole purpose is to be falsifiable proof, declared by calling
# testfloor.Mark(t) as the first line of the test func — NOT matched by test
# NAME. A name regex here used to couple the floor to a string that had
# nothing to do with the invariant: renaming the test silently emptied the
# set unless the rename and this script moved together. The marker lives in
# the test body, so a rename is safe and the floor still finds the same
# probes.
#
# Every suite (unit, pg, docker, k8s) carries its own probes, discovered
# fresh from this run's own JSON — nothing here is suite-specific. The one
# exception: the pg suite's floor applies only when the lane actually
# declared a database (WARDYN_TEST_PG set); with it unset there is no
# substrate, and skipping what the environment genuinely cannot provide is
# the one sanctioned skip.
DECLARE_FLOOR=1
if [ "$SUITE" = "pg" ] && [ -z "${WARDYN_TEST_PG:-}" ]; then
  DECLARE_FLOOR=0
fi
if [ -n "${WARDYN_TEST_REPORT_SKIP_FLOOR:-}" ]; then
  DECLARE_FLOOR=0
fi
if [ "$DECLARE_FLOOR" = "1" ] && [ -s "$OUT/test-output.json" ]; then
  # Suite-scoped: a probe declares which suite it gates
  # (testfloor.Mark(t, "pg")), because files with no build tag — internal/api's,
  # notably — compile into every suite's binary, and a probe that skips on a
  # missing precondition (WARDYN_TEST_PG unset) would otherwise trip suites it
  # was never meant to gate.
  FLOOR_MARKER="WARDYN_FLOOR_PROBE:$SUITE"
  # go test -json emits one event per line; a top-level test's outcome is the
  # event whose Test is the bare name (subtests carry a "/", and
  # testfloor.Mark is only ever called from a top-level test func). Extracted
  # with grep/sed so this needs no jq on the runner.
  marked_names() {
    grep -F "$FLOOR_MARKER" "$OUT/test-output.json" \
      | grep -o '"Test":"[^"/]*"' | sed 's/^"Test":"//; s/"$//' | sort -u
  }
  names() {
    grep -o "\"Action\":\"$1\",\"Package\":\"[^\"]*\",\"Test\":\"[^\"/]*\"" "$OUT/test-output.json" \
      | sed 's/.*"Test":"//; s/"$//' | sort -u
  }
  MARKED="$(marked_names)"
  if [ -z "$MARKED" ]; then
    echo ">> SKIP FLOOR: no test in suite '$SUITE' called testfloor.Mark(t)." >&2
    echo ">> Those probes are the falsifiable proof a real invariant holds; a suite with none marked" >&2
    echo ">> is a floor that got silently emptied, not a suite with nothing to check." >&2
    GO_EXIT=1
  else
    SKIPPED="$(comm -12 <(names skip) <(echo "$MARKED"))"
    if [ -n "$SKIPPED" ]; then
      echo ">> SKIP FLOOR: these probes SKIPPED:" >&2
      echo "$SKIPPED" | sed 's/^/>>   /' >&2
      if [ "$SUITE" = "pg" ]; then
        echo ">> A skip here reports \`ok\` and exit 0 while proving nothing. Give the lane a CREATE ROLE-capable" >&2
        echo ">> role over a URL-form DSN, or set WARDYN_TEST_PG_SUPERUSER=1 to assert it." >&2
      else
        echo ">> A skip here reports \`ok\` and exit 0 while proving nothing. These probes need no real" >&2
        echo ">> daemon/cluster/curl — they are the fake-backed core cases every lane can run. A skip means" >&2
        echo ">> the environment broke, not that it is missing. WARDYN_TEST_REPORT_SKIP_FLOOR=1 overrides." >&2
      fi
      GO_EXIT=1
    else
      PASSED="$(comm -12 <(names pass) <(echo "$MARKED"))"
      echo ">> skip floor: $(echo "$MARKED" | wc -l | tr -d ' ') probe(s) marked, $(echo "$PASSED" | grep -c .) passed"
    fi
  fi
fi

echo ">> reports in $OUT"
exit $GO_EXIT
