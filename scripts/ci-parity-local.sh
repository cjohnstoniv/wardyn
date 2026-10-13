#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# ci-parity-local.sh — the two hosted-CI conditions a plain local `go test` misses, on the packages
# that changed: `-race` with no Postgres (the go jobs), then the Postgres-gated tests with the
# nested-acquire guard, superuser asserted, a `localhost` DSN, `-p 1` and go's default package timeout
# (the test-pg shards). The Postgres is a throwaway tmpfs postgres:17, removed on exit.
# "Changed" is against the merge base with $BASE (default origin/main), plus the working tree.
#
# Usage: [BASE=<ref>] scripts/ci-parity-local.sh
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=lib/common.sh
source scripts/lib/common.sh

BASE="${BASE:-origin/main}"
base=$(git merge-base "${BASE}" HEAD) || die "no merge base with ${BASE} (run: git fetch origin)"
mapfile -t pkgs < <( { git diff --name-only --diff-filter=d "${base}"; git ls-files --others --exclude-standard; } |
  grep '\.go$' | xargs -r -n1 dirname | sort -u | sed 's#^#./#' || true)
[ "${#pkgs[@]}" -gt 0 ] || { log "no changed Go packages"; exit 0; }
mapfile -t pkgs < <(go list -e -f '{{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' "${pkgs[@]}")

log "1/2 go test -race ${pkgs[*]} (WARDYN_TEST_PG unset, as the go jobs)"
WARDYN_TEST_PG='' go test -race -count=1 "${pkgs[@]}"

name="ci-parity-pg-$$"
trap 'docker rm -fv "${name}" >/dev/null 2>&1 || true' EXIT
docker run -d --name "${name}" -e POSTGRES_PASSWORD=wardyn -e POSTGRES_DB=wardyn --tmpfs /var/lib/postgresql/data \
  -p 127.0.0.1::5432 postgres:17 -c fsync=off >/dev/null
# The image restarts its server once after initdb: ready is the second "ready to accept connections".
timeout 90 awk '/ready to accept connections/ { if (++n == 2) exit }' < <(docker logs -f "${name}" 2>&1)
port=$(docker port "${name}" 5432/tcp | head -1 | sed 's/.*://')

log "2/2 go test -p 1 ${pkgs[*]} (Postgres on localhost:${port}, pool guard on, default timeout)"
WARDYN_TEST_PG="postgres://postgres:wardyn@localhost:${port}/wardyn?sslmode=disable" \
  WARDYN_TEST_PG_SUPERUSER=1 WARDYN_TEST_POOL_GUARD=1 go test -p 1 -count=1 "${pkgs[@]}"

log "parity checks passed (not a substitute for hosted CI)"
