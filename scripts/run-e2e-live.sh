#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Run the LIVE end-to-end task orchestrator (test/e2e/live) against a real
# host-mode wardynd with the docker runner: real sandboxes, real graders.
#
# It proves — per confinement tier — that agents actually complete tasks AND that
# sandboxes allow/block egress correctly:
#   - TierMatrix:      CC1 runs; an uninstalled CC2/CC3 FAILS CLOSED (422).
#   - Tasks (oracle):  every corpus task COMPLETES + grades PASS on final state.
#   - Interactive:     attach WS-PTY + in-session egress boundary (evil -> 403).
#   - RecordingReplay: relaunch from a recorded profile stays confined + works.
#   - RealModel (opt): a real claude-code agent does the work (needs staged creds
#                      + WARDYN_E2E_REAL_MODEL=1; skipped otherwise).
#
# GUARD: Docker-dependent. No-op unless WARDYN_TEST_DOCKER=1.
# Idempotent: reuses a wardynd already listening on :8080, else starts one and
# tears it down on exit. On native docker (Linux/CI) the proxy->control-plane
# egress callback routes, so the recording-replay SYNTHESIS allowlist is populated
# too; on a managed-VM docker (Docker Desktop/WSL) that callback may not route
# (the relaunch-confinement proof still holds).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
source "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  skip_lane "run-e2e-live: set WARDYN_TEST_DOCKER=1 to run the Docker-dependent live e2e (skipping)."
fi
BASE="${WARDYN_E2E_BASE_URL:-http://localhost:8080}"
export WARDYN_ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"
STARTED_WARDYND=""
WARDYND_LOG="$(mktemp /tmp/wardynd-e2e-live.XXXXXX.log)"

die() { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; teardown; exit 1; }

teardown() {
  if [[ -n "${STARTED_WARDYND}" ]]; then
    log "stopping the wardynd this script started (pid ${STARTED_WARDYND})"
    kill "${STARTED_WARDYND}" >/dev/null 2>&1 || true
  fi
}
trap teardown EXIT

command -v docker >/dev/null 2>&1 || die "docker not found"

# ── build the pieces the orchestrator needs ──────────────────────────────────
log "building wardynd (-tags docker), wardyn, and the oracle agent image"
go build -tags docker -o bin/wardynd ./cmd/wardynd || die "build wardynd failed"
go build -o bin/wardyn ./cmd/wardyn || die "build wardyn failed"
docker build -q -f deploy/images/oracle/Dockerfile -t wardyn/agent-oracle:local . >/dev/null || die "build oracle image failed"
# The proxy sidecar image + a built UI are only needed if we start wardynd.

# ── ensure a wardynd is listening ────────────────────────────────────────────
if curl -sf "${BASE}/healthz" >/dev/null 2>&1; then
  log "reusing the wardynd already listening at ${BASE}"
else
  # THE PROXY SIDECAR IMAGE, on a host that has never built one. Every sandbox
  # is an agent container PLUS a wardyn-proxy sidecar, and run-host.sh points the
  # daemon at wardyn/wardyn-proxy:local — published nowhere. A dev box has it from
  # `make setup`; a cold CI runner does not, and then every run dies at dispatch
  # in under a second with nothing in this script's output saying why (the hosted
  # nightly was red on exactly that, behind the 422 that 0.7.5 fixed).
  PROXY_IMAGE="${WARDYN_PROXY_IMAGE:-wardyn/wardyn-proxy:local}"
  if ! docker image inspect "${PROXY_IMAGE}" >/dev/null 2>&1; then
    log "building the proxy sidecar image (${PROXY_IMAGE} absent on this host)"
    docker build -q -f deploy/compose/Dockerfile.proxy -t "${PROXY_IMAGE}" . >/dev/null || die "build proxy sidecar image failed"
  fi
  log "starting a host-mode wardynd (logs: ${WARDYND_LOG})"
  # Ensure compose Postgres is up (run-host.sh expects 127.0.0.1:5432).
  docker compose -f deploy/compose/docker-compose.yaml up -d postgres >/dev/null 2>&1 || true
  [[ -d ui/dist ]] || { log "building the UI (ui/dist absent)"; (cd ui && pnpm install --frozen-lockfile >/dev/null 2>&1 && pnpm build >/dev/null 2>&1) || true; }
  nohup ./scripts/run-host.sh >"${WARDYND_LOG}" 2>&1 &
  STARTED_WARDYND=$!
  wait_healthy "${BASE}" 30 || { tail -20 "${WARDYND_LOG}"; die "wardynd did not become healthy"; }
fi

log "confinement classes: $(curl -s "${BASE}/healthz" | sed -n 's/.*"confinement_classes":\(\[[^]]*\]\).*/\1/p')"

# ── run the orchestrator ─────────────────────────────────────────────────────
RUN_FILTER="TestLive_TierMatrix|TestLive_Tasks|TestLive_Interactive|TestLive_RecordingReplay"
if [[ "${WARDYN_E2E_REAL_MODEL:-}" == "1" ]]; then
  RUN_FILTER="${RUN_FILTER}|TestLive_RealModel"
  log "REAL-MODEL lane ENABLED (WARDYN_E2E_REAL_MODEL=1)"
fi

log "running: go test -tags docker ./test/e2e/live -run '${RUN_FILTER}'"
WARDYN_TEST_DOCKER=1 WARDYN_E2E_BASE_URL="${BASE}" \
  go test -tags docker ./test/e2e/live/ -run "${RUN_FILTER}" -count=1 -v
rc=$?

[[ ${rc} -eq 0 ]] && log "LIVE e2e PASSED" || log "LIVE e2e FAILED (rc=${rc})"
# A run that goes FAILED at dispatch tells the TEST nothing but its state; the
# reason is in the daemon's log, which on a CI runner is gone with the VM. Print
# its warnings and errors on a red run so the next hosted failure names itself.
if [[ ${rc} -ne 0 && -n "${STARTED_WARDYND:-}" && -s "${WARDYND_LOG}" ]]; then
  log "wardynd WARN/ERROR lines (${WARDYND_LOG}):"
  grep -Ei 'level=(warn|error)|"level":"(warn|error)"' "${WARDYND_LOG}" | tail -40 || true
fi
exit ${rc}
