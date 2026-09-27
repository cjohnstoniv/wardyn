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
panic_test = {}    # Package -> the test that panic was attributed to, if any


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
                panic_test[pkg] = ev.get("Test", "")
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

named_fail_pkgs = {pkg for pkg, _ in fails}
# An ordinary panic in a test fails that test, already named above; a -timeout
# panic fails no test, so it still needs its package listed here.
orphan_panics = {p for p in panics if (p, panic_test[p]) not in fails}
unnamed = sorted(p for p in pkg_fails if p in orphan_panics or p not in named_fail_pkgs)
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
# exactly like a probe that passed, and the invariant it proves - e.g. that
# audit_events is append-only, or that a sandbox really has no default route -
# was unfalsifiable from CI's own output.
#
# NAMED, not "no skips anywhere": a suite legitimately skips what its lane
# cannot provide (no Docker, no cluster). The floor covers the tests whose
# whole purpose is to be falsifiable proof, declared by calling
# testfloor.Mark(t, "<suite>") as the first line of the test func — NOT matched
# by test NAME. A name regex here used to couple the floor to a string that had
# nothing to do with the invariant: renaming the test silently emptied the set
# unless the rename and this script moved together. The marker lives in the
# test body, so a rename is safe and the floor still finds the same probes.
#
# Every suite (unit, pg, docker, k8s) carries its own probes. The one
# exception: the pg suite's floor applies only when the lane actually declared
# a database (WARDYN_TEST_PG set); with it unset there is no substrate, and
# skipping what the environment genuinely cannot provide is the one sanctioned
# skip.
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
  MODULE="$(sed -n 's/^module //p' "$ROOT/go.mod")"
  # Every probe is keyed "<import path> <top-level test>": test names are only
  # unique within a package. go test -json emits one event per line, and a
  # top-level test's event is the one whose Test is the bare name (subtests
  # carry a "/"). Extracted with grep/sed/awk so this needs no jq on the runner.
  #
  # The EXPECTED set is read from the SOURCE, not only from what this run
  # logged: every `testfloor.Mark(t, "<suite>")` line in a *_test.go file of a
  # package this run tested, named by the top-level func it sits in. A probe
  # that skips or returns before its Mark line never logs the marker, so a set
  # built from the run's output alone would just lose it, with no error.
  source_probes() {
    grep -o '"Package":"[^"]*"' "$OUT/test-output.json" | sed 's/^"Package":"//; s/"$//' | sort -u \
      | while IFS= read -r pkg; do
          set -- "$ROOT${pkg#"$MODULE"}"/*_test.go
          [ -f "$1" ] || continue
          awk -v pkg="$pkg" -v call="testfloor.Mark(t, \"$SUITE\")" '
            FNR == 1 { name = "" }
            /^func / { name = $2; sub(/\(.*/, "", name) }
            { line = $0; sub(/^[ \t]+/, "", line) }
            index(line, call) == 1 && name ~ /^Test/ { print pkg " " name }' "$@"
        done
  }
  # The probes that logged the marker in this run.
  marked() {
    grep -F "$FLOOR_MARKER\\n" "$OUT/test-output.json" \
      | sed -n 's/.*"Package":"\([^"]*\)","Test":"\([^"/]*\)".*/\1 \2/p' | sort -u
  }
  # The top-level tests whose outcome was $1 (pass, skip or fail).
  outcome() {
    grep -o "\"Action\":\"$1\",\"Package\":\"[^\"]*\",\"Test\":\"[^\"/]*\"" "$OUT/test-output.json" \
      | sed 's/.*"Package":"\([^"]*\)","Test":"\([^"]*\)"$/\1 \2/' | sort -u
  }
  MARKED="$(marked)"
  EXPECTED="$( { source_probes; echo "$MARKED"; } | grep . | sort -u)"
  PROVEN="$(comm -12 <(outcome pass) <(echo "$MARKED"))"
  MISSING="$(comm -23 <(echo "$EXPECTED") <(echo "$PROVEN"))"
  if [ -z "$EXPECTED" ]; then
    echo ">> SKIP FLOOR: no package suite '$SUITE' tested calls testfloor.Mark(t, \"$SUITE\")." >&2
    echo ">> Those probes are the falsifiable proof a real invariant holds; a suite with none marked" >&2
    echo ">> is a floor that got silently emptied, not a suite with nothing to check." >&2
    GO_EXIT=1
  elif [ -n "$MISSING" ]; then
    SKIPS="$(outcome skip)"
    FAILS="$(outcome fail)"
    PASSES="$(outcome pass)"
    echo ">> SKIP FLOOR: these probes did not pass through testfloor.Mark(t, \"$SUITE\"):" >&2
    while IFS= read -r probe; do
      if echo "$SKIPS" | grep -qxF "$probe"; then why="SKIPPED"
      elif echo "$FAILS" | grep -qxF "$probe"; then why="FAILED"
      elif echo "$PASSES" | grep -qxF "$probe"; then why="passed without reaching its Mark line"
      else why="did not run"
      fi
      echo ">>   $probe — $why" >&2
    done <<<"$MISSING"
    echo ">> A probe that skips, or returns before its Mark line, reports \`ok\` and exit 0 while proving nothing." >&2
    case "$SUITE" in
      pg)
        echo ">> Give the lane a CREATE ROLE-capable role over a URL-form DSN, or set" >&2
        echo ">> WARDYN_TEST_PG_SUPERUSER=1 to assert it." >&2 ;;
      unit)
        echo ">> Put curl on PATH — these probes exist to prove a real curl round-trip and cannot do that" >&2
        echo ">> skipped. A minimal dev container without curl can set WARDYN_TEST_REPORT_SKIP_FLOOR=1." >&2 ;;
      *)
        echo ">> These probes need no real daemon or cluster — they are the fake-backed core cases every" >&2
        echo ">> lane can run, so a skip means the environment broke, not that it is missing." >&2
        echo ">> WARDYN_TEST_REPORT_SKIP_FLOOR=1 overrides." >&2 ;;
    esac
    GO_EXIT=1
  else
    echo ">> skip floor: $(echo "$EXPECTED" | wc -l | tr -d ' ') probe(s) marked, all passed"
  fi
fi

echo ">> reports in $OUT"
exit $GO_EXIT
