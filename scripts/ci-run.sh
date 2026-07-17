#!/usr/bin/env bash
# Wardyn CI one-shot: bring up a fresh control plane from nothing, launch ONE
# governed sandboxed run, wait for its outcome, collect artifacts, tear down,
# and exit with the run's exit code. Designed for CI runners (GitHub Actions,
# Azure DevOps) — no UI, no human, no pre-running wardyn. See docs/CI.md.
#
# BYOA (bring your own agent/container): set WARDYN_CI_IMAGE to any OCI image;
# Wardyn wraps it with the runner tools and governs it (egress allowlist,
# brokered creds, confinement, audit). WARDYN_CI_TASK_MODE=exec runs the task
# as a plain shell command in that image — no agent, no LLM credentials.
#
# Usage:  scripts/ci-run.sh          # env-driven, see the table below
#
# Env:
#   WARDYN_CI_TASK        task text (exec mode: the shell command)   [required]
#   WARDYN_CI_IMAGE       BYOA base image ref (e.g. ubuntu:24.04)    [optional]
#   WARDYN_CI_TASK_MODE   harness (agent) | exec (plain command)     [harness]
#   WARDYN_CI_AGENT       agent name for harness mode / tools source [claude-code]
#   WARDYN_CI_REPO        org/name to clone into the workspace       [optional]
#   WARDYN_CI_POLICY_FILE RunPolicySpec JSON path                    [examples/policies/ci.json]
#   WARDYN_CI_SECRETS     name=value[,name=value...] seeded pre-run  [optional]
#   WARDYN_CI_TIMEOUT     wardyn run --wait timeout                  [30m]
#   WARDYN_CI_OUT         artifact dir (run.json, audit.json)        [./ci-artifacts]
#   WARDYN_CI_KEEP        1 = leave the stack up for debugging       [unset]
#   WARDYN_CI_SKIP_BUILD  1 = reuse existing local images            [unset]
#   WARDYN_ADMIN_TOKEN    admin bearer token                         [demo-admin-token]
#
# Exit code: the run's outcome from `wardyn run --wait` — 0 COMPLETED,
# agent/command exit code on FAILED, 2 KILLED/STOPPED, 124 timeout.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
source "${REPO_ROOT}/scripts/lib/common.sh"

COMPOSE_FILE="${REPO_ROOT}/deploy/compose/docker-compose.yaml"
CI_OVERLAY="${REPO_ROOT}/deploy/compose/docker-compose.ci.yaml"
COMPOSE=(docker compose -f "${COMPOSE_FILE}" -f "${CI_OVERLAY}")

TASK="${WARDYN_CI_TASK:-}"
IMAGE="${WARDYN_CI_IMAGE:-}"
TASK_MODE="${WARDYN_CI_TASK_MODE:-}"
AGENT="${WARDYN_CI_AGENT:-claude-code}"
CI_REPO="${WARDYN_CI_REPO:-}"
POLICY_FILE="${WARDYN_CI_POLICY_FILE:-${REPO_ROOT}/examples/policies/ci.json}"
TIMEOUT="${WARDYN_CI_TIMEOUT:-30m}"
OUT_DIR="${WARDYN_CI_OUT:-./ci-artifacts}"
export WARDYN_ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"
BASE_URL="http://localhost:${WARDYN_UP_PORT:-8080}"

[[ -n "${TASK}" ]] || die "WARDYN_CI_TASK is required (the task / command to run)"
[[ -f "${POLICY_FILE}" ]] || die "policy file not found: ${POLICY_FILE}"
command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
docker compose version >/dev/null 2>&1 || die "docker compose v2 required"

