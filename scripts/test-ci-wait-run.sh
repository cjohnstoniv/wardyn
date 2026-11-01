#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-ci-wait-run.sh — train-suites waits on the dispatched nightly through
# ci-wait-run.sh: a read that keeps failing must end the step within a bound
# naming the last error, a single failed read must not, and the verdict is the
# run's conclusion.
#
# Daemon-free, network-free: a fake `gh` on PATH replays a queue of canned
# replies (`FAIL:<stderr>` fails that call; once the queue is empty every call
# fails with "HTTP 404").
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "FAIL: $*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir "$WORK/bin"
cat >"$WORK/bin/gh" <<'GH'
#!/usr/bin/env bash
echo x >>"$FAKE_GH_CALLS"
line="$(head -n1 "$FAKE_GH_QUEUE" 2>/dev/null || true)"
sed -i 1d "$FAKE_GH_QUEUE" 2>/dev/null || true
case "$line" in
  "") echo "HTTP 404: Not Found" >&2; exit 1 ;;
  FAIL:*) echo "${line#FAIL:}" >&2; exit 1 ;;
  *) echo "$line" ;;
esac
GH
chmod +x "$WORK/bin/gh"

# run <name> <reply>... runs the wait against a queue of replies; sets rc, out, calls.
run() {
  local name="$1"; shift
  printf '%s\n' "$@" >"$WORK/queue"
  : >"$WORK/calls"
  rc=0
  out="$(PATH="$WORK/bin:$PATH" FAKE_GH_QUEUE="$WORK/queue" FAKE_GH_CALLS="$WORK/calls" \
    POLL_SECS=0 RETRY_SECS=0 "$ROOT/scripts/ci-wait-run.sh" 42 2>&1)" || rc=$?
  calls="$(wc -l <"$WORK/calls")"
  echo "-- $name: rc=$rc calls=$calls"
}

# A read that always fails ends the step at the bound, naming the error.
run always-fails ""
[ "$rc" -ne 0 ] || fail "always-failing read: want a failed step, got rc=0"
[ "$calls" -eq 10 ] || fail "always-failing read: want 10 reads, got $calls"
grep -q 'HTTP 404' <<<"$out" || fail "always-failing read: last error not named: $out"

# One failed read followed by a success still passes.
run fails-once "FAIL:HTTP 502" in_progress completed success
[ "$rc" -eq 0 ] || fail "one failed read then success: want rc=0, got $rc: $out"

# Failures that never reach the bound in a row are tolerated (the count resets).
run nine-then-ok "FAIL:a" "FAIL:a" "FAIL:a" "FAIL:a" "FAIL:a" "FAIL:a" "FAIL:a" "FAIL:a" "FAIL:a" \
  in_progress "FAIL:b" completed success
[ "$rc" -eq 0 ] || fail "nine failed reads in a row then success: want rc=0, got $rc: $out"

# One line per poll.
run poll-lines in_progress in_progress completed success
[ "$(grep -c 'status ' <<<"$out")" -eq 3 ] || fail "want one status line per poll (3): $out"

# The verdict is the conclusion.
run failure completed failure
[ "$rc" -ne 0 ] || fail "concluded failure: want a failed step"
grep -q "concluded 'failure'" <<<"$out" || fail "concluded failure: not named: $out"

echo "test-ci-wait-run: ok"
