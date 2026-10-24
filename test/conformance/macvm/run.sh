#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Runs the macOS VM live suite one P-test at a time, keeping each one's raw -test.v=test2json
# output, stderr and exit status under <out-dir>, then converts the raw output with Go's own
# test2json when a Go toolchain is on PATH (otherwise convert later:
# `go tool test2json -t -p macvm < P04.raw > P04.json`).
#
#   go test -c -tags macvm_live -o macvm_live.test ./test/conformance/macvm/
#   WARDYN_MACVM_LIVE=1 WARDYN_MACVM_RUNNER=<runner> test/conformance/macvm/run.sh ./macvm_live.test window/p-suite [regex]
#
# With no regex every TestP* runs, in order; a P-test that needs a person skips unless
# WARDYN_MACVM_PROMPTS=1. The script's own exit status is the worst of the P-tests'.
set -uo pipefail

bin=${1:?usage: run.sh <macvm_live.test> <out-dir> [test regex]}
out=${2:?usage: run.sh <macvm_live.test> <out-dir> [test regex]}
pattern=${3:-^TestP}
mkdir -p "$out"
export WARDYN_MACVM_OUT=${WARDYN_MACVM_OUT:-$out/evidence}

worst=0
for name in $("$bin" -test.list "$pattern"); do
  "$bin" -test.v=test2json -test.run "^${name}\$" -test.timeout "${WARDYN_MACVM_TIMEOUT:-5h}" \
    >"$out/$name.raw" 2>"$out/$name.stderr"
  code=$?
  echo "$code" >"$out/$name.exit"
  echo "$name exit $code"
  (( code > worst )) && worst=$code
  if command -v go >/dev/null; then
    go tool test2json -t -p macvm <"$out/$name.raw" >"$out/$name.json" || echo "$name: test2json failed" >&2
  fi
done
exit "$worst"
