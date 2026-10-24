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
# Exit status: the summary's (2 when any row is BLOCK, 1 when any is FAIL, else 0), or 2 when
# nothing could be run.
set -uo pipefail

bin=${1:?usage: run.sh <macvm_live.test> <out-dir> [test regex]}
out=${2:?usage: run.sh <macvm_live.test> <out-dir> [test regex]}
pattern=${3:-^TestP}

if [[ "${WARDYN_MACVM_LIVE:-}" != 1 ]]; then
  echo "run.sh: WARDYN_MACVM_LIVE is not 1: every P-test would skip, so nothing is run" >&2
  exit 2
fi
command -v go >/dev/null || { echo "run.sh: no go on PATH: test2json cannot convert the output" >&2; exit 2; }

mkdir -p "$out"
export WARDYN_MACVM_EVIDENCE=${WARDYN_MACVM_EVIDENCE:-$out/evidence}

listed=$("$bin" -test.list "$pattern") || { echo "run.sh: -test.list failed" >&2; exit 2; }
names=$(grep -v '^TestP19_' <<<"$listed"; grep '^TestP19_' <<<"$listed")
if [[ -z "${names//[[:space:]]/}" ]]; then
  echo "run.sh: no test matches $pattern" >&2
  exit 2
fi

for name in $names; do
  "$bin" -test.v=test2json -test.run "^${name}\$" -test.timeout "${WARDYN_MACVM_TIMEOUT:-5h}" \
    >"$out/$name.raw" 2>"$out/$name.stderr"
  code=$?
  echo "$code" >"$out/$name.exit"
  echo "$name exit $code"
  go tool test2json -t -p macvm <"$out/$name.raw" >"$out/$name.json" || echo "$name: test2json failed" >&2
done

WARDYN_MACVM_SUMMARIZE="$out" "$bin" -test.run '^$'
