#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# regenerates docs/img UI screenshots; run after visible UI changes and commit the diff.
#
# Same shape as run-ui-e2e.sh; dedicated :8098/:8099/wardyn_shots so it can't collide.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# One daemon everywhere: `up.sh pg` and the spec's `docker exec` must hit the
# daemon e2e-backend.sh provisions on.
WARDYN_LOG_TAG="[screenshots]"
. "${REPO_ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

export WARDYN_E2E_ADDR="127.0.0.1:8098"
export WARDYN_E2E_UI_ADDR="127.0.0.1:8099"
export WARDYN_E2E_PG_DBNAME="wardyn_shots"
# Overridable PG host:port (same convention as run-ui-e2e.sh); the DB name
# stays pinned to wardyn_shots — the spec self-gates on it.
PG_HOSTPORT="${WARDYN_E2E_PG_HOSTPORT:-localhost:55432}"
export WARDYN_E2E_DSN="postgres://wardyn:wardyn@${PG_HOSTPORT}/wardyn_shots?sslmode=disable"
export WARDYN_E2E_PG_CONTAINER="${WARDYN_E2E_PG_CONTAINER:-wardyn-test-pg}"
export WARDYN_E2E_BASE_URL="http://localhost:8098"

# log() uses WARDYN_LOG_TAG="[screenshots]" set before sourcing common.sh above.

# The spec self-skips without this (so a bare `pnpm e2e` can never clobber the
# tracked docs/img PNGs from the wrong backend).
export WARDYN_SCREENSHOTS=1

# Absolute path: the capture step cd's into ui/, so a relative trap would resolve
# to nothing and silently leave :8098 up.
cleanup() { "${REPO_ROOT}/scripts/e2e-backend.sh" down >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Ensure the dockerized test Postgres EXISTS on a fresh box (idempotent).
# e2e-backend.sh's `up` then creates the dedicated wardyn_shots database itself.
./scripts/up.sh pg || { echo "test postgres provisioning failed"; exit 1; }
log "Building backend + UI bundle"
./scripts/e2e-backend.sh build || { echo "build failed"; exit 1; }
export WARDYN_E2E_SKIP_BUILD=1
./scripts/e2e-backend.sh up || { echo "backend up failed"; exit 1; }
WARDYN_E2E_TOKEN="$(./scripts/e2e-backend.sh token)" || exit 1
export WARDYN_E2E_TOKEN

log "Capturing docs/img screenshots"
cd ui && pnpm exec playwright test --project=screenshots --workers=1
