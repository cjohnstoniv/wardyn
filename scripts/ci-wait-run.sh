#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# ci-wait-run.sh <run-id> — ci.yml's train-suites job waits here for the
# dispatched nightly to finish, then passes only if it concluded `success`.
#
# It polls `gh run view` rather than `gh run watch`: one API 502 ended a watch
# early and read an in-progress run's empty conclusion. A single failed read is
# retried, so one API error cannot end the wait early; MAX_READ_FAILS failed
# reads in a row (a deleted run, a 404, a revoked token) fail the step naming
# the last error, rather than holding a runner until the job timeout. One line
# is printed per poll.
#
# Env: POLL_SECS (60) between status polls, RETRY_SECS (15) between conclusion
# reads, MAX_READ_FAILS (10).
set -euo pipefail
id="${1:?run id}"
poll="${POLL_SECS:-60}"
retry="${RETRY_SECS:-15}"
max="${MAX_READ_FAILS:-10}"
errf="$(mktemp)"
trap 'rm -f "$errf"' EXIT

# read_field <status|conclusion> sets $val, or returns 1 and sets $err.
read_field() {
  if val="$(gh run view "$id" --json "$1" --jq ".$1" 2>"$errf")"; then
    return 0
  fi
  err="$(tr '\n' ' ' <"$errf")"
  return 1
}

fails=0
while :; do
  if read_field status; then
    fails=0
    echo "run $id: status ${val:-unknown}"
    [ "$val" != completed ] || break
  else
    fails=$((fails + 1))
    echo "run $id: status read failed ($fails/$max): $err"
    if [ "$fails" -ge "$max" ]; then
      echo "::error::reading run $id failed $max times in a row; last error: $err"
      exit 1
    fi
  fi
  sleep "$poll"
done

conclusion=
for _ in 1 2 3 4 5; do
  if read_field conclusion; then
    conclusion="$val"
    break
  fi
  echo "run $id: conclusion read failed: $err"
  sleep "$retry"
done
[ "$conclusion" = success ] || { echo "::error::nightly run $id concluded '$conclusion'"; exit 1; }
echo "run $id: concluded success"
