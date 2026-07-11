#!/usr/bin/env bash
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
# (the relaunch-confinement proof still holds — see test/e2e/RESULTS.md).
set -uo pipefail

if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  echo "run-e2e-live: set WARDYN_TEST_DOCKER=1 to run the Docker-dependent live e2e (skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
source "${ROOT}/scripts/lib/common.sh"
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
exit ${rc}
