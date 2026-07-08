#!/usr/bin/env bash
# Run the SUBSCRIPTION proxy-side token-injection live e2e lanes (test/e2e/live):
#
#   - TestLive_SubscriptionInject      — the safe default. Launching a subscription
#     run authors the re-mintable injection grant + auto-enables TLS-MITM of
#     api.anthropic.com (proven by the run.llm.subscription_inject audit event);
#     `wardyn attach` then reaches a live shell whose curl to api.anthropic.com
#     traverses the injected+MITM'd path.
#   - TestLive_SubscriptionEscapeHatch — WARDYN_SUBSCRIPTION_INJECT=off. NO
#     injection grant is authored (no audit event, no MITM), so a garbage sandbox
#     credential reaches Anthropic over the opaque tunnel unmodified and is
#     rejected 401 (the legacy resident-copy behavior).
#
# A single wardynd is in exactly ONE inject mode, so this driver RESTARTS wardynd
# with WARDYN_SUBSCRIPTION_INJECT flipped between the two lanes and RESTORES the
# safe default (on) at the end. It owns the wardynd lifecycle for its run.
#
# GUARD: Docker-dependent. No-op unless WARDYN_TEST_DOCKER=1.
# PREREQ: staged Claude subscription creds (scripts/stage-claude-creds.sh) so the
# .claude mounts can engage subscription mode. Without them both lanes SKIP.
#
# Optional: WARDYN_E2E_REAL_MODEL=1 additionally drives a real `claude` turn in the
# attached PTY of the inject-on run (needs the claude-code image staged); a
# rate-limit reply counts as PASS, only an auth error fails.
set -uo pipefail

if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  echo "run-e2e-subscription: set WARDYN_TEST_DOCKER=1 to run the Docker-dependent subscription e2e (skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
source "${ROOT}/scripts/lib/images.sh"
source "${ROOT}/scripts/lib/common.sh"
BASE="${WARDYN_E2E_BASE_URL:-http://localhost:8080}"
export WARDYN_ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"
WARDYND_LOG="$(mktemp /tmp/wardynd-e2e-sub.XXXXXX.log)"

# Honor an existing DOCKER_HOST; else target the wardyn docker socket if present
# (the host-mode default on this box). The runner daemon holds the agent images.
if [[ -z "${DOCKER_HOST:-}" && -S /var/run/wardyn-docker.sock ]]; then
  export DOCKER_HOST="unix:///var/run/wardyn-docker.sock"
fi

warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; restore_default; exit 1; }

command -v docker >/dev/null 2>&1 || die "docker not found"

# ── prereq: staged subscription creds ────────────────────────────────────────
CREDS_DIR="${WARDYN_E2E_CLAUDE_CREDS:-$HOME/.wardyn/claude-creds}"
if [[ ! -d "${CREDS_DIR}/.claude" || ! -f "${CREDS_DIR}/.claude.json" ]]; then
  warn "no staged Claude subscription creds at ${CREDS_DIR} (run scripts/stage-claude-creds.sh)."
  warn "the subscription lanes REQUIRE the .claude mounts to engage subscription mode — nothing to prove; exiting 0 (skip)."
  exit 0
fi

# ── build the pieces ─────────────────────────────────────────────────────────
log "building wardynd (-tags docker) + wardyn"
go build -tags docker -o bin/wardynd ./cmd/wardynd || die "build wardynd failed"
go build -o bin/wardyn ./cmd/wardyn || die "build wardyn failed"

# Ensure the images a started wardynd needs exist on the runner daemon.
image_missing wardyn/wardyn-proxy:demo && { log "building wardyn-proxy image"; docker compose -f deploy/compose/docker-compose.yaml --profile build-only build proxy-image >/dev/null 2>&1 || die "build proxy image failed"; }
image_missing wardyn/agent-oracle:demo && { log "building oracle image"; docker build -q -f deploy/images/oracle/Dockerfile -t wardyn/agent-oracle:demo . >/dev/null || die "build oracle image failed"; }
if [[ "${WARDYN_E2E_REAL_MODEL:-}" == "1" ]]; then
  image_missing wardyn/agent-claude-code:demo && { log "building claude-code image (real-model lane)"; docker build -q -f deploy/images/claude-code/Dockerfile -t wardyn/agent-claude-code:demo . >/dev/null || die "build claude-code image failed"; }
