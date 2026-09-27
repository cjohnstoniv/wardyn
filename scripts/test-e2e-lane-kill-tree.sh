#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free regression test for run-ui-e2e.sh's kill_tree(), extracted
# straight from the real source (same live-source pattern as
# scripts/test-up-policy.sh's extract_func) so this pins the function that
# actually ships, not a reimplementation of it.
#
# THE regression this pins (PR #791 review): stop_lanes used to do
# `kill "${pids[@]}"`, which only reaches the backgrounded lane subshell. Its
# own child — the `pnpm exec playwright` process, and chromium under that —
# is reparented and keeps running: it finishes its remaining tests against a
# backend the script is tearing down and keeps writing into
# ui/test-results/<spec>/ and test/reports/e2e/ after the script has exited.
# kill_tree() walks the descendant tree via `pgrep -P` (never `pkill -f`, so
# it cannot hit an unrelated process sharing a name) and kills leaves first.
#
# This test builds a 3-generation `sleep` tree standing in for
# lane-subshell -> playwright -> chromium, confirms a plain `kill` of just
# the top PID leaks the descendants (the bug, demonstrated here rather than
# asserted, since asserting a known bug would make this test always red), then
# confirms kill_tree of the same top PID takes out every generation.
#
# Usage: scripts/test-e2e-lane-kill-tree.sh   (exit 0 = PASS, non-zero = FAIL)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_E2E_SH="${REPO_ROOT}/scripts/run-ui-e2e.sh"

extract_func() {  # $1=function name -> its body, verbatim
  sed -n "/^$1() {/,/^}/p" "${RUN_E2E_SH}"
}
body="$(extract_func kill_tree)"
[[ -n "${body}" ]] || { echo "test-e2e-lane-kill-tree: FAIL: kill_tree() not found in ${RUN_E2E_SH}" >&2; exit 1; }
eval "${body}"

fail() { echo "test-e2e-lane-kill-tree: FAIL: $*" >&2; exit 1; }

# Pins the wiring, not just the function in isolation: stop_lanes must
# actually call kill_tree on each lane pid. Without this, kill_tree() could
# pass every check above while stop_lanes silently went back to a plain
# `kill "${pids[@]}"` (the original regression) and this test would never
# notice, since it evals kill_tree() straight out of the source rather than
# exercising stop_lanes itself.
stop_lanes_body="$(extract_func stop_lanes)"
[[ -n "${stop_lanes_body}" ]] || fail "stop_lanes() not found in ${RUN_E2E_SH}"
echo "${stop_lanes_body}" | grep -q 'kill_tree' \
  || fail "stop_lanes() no longer calls kill_tree() in ${RUN_E2E_SH}"

# make_tree DURATION: a 3-generation `sleep DURATION` tree (grandparent ->
# parent -> leaf), standing in for lane-subshell -> playwright -> chromium.
# Each level has a trailing statement after `sleep` so bash forks a real
# child instead of exec-replacing the subshell into `sleep` itself, which
# would collapse the generations this test needs. Sets MADE_TREE_PID rather
# than echoing it: run through `x="$(make_tree ...)"`, the background job
# lands in the COMMAND-SUBSTITUTION subshell instead of this script, and it
# does not survive that subshell's exit (confirmed empirically — not the
# ordinary "orphans get reparented and keep running" case).
make_tree() {
  local duration="$1"
  ( ( ( sleep "${duration}"; : ) & wait ) & wait ) &
  MADE_TREE_PID=$!
}
wait_for_leaf() {  # TOP_PID -> the tree's `sleep` pid on stdout, "" if it never showed up
  # Walks descendants via `pgrep -P` from TOP_PID, exactly like kill_tree()
  # itself, instead of `pgrep -f` matching the sleep duration against the
  # whole machine: a stray `sleep 31` or `sleep 32` left over from another
  # worktree or an aborted run would otherwise be picked up (and killed) by
  # this test, and would falsely report "the orphan-process regression is
  # back".
  local top="$1" pid child comm
  for _ in $(seq 1 20); do
    pid="${top}"
    while child="$(pgrep -P "${pid}" 2>/dev/null | head -1)" && [[ -n "${child}" ]]; do
      pid="${child}"
    done
    comm="$(ps -o comm= -p "${pid}" 2>/dev/null)"
    if [[ "${pid}" != "${top}" && "${comm}" == "sleep" ]]; then
      echo "${pid}"
      return
    fi
    sleep 0.1
  done
  echo ""
}

# RED, demonstrated on its own fixture: killing only the top PID must NOT
# take the leaf with it — that is the exact shape of the bug
# (`kill "${pids[@]}"` alone, before this fix).
make_tree 31
red_top="${MADE_TREE_PID}"
red_leaf="$(wait_for_leaf "${red_top}")"
[[ -n "${red_leaf}" ]] || fail "the sleep-31 leaf never started — test fixture is broken, not the fix"
kill "${red_top}" 2>/dev/null
sleep 0.3
if ! kill -0 "${red_leaf}" 2>/dev/null; then
  fail "fixture is wrong: killing only the top PID already took the leaf — this test can't tell red from green"
fi
echo "  [confirmed] kill of the top PID alone leaves the leaf running (the bug)"
kill -9 "${red_leaf}" 2>/dev/null  # clean up the leak we just proved

# GREEN, on a fresh fixture (the tree above is already gone, so pgrep -P from
# its top PID would find nothing — a live tree is required to test the walk):
# kill_tree of the top PID must take out every generation.
make_tree 32
green_top="${MADE_TREE_PID}"
green_leaf="$(wait_for_leaf "${green_top}")"
[[ -n "${green_leaf}" ]] || fail "the sleep-32 leaf never started — test fixture is broken, not the fix"
kill_tree "${green_top}"
sleep 0.3
if kill -0 "${green_leaf}" 2>/dev/null; then
  kill -9 "${green_leaf}" 2>/dev/null
  fail "leaf (pid ${green_leaf}) survived kill_tree — the orphan-process regression is back"
fi
echo "  [pass] kill_tree reached the leaf through every generation"
