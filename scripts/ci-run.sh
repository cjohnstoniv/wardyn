#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

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
#   WARDYN_CI_OUT         artifact dir (run.json, audit.json, .cast) [./ci-artifacts]
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

# Per-job isolation on a shared, multi-job build host: scope EVERY compose object
# to a unique project + namespace so concurrent ci-run.sh invocations never
# collide on container names, the control-plane network, the recordings volume, or
# host ports — and one job's `down --volumes` teardown never touches another's.
# Caller may pin WARDYN_CI_PROJECT (e.g. to the CI job id) for a stable name;
# default is unique per invocation. The suffix comes from the kernel CSPRNG, NOT
# from $$: a PID is unique only within its own PID namespace, and the common CI
# shape — each job in its own container, all of them driving one shared host
# docker socket — puts several PID 1s (or 42s) on the same daemon at once. Two
# jobs that agree on a project name is not a naming annoyance here: the teardown
# below is `down --volumes`, so the second job destroys the first job's live
# postgres. COMPOSE_PROJECT_NAME scopes compose's own bookkeeping +
# unnamed volumes; WARDYN_NS scopes the explicitly-named objects (container_name /
# network / recordings volume) and MUST match wardynd's WARDYN_INTERNAL_NETWORK.
CI_PROJECT="${WARDYN_CI_PROJECT:-wardyn-ci-$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')}"
export COMPOSE_PROJECT_NAME="${CI_PROJECT}"
export WARDYN_NS="${CI_PROJECT}"
# Host UI/postgres/registry ports are unused in CI (the CLI runs in-container
# and health is read from the container): bind them to OS-assigned ephemeral
# ports so two jobs never fight over 8080/5432/5010. Registry included because
# `up -d ... wardynd` always starts it too (a wardynd dependency, docs/CI.md
# "Concurrent jobs on a shared host") — left at its fixed default, two
# concurrent jobs collide on the bind and the second job's wardynd never starts.
export WARDYN_UP_PORT="${WARDYN_UP_PORT:-0}"
export WARDYN_PG_PORT="${WARDYN_PG_PORT:-0}"
export WARDYN_REGISTRY_PORT="${WARDYN_REGISTRY_PORT:-0}"
COMPOSE=(docker compose -p "${CI_PROJECT}" -f "${COMPOSE_FILE}" -f "${CI_OVERLAY}")

TASK="${WARDYN_CI_TASK:-}"
IMAGE="${WARDYN_CI_IMAGE:-}"
TASK_MODE="${WARDYN_CI_TASK_MODE:-}"
AGENT="${WARDYN_CI_AGENT:-claude-code}"
CI_REPO="${WARDYN_CI_REPO:-}"
POLICY_FILE="${WARDYN_CI_POLICY_FILE:-${REPO_ROOT}/examples/policies/ci.json}"
TIMEOUT="${WARDYN_CI_TIMEOUT:-30m}"
OUT_DIR="${WARDYN_CI_OUT:-./ci-artifacts}"
export WARDYN_ADMIN_TOKEN="${WARDYN_ADMIN_TOKEN:-demo-admin-token}"

[[ -n "${TASK}" ]] || die "WARDYN_CI_TASK is required (the task / command to run)"
[[ -f "${POLICY_FILE}" ]] || die "policy file not found: ${POLICY_FILE}"
command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
docker compose version >/dev/null 2>&1 || die "docker compose v2 required"

# Same daemon for image builds AND the compose wardynd (dual-daemon boxes):
# honor DOCKER_HOST / the native-dockerd preference, and point the wardynd
# container's bind-mounted socket at it. wardyn_pick_docker_host derives both
# DOCKER_HOST and WARDYN_DOCKER_SOCK.
wardyn_pick_docker_host

