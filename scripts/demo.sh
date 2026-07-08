#!/usr/bin/env bash
# Wardyn compose demo: build the stack, bring it up, wait until healthy, create
# a governed run against a public fixture repo, and print its audit trail.
#
# This exercises the REAL control plane end to end:
#   - wardynd built with the docker runner (-tags docker)
#   - Postgres (system of record) + Dex (human SSO IdP)
#   - per-run identity mint, policy resolution, audit append
#
# The run is created via the admin-token CLI path (headless, deterministic).
# Human SSO via Dex is a UI bonus: open http://localhost:8080 and log in as
# demo@wardyn.local / password (see README for the /etc/hosts note).
#
# Usage:  scripts/demo.sh            # build + up + demo run + audit
#         scripts/demo.sh --no-build # skip image build (reuse existing)
#         scripts/demo.sh down       # tear the stack down (keeps volumes)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/deploy/compose/docker-compose.yaml"
COMPOSE=(docker compose -f "${COMPOSE_FILE}")

ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"
DEMO_AGENT="${WARDYN_DEMO_AGENT:-claude-code}"
DEMO_REPO="${WARDYN_DEMO_REPO:-octocat/Hello-World}"
DEMO_TASK="${WARDYN_DEMO_TASK:-wardyn compose smoke demo}"

source "${REPO_ROOT}/scripts/lib/common.sh"

# wardyn runs the shipped CLI inside the wardynd container with the admin token.
wardyn() {
  "${COMPOSE[@]}" exec -T \
    -e WARDYN_URL="http://localhost:8080" \
    -e WARDYN_ADMIN_TOKEN="${ADMIN_TOKEN}" \
    wardynd /usr/local/bin/wardyn "$@"
}

cmd_down() {
  log "Tearing down the compose stack (volumes preserved)"
  "${COMPOSE[@]}" down
  exit 0
}

cmd_up() {
  local do_build=1
  [[ "${1:-}" == "--no-build" ]] && do_build=0

  command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
  docker compose version >/dev/null 2>&1 || die "docker compose v2 required"

  # The docker driver launches this OCI image for the run; without it the driver
  # fails closed on the missing image and the demo run lands FAILED.
  local agent_image="wardyn/agent-${DEMO_AGENT}:demo"
  local agent_dockerfile="${REPO_ROOT}/deploy/images/${DEMO_AGENT}/Dockerfile"

  if [[ "${do_build}" -eq 1 ]]; then
    log "Building wardynd + wardyn-proxy images (this also builds with -tags docker)"
    # Build wardynd (default profile) AND the proxy image (build-only profile).
    "${COMPOSE[@]}" build
    "${COMPOSE[@]}" --profile build-only build proxy-image
    # Build the agent image the run actually launches (build context: repo root).
    if [[ -f "${agent_dockerfile}" ]]; then
      log "Building agent image ${agent_image}"
      docker build -f "${agent_dockerfile}" -t "${agent_image}" "${REPO_ROOT}"
    else
      warn "no Dockerfile for agent '${DEMO_AGENT}' at ${agent_dockerfile}; skipping agent build"
    fi
  fi

  log "Starting postgres + dex + wardynd"
  "${COMPOSE[@]}" up -d postgres dex wardynd

  log "Waiting for wardynd to become healthy"
  local tries=0
  until [[ "$("${COMPOSE[@]}" ps -q wardynd | xargs -r docker inspect -f '{{.State.Health.Status}}' 2>/dev/null || echo starting)" == "healthy" ]]; do
    tries=$((tries + 1))
    [[ "${tries}" -gt 60 ]] && { "${COMPOSE[@]}" logs --tail 50 wardynd; die "wardynd did not become healthy"; }
    sleep 2
  done
  log "wardynd is healthy"

  log "Control plane health:"
  wardyn runs list >/dev/null 2>&1 || true
  curl -fsS http://localhost:8080/healthz | sed 's/^/    /' || warn "healthz probe failed from host"
  echo

  # Guard the --no-build / pre-built path: a missing agent image lands the run FAILED.
  if ! docker image inspect "${agent_image}" >/dev/null 2>&1; then
    warn "agent image ${agent_image} is missing — the run will land FAILED."
    warn "Build it first:  make agent-images   (or re-run scripts/demo.sh without --no-build)"
  fi

  log "Creating a governed run (agent=${DEMO_AGENT} repo=${DEMO_REPO})"
  local create_out run_id
  create_out="$(wardyn run --agent "${DEMO_AGENT}" --repo "${DEMO_REPO}" --task "${DEMO_TASK}")"
  printf '%s\n' "${create_out}"
  # CLI prints: "created run <full-uuid> (state ...)". Capture the full id.
  run_id="$(printf '%s\n' "${create_out}" | awk '/^created run/{print $3; exit}')"
  echo

  log "Runs:"
  wardyn runs list
  echo

  if [[ -n "${run_id}" ]]; then
    log "Audit trail for run ${run_id}:"
    wardyn audit --run "${run_id}" || warn "audit query failed"
  else
    warn "could not parse the created run id; run 'wardyn audit --run <full-id>' manually"
  fi

  echo
  log "Demo complete."
  echo "    UI:        http://localhost:8080  (SSO: demo@wardyn.local / password)"
  echo "    Dex:       http://localhost:5556/.well-known/openid-configuration"
  echo "    Admin API: Authorization: Bearer ${ADMIN_TOKEN}"
  echo "    Tear down: scripts/demo.sh down"
}

case "${1:-up}" in
  down) cmd_down ;;
  up)   cmd_up "${2:-}" ;;
  --no-build) cmd_up --no-build ;;
  *)    die "unknown command: ${1:-} (use: up | --no-build | down)" ;;
esac