# Same daemon for image builds AND the compose wardynd (dual-daemon boxes):
# honor DOCKER_HOST / the native-dockerd preference, and point the wardynd
# container's bind-mounted socket at it.
wardyn_pick_docker_host
if [[ -n "${DOCKER_HOST:-}" && -z "${WARDYN_DOCKER_SOCK:-}" && "${DOCKER_HOST}" == unix://* ]]; then
  export WARDYN_DOCKER_SOCK="${DOCKER_HOST#unix://}"
fi

# wardyn runs the shipped CLI inside the wardynd container with the admin token
# (same shim as scripts/demo.sh — no host Go/binary needed at run time).
wardyn() {
  "${COMPOSE[@]}" exec -T \
    -e WARDYN_URL="http://localhost:8080" \
    -e WARDYN_ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN}" \
    wardynd /usr/local/bin/wardyn "$@"
}

# Tools dir: created up front (the compose overlay interpolates it on every
# compose invocation, including builds); populated after the agent image build.
TOOLS_DIR="$(mktemp -d)"
export WARDYN_CI_TOOLS_DIR="${TOOLS_DIR}"

# ── build (skippable; CI caches docker layers) ───────────────────────────────
AGENT_IMAGE="wardyn/agent-${AGENT}:local"
if [[ "${WARDYN_CI_SKIP_BUILD:-}" != "1" ]]; then
  log "Building wardynd + wardyn-proxy images"
  "${COMPOSE[@]}" build wardynd || die "build wardynd (check disk space/network; retry, or set WARDYN_CI_SKIP_BUILD=1 to reuse existing local images)"
  "${COMPOSE[@]}" --profile build-only build proxy-image || die "build proxy image (check disk space/network; retry, or set WARDYN_CI_SKIP_BUILD=1 to reuse existing local images)"
  # The agent image is needed even for pure BYOA runs: it is the source of the
  # runner tools the BYOI wrap COPYs into the user image.
  agent_dockerfile="${REPO_ROOT}/deploy/images/${AGENT}/Dockerfile"
  [[ -f "${agent_dockerfile}" ]] || die "no Dockerfile for agent '${AGENT}' at ${agent_dockerfile}"
  log "Building agent image ${AGENT_IMAGE}"
  docker build -f "${agent_dockerfile}" -t "${AGENT_IMAGE}" "${REPO_ROOT}" || die "build agent image"
fi

# ── assemble the runner-tools dir for the BYOI wrap ──────────────────────────
# FinalizeBase COPYs everything in this dir into the wrapped image; extract the
# tools from the agent image so there is one source of truth.
log "Assembling runner tools from ${AGENT_IMAGE} -> ${TOOLS_DIR}"
# The agent image has no default CMD (the driver always supplies argv), so
# docker create needs a dummy command; the container is never started.
tools_ctr="$(docker create "${AGENT_IMAGE}" true)" || die "docker create ${AGENT_IMAGE} (build it or unset WARDYN_CI_SKIP_BUILD)"
for tool in agent-run agent-run-lib.sh wardyn-rec wardyn-verify wardyn-git-helper wardyn-scan; do
  docker cp -q "${tools_ctr}:/usr/local/bin/${tool}" "${TOOLS_DIR}/" 2>/dev/null \
    || warn "tool ${tool} not present in ${AGENT_IMAGE} (continuing)"
done
docker rm -f "${tools_ctr}" >/dev/null
for required in agent-run wardyn-verify wardyn-git-helper; do
  [[ -f "${TOOLS_DIR}/${required}" ]] || die "required runner tool ${required} missing from ${AGENT_IMAGE}"
done
export WARDYN_CI_TOOLS_DIR="${TOOLS_DIR}"

# ── bring up the core stack (postgres + wardynd only; no dex, admin token) ───
cleanup() {
  local code=$?
  if [[ "${WARDYN_CI_KEEP:-}" == "1" ]]; then
    warn "WARDYN_CI_KEEP=1 — leaving the stack up (tear down with: ${COMPOSE[*]} down --volumes)"
  else
    log "Tearing down the compose stack (volumes included — the stack is ephemeral)"
    "${COMPOSE[@]}" down --volumes >/dev/null 2>&1 || true
    rm -rf "${TOOLS_DIR}"
  fi
  exit "${code}"
}
trap cleanup EXIT

# Ephemerality is load-bearing, not hygiene: a reused postgres volume holds
# secrets age-encrypted to a PREVIOUS boot's ephemeral key, and wardynd fails
# closed (by design) on the decrypt mismatch. Every invocation starts clean.
"${COMPOSE[@]}" down --volumes >/dev/null 2>&1 || true

log "Starting postgres + wardynd (WARDYN_ENVBUILD on for the BYOA wrap)"
"${COMPOSE[@]}" up -d postgres wardynd || die "compose up (check 'docker compose -f ${COMPOSE_FILE} logs postgres wardynd' and that ports 8080/5432 are free)"
log "Waiting for wardynd at ${BASE_URL}"
wait_healthy "${BASE_URL}" 90 2 || { "${COMPOSE[@]}" logs --tail 50 wardynd; die "wardynd did not become healthy"; }

# ── seed secrets (values via stdin, never argv) ──────────────────────────────
if [[ -n "${WARDYN_CI_SECRETS:-}" ]]; then
  IFS=',' read -ra pairs <<<"${WARDYN_CI_SECRETS}"
  for pair in "${pairs[@]}"; do
    name="${pair%%=*}"; value="${pair#*=}"
    [[ -n "${name}" && "${pair}" == *"="* ]] || die "WARDYN_CI_SECRETS entry '${pair}' is not name=value"
    log "Seeding secret ${name}"
    printf '%s' "${value}" | wardyn secret set "${name}" || die "seed secret ${name} (check wardynd is healthy at ${BASE_URL} and WARDYN_ADMIN_TOKEN is correct)"
  done
fi

# ── preflight (best-effort; a dry-run of launch resolution, mints nothing) ───
if command -v jq >/dev/null 2>&1 && curl -sf "${BASE_URL}/healthz" >/dev/null 2>&1; then
  preflight_body="$(jq -n \
    --arg agent "${AGENT}" --arg repo "${CI_REPO}" --arg image "${IMAGE}" \
    --arg task "${TASK}" --arg task_mode "${TASK_MODE}" \
    --slurpfile policy "${POLICY_FILE}" \
    '{agent:$agent, task:$task, inline_policy:$policy[0]}
     + (if $repo != "" then {repo:$repo} else {} end)
     + (if $image != "" then {image:$image} else {} end)
     + (if $task_mode != "" then {task_mode:$task_mode} else {} end)')"
  if preflight="$(curl -sf -X POST -H "Authorization: Bearer ${WARDYN_ADMIN_TOKEN}" \
      -H "Content-Type: application/json" -d "${preflight_body}" \
      "${BASE_URL}/api/v1/runs/preflight" 2>/dev/null)"; then
    log "Preflight: $(jq -c '{enforced_confinement_class, setup_items: [.setup_items[]? | .title // .]}' <<<"${preflight}")"
  else
    warn "preflight call failed (continuing — launch will fail closed on real blockers)"
  fi
else
  warn "jq or host->wardynd reachability missing; skipping preflight preview"
fi

# ── launch + wait ────────────────────────────────────────────────────────────
mkdir -p "${OUT_DIR}"
"${COMPOSE[@]}" cp "${POLICY_FILE}" wardynd:/tmp/wardyn-ci-policy.json >/dev/null || die "copy policy into wardynd"

run_args=(run --agent "${AGENT}" --task "${TASK}" --policy-file /tmp/wardyn-ci-policy.json --wait --timeout "${TIMEOUT}" --json)
[[ -n "${CI_REPO}" ]] && run_args+=(--repo "${CI_REPO}")
[[ -n "${IMAGE}" ]] && run_args+=(--image "${IMAGE}")
[[ -n "${TASK_MODE}" ]] && run_args+=(--task-mode "${TASK_MODE}")

log "Launching governed run: wardyn ${run_args[*]}"
run_json="${OUT_DIR}/run.json"
run_log="${OUT_DIR}/run.log"
wardyn "${run_args[@]}" >"${run_json}" 2>"${run_log}"
run_code=$?
cat "${run_log}" >&2

# --json's structured stdout survives a --wait run cleanly; the old scrape of a
# "created run <id>..." text line broke the moment anything else printed first.
# jq is the happy path; the sed fallback covers CI images without it.
run_id="$(jq -r '.id' "${run_json}" 2>/dev/null || sed -n 's/.*"id"[^"]*"\([0-9a-f-]\{8,\}\)".*/\1/p' "${run_json}" | head -1)"

# ── collect artifacts ────────────────────────────────────────────────────────
if [[ -n "${run_id}" ]]; then
  log "Collecting artifacts for run ${run_id} -> ${OUT_DIR}"
  wardyn run get "${run_id}" --json >"${run_json}" 2>/dev/null || warn "run get failed"
  wardyn audit --run "${run_id}" --json >"${OUT_DIR}/audit.json" 2>/dev/null || warn "audit fetch failed"
else
  warn "no run id parsed from output; skipping artifact collection"
fi

log "Run finished with exit code ${run_code} (artifacts in ${OUT_DIR})"
exit "${run_code}"