# A project name only isolates this job if nothing else is USING it. The
# `down --volumes` further down is unconditional and ephemerality is
# load-bearing (see its comment), so a name already in use would be torn out
# from under whoever holds it — a concurrent job, or an operator's
# WARDYN_CI_KEEP=1 stack. Refuse instead. The query is by compose's own project
# label rather than `compose ps`, because ${COMPOSE[@]} carries the CI overlay
# and that file cannot even be loaded until WARDYN_CI_TOOLS_DIR exists (set
# below). Only RUNNING containers count, so an ordinary retry of a pinned
# WARDYN_CI_PROJECT whose stack already exited still proceeds.
_live="$(docker ps -q --filter "label=com.docker.compose.project=${CI_PROJECT}" 2>/dev/null || true)"
if [[ -n "${_live}" ]]; then
  die "compose project '${CI_PROJECT}' already has running containers — this job's 'down --volumes' would destroy them. Wait for that job, pick another WARDYN_CI_PROJECT, or tear it down: docker compose -p '${CI_PROJECT}' down --volumes"
fi
unset _live

# wardyn runs the shipped CLI inside the wardynd container (same shim as
# scripts/demo.sh — no host Go/binary needed at run time).
#
# It does NOT re-inject the admin bearer. wardynd already has it: compose
# interpolates WARDYN_ADMIN_TOKEN (exported above) into the service's own
# environment, so `exec` inherits it inside the container. Passing it again put
# a real fleet token on the HOST `docker` process argv — world-readable in `ps`
# and /proc/<pid>/cmdline to every other user on a shared runner — on every
# single CLI call this job makes. WARDYN_URL stays: it is an endpoint, not a
# credential, and the container has no reason to know it otherwise.
wardyn() {
  "${COMPOSE[@]}" exec -T \
    -e WARDYN_URL="http://localhost:8080" \
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
for tool in agent-run agent-run-lib.sh wardyn-rec wardyn-git-helper wardyn-scan; do
  docker cp -q "${tools_ctr}:/usr/local/bin/${tool}" "${TOOLS_DIR}/" 2>/dev/null \
    || warn "tool ${tool} not present in ${AGENT_IMAGE} (continuing)"
done
docker rm -f "${tools_ctr}" >/dev/null
for required in agent-run agent-run-lib.sh wardyn-rec wardyn-git-helper; do
  [[ -f "${TOOLS_DIR}/${required}" ]] || die "required runner tool ${required} missing from ${AGENT_IMAGE}"
done
export WARDYN_CI_TOOLS_DIR="${TOOLS_DIR}"

# ── bring up the core stack (postgres + wardynd only; no dex, admin token) ───
cleanup() {
  local code=$?
  if [[ "${WARDYN_CI_KEEP:-}" == "1" ]]; then
    # Print a teardown line the operator can actually paste into a fresh shell.
    # THREE env vars are load-bearing and none of them survives this process: the
    # CI overlay binds ${WARDYN_CI_TOOLS_DIR:?...}, so compose refuses to load the
    # project at all without it; WARDYN_NS is what the recordings volume and the
    # control-plane network are NAMED after, so a `down --volumes` without it
    # reaps the default-named objects and leaves this job's behind; and
    # DOCKER_HOST names the daemon the stack actually lives on —
    # wardyn_pick_docker_host derives it (a dual-daemon box lands on the native
    # dockerd, not the default context), so a paste without it addresses the WRONG
    # daemon, finds no such project, and exits 0 having removed nothing while the
    # trailing rm still deletes the tools dir the still-live wardynd is bind-
    # mounting. Emitted only when set, so a single-daemon host gets no noise.
    # Keeping the stack also skips the rm -rf below, so the tools dir outlives the
    # job with its path visible only in scrollback — hence the trailing rm, in the
    # same command, on the same line.
    warn "WARDYN_CI_KEEP=1 — leaving the stack up, and the runner-tools dir ${TOOLS_DIR} with it. Tear both down with:"
    warn "  ${DOCKER_HOST:+DOCKER_HOST='${DOCKER_HOST}' }WARDYN_NS='${WARDYN_NS}' WARDYN_CI_TOOLS_DIR='${TOOLS_DIR}' ${COMPOSE[*]} down --volumes && rm -rf '${TOOLS_DIR}'"
  else
    log "Tearing down the compose stack (volumes included — the stack is ephemeral)"
    # compose down only reaps objects compose itself created. The docker runner
    # mints the agent + proxy containers and the per-run internal network
    # directly via the Docker API (internal/runner/docker/naming.go), so they
    # are invisible to compose and survive `down` untouched. Remove them before
    # the network they share endpoints on goes away, or every CI run leaks a
    # sandbox.
    # A cancel that lands while `run --wait` is still blocked leaves run_id
    # unset (it is parsed only after the wait returns), so a name-only removal
    # misses exactly the sandbox a cancel is most likely to hit. Reap by label
    # instead, scoped to THIS job's control-plane network: the proxy is the only
    # Wardyn-minted container attached to it and carries the run-id label every
    # sibling object is named after, so a concurrent job's sandbox — on its own
    # ${WARDYN_NS}-internal — is never touched. run_id is still appended for the
    # one case the selector misses (proxy already gone, agent lingering).
    for _rid in $(docker ps -a --filter "label=wardyn.managed=true" \
      --filter "network=${WARDYN_NS}-internal" \
      --format '{{.Label "wardyn.run-id"}}' 2>/dev/null) "${run_id:-}"; do
      [[ -n "${_rid}" ]] || continue
      docker rm -f "wardyn-proxy-${_rid}" "wardyn-agent-${_rid}" >/dev/null 2>&1 || true
      docker network rm "wardyn-int-${_rid}" >/dev/null 2>&1 || true
    done
    "${COMPOSE[@]}" down --volumes >/dev/null 2>&1 || true
    rm -rf "${TOOLS_DIR}"
  fi
  exit "${code}"
}
# W16-S1-5: EXIT alone never fires on a hard job-cancel (SIGTERM, the signal a
# CI runner sends to abort a job) — the sandbox + control-plane network it
# started leaks past the job. `exit` inside a signal handler still fires the
# EXIT trap (bash re-enters it exactly once with the handler's own exit code),
# so TERM/INT just need to exit — cleanup itself stays registered only on
# EXIT, never running twice. SIGKILL still cannot be trapped by any process;
# nothing short of a reaper outside this shell recovers from that one.
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

# Ephemerality is load-bearing, not hygiene: a reused postgres volume holds
# secrets age-encrypted to a PREVIOUS boot's ephemeral key, and wardynd fails
# closed (by design) on the decrypt mismatch. Every invocation starts clean.
"${COMPOSE[@]}" down --volumes >/dev/null 2>&1 || true

log "Starting postgres + wardynd (WARDYN_ENVBUILD on for the BYOA wrap; project ${CI_PROJECT})"
"${COMPOSE[@]}" up -d postgres wardynd || die "compose up (check '${COMPOSE[*]} logs postgres wardynd')"
# Health from the CONTAINER, not a host port: CI publishes wardynd on an ephemeral
# host port (WARDYN_UP_PORT=0), so localhost:PORT is unknown here — but the
# container's own healthcheck runs `wardyn runs list` inside it. This is also
# topology-independent (works under Docker Desktop + WSL2 NAT).
log "Waiting for wardynd (container health, project ${CI_PROJECT})"
_cid="$("${COMPOSE[@]}" ps -q wardynd 2>/dev/null || true)"
_tries=0
until [ -n "${_cid}" ] && [ "$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${_cid}" 2>/dev/null)" = "healthy" ]; do
  _tries=$((_tries + 1))
  if [ "${_tries}" -gt 45 ]; then "${COMPOSE[@]}" logs --tail 50 wardynd; die "wardynd did not become healthy"; fi
  sleep 2
  _cid="$("${COMPOSE[@]}" ps -q wardynd 2>/dev/null || true)"
done
log "wardynd healthy"

# ── seed secrets (values via stdin, never argv) ──────────────────────────────
if [[ -n "${WARDYN_CI_SECRETS:-}" ]]; then
  IFS=',' read -ra pairs <<<"${WARDYN_CI_SECRETS}"
  for pair in "${pairs[@]}"; do
    name="${pair%%=*}"; value="${pair#*=}"
    [[ -n "${name}" && "${pair}" == *"="* ]] || die "WARDYN_CI_SECRETS entry '${pair}' is not name=value"
    log "Seeding secret ${name}"
    printf '%s' "${value}" | wardyn secret set "${name}" || die "seed secret ${name} (check wardynd health: '${COMPOSE[*]} ps wardynd' / '${COMPOSE[*]} logs wardynd', and that WARDYN_ADMIN_TOKEN is correct)"
  done
fi

# A subscription credential belongs to one human; CI runs on behalf of everyone
# who can trigger the pipeline. Connecting one here makes that person's Claude
# subscription serve other people's work, which the harness vendor's terms
# prohibit — and it is the OPERATOR who ends up in breach, not Wardyn. The daemon
# refuses it structurally now (single-user posture only), so fail here with the
# reason rather than connecting something that will not resolve at run time.
if [[ -n "${WARDYN_SUBSCRIPTION_TOKEN:-}" ]]; then
  die "WARDYN_SUBSCRIPTION_TOKEN is not supported in CI: a subscription credential belongs to one person, and a pipeline runs on behalf of everyone who can trigger it. Use an API key (WARDYN_CI_SECRETS=anthropic-api-key=sk-...) or Bedrock — see docs/CI.md."
fi

# ── launch args (shared by the preflight preview and the real launch) ────────
mkdir -p "${OUT_DIR}"
"${COMPOSE[@]}" cp "${POLICY_FILE}" wardynd:/tmp/wardyn-ci-policy.json >/dev/null || die "copy policy into wardynd"

base_args=(run --agent "${AGENT}" --task "${TASK}" --policy-file /tmp/wardyn-ci-policy.json)
[[ -n "${CI_REPO}" ]] && base_args+=(--repo "${CI_REPO}")
[[ -n "${IMAGE}" ]] && base_args+=(--image "${IMAGE}")
[[ -n "${TASK_MODE}" ]] && base_args+=(--task-mode "${TASK_MODE}")

# ── preflight (best-effort; a dry-run of launch resolution, mints nothing) ───
# Same body the launch posts — the CLI builds it, so this can never drift from
# the real run the way the old hand-curled jq body could.
wardyn "${base_args[@]}" --dry-run 2>&1 | sed 's/^/  preflight: /' ||
  warn "preflight failed (continuing — launch will fail closed on real blockers)"

# ── launch + wait ────────────────────────────────────────────────────────────
run_args=("${base_args[@]}" --wait --timeout "${TIMEOUT}" --json)

log "Launching governed run: wardyn ${run_args[*]}"
run_json="${OUT_DIR}/run.json"
run_log="${OUT_DIR}/run.log"
# --wait blocks for the whole governed run — minutes, often tens of them. Buffer
# its stderr into run.log and cat the file afterwards (what this used to do) and
# the job prints NOTHING until the run is already over: not even the run id, and
# a job the pipeline kills mid-wait loses the whole buffer, console and artifact
# alike. tee puts each line on the console as it happens AND still lands the copy
# in run.log. This is NOT a heartbeat: waitForRun (cmd/wardyn/commands.go) prints
# once when the wait starts and once when it ends, so a long wait is still silent
# in between — an inactivity timeout on the pipeline still needs its own answer.
# Redirection order is load-bearing: `2>&1` duplicates stderr onto the pipe while
# stdout is still the pipe, and only THEN does stdout move to run.json — so the
# --json contract keeps the structured stdout clean, with no progress mixed in.
# PIPESTATUS[0] rather than $? because $? is now tee's: this script's whole
# contract is exiting with the RUN's code.
wardyn "${run_args[@]}" 2>&1 >"${run_json}" | tee "${run_log}" >&2
run_code=${PIPESTATUS[0]}

# --json's structured stdout survives a --wait run cleanly; the old scrape of a
# "created run <id>..." text line broke the moment anything else printed first.
# jq is the happy path; the sed fallback covers CI images without it.
run_id="$(jq -r '.id' "${run_json}" 2>/dev/null || sed -n 's/.*"id"[^"]*"\([0-9a-f-]\{8,\}\)".*/\1/p' "${run_json}" | head -1)"

