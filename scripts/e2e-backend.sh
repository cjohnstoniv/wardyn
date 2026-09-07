#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

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
# Env (this file's own reads only — WARDYN_E2E_PG_HOSTPORT and the rest of
# docs/ENV.md's Test/internal-only e2e table are wrapper knobs run-ui-e2e.sh /
# screenshots.sh convert INTO WARDYN_E2E_DSN before this script ever runs;
# F063):
#   WARDYN_E2E_DSN           Postgres DSN     (default: dockerized wardyn-test-pg :55432/wardyn_e2e)
#   WARDYN_E2E_TOKEN         admin bearer token (default: wardyn-e2e-token)
#   WARDYN_E2E_ADDR          listen address   (default: :8088)
#   WARDYN_E2E_UI_ADDR       UI-sandbox gateway listen address (default: :8089) — must differ from WARDYN_E2E_ADDR
#   WARDYN_E2E_PG_CONTAINER  psql container   (default: wardyn-test-pg) — used for SQL state seeding
#   WARDYN_E2E_PG_DBNAME     e2e database name (default: wardyn_e2e)
#   WARDYN_E2E_AGE_KEY       pinned age identity (default: unset, mint a fresh one per `up`)
#   WARDYN_E2E_SKIP_BUILD    1 reuses the built .e2e-bin/wardynd instead of rebuilding it
#   WARDYN_E2E_NO_UI_BUILD   1 reuses the existing ui/dist instead of rebuilding it
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
source "${REPO_ROOT}/scripts/lib/common.sh"

# Provision + exec wardyn-test-pg on the same daemon as up.sh pg (dual-daemon boxes).
wardyn_pick_docker_host

