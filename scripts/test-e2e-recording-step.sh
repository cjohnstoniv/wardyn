#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for #965: the security e2e's recording assertion (step i) must
# run on a run that can END by itself. wardyn-rec uploads a cast only when the
# wrapped process exits, and the real-agent run's brokered clone waits on a
# credential approval the suite grants only later, so asserting that run's cast
# can never pass. Extracts the REAL recording_step function from test/e2e/e2e.sh
# (not a copy) and drives it against stubbed CLI and API answers: a finite run,
# an upload that lands after the run settles, a create that fails, a run that
# fails before recording anything, and a served cast without the sentinel.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fn="$(sed -n '/^recording_step()/,/^}/p' "$ROOT/test/e2e/e2e.sh")"
[ -n "$fn" ] || { echo "FAIL: could not extract recording_step() from test/e2e/e2e.sh" >&2; exit 1; }
eval "$fn"

T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
WORKDIR="$T"; ADMIN_TOKEN=t; BASE=http://api; CC_AGENT=claude-code; REC_RUN_ID=""
RID=11111111-2222-3333-4444-555555555555
CAST_OK='{"version": 2}
[0.1, "o", "wardyn-e2e-recording-ok\r\n"]'

pass=0; fail=0
log() { :; }
ok() { pass=$((pass+1)); }
bad() { fail=$((fail+1)); printf '    (bad) %s\n' "$*"; }
note() { :; }
sleep() { :; }
docker() { :; }

# Scenario knobs, read by the stubs: CREATE_OUT is what the CLI prints; the Nth
# poll of the run answers STATES[N] (the last entry repeats), and the recording
# GET answers 404 until the poll number reaches UPLOAD_AT (0 = never).
e2e_wardyn() { printf '%s\n' "$*" >"$T/argv"; printf '%s\n' "${CREATE_OUT}"; }
run_state() {
  local n; n=$(( $(cat "$T/polls") + 1 )); echo "$n" >"$T/polls"
  local i=$(( n <= ${#STATES[@]} ? n - 1 : ${#STATES[@]} - 1 ))
  printf '%s\n' "${STATES[$i]}"
}
hc() {
  local out="" url="${*: -1}"
  while [ $# -gt 0 ]; do case "$1" in -o) out="$2"; shift 2 ;; *) shift ;; esac; done
  echo "$url" >>"$T/urls"
  if [ "${UPLOAD_AT}" -gt 0 ] && [ "$(cat "$T/polls")" -ge "${UPLOAD_AT}" ]; then
    printf '%s\n' "${CAST_BODY}" >"$out"; printf 200
  else
    printf '{"error":"not found"}' >"$out"; printf 404
  fi
}

errors=0
scenario() {  # name want_pass want_fail
  echo 0 >"$T/polls"; : >"$T/urls"; : >"$T/argv"; pass=0; fail=0; REC_RUN_ID=""
  recording_step >/dev/null
  if [ "$pass" -eq "$2" ] && [ "$fail" -eq "$3" ]; then
    echo "ok: $1 (pass=$pass fail=$fail)"
  else
    echo "FAIL: $1: want pass=$2 fail=$3, got pass=$pass fail=$fail" >&2; errors=$((errors+1))
  fi
}
check() {  # description, then a command that must succeed
  local d="$1"; shift
  if "$@"; then echo "ok: $d"; else echo "FAIL: $d" >&2; errors=$((errors+1)); fi
}

CREATE_OUT="created run ${RID} (state STARTING)"; CAST_BODY="${CAST_OK}"

STATES=(STARTING RUNNING RUNNING COMPLETED); UPLOAD_AT=4
scenario "finite run completes and its cast is served with the sentinel" 2 0
check "the recording run declares no repository (its clone would wait on (ii-b)'s approval)" \
  bash -c '! grep -q -- "--repo" "$1"' _ "$T/argv"
check "the recording run runs a plain command (task mode exec), not the model harness" \
  grep -q -- "--task-mode exec" "$T/argv"
check "the sentinel is not in the command text, so only the command's output can carry it" \
  bash -c '! grep -q "wardyn-e2e-recording-ok" "$1"' _ "$T/argv"
check "the cast is fetched from the recording run's own route" \
  grep -q "/runs/${RID}/recording/${RID}\$" "$T/urls"
check "the run id is kept for teardown" test "${REC_RUN_ID}" = "${RID}"

STATES=(COMPLETED); UPLOAD_AT=4
scenario "an upload that lands after the run settles still passes" 2 0

CREATE_OUT="Error: 403 forbidden"
scenario "a create that fails is a FAIL, with no polling" 0 1
check "a failed create polls nothing" test ! -s "$T/urls"
CREATE_OUT="created run ${RID} (state STARTING)"

STATES=(STARTING FAILED); UPLOAD_AT=0
scenario "a run that fails before recording anything reports both FAILs" 0 2
check "a settled run without a cast is not waited on for the whole budget" \
  test "$(wc -l <"$T/urls")" -le 10

STATES=(COMPLETED); UPLOAD_AT=1; CAST_BODY='{"version": 2}'
scenario "a served cast without the sentinel is a FAIL" 1 1

if [ "$errors" -eq 0 ]; then
  echo "--- test-e2e-recording-step: PASS ---"
else
  echo "--- test-e2e-recording-step: FAIL ($errors) ---"; exit 1
fi
