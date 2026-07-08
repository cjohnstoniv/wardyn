#!/usr/bin/env bash
# Seeded test backend for the Playwright UI e2e suite (task #8).
#
# Boots a fast, hermetic control plane — real wardynd + real Postgres + the `none`
# runner (no agent containers) — serving the BUILT embedded UI (ui/dist), seeded
# with deterministic fixtures via the public API + a fixed admin token. This is
# the default Playwright gate target (NOT the full docker-compose stack), so PR
# runs are fast and reproducible. The nightly job runs Playwright against the
# real compose stack instead (see .github/workflows/nightly.yml).
#
# Usage:
#   scripts/e2e-backend.sh up       # build, migrate, seed, serve (foreground-ish: writes a PID file)
#   scripts/e2e-backend.sh down     # stop wardynd, drop the e2e DB contents
#   scripts/e2e-backend.sh seed     # (re)seed fixtures into a running backend
#   scripts/e2e-backend.sh wait     # block until /healthz is ready
#
# Env:
#   WARDYN_E2E_DSN    Postgres DSN          (default: dockerized wardyn-test-pg :55432/wardyn_e2e)
#   WARDYN_E2E_TOKEN  admin bearer token    (default: wardyn-e2e-token)
#   WARDYN_E2E_ADDR   listen address        (default: :8080)
#   WARDYN_E2E_PG_CONTAINER  psql container (default: wardyn-test-pg) — used for SQL state seeding
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
source "${REPO_ROOT}/scripts/lib/common.sh"

DSN="${WARDYN_E2E_DSN:-postgres://wardyn:wardyn@localhost:55432/wardyn_e2e?sslmode=disable}"
TOKEN="${WARDYN_E2E_TOKEN:-wardyn-e2e-token}"
ADDR="${WARDYN_E2E_ADDR:-:8088}"
# A FIXED age identity so wardynd's signing/session keys persist across reboots.
# This protects only ephemeral test-backend secrets (it never guards real data),
# so it is safe to hard-code for the hermetic e2e gate. Without it, wardynd
# correctly fails closed on the second boot (it cannot decrypt keys stored under
# the previous boot's ephemeral key — the boot-key fail-closed HIGH fix).
AGE_KEY="${WARDYN_E2E_AGE_KEY:-AGE-SECRET-KEY-1CMRQ5GEN2G4NKWXQQ4DKK7GSMJDZXXW69W9QN3ALX8Y49CF6RLYS7Y6KHF}"
PG_CONTAINER="${WARDYN_E2E_PG_CONTAINER:-wardyn-test-pg}"
PG_DBNAME="${WARDYN_E2E_PG_DBNAME:-wardyn_e2e}"
BASE_URL="http://localhost${ADDR#*:}"
[[ "${ADDR}" == :* ]] && BASE_URL="http://localhost${ADDR}"
BIN_DIR="${REPO_ROOT}/.e2e-bin"
# PID/log keyed by listen port so multiple isolated instances (the per-screen e2e
# fanout: own port + own DB each) never kill or clobber each other.
_PORT="${ADDR#*:}"
PID_FILE="${BIN_DIR}/wardynd-${_PORT}.pid"
LOG_FILE="${BIN_DIR}/wardynd-${_PORT}.log"

log()  { printf '\033[1;34m[e2e]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[e2e:err]\033[0m %s\n' "$*" >&2; exit 1; }

# psql helper against the seeding container.
psql_e2e() { docker exec -i "${PG_CONTAINER}" psql -U wardyn -d "${PG_DBNAME}" "$@"; }

api() {  # api METHOD PATH [JSON_BODY]
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "${body}" ]]; then
    curl -fsS -X "${method}" -H "Authorization: Bearer ${TOKEN}" \
      -H 'Content-Type: application/json' -d "${body}" "${BASE_URL}${path}"
  else
    curl -fsS -X "${method}" -H "Authorization: Bearer ${TOKEN}" "${BASE_URL}${path}"
  fi
}

cmd_build() {
  # Skip when the binaries are already built (the per-screen fanout pre-builds
  # once, then each isolated instance reuses .e2e-bin to avoid racing on the
  # shared output path).
  if [[ "${WARDYN_E2E_SKIP_BUILD:-0}" == "1" && -x "${BIN_DIR}/wardynd" ]]; then
    log "Reusing pre-built ${BIN_DIR}/wardynd (WARDYN_E2E_SKIP_BUILD=1)"
    return 0
  fi
  log "Building wardynd (none runner; no -tags docker needed) + wardyn CLI"
  go build -o "${BIN_DIR}/wardynd" ./cmd/wardynd
  go build -o "${BIN_DIR}/wardyn"  ./cmd/wardyn
  # Always rebuild the UI bundle on a non-skip build so the served app reflects
  # the current ui/src (reusing a stale ui/dist silently serves old UI — a real
  # footgun when iterating on the composer). Set WARDYN_E2E_NO_UI_BUILD=1 to reuse
  # an existing dist deliberately.
  if [[ "${WARDYN_E2E_NO_UI_BUILD:-0}" == "1" && -d "${REPO_ROOT}/ui/dist" ]]; then
    log "Reusing existing ui/dist (WARDYN_E2E_NO_UI_BUILD=1)"
  else
    log "Building UI bundle (ui/dist)"
    ( cd ui && pnpm install --frozen-lockfile && pnpm build )
  fi
}

cmd_wait() {
  log "Waiting for ${BASE_URL}/healthz"
  if ! wait_healthy "${BASE_URL}" 60 0.5; then
    cat "${LOG_FILE}" 2>/dev/null | tail -30
    die "wardynd did not become healthy"
  fi
  log "wardynd healthy"
}

