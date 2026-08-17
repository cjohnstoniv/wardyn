#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Canonical Playwright UI e2e runner.
#
# Each spec file runs against a FRESHLY SEEDED backend (scripts/e2e-backend.sh up
# resets the schema and re-seeds deterministic fixtures), serially (--workers=1).
# This reproduces exactly the isolation each spec was authored and verified under,
# so mutating specs (kill/approve/create/delete) never contaminate one another —
# the alternative (all specs against one shared backend in parallel) is
# non-deterministic by construction.
#
# Prereqs: the dockerized Postgres "wardyn-test-pg" on :55432 and a built ui/dist +
# .e2e-bin/wardynd (this script builds them once unless WARDYN_E2E_SKIP_BUILD=1).
#
# Usage:  scripts/run-ui-e2e.sh                 # all specs
#         scripts/run-ui-e2e.sh runs secrets    # only runs.spec.ts + secrets.spec.ts
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# One daemon everywhere (same rule as setup/up/e2e-backend): export the picked
# DOCKER_HOST here so the Playwright child processes (approvals.spec.ts shells
# out to `docker exec wardyn-test-pg`) hit the daemon e2e-backend.sh provisions
# on — not the default one.
. "${REPO_ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

PORT="${WARDYN_E2E_ADDR:-:8088}"; PORT="${PORT#*:}"
DB="${WARDYN_E2E_PG_DBNAME:-wardyn_e2e}"
# Overridable PG host:port (the default may be held by a foreign container on a
# shared box); the database name stays coupled to WARDYN_E2E_PG_DBNAME. The
# seed/reset path (e2e-backend.sh) still goes through `docker exec
# WARDYN_E2E_PG_CONTAINER`, blind to this port — pairing a non-default
# WARDYN_E2E_PG_HOSTPORT with a container that doesn't actually publish it
# would silently serve one database while seeding another, so e2e-backend.sh's
# cmd_up fails loudly on that mismatch before touching anything.
PG_HOSTPORT="${WARDYN_E2E_PG_HOSTPORT:-localhost:55432}"
export WARDYN_E2E_ADDR=":${PORT}"
export WARDYN_E2E_DSN="postgres://wardyn:wardyn@${PG_HOSTPORT}/${DB}?sslmode=disable"
export WARDYN_E2E_PG_DBNAME="${DB}"
export WARDYN_E2E_PG_CONTAINER="${WARDYN_E2E_PG_CONTAINER:-wardyn-test-pg}"
export WARDYN_E2E_BASE_URL="http://localhost:${PORT}"

log() { printf '\033[1;34m[e2e-ui]\033[0m %s\n' "$*"; }

# Build the backend + UI once; subsequent per-spec `up` calls reuse them (each
# `up` also creates ${DB} if it does not exist — see e2e-backend.sh cmd_up).
log "Building backend + UI bundle once"
./scripts/e2e-backend.sh build || { echo "build failed"; exit 1; }
export WARDYN_E2E_SKIP_BUILD=1

# Spec selection: args map to e2e/<arg>.spec.ts; default = all *.spec.ts.
specs=()
if [[ $# -gt 0 ]]; then
  for a in "$@"; do specs+=("ui/e2e/${a}.spec.ts"); done
else
  for f in ui/e2e/*.spec.ts; do specs+=("$f"); done
fi

pass=0; fail=0; failed_specs=()
for spec in "${specs[@]}"; do
  base="$(basename "${spec}")"
  # Retry once, and SHOW the failure. This used to be a single attempt with all
  # output sent to /dev/null, so a transient port race looked identical to a
  # genuine spec failure — the run reported "<spec> failed" with nothing to read.
  if ! ./scripts/e2e-backend.sh up >/tmp/wardyn-e2e-up.$$ 2>&1; then
    log "backend up failed for ${base} — retrying once"
    tail -20 /tmp/wardyn-e2e-up.$$ >&2 || true
    ./scripts/e2e-backend.sh down >/dev/null 2>&1 || true
    if ! ./scripts/e2e-backend.sh up >/tmp/wardyn-e2e-up.$$ 2>&1; then
      log "backend up failed for ${base} (twice) — this is the backend, not the spec"
      tail -30 /tmp/wardyn-e2e-up.$$ >&2 || true
      rm -f /tmp/wardyn-e2e-up.$$
      fail=$((fail+1)); failed_specs+=("${base}"); continue
    fi
  fi
  rm -f /tmp/wardyn-e2e-up.$$
  log "running ${base} against a fresh backend"
  if ( cd ui && pnpm exec playwright test "e2e/${base}" --workers=1 --reporter=line ); then
    pass=$((pass+1))
  else
    fail=$((fail+1)); failed_specs+=("${base}")
  fi
done

./scripts/e2e-backend.sh down >/dev/null 2>&1 || true
echo
log "UI e2e summary: ${pass} spec file(s) passed, ${fail} failed"
if [[ ${fail} -gt 0 ]]; then
  log "failed: ${failed_specs[*]}"
  exit 1
fi