DSN="${WARDYN_E2E_DSN:-postgres://wardyn:wardyn@localhost:55432/wardyn_e2e?sslmode=disable}"
TOKEN="${WARDYN_E2E_TOKEN:-wardyn-e2e-token}"
ADDR="${WARDYN_E2E_ADDR:-:8088}"
# Age identity for wardynd's secret store. MINTED PER `up` (cmd_up, via
# `wardynd -gen-age-key` — the same mint scripts/up.sh + scripts/setup.sh use)
# rather than hard-coded: a committed key is published the moment it is pushed,
# and wardynd fail-closed refuses publicly-known keys (knownPublicAgeKeys in
# cmd/wardynd/main.go). A fresh key per boot is safe here precisely because
# cmd_up also resets the schema, so no ciphertext outlives the key it was
# written under — the boot-key fail-closed path is never hit.
# Override to pin your own (e.g. to keep a hand-seeded DB readable).
AGE_KEY="${WARDYN_E2E_AGE_KEY:-}"
# The UI-sandbox gateway's SECOND listener (docs/UI-SANDBOXES.md). On by
# default here so ui-sandbox.spec.ts drives the run-detail lane's real
# enabled states against a real /healthz instead of a stubbed one; it must
# differ from ADDR or wardynd refuses to boot. The per-screen fanout gives
# each instance its own ports, so override this alongside WARDYN_E2E_ADDR.
UI_ADDR="${WARDYN_E2E_UI_ADDR:-:8089}"
PG_CONTAINER="${WARDYN_E2E_PG_CONTAINER:-wardyn-test-pg}"
PG_DBNAME="${WARDYN_E2E_PG_DBNAME:-wardyn_e2e}"
# Derive the URL port by splitting on the LAST colon, so every documented ADDR
# shape yields a valid URL: ':8088' -> 8088, '0.0.0.0:9000' -> 9000, 'host:80'
# -> 80. (The old ${ADDR#*:} stripped through the FIRST colon and never inserted
# the ':' separator for non-':PORT' shapes, e.g. 'http://localhost9000'.)
BASE_URL="http://localhost:${ADDR##*:}"
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
  # Parsed up front (not just "a few lines later") so BOTH fail-closed messages
  # below can report the port this run is actually configured for — F062: the
  # first die() used to hardcode ":55432" even when WARDYN_E2E_DSN pointed
  # elsewhere, telling an operator on a repointed lane to look at the wrong
  # port entirely.
  # -n ... p, NOT a bare s///: sed echoes its INPUT UNCHANGED when the pattern
  # does not match, so a DSN with no explicit port (postgres://host/db) used to
  # set dsn_port to the entire DSN — which printed a whole DSN where the die()
  # below promises a port, made the `<unknown ...>` fallback unreachable, and
  # tripped the mismatch check below into aborting a perfectly valid run.
  # Suppress-and-print makes a non-match yield the empty string instead.
  dsn_port="$(printf '%s' "${DSN}" | sed -nE 's#^[a-zA-Z]+://[^/]*:([0-9]+)/.*#\1#p')"
  if ! docker exec "${PG_CONTAINER}" pg_isready -U wardyn >/dev/null 2>&1; then
    if [[ "${PG_CONTAINER}" == "wardyn-test-pg" ]]; then
      log "Postgres container '${PG_CONTAINER}' not ready; self-provisioning via scripts/up.sh pg"
      "${REPO_ROOT}/scripts/up.sh" pg || die "scripts/up.sh pg failed to provision ${PG_CONTAINER}"
    else
      die "Postgres container '${PG_CONTAINER}' not ready on :${dsn_port:-<unknown — WARDYN_E2E_DSN did not parse>}. Start it (docker run ... postgres) on that port, or unset WARDYN_E2E_PG_CONTAINER to let 'scripts/up.sh pg' self-provision the default."
    fi
  fi
  # wardynd CONNECTS via DSN's host:port, but every seed/reset call below goes
  # through `docker exec ${PG_CONTAINER}` — a plain container exec, which is
  # blind to DSN and always reaches whatever THAT container serves on ITS OWN
  # published port. A caller who repoints DSN's port (WARDYN_E2E_DSN — the
  # variable this script actually reads; run-ui-e2e.sh's WARDYN_E2E_PG_HOSTPORT
  # and screenshots.sh's equivalent are wrapper-only knobs converted INTO a
  # DSN before this script ever sees them) without repointing PG_CONTAINER to
  # match would otherwise silently serve one database while seeding another.
  # Loud and early beats that silent split: fail closed rather than run a spec
  # suite against data it never actually seeded.
  container_port="$(docker port "${PG_CONTAINER}" 5432/tcp 2>/dev/null | head -1 | sed -E 's#.*:##')"
  if [[ -n "${dsn_port}" && -n "${container_port}" && "${dsn_port}" != "${container_port}" ]]; then
    die "DSN port ${dsn_port} != '${PG_CONTAINER}' published port ${container_port} — wardynd would SERVE ${dsn_port} while every seed/reset below hits ${container_port}. Point both at the same Postgres (set WARDYN_E2E_DSN to a DSN on port ${container_port}, or set WARDYN_E2E_PG_CONTAINER to the container actually listening on ${dsn_port})."
  fi
  # THE one place the e2e database gets created (callers used to each hand-copy
  # this line). up.sh pg only precreates the default wardyn_e2e, so a custom
  # WARDYN_E2E_PG_DBNAME — screenshots.sh's wardyn_shots, the per-screen fanout —
  # would otherwise never exist and wardynd would fail to connect. Idempotent:
  # "already exists" is the expected no-op on every run after the first.
  docker exec "${PG_CONTAINER}" psql -U wardyn -d wardyn -c "CREATE DATABASE \"${PG_DBNAME}\"" >/dev/null 2>&1 || true
  cmd_build
  # Mint the per-boot age identity (see AGE_KEY above). Must follow cmd_build:
  # it runs the binary we just built.
  if [[ -z "${AGE_KEY}" ]]; then
    AGE_KEY="$("${BIN_DIR}/wardynd" -gen-age-key | grep -E '^AGE-SECRET-KEY-' | head -1 || true)"
    [[ -n "${AGE_KEY}" ]] || die "wardynd -gen-age-key produced no key"
  fi
  cmd_down_quiet
  # Fresh schema each up so seeded fixtures are deterministic (wardynd re-migrates
  # and re-stores its keys under this boot's AGE_KEY).
  log "Resetting ${PG_DBNAME} schema for deterministic fixtures"
  psql_e2e -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null 2>&1 || true
  log "Starting wardynd (runner=none) on ${ADDR} → ${BASE_URL}, DB ${PG_DBNAME}"
  # WARDYN_RUNNER_TARGET: -runner none resolves the target "none", which no user
  # drive backend can name, so without it the API refuses EVERY drive with a 400
  # and the drives screen has nothing to exercise. Registration only — this
  # daemon still dispatches nothing.
  WARDYN_PG_DSN="${DSN}" WARDYN_ADMIN_TOKEN="${TOKEN}" WARDYN_AGE_KEY="${AGE_KEY}" \
    WARDYN_RUNNER_TARGET=docker \
    "${BIN_DIR}/wardynd" \
      -runner none \
      -listen "${ADDR}" \
      -ui-sandbox-listen "${UI_ADDR}" \
      -ui-sandbox-advertise "http://localhost:${UI_ADDR##*:}" \
      -ui-dir "${REPO_ROOT}/ui/dist" \
      -default-policy "${REPO_ROOT}/examples/policies/demo.json" \
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
  # Both listeners: the UI-sandbox gateway's port has the same lifecycle as the
  # console's, so leaving it held would make the NEXT `up` fail to bind exactly
  # the way the console port used to (see the wait loop below).
  local ui_port="${UI_ADDR##*:}"
  if command -v fuser >/dev/null 2>&1; then
    fuser -k "${port}/tcp" >/dev/null 2>&1 || true
    fuser -k "${ui_port}/tcp" >/dev/null 2>&1 || true
  fi

  # WAIT for the port to actually be free, rather than guessing.
  #
  # This was `sleep 0.3`, and run-ui-e2e.sh brings a fresh backend up per spec
  # file on the SAME port, so the next `up` raced a socket the kill had not
  # finished releasing. It flaked two ways, both seen: the new wardynd fails to
  # bind (run-ui-e2e reports "backend up failed for <spec>" and scores the whole
  # file as failed), or it binds while the dying process still answers, and the
  # spec loads against a backend that stops mid-run.
  local waited=0
  while [[ ${waited} -lt 50 ]]; do
    if command -v fuser >/dev/null 2>&1; then
      fuser "${port}/tcp" >/dev/null 2>&1 || return 0
    else
      sleep 0.3
      return 0
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  log "port ${port} still busy after 5s — continuing; the next bind may fail"
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
  # The fixture install is ONBOARDED. The suite's specs exercise the console,
  # not the first-run funnel (which has its own specs and mocks) — and this
  # backend deliberately runs with NO runner, whose permanent runner:fail would
  # otherwise hold every route behind the setup gate. This replaces the
  # localStorage seam three specs used to fake per-browser; the install-side
  # mark is the honest version of the same statement.
  api POST /api/v1/setup/onboarding-complete '' >/dev/null 2>&1 || true
  # A handful of runs (the none runner leaves them PENDING; we re-state below).
  local agents=(claude-code codex-cli claude-code claude-code codex-cli claude-code claude-code claude-code claude-code)
  # Fixtures 0 and 1 deliberately SHARE a title so the Runs board actually has a
  # group to render — a title held by only one run is not a group (runs.tsx's
  # titleGroups), so without a repeat the grouping path would go unexercised.
  # The rest stay untitled on purpose: that is the legacy/CLI/system-run shape,
  # and it must keep rendering by task (runHeadline).
  local titles=("e2e group" "e2e group" "" "" "" "" "" "" "")
  for i in "${!agents[@]}"; do
    api POST /api/v1/runs "{\"agent\":\"${agents[$i]}\",\"repo\":\"acme/widgets\",\"title\":\"${titles[$i]}\",\"task\":\"e2e fixture ${i}\"}" >/dev/null || true
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
  # sources[]-shaped (the composition model, migration 0029) — NOT the legacy
  # kind/source scalar shape decodeWorkspaceRequest still folds for old callers;
  # this is what the API itself now returns for every workspace, and what the
  # UI's workspace-wizard/picker render against.
  api POST /api/v1/workspaces '{"name":"payments","sources":[{"type":"local_dir","path":"/home/me/projects/payments"}]}' >/dev/null 2>&1 || true
  # Run detail's UI-apps lane (docs/UI-SANDBOXES.md) reads ui_apps off the
  # run.policy.effective envelope DISPATCH records (effectiveUIApps) — and the
  # `none` runner never dispatches, so the envelope goes in here exactly as
  # dispatch would write it. Attached to the RUNNING fixture rather than a new
  # run on purpose: the lane is owner-and-RUNNING-only, and the seeded run
  # count is load-bearing for other specs (runs, recording).
  psql_e2e >/dev/null 2>&1 <<'SQL' || true
INSERT INTO audit_events (id, time, run_id, actor_type, actor, action, target, outcome, data)
SELECT gen_random_uuid(), now(), id, 'system', 'wardynd', 'run.policy.effective', id::text, 'success',
       '{"allowed_domains":[],"first_use_approval":"always_deny","min_confinement_class":"CC1","ui_apps":[{"name":"vscode","port":8080,"path":"/"}]}'::jsonb
FROM agent_runs WHERE task = 'e2e fixture 2';
SQL
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
