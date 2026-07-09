#!/usr/bin/env bash
# Run a FRESH, EMPTY Wardyn control plane locally with the AI Run Composer enabled,
# for hands-on testing in the browser. Unlike scripts/e2e-backend.sh this seeds
# NOTHING — you start with an empty Runs list and create your first run via the
# composer.
#
# Backend: the composer defaults to your resident Claude subscription (the `claude`
# CLI, no API key) with a deterministic 'fake' backend as a fallback you can pick
# in the provider dropdown. Override the registry with WARDYN_LOCAL_COMPOSER.
#
# Usage:
#   scripts/run-local.sh            # build, fresh DB, serve (backgrounded; prints URL)
#   scripts/run-local.sh down       # stop it + drop the local DB
#   scripts/run-local.sh logs       # tail the wardynd log
#
# Env: WARDYN_LOCAL_PORT (default 9090), WARDYN_LOCAL_TOKEN (default wardyn-local-admin),
#      WARDYN_LOCAL_PG_CONTAINER (default wardyn-test-pg), WARDYN_LOCAL_COMPOSER (JSON).
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
source "${REPO_ROOT}/scripts/lib/images.sh"
source "${REPO_ROOT}/scripts/lib/common.sh"

PORT="${WARDYN_LOCAL_PORT:-9090}"
TOKEN="${WARDYN_LOCAL_TOKEN:-wardyn-local-admin}"
PG_CONTAINER="${WARDYN_LOCAL_PG_CONTAINER:-wardyn-test-pg}"
DB="wardyn_local"
DSN="postgres://wardyn:wardyn@localhost:55432/${DB}?sslmode=disable"
# Fixed age identity so signing/session keys survive restarts (local test only).
AGE_KEY="${WARDYN_LOCAL_AGE_KEY:-AGE-SECRET-KEY-1CMRQ5GEN2G4NKWXQQ4DKK7GSMJDZXXW69W9QN3ALX8Y49CF6RLYS7Y6KHF}"
BIN_DIR="${REPO_ROOT}/.local-bin"
PID_FILE="${BIN_DIR}/wardynd-${PORT}.pid"
LOG_FILE="${BIN_DIR}/wardynd-${PORT}.log"
BASE_URL="http://localhost:${PORT}"

# Composer registry: your Claude subscription (default) + a deterministic fake.
# A local ollama model could be added as an OpenAI-compatible backend, but its
# strict-schema support varies, so the subscription is the reliable default.
DEFAULT_COMPOSER='{"default":"claude-opus","backends":[
  {"name":"claude-opus","wire":"cli","transport":"claude","model":"opus","enabled":true},
  {"name":"claude-sonnet","wire":"cli","transport":"claude","model":"sonnet","enabled":true},
  {"name":"demo-fake","wire":"fake","model":"deterministic-stub"},
  {"name":"demo-interview","wire":"fake","transport":"interview","model":"deterministic-stub"}
]}'
COMPOSER_CFG="${WARDYN_LOCAL_COMPOSER:-$DEFAULT_COMPOSER}"

log()  { printf '\033[1;36m[wardyn-local]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[wardyn-local:err]\033[0m %s\n' "$*" >&2; exit 1; }
psql_local() { docker exec -i "${PG_CONTAINER}" psql -U wardyn -d "$1" "${@:2}"; }

cmd_down() {
  if [[ -f "${PID_FILE}" ]]; then kill "$(cat "${PID_FILE}")" >/dev/null 2>&1 || true; rm -f "${PID_FILE}"; fi
  if command -v fuser >/dev/null 2>&1; then fuser -k "${PORT}/tcp" >/dev/null 2>&1 || true; fi
  psql_local wardyn -c "DROP DATABASE IF EXISTS ${DB} WITH (FORCE);" >/dev/null 2>&1 || true
  log "stopped; ${DB} dropped"
}

cmd_logs() { tail -n 80 -f "${LOG_FILE}"; }

