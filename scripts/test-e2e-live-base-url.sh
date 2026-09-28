#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free regression test for run-ui-e2e.sh's run_lane() (issue #1207).
#
# THE regression this pins: run_lane() unconditionally exported
# WARDYN_E2E_BASE_URL="http://localhost:${LANE_ADDR[lane]#*:}", clobbering
# the WARDYN_E2E_BASE_URL that LIVE mode (WARDYN_E2E_WALK_BASE_URL set) had
# already exported earlier in the same script to point at the real,
# already-running external Wardyn the walk drives. LANE_ADDR[0] in LIVE mode
# is only the unused, auto-picked port reserved for a hermetic backend that
# never boots there, so every walk spec silently navigated to a random local
# port nothing was listening on (kind-sso-walk, compose-sso-roles).
#
# Extracts run_lane()'s env-setup preamble straight from the live source
# (same live-source pattern as scripts/test-e2e-lane-kill-tree.sh's
# extract_func), stopping before the per-spec loop that would otherwise try
# to shell out to e2e-backend.sh / playwright — this test only needs to
# prove which value WARDYN_E2E_BASE_URL ends up holding.
#
# Usage: scripts/test-e2e-live-base-url.sh   (exit 0 = PASS, non-zero = FAIL)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_E2E_SH="${REPO_ROOT}/scripts/run-ui-e2e.sh"

fail() { echo "test-e2e-live-base-url: FAIL: $*" >&2; exit 1; }

# Pull run_lane()'s body from "run_lane() {" through the line just before
# the "if [[ -n "${label}" ]]" block (label/log() redefinition and the whole
# per-spec loop are irrelevant to — and, for the loop, unsafe to run in —
# this test), then close it as a standalone function.
preamble="$(sed -n '/^run_lane() {$/,/^  if \[\[ -n "\${label}" \]\]; then$/p' "${RUN_E2E_SH}" | head -n -1)"
[[ -n "${preamble}" ]] || fail "run_lane() preamble not found in ${RUN_E2E_SH}"
echo "${preamble}" | grep -q 'WARDYN_E2E_BASE_URL' \
  || fail "extracted preamble no longer sets WARDYN_E2E_BASE_URL — extraction markers are stale"
eval "${preamble}
}"

# Fixture: LIVE mode with a single lane, matching what run-ui-e2e.sh itself
# sets up (NUM_LANES forced to 1, LANE_ADDR[0] seeded from the ordinary
# auto-picked port that is never actually bound in LIVE mode).
LIVE_BASE_URL="http://localhost:8280"
NUM_LANES=1
LANE_ADDR=(":54321")
LANE_UI_ADDR=(":54322")
LANE_INTERNAL_ADDR=(":54323")
LANE_DB=("wardyn_e2e_test")
PG_HOSTPORT="localhost:55432"
# Mirrors the real script: LIVE mode exports WARDYN_E2E_BASE_URL from
# LIVE_BASE_URL well before run_lane() is ever called (line ~146).
export WARDYN_E2E_BASE_URL="${LIVE_BASE_URL}"

run_lane 0

[[ "${WARDYN_E2E_BASE_URL}" == "${LIVE_BASE_URL}" ]] \
  || fail "WARDYN_E2E_BASE_URL=${WARDYN_E2E_BASE_URL@Q}, want ${LIVE_BASE_URL@Q} (LIVE mode clobbered by the per-lane port — the #1207 regression is back)"
echo "  [pass] LIVE mode: run_lane() left WARDYN_E2E_BASE_URL at ${WARDYN_E2E_BASE_URL}"

# Non-LIVE (hermetic) mode must be unchanged: run_lane() still sets
# WARDYN_E2E_BASE_URL from its own lane's auto-picked port.
LIVE_BASE_URL=""
run_lane 0
want="http://localhost:54321"
[[ "${WARDYN_E2E_BASE_URL}" == "${want}" ]] \
  || fail "hermetic mode: WARDYN_E2E_BASE_URL=${WARDYN_E2E_BASE_URL@Q}, want ${want@Q}"
echo "  [pass] hermetic mode: run_lane() still sets WARDYN_E2E_BASE_URL from its own lane port"
