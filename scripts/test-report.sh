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
# exactly like a probe that passed, and the invariant it proves - audit_events is
# append-only, and the boot check says so honestly - was unfalsifiable from CI's
# own output. The probes now derive fail-not-skip from the connection, but that
# derivation is itself only observable here: if it ever stops holding, this is
# the step that says so.
#
# NAMED, not "no skips anywhere": a suite legitimately skips what its lane cannot
# provide (no Docker, no cluster). The floor covers the tests whose whole purpose
# is to be falsifiable, matched by NAME so a rename cannot quietly empty the set -
# an empty match is itself a failure. The floor is a space-separated list of
# regexes and EACH must match a test that ran, so one member of the set cannot
# vanish behind the others.
# The DEFAULT floor applies to the pg suite, and only when the lane actually
# declared a database: a run with WARDYN_TEST_PG unset has no substrate, and
# skipping what the environment genuinely cannot provide is the one sanctioned
# skip. An explicitly-set regex is honoured either way, because then somebody
# asserted the lane can satisfy it.
REQUIRE_PASS="${WARDYN_TEST_REPORT_REQUIRE_PASS:-}"
if [ -z "$REQUIRE_PASS" ] && [ "$SUITE" = "pg" ] && [ -n "${WARDYN_TEST_PG:-}" ]; then
  # TestFederation_: the one-audit-stream proof (#104), falsifiable only with a
  # database it can create and a role that can edit and purge the laptop's table.
  REQUIRE_PASS='^TestPG_ProbeF11_ ^TestFederation_'
fi
# X2-F16: the unit suite has its own falsifiable floor. The regex is
# deliberately broader than "the seven curl skips": it matches all 17
# top-level TestF7_*/TestRedirectProbe* tests across internal/api and
# internal/egress/proxy, not just the site_config_probe* cases that carry
# `t.Skip("curl not on PATH")` — tightening it to exactly those would be
# fragile (a rename slips through either way) for no safety gain: every
# other test in the set either always passes or, for the one
# environment-dependent skip inside site_config_redirect_probe2_test.go, is a
# SUBTEST (`t.Run`), whose outcome the "[^\"/]*" bare-name filter in names()
# below does not see, so the parent still reports pass. Unlike the pg floor
# above, curl is assumed present on every lane that can build the module at
# all, so this applies unconditionally rather than gated on a declared
# substrate.
if [ -z "$REQUIRE_PASS" ] && [ "$SUITE" = "unit" ]; then
  REQUIRE_PASS='^(TestF7_|TestRedirectProbe)'
fi
if [ -n "$REQUIRE_PASS" ] && [ -s "$OUT/test-output.json" ]; then
  # go test -json emits one event per line; a top-level test's outcome is the
  # event whose Test is the bare name (subtests carry a "/"). Extracted with
  # grep/sed so this needs no jq on the runner.
  names() {
    grep -o "\"Action\":\"$1\",\"Package\":\"[^\"]*\",\"Test\":\"[^\"/]*\"" "$OUT/test-output.json" \
      | sed 's/.*"Test":"//; s/"$//' | grep -E "$2" | sort -u
  }
  read -r -a FLOOR <<< "$REQUIRE_PASS"
  ANY="$(IFS='|'; echo "${FLOOR[*]}")"
  PASSED="$(names pass "$ANY")"
  SKIPPED="$(names skip "$ANY")"
  FAILED="$(names fail "$ANY")"
  MISSING=""
  for re in "${FLOOR[@]}"; do
    if [ -z "$(names pass "$re")$(names skip "$re")$(names fail "$re")" ]; then MISSING="$MISSING /$re/"; fi
  done
  if [ -n "$MISSING" ]; then
    echo ">> SKIP FLOOR: no test matching$MISSING ran in suite '$SUITE'." >&2
    echo ">> Those probes are the falsifiable proofs this suite exists to run; a regex that matches nothing" >&2
    echo ">> is a rename that silently removed the floor, not a suite with nothing to check." >&2
    GO_EXIT=1
  elif [ -n "$SKIPPED" ]; then
    echo ">> SKIP FLOOR: these probes SKIPPED:" >&2
    echo "$SKIPPED" | sed 's/^/>>   /' >&2
    if [ "$SUITE" = "pg" ]; then
      echo ">> A skip here reports \`ok\` and exit 0 while proving nothing. Give the lane a CREATE ROLE-capable" >&2
      echo ">> role over a URL-form DSN, or set WARDYN_TEST_PG_SUPERUSER=1 to assert it." >&2
    else
      echo ">> A skip here reports \`ok\` and exit 0 while proving nothing. Put curl on PATH — these probes" >&2
      echo ">> exist to prove a real curl round-trip and cannot do that skipped. A minimal dev container" >&2
      echo ">> without curl can override this floor with WARDYN_TEST_REPORT_REQUIRE_PASS=<regex-or-empty>." >&2
    fi
    GO_EXIT=1
  else
    echo ">> skip floor: $(echo "$PASSED" | wc -l | tr -d ' ') probe(s) matching /$REQUIRE_PASS/ passed"
  fi
fi

echo ">> reports in $OUT"
exit $GO_EXIT
