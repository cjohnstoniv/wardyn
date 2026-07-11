#!/usr/bin/env bash
# Run wardynd on the HOST (outside compose) so the Claude CLI composer backend can exec
# the resident `claude` binary (your subscription) and so the docker runner can launch
# sandboxes against the host daemon.
#
# Prereqs:
#   - go build -tags docker -o bin/wardynd ./cmd/wardynd   (and: -o bin/wardyn ./cmd/wardyn)
#   - cd ui && pnpm build                                  (serves ui/dist)
#   - compose Postgres up with its loopback port published (deploy/compose/docker-compose.yaml
#     publishes 127.0.0.1:5432) — e.g. `docker compose -f deploy/compose/docker-compose.yaml up -d postgres`
#   - `claude` logged in on the host PATH (for the cli composer backend)
#
# Secrets (the age key) are read from the gitignored deploy/compose/.env so they are never
# committed. Override any WARDYN_* var by exporting it before running this script.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Pull the pinned age key from the gitignored compose env (so persisted secrets decrypt).
if [ -z "${WARDYN_AGE_KEY:-}" ] && [ -f deploy/compose/.env ]; then
  WARDYN_AGE_KEY="$(grep -E '^WARDYN_AGE_KEY=' deploy/compose/.env | head -1 | cut -d= -f2-)"
  export WARDYN_AGE_KEY
fi

# Resident `claude` CLI for the composer backend.
export PATH="$HOME/.local/bin:$PATH"

# Reach the host control plane from agent/proxy sandbox containers. Use the cross-docker
# host alias host.docker.internal, which the proxy sidecar maps to the docker host gateway
# via ExtraHosts (host-gateway). This works on Docker Desktop (alias auto-injected) AND on
# native Linux/WSL docker (explicit host-gateway mapping) — unlike the raw docker0 gateway
# IP, which is reachable only on native docker. Override for unusual setups.
# NOTE: on Docker Desktop + WSL2 in default NAT mode, host.docker.internal resolves to the
# Windows host, not the WSL distro running wardynd, so the recording upload may still not
# reach it (enable WSL mirrored networking to fully close that gap). Recording delivery is
# best-effort, so the run still completes regardless.
export WARDYN_CONTROL_PLANE_URL="${WARDYN_CONTROL_PLANE_URL:-http://host.docker.internal:8080}"

export WARDYN_PG_DSN="${WARDYN_PG_DSN:-postgres://wardyn:wardyn-dev@127.0.0.1:5432/wardyn?sslmode=disable}"
export WARDYN_LISTEN="${WARDYN_LISTEN:-:8080}"
export WARDYN_LOCAL_MODE="${WARDYN_LOCAL_MODE:-true}"
export WARDYN_RUNNER="${WARDYN_RUNNER:-docker}"
export WARDYN_UI_DIR="${WARDYN_UI_DIR:-$ROOT/ui/dist}"
export WARDYN_PROXY_IMAGE="${WARDYN_PROXY_IMAGE:-wardyn/wardyn-proxy:local}"
# composer-dev.json is the composer-capable ceiling: it lists an api_key grant
# (so a composed LLM run's brokered model credential survives the clamp) and the
# LLM egress domains. demo.json (github_token only) would clamp the model grant
# away, leaving composed runs with no model access. When the operator has staged
# subscription creds (scripts/stage-claude-creds.sh), prefer the generated
# subscription ceiling — it additionally blesses the ~/.claude cred mounts that
# a composed run receives on the per-run "Use my Claude subscription" opt-in.
SUB_POLICY="${HOME}/.wardyn/composer-dev-subscription.json"
if [ -z "${WARDYN_DEFAULT_POLICY:-}" ] && [ -f "${SUB_POLICY}" ]; then
  WARDYN_DEFAULT_POLICY="${SUB_POLICY}"
fi
export WARDYN_DEFAULT_POLICY="${WARDYN_DEFAULT_POLICY:-$ROOT/examples/policies/composer-dev.json}"
# Map agent names to the LOCALLY-built demo images (else the runner pulls the ghcr
# convention image, which doesn't exist → run.create fails "registry: denied").
# The "oracle" agent (wardyn/agent-oracle:local, deploy/images/oracle) runs a
# task's mounted solution.sh — the $0 deterministic lane for the e2e orchestrator.
export WARDYN_AGENT_IMAGES="${WARDYN_AGENT_IMAGES:-{\"claude-code\":\"wardyn/agent-claude-code:local\",\"codex-cli\":\"wardyn/agent-codex-cli:local\",\"oracle\":\"wardyn/agent-oracle:local\"}}"

# Pin the claude-code agent to Opus so it never falls back to the account default
# (a promo can push that to Fable). Overridable; empty uses the CLI default.
export WARDYN_AGENT_ANTHROPIC_MODEL="${WARDYN_AGENT_ANTHROPIC_MODEL:-opus}"

# Host mode uses the Claude CLI composer backend (Opus via your subscription). This
# OVERRIDES any WARDYN_COMPOSER_CONFIG from .env — the compose container can't exec the
# CLI backend, so it belongs to host mode only.
export WARDYN_COMPOSER_CONFIG="${WARDYN_COMPOSER_CONFIG_OVERRIDE:-$ROOT/examples/composer-configs/claude-cli-opus.json}"

# Ensure the control-plane-facing network the proxy sidecar joins exists. The
# compose stack defines it (deploy/compose/docker-compose.yaml: name
# wardyn-internal); in host mode (esp. against a fresh native dockerd) it may not,
# and run.create fails "network wardyn-internal not found". Create it idempotently
# on whichever daemon wardynd targets (honors DOCKER_HOST).
WARDYN_INTERNAL_NETWORK="${WARDYN_INTERNAL_NETWORK:-wardyn-internal}"
if ! docker network inspect "$WARDYN_INTERNAL_NETWORK" >/dev/null 2>&1; then
  docker network create "$WARDYN_INTERNAL_NETWORK" >/dev/null 2>&1 \
    && echo "wardynd (host mode): created control-plane network $WARDYN_INTERNAL_NETWORK" \
    || echo "wardynd (host mode): WARNING could not create network $WARDYN_INTERNAL_NETWORK"
fi

echo "wardynd (host mode): listen=$WARDYN_LISTEN composer=claude-cli-opus control-plane=$WARDYN_CONTROL_PLANE_URL"
exec ./bin/wardynd
