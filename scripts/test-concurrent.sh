#!/usr/bin/env bash
# Two-job shared-host concurrency acceptance test.
#
# Proves the compose control plane is concurrency-safe on a shared, multi-job
# build host: two ci-run-style stacks, brought up CONCURRENTLY under one user with
# distinct WARDYN_NS/COMPOSE_PROJECT_NAME, must
#   1. both come up healthy with no name/network/volume/port collision,
#   2. stay isolated — a container on job A's network cannot reach job B's wardynd,
#   3. survive the OTHER job's `down --volumes` (project-scoped teardown), and
#   4. tear down independently.
#
# This is the proof that cannot be generated from static source — it needs a live
# daemon. Requires wardyn/wardynd:local (build: docker compose -f
# deploy/compose/docker-compose.yaml build wardynd) and a reachable docker daemon.
#
# Usage: scripts/test-concurrent.sh      (exit 0 = PASS, non-zero = FAIL)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
source "${REPO_ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

COMPOSE_FILE="${REPO_ROOT}/deploy/compose/docker-compose.yaml"
A="wardyn-cc-a-$$"
B="wardyn-cc-b-$$"
FAILED=0
note() { printf '  %s %s\n' "$1" "$2"; }
pass() { note "[pass]" "$1"; }
fail() { note "[FAIL]" "$1"; FAILED=1; }

# compose_ns PROJECT -- run compose for a job with its own project + namespace +
# ephemeral host ports (so the two jobs never fight over 8080/5432).
compose_ns() {
  local proj="$1"; shift
  COMPOSE_PROJECT_NAME="${proj}" WARDYN_NS="${proj}" WARDYN_UP_PORT=0 WARDYN_PG_PORT=0 \
    docker compose -p "${proj}" -f "${COMPOSE_FILE}" "$@"
}

teardown() {
  compose_ns "${A}" down --volumes >/dev/null 2>&1 || true
  compose_ns "${B}" down --volumes >/dev/null 2>&1 || true
}
trap teardown EXIT

docker image inspect wardyn/wardynd:local >/dev/null 2>&1 \
  || die "wardyn/wardynd:local not built — run: docker compose -f ${COMPOSE_FILE} build wardynd"

# Clean any stragglers from a prior aborted run.
teardown

log "Bringing up two stacks concurrently: ${A} and ${B}"
compose_ns "${A}" up -d postgres wardynd >/dev/null 2>&1 &
pid_a=$!
compose_ns "${B}" up -d postgres wardynd >/dev/null 2>&1 &
pid_b=$!
wait "${pid_a}"; rc_a=$?
wait "${pid_b}"; rc_b=$?
[ "${rc_a}" = 0 ] && pass "stack ${A} came up" || fail "stack ${A} up failed (rc=${rc_a})"
[ "${rc_b}" = 0 ] && pass "stack ${B} came up" || fail "stack ${B} up failed (rc=${rc_b})"

# Wait for both wardynd containers to report healthy (container health, no host port).
wait_health() {
  local proj="$1" tries=0 cid
  cid="$(compose_ns "${proj}" ps -q wardynd 2>/dev/null || true)"
  until [ -n "${cid}" ] && [ "$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${cid}" 2>/dev/null)" = "healthy" ]; do
    tries=$((tries + 1)); [ "${tries}" -gt 45 ] && return 1
    sleep 2; cid="$(compose_ns "${proj}" ps -q wardynd 2>/dev/null || true)"
  done
  return 0
}
wait_health "${A}" && pass "wardynd ${A} healthy" || fail "wardynd ${A} not healthy"
wait_health "${B}" && pass "wardynd ${B} healthy" || fail "wardynd ${B} not healthy"

# Distinct, collision-free named objects.
docker container inspect "${A}-api" >/dev/null 2>&1 && docker container inspect "${B}-api" >/dev/null 2>&1 \
  && pass "distinct container names (${A}-api, ${B}-api both exist)" \
  || fail "expected distinct per-project container names"
docker network inspect "${A}-internal" >/dev/null 2>&1 && docker network inspect "${B}-internal" >/dev/null 2>&1 \
  && pass "distinct control-plane networks (${A}-internal, ${B}-internal)" \
  || fail "expected distinct per-project networks"

# ISOLATION: a container on job A's network must NOT reach job B's wardynd. Job B's
# wardynd is only on ${B}-internal; A's network has no route to it, and B's
# service DNS name resolves only within B's network.
if docker run --rm --network "${A}-internal" curlimages/curl:latest \
     -s -m 5 -o /dev/null "http://wardynd:8080/healthz" >/dev/null 2>&1; then
  pass "job A's network reaches its OWN wardynd (baseline sanity)"
else
  fail "job A's network could not reach its own wardynd (baseline broke)"
fi
# From A's network, B's api container (by name) must be unreachable/unresolvable.
if docker run --rm --network "${A}-internal" curlimages/curl:latest \
     -s -m 4 -o /dev/null "http://${B}-api:8080/healthz" >/dev/null 2>&1; then
  fail "ISOLATION BREACH: job A's network reached job B's wardynd (${B}-api)"
else
  pass "isolation: job A's network cannot reach job B's wardynd"
fi

# CROSS-JOB TEARDOWN SAFETY: down --volumes on A must NOT touch B.
log "Tearing down ${A} (project-scoped) — ${B} must survive"
compose_ns "${A}" down --volumes >/dev/null 2>&1 || true
if docker container inspect "${A}-api" >/dev/null 2>&1; then
  fail "job A's container still present after its own teardown"
else
  pass "job A torn down"
fi
if docker container inspect "${B}-api" >/dev/null 2>&1 \
   && [ "$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${B}-api" 2>/dev/null)" = "healthy" ]; then
  pass "job B SURVIVED job A's down --volumes (still healthy)"
else
  fail "REGRESSION: job A's down --volumes tore down / unhealthied job B"
fi
docker network inspect "${B}-internal" >/dev/null 2>&1 \
  && pass "job B's network survived A's teardown" \
  || fail "job B's network was removed by A's teardown"

if [ "${FAILED}" = 0 ]; then
  log "CONCURRENCY TEST: PASS (two jobs coexisted, stayed isolated, and tore down independently)"
else
  printf '\033[1;31m[error]\033[0m %s\n' "CONCURRENCY TEST: FAIL (see [FAIL] lines above)" >&2
fi
exit "${FAILED}"
