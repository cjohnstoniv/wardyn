# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/lib/verify-demo-take-grade.sh — the shared grader runner.
# Sourced by scripts/verify-demo-take.sh (with every other lib in this
# directory); uses the caller's ok/bad, resolved at call time after the caller
# defines them. It lives out of line for the reason the caller's own source loop
# states: scripts/check-file-size.sh caps every scripts/*.sh at 1000 lines, and
# the budget is spent on a new lib rather than on inlining.
# grade_py runs one grader program and refuses to be silent about its own failure.
#
#   grade_py <outfile> <what> [argv…]  3<<'PYEOF' < <(printf '%s\0' "$body" …)
#
# THE PROGRAM RIDES fd 3, THE FEED RIDES STDIN, and that swap is the whole fix.
# Every call site used to hand a response BODY to python as an ARGV STRING under
# `2>/dev/null`. Past MAX_ARG_STRLEN — 128 KiB, which one busy `/audit?limit=1000`
# clears — execve fails E2BIG, the message went to /dev/null, the output file came
# back EMPTY, and the `while read` loop after the call emitted NO VERDICTS AT ALL.
# A grader that could not read its input graded as "the thing is absent": a green
# take on exactly the stack busy enough to be worth checking. printf is a bash
# BUILTIN, so nothing is exec'd with the body on stdin, at any size.
#
# NO BLOCK MAY EMIT ZERO VERDICTS WITHOUT FAILING. A non-zero exit or an empty
# output file is a `bad` here — never a skip, never a silent pass.
#
# The program reads its feeds with `sys.stdin.buffer.read().split(b"\0")` (in the
# order the caller printf'd them) and its short arguments from sys.argv[1:].
grade_py() {
  local out="$1" what="$2"; shift 2
  local rc
  python3 /dev/fd/3 "$@" >"${out}" 2>"${out}.err"; rc=$?
  if ((rc != 0)); then
    bad "${what}: the grader program itself failed (exit ${rc}) — every verdict it would have printed is MISSING, not passing: $(tr '\n' ' ' <"${out}.err" | tail -c 400)"
  elif [[ ! -s "${out}" ]]; then
    bad "${what}: the grader program printed nothing — a block that emits zero verdicts is a silent pass, not a green take"
  fi
  rm -f "${out}.err"
  ((rc == 0))
}