fi

# ── wardynd lifecycle (this driver owns it for the duration) ─────────────────
STARTED_PID=""

stop_wardynd() {
  pkill -f 'bin/wardynd' >/dev/null 2>&1 || true
  wait_down "${BASE}" || warn "a wardynd is still answering ${BASE} after stop"
  STARTED_PID=""
}

start_wardynd() {  # $1 = on|off
  local mode="$1"
  # Stop any wardynd already bound to :8080 first — otherwise this start would
  # fail to bind, wait_healthy would see the OLD instance, and the lane would run
  # against the wrong inject mode.
  stop_wardynd
  log "starting wardynd (WARDYN_SUBSCRIPTION_INJECT=${mode}, DOCKER_HOST=${DOCKER_HOST:-<default>})"
  # run-host.sh reproduces the rest of the host-mode env (runner=docker, the
  # subscription ceiling policy, model pin); we only pin the inject mode + socket.
  WARDYN_SUBSCRIPTION_INJECT="${mode}" nohup ./scripts/run-host.sh >>"${WARDYND_LOG}" 2>&1 &
  STARTED_PID=$!
  if ! wait_healthy "${BASE}"; then
    tail -25 "${WARDYND_LOG}" >&2
    die "wardynd (inject=${mode}) did not become healthy at ${BASE}"
  fi
}

restore_default() {
  # Leave the box in the safe default (inject on), matching normal operation.
  # Guarded so `die`-then-EXIT-trap can't restart it twice.
  if [[ "${SKIP_RESTORE:-}" == "1" || "${RESTORE_DONE:-}" == "1" ]]; then return; fi
  RESTORE_DONE=1
  stop_wardynd
  log "restoring wardynd to the safe default (inject=on)"
  WARDYN_SUBSCRIPTION_INJECT="on" nohup ./scripts/run-host.sh >>"${WARDYND_LOG}" 2>&1 &
  wait_healthy "${BASE}" || warn "wardynd did not come back healthy after restore (logs: ${WARDYND_LOG})"
}
trap restore_default EXIT

# ── run one lane, distinguishing PASS from SKIP (go test exits 0 on skip) ────
FAILED=0
run_lane() {  # $1 = expect(on|off)  $2 = test name
  local expect="$1" name="$2"
  local out
  log "running ${name} (WARDYN_E2E_EXPECT_INJECT=${expect})"
  out="$(WARDYN_TEST_DOCKER=1 WARDYN_E2E_EXPECT_INJECT="${expect}" WARDYN_E2E_BASE_URL="${BASE}" \
        go test -tags docker ./test/e2e/live/ -run "^${name}\$" -count=1 -v -timeout 360s 2>&1)"
  local rc=$?
  echo "${out}"
  if [[ ${rc} -ne 0 ]]; then
    warn "${name} FAILED (rc=${rc})"; FAILED=1; return
  fi
  if grep -q -- "--- SKIP: ${name}" <<<"${out}"; then
    warn "${name} SKIPPED (prerequisite not met) — NOT a proof"; FAILED=1; return
  fi
  if grep -q -- "--- PASS: ${name}" <<<"${out}"; then
    log "${name} PASSED"; return
  fi
  warn "${name}: could not confirm PASS in output"; FAILED=1
}

# Lane 1: inject ON (the safe default).
start_wardynd on
run_lane on TestLive_SubscriptionInject

# Lane 2: escape hatch (inject OFF).
stop_wardynd
start_wardynd off
run_lane off TestLive_SubscriptionEscapeHatch

# restore_default runs on EXIT.
if [[ ${FAILED} -eq 0 ]]; then
  log "SUBSCRIPTION e2e PASSED (inject-on attach-walkthrough + inject-off escape hatch)"
  exit 0
fi
die "SUBSCRIPTION e2e had failures/skips — see output above"
