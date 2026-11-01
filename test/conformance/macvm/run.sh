#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Runs the macOS VM live suite one P-test at a time, P19 (teardown hygiene) last, keeping each
# one's raw -test.v=test2json output, stderr and exit status under <out-dir>. It converts each
# with Go's own test2json, then has the suite binary write summary.tsv (one row per P-row: PASS,
# FAIL, BLOCK or NOT_RUN) and detail.tsv (every test and subtest).
#
#   go test -c -tags macvm_live -o macvm_live.test ./test/conformance/macvm/
#   WARDYN_MACVM_LIVE=1 WARDYN_MACVM_RUNNER=<runner> test/conformance/macvm/run.sh ./macvm_live.test window/p-suite [regex]
#
# A P-test that needs a person skips (NOT_RUN in the summary) unless WARDYN_MACVM_PROMPTS=1.
#
# The security row P23 runs first, P19 last, the rest in the binary's listed order.
#
# Stop time: WARDYN_MACVM_STOP_AT (RFC 3339, UTC, like 2026-11-02T16:03:00Z). It decides only between
# rows: no test starts past it, and a test already running is never cut short by it. P19 keeps a
# reserve of WARDYN_MACVM_P19_RESERVE seconds (default 600): the other tests stop starting that much
# before the stop time, so P19 still runs if time remains for it. A row not started is NOT_RUN with the
# reason "stopped: out of time" (<test>.stopped). How long a row may run is bounded by its own
# budgets, which the summary names: WARDYN_MACVM_TIMEOUT (per row), WARDYN_MACVM_CLOCK_FOR (P21),
# WARDYN_MACVM_WARM_TRAVEL and WARDYN_MACVM_WARM_FRESH (P4). Each test's wall seconds go to
# <test>.secs for the summary's last column.
#
# Exit status: the summary's, or 2 when nothing could be run (a guard refused).
#   0  every row PASS, or NOT_RUN on non-security rows only
#   1  a FAIL
#   2  a BLOCK, or nothing could run (a guard refused)
#   3  the summary could not be written
#   4  incomplete: no FAIL or BLOCK, but no row PASSed or a security row (a blockItems entry) is NOT_RUN
# The summary leads with its counts, the BLOCK NOT RUN line and "result: complete" or
# "result: incomplete" (also in <out>/result.txt).
set -uo pipefail
# Only the last step summarizes; one inherited from the caller would turn every test run into a summary.
unset WARDYN_MACVM_SUMMARIZE

bin=${1:?usage: run.sh <macvm_live.test> <out-dir> [test regex]}
out=${2:?usage: run.sh <macvm_live.test> <out-dir> [test regex]}
pattern=${3:-^TestP}

if [[ "${WARDYN_MACVM_LIVE:-}" != 1 ]]; then
  echo "run.sh: WARDYN_MACVM_LIVE is not 1: every P-test would skip, so nothing is run" >&2
  exit 2
fi
command -v go >/dev/null || { echo "run.sh: no go on PATH: test2json cannot convert the output" >&2; exit 2; }

# A used dir would mix an earlier window's rows into this summary.
if [[ -e "$out" && -n "$(ls -A "$out" 2>/dev/null)" ]]; then
  echo "run.sh: $out is not empty: give each run its own out dir" >&2
  exit 2
fi
mkdir -p "$out"
export WARDYN_MACVM_EVIDENCE=${WARDYN_MACVM_EVIDENCE:-$out/evidence}

listed=$("$bin" -test.list "$pattern") || { echo "run.sh: -test.list failed" >&2; exit 2; }
names=$(grep -E '^TestP23_' <<<"$listed"; grep -Ev '^TestP(19|23)_' <<<"$listed"; grep '^TestP19_' <<<"$listed")
if [[ -z "${names//[[:space:]]/}" ]]; then
  echo "run.sh: no test matches $pattern" >&2
  exit 2
fi

# secs turns 90s, 5m or 5h into seconds; anything else is not a duration this script bounds by.
secs() {
  [[ "$1" =~ ^([0-9]+)([smh]?)$ ]] || return 1
  case "${BASH_REMATCH[2]}" in
    m) echo "$(( ${BASH_REMATCH[1]} * 60 ))" ;;
    h) echo "$(( ${BASH_REMATCH[1]} * 3600 ))" ;;
    *) echo "${BASH_REMATCH[1]}" ;;
  esac
}
timeout_s=$(secs "${WARDYN_MACVM_TIMEOUT:-5h}") || { echo "run.sh: WARDYN_MACVM_TIMEOUT must be like 90s, 30m or 5h" >&2; exit 2; }
reserve=${WARDYN_MACVM_P19_RESERVE:-600}
[[ "$reserve" =~ ^[0-9]+$ ]] || { echo "run.sh: WARDYN_MACVM_P19_RESERVE must be a number of seconds" >&2; exit 2; }
stop=
hasP19=; grep -q '^TestP19_' <<<"$names" && hasP19=1
if [[ -n "${WARDYN_MACVM_STOP_AT:-}" ]]; then
  # GNU date (Linux) first, then BSD date (macOS); only the Z form is accepted.
  [[ "$WARDYN_MACVM_STOP_AT" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || {
    echo "run.sh: WARDYN_MACVM_STOP_AT must be like 2026-11-02T16:03:00Z (RFC 3339, UTC)" >&2; exit 2; }
  stop=$(date -u -d "$WARDYN_MACVM_STOP_AT" +%s 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$WARDYN_MACVM_STOP_AT" +%s 2>/dev/null) || {
    echo "run.sh: cannot read WARDYN_MACVM_STOP_AT $WARDYN_MACVM_STOP_AT" >&2; exit 2; }
  if (( $(date +%s) >= stop )); then
    echo "run.sh: WARDYN_MACVM_STOP_AT $WARDYN_MACVM_STOP_AT has passed: nothing is run" >&2
    exit 2
  fi
  echo "run.sh: stop at $WARDYN_MACVM_STOP_AT; P19 reserve ${reserve}s"
fi

for name in $names; do
  if [[ -n "$stop" ]]; then
    left=$(( stop - $(date +%s) ))
    [[ $name == TestP19_* || -z "$hasP19" ]] || left=$(( left - reserve ))
    if (( left < 1 )); then
      echo "stopped: out of time" >"$out/$name.stopped"
      echo "$name not started: out of time"
      continue
    fi
  fi
  began=$(date +%s)
  "$bin" -test.v=test2json -test.run "^${name}\$" -test.timeout "${timeout_s}s" \
    >"$out/$name.raw" 2>"$out/$name.stderr"
  code=$?
  echo "$(( $(date +%s) - began ))" >"$out/$name.secs"
  echo "$code" >"$out/$name.exit"
  echo "$name exit $code"
  go tool test2json -t -p macvm <"$out/$name.raw" >"$out/$name.json" || echo "$name: test2json failed" >&2
done

WARDYN_MACVM_SUMMARIZE="$out" "$bin" -test.run '^$'