cmd_up() {
  mkdir -p "${BIN_DIR}"
  command -v claude >/dev/null 2>&1 || log "NOTE: 'claude' CLI not on PATH — the claude-subscription backend will error; use the demo-fake backend in the dropdown."
  if ! docker exec "${PG_CONTAINER}" pg_isready -U wardyn >/dev/null 2>&1; then
    if [[ "${PG_CONTAINER}" == "wardyn-test-pg" ]]; then
      log "Postgres container '${PG_CONTAINER}' not ready; self-provisioning via scripts/up.sh pg"
      "${REPO_ROOT}/scripts/up.sh" pg || die "scripts/up.sh pg failed to provision ${PG_CONTAINER}"
    else
      die "Postgres container '${PG_CONTAINER}' not ready on :55432. Start it (docker run ... postgres), or unset WARDYN_LOCAL_PG_CONTAINER to let 'scripts/up.sh pg' self-provision the default."
    fi
  fi

  if [[ "${WARDYN_LOCAL_SKIP_BUILD:-0}" != "1" ]]; then
    log "Building wardynd (-tags docker) + wardyn + UI bundle"
    # -tags docker compiles in the docker sandbox runner so runs (and workspace
    # Verify) can actually launch. The runner is still SELECTED at startup below.
    go build -tags docker -o "${BIN_DIR}/wardynd" ./cmd/wardynd || die "wardynd build failed"
    go build -o "${BIN_DIR}/wardyn"  ./cmd/wardyn  || die "wardyn build failed"
    ( cd ui && pnpm install --frozen-lockfile >/dev/null 2>&1 && pnpm build >/dev/null 2>&1 ) || die "UI build failed"
  fi

  # ── Sandbox runner selection ────────────────────────────────────────────────
  # Default to the docker runner (CC1/Fence) so runs + workspace Verify can launch;
  # fall back to -runner none (headless API only) if Docker is unreachable or the
  # operator forces it (WARDYN_LOCAL_RUNNER=none).
  RUNNER="${WARDYN_LOCAL_RUNNER:-docker}"
  if [[ "${RUNNER}" == "docker" ]] && ! docker version >/dev/null 2>&1; then
    log "NOTE: Docker not reachable — falling back to -runner none (runs can't launch)."
    RUNNER="none"
  fi
  if [[ "${RUNNER}" == "docker" ]]; then
    NET="${WARDYN_INTERNAL_NETWORK:-wardyn-internal}"
    if ! docker network inspect "$NET" >/dev/null 2>&1; then
      docker network create "$NET" >/dev/null 2>&1 || log "WARNING could not create control-plane network $NET"
    fi
    # Ensure the proxy sidecar + agent images exist (build if missing —
    # first run only; the agent image build is slow). oracle is the e2e oracle
    # image, advertised in WARDYN_AGENT_IMAGES below, so build it here too.
    if image_missing wardyn/wardyn-proxy:demo; then
      log "Building the wardyn-proxy sidecar image (first run)…"
      docker compose -f deploy/compose/docker-compose.yaml --profile build-only build proxy-image >/dev/null 2>&1 \
        || log "WARNING proxy image build failed — runs may fail until 'make compose-build'"
    fi
    if image_missing wardyn/agent-claude-code:demo; then
      log "Building the claude-code agent image (first run; slow)…"
      docker build -q -f deploy/images/claude-code/Dockerfile -t wardyn/agent-claude-code:demo . >/dev/null 2>&1 \
        || log "WARNING agent image build failed — runs may fail until 'make agent-images'"
    fi
    if image_missing wardyn/agent-oracle:demo; then
      log "Building the oracle agent image (first run)…"
      docker build -q -f deploy/images/oracle/Dockerfile -t wardyn/agent-oracle:demo . >/dev/null 2>&1 \
        || log "WARNING oracle image build failed — oracle runs fail until 'make agent-images'"
    fi
    # Sandbox containers reach the host control plane via host.docker.internal (the
    # proxy sidecar maps it to the docker host gateway). Agent names → locally-built
    # demo images (else the runner pulls a non-existent ghcr image).
    export WARDYN_CONTROL_PLANE_URL="http://host.docker.internal:${PORT}"
    export WARDYN_PROXY_IMAGE="wardyn/wardyn-proxy:demo"
    export WARDYN_AGENT_IMAGES='{"claude-code":"wardyn/agent-claude-code:demo","codex-cli":"wardyn/agent-codex-cli:demo","oracle":"wardyn/agent-oracle:demo"}'
    export WARDYN_AGENT_ANTHROPIC_MODEL="${WARDYN_AGENT_ANTHROPIC_MODEL:-opus}"
    log "Sandbox runner: docker (CC1/Fence) — runs + Verify can launch"
  else
    log "Sandbox runner: none (headless API only) — set WARDYN_LOCAL_RUNNER=docker + a reachable Docker to enable runs"
  fi

  # Fresh, EMPTY database (no seeding).
  cmd_down >/dev/null 2>&1 || true
  psql_local wardyn -c "CREATE DATABASE ${DB};" >/dev/null 2>&1 || true

  log "Starting wardynd (fresh, empty) on ${BASE_URL}"
  WARDYN_PG_DSN="${DSN}" WARDYN_ADMIN_TOKEN="${TOKEN}" WARDYN_AGE_KEY="${AGE_KEY}" \
    "${BIN_DIR}/wardynd" \
      -runner "${RUNNER}" \
      -listen ":${PORT}" \
      -ui-dir "${REPO_ROOT}/ui/dist" \
      -default-policy "${WARDYN_LOCAL_POLICY:-${REPO_ROOT}/examples/policies/composer-dev.json}" \
      -composer-config "${COMPOSER_CFG}" \
      >"${LOG_FILE}" 2>&1 &
  echo $! > "${PID_FILE}"

  if ! wait_healthy "${BASE_URL}" 60 0.5; then
    tail -20 "${LOG_FILE}"; die "wardynd did not become healthy"
  fi

  # Sandbox → control-plane reachability probe. A governed run's proxy sidecar
  # must reach this host-mode wardynd to deliver scan/verify RESULTS. On Docker
  # Desktop + WSL2 in NAT mode, containers cannot route to a WSL-hosted service,
  # so Verify would run but never report (it now fails cleanly with a networking
  # message rather than hanging). Warn loudly + point at the fix.
  if [[ "${RUNNER}" == "docker" ]]; then
    local cp_ok=""
    docker run --rm curlimages/curl:latest -s -m 5 -o /dev/null \
      "http://host.docker.internal:${PORT}/healthz" >/dev/null 2>&1 && cp_ok=1 || true
    if [[ -z "${cp_ok}" ]]; then
      log "⚠  Sandbox containers CANNOT reach this host-mode control plane (host.docker.internal:${PORT})."
      log "   Runs launch and scan/configure/finalize work, but workspace VERIFY can't report its"
      log "   result (it will fail with a networking message). This is the Docker Desktop + WSL2 NAT"
      log "   isolation. To make Verify work, enable WSL2 MIRRORED networking:"
      log "     1) In Windows, put this in %UserProfile%\\.wslconfig :"
      log "           [wsl2]"
      log "           networkingMode=mirrored"
      log "     2) Run 'wsl --shutdown' in Windows, reopen this WSL shell, then re-run run-local.sh up."
      log "   (Alternatively run the containerized stack: 'make compose-up'.)"
    else
      log "Sandbox → control-plane reachability: OK (Verify can report results)"
    fi
  fi

  log "READY — fresh, empty Wardyn with the AI Run Composer:"
  log "  Open:        ${BASE_URL}"
  log "  Admin token: ${TOKEN}   (paste into the sign-in screen)"
  log "  Composer:    backends = $(curl -fsS -H "Authorization: Bearer ${TOKEN}" "${BASE_URL}/api/v1/composer/backends" | python3 -c 'import sys,json;print([b["name"] for b in json.load(sys.stdin)["backends"]])' 2>/dev/null)"
  log "  Logs:        scripts/run-local.sh logs   (or ${LOG_FILE})"
  log "  Stop:        scripts/run-local.sh down"
}

case "${1:-up}" in
  up)   cmd_up ;;
  down) cmd_down ;;
  logs) cmd_logs ;;
  *) die "usage: $0 {up|down|logs}" ;;
esac
