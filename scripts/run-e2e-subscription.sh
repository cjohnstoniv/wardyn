#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

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
# stop_wardynd below frees the port with fuser, or ss + kill where fuser is
# absent; with neither the stop is a no-op and the next start would silently
# reach the OLD daemon.
command -v fuser >/dev/null 2>&1 || command -v ss >/dev/null 2>&1 \
  || die "fuser (psmisc) or ss (iproute2) required"

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
image_missing wardyn/wardyn-proxy:local && { log "building wardyn-proxy image"; docker compose -f deploy/compose/docker-compose.yaml --profile build-only build proxy-image >/dev/null 2>&1 || die "build proxy image failed"; }
image_missing wardyn/agent-oracle:local && { log "building oracle image"; docker build -q -f deploy/images/oracle/Dockerfile -t wardyn/agent-oracle:local . >/dev/null || die "build oracle image failed"; }
if [[ "${WARDYN_E2E_REAL_MODEL:-}" == "1" ]]; then
  image_missing wardyn/agent-claude-code:local && { log "building claude-code image (real-model lane)"; docker build -q -f deploy/images/claude-code/Dockerfile -t wardyn/agent-claude-code:local . >/dev/null || die "build claude-code image failed"; }
fi

# ── wardynd lifecycle (this driver owns it for the duration) ─────────────────
# ponytail: teardown targets whatever holds BASE's port, not a remembered PID,
# on purpose — it must also reap a wardynd this script did not start (see
# start_wardynd below). Do not "improve" it by remembering the nohup'd PID;
# that reintroduces the false-green this guards against. (run-e2e-live.sh
# reads its own PID because it has the opposite policy: kill only what it
# started.)
#
# This used to be `pkill -f 'bin/wardynd'`, which matches every wardynd on the
# host by command-line substring — including a developer's own daemon serving
# an unrelated port (#210). Killing only the process bound to BASE's port
# keeps the "reap a stray/foreign wardynd" property the pattern match was for,
# without touching one bound elsewhere.
# The port is what follows the LAST colon of the host part, as e2e-backend.sh
# splits its ADDR; the path is cut first so a trailing slash cannot ride along.
BASE_PORT="${BASE#*://}"; BASE_PORT="${BASE_PORT%%/*}"; BASE_PORT="${BASE_PORT##*:}"
if [[ ! "${BASE_PORT}" =~ ^[0-9]+$ ]]; then
  warn "WARDYN_E2E_BASE_URL (${BASE}) names no port; this driver stops whatever holds that port, so it needs one"
  exit 1
fi
stop_wardynd() {
  local pids
  if command -v fuser >/dev/null 2>&1; then
    # -TERM, not fuser's SIGKILL default: wardynd does a graceful shutdown on
    # SIGTERM (cmd/wardynd/boot_serve.go waits for background work, adapters.go
    # flushes audit), and the ss fallback below already sends SIGTERM via plain
    # `kill`. A bare -k would skip that shutdown and disagree with the fallback
    # for no reason.
    fuser -k -TERM "${BASE_PORT}/tcp" >/dev/null 2>&1 || true
  else
    pids="$(ss -ltnpH "sport = :${BASE_PORT}" 2>/dev/null | grep -oE 'pid=[0-9]+' | cut -d= -f2 | sort -u)"
    # shellcheck disable=SC2086 # one PID per word
    [[ -n "${pids}" ]] && kill ${pids} 2>/dev/null
  fi
  wait_down "${BASE}" || warn "a wardynd is still answering ${BASE} after stop"
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