# ── collect artifacts ────────────────────────────────────────────────────────
if [[ -n "${run_id}" ]]; then
  log "Collecting artifacts for run ${run_id} -> ${OUT_DIR}"
  # W16-S1-4: `>"${run_json}"` truncates the file the moment the shell sets up
  # redirection — BEFORE `wardyn run get` runs — so a failed refetch destroyed
  # the run.json already captured from the --wait launch above. Refetch into a
  # temp file and only replace run.json once the call actually succeeded.
  run_json_tmp="$(mktemp)"
  if wardyn run get "${run_id}" --json >"${run_json_tmp}" 2>/dev/null; then
    mv "${run_json_tmp}" "${run_json}"
  else
    warn "run get failed — keeping the run.json captured at launch"
    rm -f "${run_json_tmp}"
  fi

  # The terminal recording is an artifact too: exec runs are recorded exactly
  # like harness ones (agent-run-lib.sh), and the teardown below drops the
  # recordings volume — uncollected means gone for good. Best-effort; a run
  # that died before writing a cast simply has none to fetch. Note the cast
  # comes back on STDOUT, not via `-o`: the wardyn shim runs the CLI inside the
  # wardynd container, so `-o` would write the file into that container and the
  # artifact dir would stay empty. Temp-then-move for the same reason run.json
  # does it — a failed fetch must not leave a 0-byte "recording" behind.
  cast_tmp="$(mktemp)"
  if wardyn run recording "${run_id}" >"${cast_tmp}" 2>/dev/null && [[ -s "${cast_tmp}" ]]; then
    mv "${cast_tmp}" "${OUT_DIR}/session.cast"
  else
    rm -f "${cast_tmp}"
    warn "no terminal recording collected for run ${run_id}"
  fi

  # The per-run audit trail truncates at 1000 events (oldest-first) unless
  # paged (W16-S1-2) — a run with more tool calls/egress decisions than that
  # would otherwise silently lose its newest events, INCLUDING run.complete,
  # from this artifact. Page with --limit/--offset until a short page proves
  # we reached the end (the same "walk forward to the newest events" contract
  # docs/sdk.md documents). jq is required to splice pages into one array;
  # without it, fall back to a single (possibly-truncated) call rather than
  # hand-rolling JSON concatenation in sed.
  if command -v jq >/dev/null 2>&1; then
    audit_jsonl="$(mktemp)"
    audit_limit=1000
    audit_offset=0
    while :; do
      page="$(wardyn audit "${run_id}" --limit "${audit_limit}" --offset "${audit_offset}" --json 2>/dev/null)" \
        || { warn "audit fetch failed at offset ${audit_offset}"; break; }
      page_count="$(printf '%s' "${page}" | jq 'length' 2>/dev/null)" || page_count=0
      [[ "${page_count}" -gt 0 ]] && printf '%s' "${page}" | jq -c '.[]' >>"${audit_jsonl}"
      audit_offset=$((audit_offset + page_count))
      [[ "${page_count}" -ge "${audit_limit}" ]] || break
    done
    jq -s '.' "${audit_jsonl}" >"${OUT_DIR}/audit.json" 2>/dev/null || warn "audit artifact assembly failed"
    rm -f "${audit_jsonl}"
  else
    warn "jq not found: collecting a single (possibly truncated) audit page"
    # Same temp-then-move as run.json above: a direct `>audit.json` would leave
    # an empty artifact that reads as "this run had no audit events".
    audit_tmp="$(mktemp)"
    if wardyn audit "${run_id}" --json >"${audit_tmp}" 2>/dev/null; then
      mv "${audit_tmp}" "${OUT_DIR}/audit.json"
    else
      rm -f "${audit_tmp}"
      warn "audit fetch failed"
    fi
  fi
else
  warn "no run id parsed from output; skipping artifact collection"
fi

log "Run finished with exit code ${run_code} (artifacts in ${OUT_DIR})"
exit "${run_code}"
