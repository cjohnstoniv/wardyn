#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for W16-S1-1: scripts/test-concurrent.sh's compose_ns helper
# must scope WARDYN_REGISTRY_PORT=0, mirroring ci-run.sh — otherwise the two
# concurrent stacks make test-e2e-concurrent brings up both collide on the
# devcontainer-build registry sidecar's fixed default port (5010; `up -d
# postgres wardynd` always starts it too, a wardynd dependency) and the
# second job's wardynd never starts. Extracts the REAL compose_ns function
# body from the script (not a copy) so a future edit that drops the var again
# still fails this test.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fn="$(sed -n '/^compose_ns()/,/^}/p' "$ROOT/scripts/test-concurrent.sh")"
[ -n "$fn" ] || { echo "FAIL: could not extract compose_ns() from scripts/test-concurrent.sh" >&2; exit 1; }
eval "$fn"

# Stub `docker` so compose_ns runs nothing real; just dump the env it saw.
docker() {
  printf 'WARDYN_UP_PORT=%s WARDYN_PG_PORT=%s WARDYN_REGISTRY_PORT=%s\n' \
    "${WARDYN_UP_PORT:-unset}" "${WARDYN_PG_PORT:-unset}" "${WARDYN_REGISTRY_PORT:-unset}"
}

COMPOSE_FILE="unused-in-this-test"
out="$(compose_ns test-proj-$$ noop)"

if echo "$out" | grep -q 'WARDYN_REGISTRY_PORT=0'; then
  echo "ok: compose_ns scopes WARDYN_REGISTRY_PORT=0 ($out)"
  echo "--- test-compose-ns-registry-port: PASS ---"
  exit 0
else
  echo "FAIL: compose_ns did not scope WARDYN_REGISTRY_PORT ($out)" >&2
  echo "--- test-compose-ns-registry-port: FAIL ---"
  exit 1
fi