cmd_up() {
  mkdir -p "${BIN_DIR}"
  if ! docker exec "${PG_CONTAINER}" pg_isready -U wardyn >/dev/null 2>&1; then
    if [[ "${PG_CONTAINER}" == "wardyn-test-pg" ]]; then
      log "Postgres container '${PG_CONTAINER}' not ready; self-provisioning via scripts/up.sh pg"
      "${REPO_ROOT}/scripts/up.sh" pg || die "scripts/up.sh pg failed to provision ${PG_CONTAINER}"
    else
      die "Postgres container '${PG_CONTAINER}' not ready on :55432. Start it (docker run ... postgres), or unset WARDYN_E2E_PG_CONTAINER to let 'scripts/up.sh pg' self-provision the default."
    fi
  fi
  cmd_build
  cmd_down_quiet
  # Fresh schema each up so seeded fixtures are deterministic (wardynd re-migrates
  # and re-stores its keys under the pinned AGE_KEY on boot).
  log "Resetting ${PG_DBNAME} schema for deterministic fixtures"
  psql_e2e -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null 2>&1 || true
  log "Starting wardynd (runner=none) on ${ADDR} → ${BASE_URL}, DB ${PG_DBNAME}"
  # AI Run Composer: a deterministic 'fake' backend (no keys/network) so the UI
  # describe-mode + compose endpoints are exercisable hermetically.
  local composer_cfg='{"default":"fake-claude","backends":[{"name":"fake-claude","wire":"fake","model":"claude-opus-4-8"},{"name":"fake-gpt","wire":"fake","model":"gpt-5.5"},{"name":"fake-risky","wire":"fake","transport":"high","model":"claude-opus-4-8"},{"name":"fake-interview","wire":"fake","transport":"interview","model":"claude-opus-4-8"}]}'
  WARDYN_PG_DSN="${DSN}" WARDYN_ADMIN_TOKEN="${TOKEN}" WARDYN_AGE_KEY="${AGE_KEY}" \
    "${BIN_DIR}/wardynd" \
      -runner none \
      -listen "${ADDR}" \
      -ui-dir "${REPO_ROOT}/ui/dist" \
      -default-policy "${REPO_ROOT}/examples/policies/demo.json" \
      -composer-config "${composer_cfg}" \
      >"${LOG_FILE}" 2>&1 &
  echo $! > "${PID_FILE}"
  cmd_wait
  cmd_seed
  log "Seeded backend ready:"
  log "  URL:   ${BASE_URL}"
  log "  token: ${TOKEN}"
  log "  logs:  ${LOG_FILE}"
}

cmd_down_quiet() {
  if [[ -f "${PID_FILE}" ]]; then
    local pid; pid="$(cat "${PID_FILE}")"
    kill "${pid}" >/dev/null 2>&1 || true
    rm -f "${PID_FILE}"
  fi
  # Free ONLY this instance's listen port (do NOT broad-kill every .e2e-bin/wardynd
  # — that would tear down sibling instances during the per-screen e2e fanout).
  local port="${ADDR#*:}"
  if command -v fuser >/dev/null 2>&1; then fuser -k "${port}/tcp" >/dev/null 2>&1 || true; fi
  sleep 0.3
}

cmd_down() {
  log "Stopping wardynd + clearing ${PG_DBNAME}"
  cmd_down_quiet
  psql_e2e -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null 2>&1 || true
}

# Deterministic fixtures. Created via the API (correct shapes), then diversified
# into every RunState / ApprovalState via SQL so the UI renders the full matrix —
# crucially COMPLETED, the state that previously crashed the console.
cmd_seed() {
  log "Seeding deterministic fixtures via API + SQL"
  # A handful of runs (the none runner leaves them PENDING; we re-state below).
  local agents=(claude-code codex-cli claude-code claude-code codex-cli claude-code claude-code claude-code claude-code)
  for i in "${!agents[@]}"; do
    api POST /api/v1/runs "{\"agent\":\"${agents[$i]}\",\"repo\":\"acme/widgets\",\"task\":\"e2e fixture ${i}\"}" >/dev/null || true
  done
  # Diversify states deterministically by created order so specs can target them.
  psql_e2e >/dev/null <<'SQL' || true
WITH ordered AS (
  SELECT id, row_number() OVER (ORDER BY created_at) AS rn FROM agent_runs
)
UPDATE agent_runs a SET state = v.state
FROM ordered o
JOIN (VALUES
  (1,'PENDING'),(2,'STARTING'),(3,'RUNNING'),(4,'WAITING_FOR_CONFIRMATION'),
  (5,'COMPLETED'),(6,'STOPPED'),(7,'FAILED'),(8,'KILLED'),(9,'ARCHIVED')
) AS v(rn,state) ON v.rn = o.rn
WHERE a.id = o.id;
SQL
  # A secret (metadata only; value never readable via API). Name must be
  # lowercase alphanumerics + . _ - ; body is {"value":"..."}.
  api PUT /api/v1/secrets/e2e-test-secret '{"value":"e2e-secret-value"}' >/dev/null 2>&1 || true
  # One onboarded workspace so the manual wizard's Basics step has something to
  # attach — the mount gate accepts onboarded sources ONLY, and it checks
  # membership (any status), so pending_scan is fine and the dir need not exist.
  api POST /api/v1/workspaces '{"name":"payments","kind":"local_dir","source":"/home/me/projects/payments"}' >/dev/null 2>&1 || true
  log "Seed complete: $(psql_e2e -tAc 'SELECT count(*) FROM agent_runs') runs"
}

case "${1:-up}" in
  up)    cmd_up ;;
  down)  cmd_down ;;
  seed)  cmd_seed ;;
  wait)  cmd_wait ;;
  build) cmd_build ;;
  *) die "usage: $0 {up|down|seed|wait|build}" ;;
esac
