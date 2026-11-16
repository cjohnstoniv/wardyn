#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# ci-api-prebuild.sh <workers> [go test build flags...]: compiles internal/api's test binary once, so the job's next
# step finds it in the build cache, and measures it. That compile is the memory peak of every job that builds it:
# its largest single process reached 15.4-15.6 GB of a 16 GB hosted runner with no heap limit (about 21-22 GB
# counting swap), and 12.9-14.4 GB with GOMEMLIMIT=10GiB, which the callers set. GNU time's maximum resident set is
# that one process's peak, not the sum of the processes; it goes to the log and to a notice whether the compile
# passes or fails. vmstat writes memory to the log every 15 s, which a runner lost mid-step still leaves. Last, the
# count of internal/api compiles left for the next step: 0 means the cache holds them all.
set -euo pipefail
workers=${1:?usage: ci-api-prebuild.sh <workers> [go test build flags...]}
shift
tmp="${RUNNER_TEMP:-/tmp}"
vmstat -t 15 &
vm=$!
trap 'kill "$vm" 2>/dev/null || true' EXIT
free -m; df -h /
rc=0
if [ -x /usr/bin/time ]; then
  /usr/bin/time -v -o "$tmp/api.time" go test -c -p "$workers" "$@" -o "$tmp/api.test" ./internal/api/ || rc=$?
  cat "$tmp/api.time"
  echo "::notice title=internal/api test compile, the largest single process::exit $rc; $(grep -E 'Maximum resident|Elapsed' "$tmp/api.time" | sed 's/^[[:space:]]*//' | paste -sd ';' -)"
else
  start=$SECONDS
  go test -c -p "$workers" "$@" -o "$tmp/api.test" ./internal/api/ || rc=$?
  echo "::warning title=internal/api test compile::exit $rc after $((SECONDS - start)) s; /usr/bin/time is missing, so the peak is not measured"
fi
rm -f "$tmp/api.test"
free -m; df -h /
[ "$rc" = 0 ] || exit "$rc"
echo "internal/api compiles left for the next step, testmain aside: $(go test -n "$@" ./internal/api/ 2>&1 | grep '/compile ' | grep -vc -- ' -p main ' || true)"
