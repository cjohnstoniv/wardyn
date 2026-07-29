#!/usr/bin/env bash
# regenerates docs/img UI screenshots; run after visible UI changes and commit the diff.
#
# Same shape as run-ui-e2e.sh; dedicated :8098/wardyn_shots so it can't collide.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# One daemon everywhere: CREATE DATABASE and the spec's `docker exec` must hit
# the daemon e2e-backend.sh provisions on.
. "${REPO_ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

export WARDYN_E2E_ADDR=":8098"
export WARDYN_E2E_PG_DBNAME="wardyn_shots"
export WARDYN_E2E_DSN="postgres://wardyn:wardyn@localhost:55432/wardyn_shots?sslmode=disable"
export WARDYN_E2E_PG_CONTAINER="${WARDYN_E2E_PG_CONTAINER:-wardyn-test-pg}"
export WARDYN_E2E_BASE_URL="http://localhost:8098"

log() { printf '\033[1;34m[screenshots]\033[0m %s\n' "$*"; }

# The spec self-skips without this (so a bare `pnpm e2e` can never clobber the
# tracked docs/img PNGs from the wrong backend).
export WARDYN_SCREENSHOTS=1

# Absolute path: the capture step cd's into ui/, so a relative trap would resolve
# to nothing and silently leave :8098 up.
cleanup() { "${REPO_ROOT}/scripts/e2e-backend.sh" down >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Ensure the dockerized test Postgres EXISTS before creating our dedicated DB —
# on a fresh box the exec below would silently no-op and wardynd would then fail
# to connect to wardyn_shots (up.sh pg is idempotent).
./scripts/up.sh pg || { echo "test postgres provisioning failed"; exit 1; }
docker exec "${WARDYN_E2E_PG_CONTAINER}" psql -U wardyn -d wardyn -c "CREATE DATABASE wardyn_shots" >/dev/null 2>&1 || true
log "Building backend + UI bundle"
./scripts/e2e-backend.sh build || { echo "build failed"; exit 1; }
export WARDYN_E2E_SKIP_BUILD=1
./scripts/e2e-backend.sh up || { echo "backend up failed"; exit 1; }

log "Capturing docs/img screenshots"
cd ui && pnpm exec playwright test --project=screenshots --workers=1
