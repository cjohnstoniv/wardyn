#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free test for scripts/test-drive.sh's edges: a --section outside 1-7 is refused before
# any work, and the closing summary fails a run in which no check ran, points at the admin-token
# sign-in, and echoes only the published demo token. The summary is extracted from the live
# script and called with stubbed counters, so no stack is needed.
#
# Usage: scripts/test-test-drive.sh   (exit 0 = PASS, non-zero = FAIL)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRIVE="${ROOT}/scripts/test-drive.sh"
fail() { echo "test-test-drive: FAIL: $*" >&2; exit 1; }
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# 1) A section outside 1-7 stops before the stack is touched; a valid one gets past the check
#    (to the health check, which an unreachable URL fails with its own message).
for s in 0 8 9 abc 1x; do
  rc=0; WARDYN_URL=http://127.0.0.1:9 "$DRIVE" --section "$s" >/dev/null 2>"$TMP/err" || rc=$?
  [ "$rc" -ne 0 ] && grep -q "unknown section: $s" "$TMP/err" || fail "--section $s was not refused (rc=$rc): $(cat "$TMP/err")"
done
rc=0; WARDYN_URL=http://127.0.0.1:9 "$DRIVE" --section 3 >/dev/null 2>"$TMP/err" || rc=$?
grep -q "unknown section" "$TMP/err" && fail "--section 3 was refused"
grep -q "not reachable" "$TMP/err" || fail "--section 3 did not reach the health check: $(cat "$TMP/err")"

# 2) The summary, extracted from the live script.
fn="$(sed -n '/^summary() {$/,/^}$/p' "$DRIVE")"
[ -n "$fn" ] || fail "scripts/test-drive.sh has no summary() to extract"
eval "$fn"
note() { printf 'NOTE %s\n' "$*"; }
WARDYN_URL=http://localhost:8080 COMPOSE_FILE=compose.yaml

pass=0 fail=0 WARDYN_ADMIN_TOKEN=demo-admin-token
summary >"$TMP/out" && fail "a run in which no check ran ended green"
grep -q "no check ran" "$TMP/out" || fail "the summary did not say that no check ran"

pass=3 fail=1
summary >"$TMP/out" && fail "a run with a failed check ended green"

pass=3 fail=0
summary >"$TMP/out" || fail "a run whose checks all passed ended red"
grep -q "Bearer demo-admin-token" "$TMP/out" || fail "the published demo token is no longer shown"
grep -q "SSO" "$TMP/out" && fail "the summary still points at an SSO login the demo stack does not configure"
grep -q "sign in with the admin token" "$TMP/out" || fail "the summary no longer names the admin-token sign-in"

WARDYN_ADMIN_TOKEN=s3cr3t-real-fleet-admin-token
summary >"$TMP/out" || fail "a real token made a passing run red"
grep -q "s3cr3t-real-fleet-admin-token" "$TMP/out" && fail "the summary printed a real admin token"
grep -qF 'Bearer $WARDYN_ADMIN_TOKEN' "$TMP/out" || fail "the summary no longer names the variable in place of a real token"

echo "test-test-drive: PASS"
