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

# ── name the failure ─────────────────────────────────────────────────────────
# G11: a red `build`/`test-pg` job used to say only `make: *** [Makefile:195:
# test-report] Error 1` — the failing test names and any compiler output were
# visible only in the uploaded JSON artifact (gh run download -n
# go-test-reports). Surface both directly in the job log on a red run, plus
# any package that failed with no failing test to name (a -timeout panic, a
# panic in init, os.Exit or a failure in TestMain): its package name and the
# panic, or else its last few output lines.
if [ "$GO_EXIT" -ne 0 ] && [ -s "$OUT/test-output.json" ] && command -v python3 >/dev/null 2>&1; then
  python3 - "$OUT/test-output.json" >&2 <<'PYEOF'
import json
import sys
from collections import deque

fails = set()
build_output = {}  # ImportPath -> [Output, ...], buffered until we see build-fail
build_fails = {}   # ImportPath -> [Output, ...]
pkg_fails = set()  # Package: failed with no Test and not a build failure
tail = {}          # Package -> its last few output lines
panics = {}        # Package -> its first `panic:` line and the lines after it


def emit(out):
    sys.stdout.write(">>     " + out if out.endswith("\n") else ">>     " + out + "\n")

with open(sys.argv[1]) as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            ev = json.loads(line)
        except ValueError:
            continue  # a non-JSON line (e.g. `go test` itself failed to start)
        action = ev.get("Action")
        if action == "build-output":
            build_output.setdefault(ev.get("ImportPath", ""), []).append(ev.get("Output", ""))
        elif action == "build-fail":
            ip = ev.get("ImportPath", "")
            build_fails[ip] = build_output.get(ip, [])
        elif action == "output" and ev.get("Package"):
            pkg, out = ev["Package"], ev.get("Output", "")
            tail.setdefault(pkg, deque(maxlen=8)).append(out)
            # A -timeout panic is attributed to the running test but emits no
            # fail event for it, and its last lines are goroutine frames: the
            # `panic:` line and the ones after it are what name the cause.
            if pkg not in panics and out.startswith("panic: "):
                panics[pkg] = [out]
            elif pkg in panics and len(panics[pkg]) < 8:
                panics[pkg].append(out)
        elif action == "fail" and ev.get("Test"):
            fails.add((ev.get("Package", ""), ev["Test"]))
        elif action == "fail" and not ev.get("FailedBuild"):
            pkg_fails.add(ev.get("Package", ""))

if fails:
    print(">> failing tests:")
    for pkg, test in sorted(fails):
        print(f">>   {pkg} {test}")

if build_fails:
    print(">> failed to build:")
    for ip, lines in sorted(build_fails.items()):
        print(f">>   {ip}")
        for out in lines:
            emit(out)

unnamed = sorted(pkg_fails - {pkg for pkg, _ in fails})
if unnamed:
    print(">> failed outside any named test:")
    for pkg in unnamed:
        print(f">>   {pkg}")
        for out in panics.get(pkg) or tail.get(pkg, []):
            emit(out)
PYEOF
fi

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
