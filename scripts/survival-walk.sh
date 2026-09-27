#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# T-40 (#700): the COMPOSE leg only — `docker kill` wardynd while its stored
# AWS SSO credential is SPENT (wardynd's own refresh check says so), restart
# it, and confirm — by reading Postgres directly, not by asking the fake
# again — that the spent state survived the restart. Then resolve with a
# fresh sign-in and confirm dispatch works again.
#
# #700 ALSO ASKS FOR, AND THIS SCRIPT DOES NOT COVER:
#   - watcher re-adoption of a run, with the hold resolving and the run
#     finishing (the in-flight-request shape; see below for why this leg
#     cannot build it in plain compose);
#   - a Docker DAEMON restart under a live run -> lost(reboot), Revive
#     restoring files + the current ceiling, run.revive audited;
#   - an outage over an hour -> lost(outage);
#   - the kind leg;
#   - the hop CA staying stable across boots.
# Those remain open against #700.
#
# WHAT THIS DOES NOT PROVE, AND WHY: it does not hold a LIVE, in-flight
# sandbox Bedrock request the way the kind SSO walk already does (that walk's
# own credential_reauth proof, over a real cluster, stands on its own — this
# script does not repeat it). See the big comment further down (search
# "WHAT THIS WALK PROVES") for the live-reproduced, code-cited reason a
# sandbox-initiated hold cannot be built here: a proxy invariant
# (internal/egress/proxy/egress_target.go's onOwnSubnetOrControlPlane)
# unconditionally refuses to lift ANY address on the proxy's own attached
# subnets, and this is the ONLY kind of address a plain docker-compose
# project can ever give a fake the sandbox can reach. What THIS script proves
# instead is real and useful on its own: wardynd's OWN direct
# credential-freshness check (the same refreshAWSSSOBlob call the live path
# would also have used) correctly detects a spent credential, that the
# credential's DELETION and the spent-token mark are both actually written
# to Postgres (read back directly, not inferred from a dispatch refusal
# alone) and survive a docker-kill/restart of wardynd, and a fresh sign-in
# clears it again — using the same fake sso-oidc/portal stub the kind SSO
# walk trusts (test/awsssofake, run here as test/awsssofake/cmd's standalone
# binary rather than a k8s pod).
#
# THE PRECONDITIONS:
#  1. wardynd, Postgres and (once built) the agent images, on a dedicated
#     compose project — never the operator's own stack.
#  2. wardynd pointed at the fake for its OWN direct calls (never
#     proxy/sandbox-gated): WARDYN_ALLOW_TEST_ENDPOINTS=true,
#     WARDYN_AWS_SSO_ENDPOINT_OVERRIDE / WARDYN_BEDROCK_BASE_URL both aimed at
#     the fake by CONTAINER NAME on the project's own control-plane network
#     (docker-compose.survival-walk.yaml) — never host.docker.internal (see
#     below). internal/api/awssso_refresh.go's createAWSSSOToken is wardynd's
#     OWN outbound HTTP call, so this alone is enough for it.
#  3. the org's agent standard declared bedrock_sso/shared (PUT
#     /site-config), so a claude-code run actually dispatches on the AWS-SSO
#     Bedrock lane at all.
#
# THE CAPTURE NEVER GOES THROUGH THE SANDBOX-FACING PROXY, DELIBERATELY. A
# first attempt drove the real interactive `aws sso login` device-code flow
# inside the aws-sso login sandbox (matching scripts/kind-sso-walk.sh) — every
# individual piece worked (the fake auto-approves every device code per
# test/awsssofake/cmd/main.go's own comment, and wardynd's own env override
# reached the sandbox correctly), but the PROXY's own SSRF guard
# (internal/egress/proxy/egress_target.go's onOwnSubnetOrControlPlane)
# unconditionally refuses to lift ANY address inside its own attached
# networks' subnets — and host.docker.internal resolves to an address inside
# the proxy's own control-plane network. That refusal is a deliberate
# invariant ("never lift your own subnet"), not a bug: kind's walk reaches
# its fake over a Kubernetes Service ClusterIP, a genuinely separate,
# non-locally-attached address space with no compose equivalent, so the same
# recipe cannot be ported verbatim.
#
# Instead, this walk drives the SAME upload cmd/wardyn-aws-sso makes after a
# real device-code login — PUT ${WARDYN_PROXY_URL}/wardyn/v1/sso-token/${id},
# a BROKERED route the proxy forwards to wardynd's own
# /api/v1/internal/sso-token/{runID} with the run's identity injected, never
# policy-gated — with a blob built from a REAL RegisterClient +
# StartDeviceAuthorization + CreateToken(device_code) round trip run directly
# against the fake from the HOST (the fake also publishes its port on
# 127.0.0.1, no container networking involved for THIS half). This is the
# exact wire shape wardynd's handler validates (region/start_url bound to the
# login run's own launch stamp) — the same fabrication-over-the-real-upload-
# path technique test/awsssofake/reauth_hold_docker_test.go uses for the SDK
# side (syntheticAWSHome), just at the control-plane's own capture boundary
# instead of the sandbox filesystem.
#
# THE FAKE RUNS AS A CONTAINER ON THE PROJECT'S OWN CONTROL-PLANE NETWORK
# (the same one wardynd's container joins), not as a host process reached via
# host.docker.internal. A second real finding, on this same walk: wardynd's
# OWN refresh call (createAWSSSOToken, direct, no proxy) reaching a host
# process through host.docker.internal's NAT/port-forward was observed live
# to hang indefinitely ("Client.Timeout exceeded while awaiting headers")
# while the identical request succeeded instantly addressed by container
# name on an ordinary bridge network — a Docker Desktop/WSL2 host-gateway
# reliability issue (confirmed independently: a plain container-to-host curl
# through the same path hung the same way; the same call container-to-
# container did not), not anything this repo's code controls. Since
# wardynd's refresh call is the ONLY thing that ever needed to reach the
# fake over the network (the capture upload above never does), giving the
# fake a normal container identity on wardynd's own network removes the
# unreliable hop entirely.
#
# GUARD: like every other dedicated-stack e2e script, self-skips unless
# WARDYN_TEST_DOCKER=1. Tears down its own project (compose down --volumes,
# scoped by name) on every exit path.
set -uo pipefail

if [[ "${WARDYN_TEST_DOCKER:-}" != "1" ]]; then
  echo "survival-walk: set WARDYN_TEST_DOCKER=1 to run the Docker-dependent survival walk (skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

# DEFAULT DAEMON ONLY. Deliberately NOT wardyn_pick_docker_host: on a box with
# a second dedicated daemon at /run/wardyn-docker.sock (or the legacy
# /var/run/ path) that helper auto-picks it, and this walk must never touch a
# socket or a wardyn-* container it did not create itself. Pinned explicitly,
# and WARDYN_DOCKER_SOCK left unset so compose bind-mounts its own default.
export DOCKER_HOST="unix:///var/run/docker.sock"
unset WARDYN_DOCKER_SOCK

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
pass() { printf '  \033[1;32m[pass]\033[0m %s\n' "$*"; }
fail() { printf '  \033[1;31m[FAIL]\033[0m %s\n' "$*"; FAILED=1; }
FAILED=0
CURL_MAX_TIME="${WARDYN_SURVIVAL_CURL_MAX_TIME:-10}"

for bin in docker curl jq go; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

# ── dedicated stack identity — never the operator's/another job's stack ─────
# Settable so two copies of this walk (e.g. a real run and a mutant-proof run
# in a separate worktree) can run without colliding.
PROJECT="${WARDYN_SURVIVAL_PROJECT:-cl-700-survival}"
COMPOSE_FILE="${ROOT}/deploy/compose/docker-compose.yaml"
OVERRIDE_FILE="${ROOT}/deploy/compose/docker-compose.survival-walk.yaml"
API_PORT="${WARDYN_SURVIVAL_API_PORT:-18180}"
PG_PORT="${WARDYN_SURVIVAL_PG_PORT:-15532}"
REGISTRY_PORT="${WARDYN_SURVIVAL_REGISTRY_PORT:-15110}"
SSH_PORT="${WARDYN_SURVIVAL_SSH_PORT:-12322}"
UI_SANDBOX_PORT="${WARDYN_SURVIVAL_UI_SANDBOX_PORT:-18181}"
FAKE_PORT="${WARDYN_SURVIVAL_FAKE_PORT:-18390}"
BASE="http://127.0.0.1:${API_PORT}"
ADMIN_TOKEN="demo-admin-token" # compose's own default (WARDYN_LOCAL_MODE left off)

# Project-unique image tags — never the shared, mutable wardyn/*:local a
# concurrent job on this daemon may be mid-rebuild of (run-e2e-ssh.sh's own
# reasoning; see that script's header for the observed failure this avoids).
WARDYND_IMAGE="wardyn/wardynd:${PROJECT}"
PROXY_IMAGE="wardyn/wardyn-proxy:${PROJECT}"
AGENT_IMAGE="${PROJECT}/agent-claude-code:pinned"
AWSSSO_AGENT_IMAGE="${PROJECT}/agent-aws-sso:pinned"

# The pinned pair the fake's entitlement fixture advertises, and the model ARN
# naming that same account (internal/api/awssso_pin.go's model-account check;
# the same fixture ARN scripts/kind-sso-walk.sh already uses).
PIN_ACCOUNT="222222222222"
PIN_ROLE="WardynSurvival"
SSO_REGION="us-east-1"
BEDROCK_MODEL="arn:aws:bedrock:${SSO_REGION}:${PIN_ACCOUNT}:inference-profile/us.anthropic.claude-sonnet-4-5-20250929-v1:0"
SSO_START_URL="https://wardyn-survival.awsapps.com/start"
FAKE_CONTAINER="${PROJECT}-awsssofake"
FAKE_URL="http://${FAKE_CONTAINER}:8090" # container DNS on the project's own control-plane network — see this file's header
PG_CONTAINER="${PROJECT}-postgres"
SECRET_NAME="wardyn-harness-aws-oauth" # internal/api/harnesscred.go's harnessCredSecretName(awsSSOProvider), shared scope

EVIDENCE_DIR="${WARDYN_SURVIVAL_EVIDENCE:-${ROOT}/local/evidence/survival-walk}"
mkdir -p "${EVIDENCE_DIR}"
TMPDIR="$(mktemp -d /tmp/wardyn-survival-walk.XXXXXX)"

compose() {
  # WARDYN_SURVIVAL_FAKE_URL and WARDYN_CREDENTIAL_REAUTH_TIMEOUT are read by
  # docker-compose.survival-walk.yaml's own environment: block (see that
  # file's header for why: the base compose file's wardynd service does not
  # reference these at all, so it is the OVERRIDE file's job, not a bare
  # shell export, to get them into the container). WARDYN_BEDROCK_REGION/
  # AWS_SSO_REGION/MODEL, by contrast, the BASE file already forwards.
  COMPOSE_PROJECT_NAME="${PROJECT}" WARDYN_NS="${PROJECT}" \
    WARDYN_UP_PORT="${API_PORT}" WARDYN_PG_PORT="${PG_PORT}" WARDYN_REGISTRY_PORT="${REGISTRY_PORT}" \
    WARDYN_SSH_PORT="${SSH_PORT}" WARDYN_UI_SANDBOX_PORT="${UI_SANDBOX_PORT}" \
    WARDYN_WARDYND_IMAGE="${WARDYND_IMAGE}" WARDYN_PROXY_IMAGE="${PROXY_IMAGE}" \
    WARDYN_AGENT_IMAGES="$(jq -nc --arg cc "${AGENT_IMAGE}" --arg aws "${AWSSSO_AGENT_IMAGE}" \
      '{"claude-code":$cc,"aws-sso":$aws}')" \
    WARDYN_SURVIVAL_FAKE_URL="${FAKE_URL}" \
    WARDYN_BEDROCK_REGION="${SSO_REGION}" \
    WARDYN_BEDROCK_AWS_SSO_REGION="${SSO_REGION}" \
    WARDYN_BEDROCK_MODEL="${BEDROCK_MODEL}" \
    WARDYN_CREDENTIAL_REAUTH_TIMEOUT="${WARDYN_SURVIVAL_REAUTH_TIMEOUT:-30s}" \
    docker compose -p "${PROJECT}" -f "${COMPOSE_FILE}" -f "${OVERRIDE_FILE}" "$@"
}

# api METHOD PATH [JSON-BODY] -> writes the body to $TMPDIR/resp.json, prints
# the HTTP status code on stdout.
api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "${body}" ]]; then
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" -H "Content-Type: application/json" -d "${body}"
  else
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}"
  fi
}

# psql1 SQL -> runs one query against this project's own postgres (psql -tAc,
# so the result is the bare value, no header/padding), on stdout.
psql1() {
  docker exec "${PG_CONTAINER}" psql -U wardyn -d wardyn -tAc "$1" 2>&1
}

# DISPATCHED_RUN_IDS collects every run this script's own try_dispatch
# created (all of them throwaway probes, killed immediately) so teardown can
# sweep any whose own kill call failed, rather than trusting a swallowed
# failure to mean nothing was left running.
DISPATCHED_RUN_IDS=()
LOGIN_RUN_ID=""

teardown() {
  # Saved FIRST, before anything below can remove the container: a run that
  # dies partway through (die() calls exit, which runs this trap) previously
  # lost its wardynd log entirely, because the old save was a step near the
  # END of the successful path only — exactly the run a failure needs the
  # log from. compose logs a container that's still present (running or
  # already exited) either way.
  compose logs wardynd >"${EVIDENCE_DIR}/wardynd.log" 2>&1 || true
  local rid
  for rid in "${DISPATCHED_RUN_IDS[@]:-}" "${LOGIN_RUN_ID}"; do
    [[ -n "${rid}" ]] || continue
    curl -sS --max-time "${CURL_MAX_TIME}" -X POST "${BASE}/api/v1/runs/${rid}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1 || true
  done
  # The fake is attached to ${PROJECT}-internal, so it must be removed BEFORE
  # `compose down` — otherwise compose cannot remove that network ("Resource
  # is still in use") and `|| true` on the down call hides the leak (found
  # live: the network survived every prior version of this teardown).
  docker rm -f "${FAKE_CONTAINER}" >/dev/null 2>&1 || true
  echo "tearing down ${PROJECT} (compose down --volumes; this project only)"
  compose down --volumes >/dev/null 2>&1 || true
  rm -rf "${TMPDIR}"
}
trap teardown EXIT

# Clean any stragglers from a prior aborted run of THIS script — never touches
# a differently-named project. Same ordering as teardown (fake removed first).
docker rm -f "${FAKE_CONTAINER}" >/dev/null 2>&1 || true
compose down --volumes >/dev/null 2>&1 || true

# ── build + bring up the dedicated stack ────────────────────────────────────
step "building wardynd/proxy/agent images (project-unique tags)"
compose build wardynd >"${EVIDENCE_DIR}/build-wardynd.log" 2>&1 || { tail -40 "${EVIDENCE_DIR}/build-wardynd.log" >&2; die "build ${WARDYND_IMAGE} failed"; }
compose --profile build-only build proxy-image >"${EVIDENCE_DIR}/build-proxy.log" 2>&1 || { tail -40 "${EVIDENCE_DIR}/build-proxy.log" >&2; die "build ${PROXY_IMAGE} failed"; }
docker image inspect "${AGENT_IMAGE}" >/dev/null 2>&1 || \
  docker build -f deploy/images/claude-code/Dockerfile -t "${AGENT_IMAGE}" "${ROOT}" >"${EVIDENCE_DIR}/build-agent.log" 2>&1 || \
  { tail -40 "${EVIDENCE_DIR}/build-agent.log" >&2; die "build ${AGENT_IMAGE} failed"; }
docker image inspect "${AWSSSO_AGENT_IMAGE}" >/dev/null 2>&1 || \
  docker build -f deploy/images/aws-sso/Dockerfile -t "${AWSSSO_AGENT_IMAGE}" "${ROOT}" >"${EVIDENCE_DIR}/build-aws-sso.log" 2>&1 || \
  { tail -40 "${EVIDENCE_DIR}/build-aws-sso.log" >&2; die "build ${AWSSSO_AGENT_IMAGE} failed"; }

# A PERSISTENT age key, generated once and exported for every compose call
# below — without it wardynd mints a fresh EPHEMERAL one on every boot
# (deploy/compose/docker-compose.yaml's own WARDYN_AGE_KEY default), and the
# docker-kill/restart this walk exists to prove would then refuse to come
# back at all: the captured AWS SSO blob is a stored secret, sealed under the
# FIRST boot's ephemeral key, unreadable to the second boot's new one
# ("refusing to start: WARDYN_AGE_KEY is unset, but N stored secrets are
# sealed..." — hit live on this script's first end-to-end attempt). An
# operator whose install holds secrets must set a persistent WARDYN_AGE_KEY
# for the same reason: an ephemeral key refuses the restart, full stop,
# whether or not anything in this walk is involved.
step "minting a persistent age key (WARDYN_AGE_KEY) so a restart can still read what was captured"
export WARDYN_AGE_KEY="$(docker run --rm "${WARDYND_IMAGE}" -gen-age-key 2>"${EVIDENCE_DIR}/gen-age-key.log")"
[[ "${WARDYN_AGE_KEY}" == AGE-SECRET-KEY-* ]] || { cat "${EVIDENCE_DIR}/gen-age-key.log" >&2; die "-gen-age-key did not print an AGE-SECRET-KEY-...; needs ${WARDYND_IMAGE} built first"; }
pass "age key minted"

step "bringing up postgres (creates ${PROJECT}-internal, the network wardynd and the fake both join)"
compose up -d postgres || die "compose up (postgres) failed for ${PROJECT}"

# The fake (test/awsssofake/cmd), as a CONTAINER on wardynd's own
# control-plane network — never a host process reached via
# host.docker.internal (see this file's header for the live reachability
# finding that ruled that out). CGO_ENABLED=0 for a static binary portable
# into the small, already-pulled base image below.
#
# STARTS WITH REAUTH DISABLED (AWSSSOFAKE_REAUTH_AFTER=0): every refresh
# succeeds until this walk explicitly arms it (POST /_control/reauth?after=N,
# below). A fixed AWSSSOFAKE_REAUTH_AFTER=1 from container start was tried
# first and reds a dispatch-time refusal before this walk was ready for one.
#
# TOKEN_TTL is short (well under injectRefreshMargin) and left short even
# with reauth disabled: with it disabled, an early refresh just succeeds
# transparently (a harmless token rotation, exercised elsewhere already), so
# a short TTL costs nothing and means the credential is already past its
# margin from the moment it is captured — wardynd's OWN CREATE-RUN-time
# refreshAWSSSOBlob call (this walk's actual trigger; see "WHAT THIS WALK
# PROVES" below) attempts a real refresh on the very first dispatch after
# arming, rather than depending on a wall-clock window. ROLE_CRED_TTL is not
# load-bearing for this walk's own mechanism (no sandbox ever calls
# GetRoleCredentials here) — left short anyway since it costs nothing.
step "building and starting the fake AWS SSO + Bedrock endpoint (container ${FAKE_CONTAINER})"
CGO_ENABLED=0 go build -o "${TMPDIR}/awsssofake" ./test/awsssofake/cmd || die "build awsssofake failed"
docker run -d --name "${FAKE_CONTAINER}" --network "${PROJECT}-internal" \
  -p "127.0.0.1:${FAKE_PORT}:8090" \
  -v "${TMPDIR}/awsssofake:/awsssofake:ro" \
  -e "AWSSSOFAKE_ADDR=0.0.0.0:8090" \
  -e "AWSSSOFAKE_ACCOUNTS=$(jq -nc --arg a "${PIN_ACCOUNT}" --arg r "${PIN_ROLE}" '[{account_id:$a,roles:[$r]}]')" \
  -e "AWSSSOFAKE_TOKEN_TTL=${WARDYN_SURVIVAL_TOKEN_TTL:-60s}" \
  -e "AWSSSOFAKE_ROLE_CRED_TTL=${WARDYN_SURVIVAL_ROLE_CRED_TTL:-20s}" \
  -e "AWSSSOFAKE_REAUTH_AFTER=0" \
  alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc /awsssofake >"${EVIDENCE_DIR}/awsssofake-container.log" 2>&1 \
  || { cat "${EVIDENCE_DIR}/awsssofake-container.log" >&2; die "starting ${FAKE_CONTAINER} failed"; }
for _ in $(seq 1 30); do
  curl -sf --max-time "${CURL_MAX_TIME}" "http://127.0.0.1:${FAKE_PORT}/_seen" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf --max-time "${CURL_MAX_TIME}" "http://127.0.0.1:${FAKE_PORT}/_seen" >/dev/null 2>&1 \
  || { docker logs "${FAKE_CONTAINER}" >&2; die "the fake never answered on 127.0.0.1:${FAKE_PORT}"; }
pass "fake AWS SSO/Bedrock endpoint up (${FAKE_CONTAINER}, on ${PROJECT}-internal)"

step "bringing up wardynd (api :${API_PORT})"
compose up -d wardynd || die "compose up (wardynd) failed for ${PROJECT}"
healthy=""
for _ in $(seq 1 60); do
  curl -sf --max-time "${CURL_MAX_TIME}" "${BASE}/healthz" >/dev/null 2>&1 && { healthy=1; break; }
  sleep 2
done
[[ -n "${healthy}" ]] || { compose logs wardynd | tail -80; die "wardynd did not become healthy"; }
pass "stack up and healthy (${BASE})"

# ── the preconditions ────────────────────────────────────────────────────────
step "declaring the org agent standard: claude-code on bedrock_sso, shared credential"
code=$(api PUT /api/v1/site-config '{"agent_providers":{"agents":[{"id":"claude-code","mechanism":"bedrock_sso","credential_source":"shared"}]}}')
[[ "${code}" == "200" ]] || { cat "${TMPDIR}/resp.json" >&2; die "PUT /site-config (agent_providers) answered ${code}"; }
pass "agent standard declared"

# fake_device_login -> prints a fresh awsSSOBlob (JSON, matching
# internal/api/harnesscred.go's struct) on stdout, from a REAL RegisterClient +
# StartDeviceAuthorization + CreateToken(device_code) round trip against the
# fake, run directly from the host (see this file's header for why the
# sandbox never does this dance itself). Every caller invokes this via a
# command substitution (`blob="$(fake_device_login)"`), which forks a
# subshell — so a plain assignment made IN HERE would never reach the
# caller, only what this function prints to stdout does (caught live: an
# earlier version tried exactly that, for a client-side refresh-token
# fingerprint this walk no longer needs — see the Postgres assertions below
# for why a plain existence count replaced it).
fake_device_login() {
  local reg cid csec da dc tok access refresh expires now
  reg="$(curl -sS --max-time "${CURL_MAX_TIME}" -X POST "http://127.0.0.1:${FAKE_PORT}/client/register" \
    -H 'Content-Type: application/json' -d '{"clientName":"survival-walk","clientType":"public"}')"
  cid="$(jq -r '.clientId' <<<"${reg}")"; csec="$(jq -r '.clientSecret' <<<"${reg}")"
  [[ -n "${cid}" && "${cid}" != "null" ]] || { echo "fake_device_login: RegisterClient failed: ${reg}" >&2; return 1; }
  da="$(curl -sS --max-time "${CURL_MAX_TIME}" -X POST "http://127.0.0.1:${FAKE_PORT}/device_authorization" \
    -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg c "${cid}" --arg s "${csec}" --arg u "${SSO_START_URL}" '{clientId:$c,clientSecret:$s,startUrl:$u}')")"
  dc="$(jq -r '.deviceCode' <<<"${da}")"
  [[ -n "${dc}" && "${dc}" != "null" ]] || { echo "fake_device_login: StartDeviceAuthorization failed: ${da}" >&2; return 1; }
  tok="$(curl -sS --max-time "${CURL_MAX_TIME}" -X POST "http://127.0.0.1:${FAKE_PORT}/token" \
    -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg c "${cid}" --arg s "${csec}" --arg d "${dc}" \
      '{clientId:$c,clientSecret:$s,grantType:"urn:ietf:params:oauth:grant-type:device_code",deviceCode:$d}')")"
  access="$(jq -r '.accessToken' <<<"${tok}")"; refresh="$(jq -r '.refreshToken' <<<"${tok}")"
  [[ -n "${access}" && "${access}" != "null" ]] || { echo "fake_device_login: CreateToken(device_code) failed: ${tok}" >&2; return 1; }
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  expires="$(date -u -d "+$(jq -r '.expiresIn' <<<"${tok}") seconds" +%Y-%m-%dT%H:%M:%SZ)"
  jq -nc --arg at "${access}" --arg rt "${refresh}" --arg cid "${cid}" --arg cs "${csec}" \
    --arg u "${SSO_START_URL}" --arg r "${SSO_REGION}" --arg a "${PIN_ACCOUNT}" --arg role "${PIN_ROLE}" \
    --arg exp "${expires}" --arg now "${now}" \
    '{access_token:$at,refresh_token:$rt,client_id:$cid,client_secret:$cs,start_url:$u,region:$r,
      account_id:$a,role_name:$role,expires_at:$exp,captured_at:$now}'
}

# capture_aws_credential: launch a login run (POST /setup/harness-login, for
# a real launch-time stamp), then upload a freshly-minted blob into ITS OWN
# sandbox via the same brokered route cmd/wardyn-aws-sso uses
# (PUT ${WARDYN_PROXY_URL}/wardyn/v1/sso-token/${WARDYN_RUN_ID}, read out of
# the container's own env — never a bearer token this script holds). Sets
# LOGIN_RUN_ID (teardown's own safety net). Called again by the resolve step
# once the credential is spent.
capture_aws_credential() {
  code=$(api POST /api/v1/setup/harness-login "$(jq -nc --arg u "${SSO_START_URL}" '{provider:"aws",sso_start_url:$u}')")
  [[ "${code}" == "200" || "${code}" == "201" ]] || { cat "${TMPDIR}/resp.json" >&2; die "POST /setup/harness-login answered ${code}"; }
  LOGIN_RUN_ID="$(jq -r '.run_id // .id' "${TMPDIR}/resp.json")"
  [[ -n "${LOGIN_RUN_ID}" && "${LOGIN_RUN_ID}" != "null" ]] || { cat "${TMPDIR}/resp.json" >&2; die "harness-login: no run id in response"; }
  local sandbox="wardyn-agent-${LOGIN_RUN_ID}" up=""
  for _ in $(seq 1 30); do
    [[ "$(docker inspect -f '{{.State.Running}}' "${sandbox}" 2>/dev/null)" == "true" ]] && { up=1; break; }
    sleep 1
  done
  [[ -n "${up}" ]] || die "login sandbox ${sandbox} never started"
  local blob; blob="$(fake_device_login)" || die "fake_device_login failed (see ${EVIDENCE_DIR}/awsssofake.log)"
  local http_code
  http_code="$(docker exec "${sandbox}" sh -c "curl -sS --max-time ${CURL_MAX_TIME} -o /dev/null -w '%{http_code}' -X PUT \"\${WARDYN_PROXY_URL}/wardyn/v1/sso-token/\${WARDYN_RUN_ID}\" -H 'Content-Type: application/json' -d '${blob}'" 2>&1)"
  [[ "${http_code}" == "204" ]] || die "sso-token upload into ${sandbox} answered ${http_code}"
  local live=""
  for _ in $(seq 1 30); do
    code=$(api GET /api/v1/setup/status)
    [[ "${code}" == "200" ]] || { sleep 2; continue; }
    [[ "$(jq -r '.model_access.state // empty' "${TMPDIR}/resp.json")" == "live" ]] && { live=1; break; }
    sleep 2
  done
  # The login sandbox is an INTERACTIVE run that stays open (idle, for
  # attach) until its own auto-stop ceiling — it does not exit on its own
  # once the capture lands. Left running, its proxy sidecar stays attached to
  # ${PROJECT}-internal (blocking that network's removal at teardown) and
  # keeps making its own periodic calls back to wardynd for the rest of the
  # walk — a real bug THIS SCRIPT introduced, caught live: those calls showed
  # up as repeating "remote error: tls: bad certificate" lines in wardynd's
  # log once this walk moved the fake off host.docker.internal, easily
  # misread as a product-side mTLS/CA problem. Kill it now that the capture
  # it existed for is done.
  curl -sS --max-time "${CURL_MAX_TIME}" -X POST "${BASE}/api/v1/runs/${LOGIN_RUN_ID}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1
  local kill_rc=$?
  [[ "${kill_rc}" -eq 0 ]] || echo "note: killing login run ${LOGIN_RUN_ID} failed (rc=${kill_rc}); teardown will retry it" >&2
  [[ -n "${live}" ]]
}

step "capturing the initial AWS SSO session"
capture_aws_credential || die "the initial capture never reached model_access.state=live (see ${EVIDENCE_DIR}/awsssofake.log)"
pass "initial capture landed (model_access.state=live, via login run ${LOGIN_RUN_ID})"

# ── WHAT THIS WALK PROVES, AND WHAT IT DELIBERATELY DOES NOT ────────────────
#
# It does NOT hold a live in-sandbox Bedrock request. Two earlier attempts
# tried to get a real, dispatched sandbox to make the model call that would
# raise credential_reauth via the live per-request injection endpoint
# (internal/api/injection_awssso.go's resolveAWSSSOInjection) — the same path
# the kind SSO walk already proves against a real cluster. Both were blocked
# by the SAME proxy invariant, live-reproduced twice with two different
# destination strategies:
#
#   {"request":{"Host":"cl-700-survival-awsssofake","Port":8090,"Method":"CONNECT",...},
#    "decision":"deny","rule_source":"builtin:private-ip"}
#
# internal/egress/proxy/egress_target.go's liftInternalHost:
#
#   func (p *Proxy) liftInternalHost(host string, ip net.IP) bool {
#     if p.onOwnSubnetOrControlPlane(ip) { return false }
#     ...
#
# is an UNCONDITIONAL early return — no internal_hosts declaration can lift an
# address on the proxy's own attached subnets, by design ("never lift your
# own subnet"). The docker runner attaches every per-run proxy to exactly two
# networks (its own per-run network, and the control-plane network) and
# nothing in a compose script can add a third; kind's own walk sidesteps this
# entirely because a Kubernetes Service ClusterIP is a genuinely separate,
# non-attached virtual address space with no compose equivalent. So: in
# plain docker-compose, ANY fake this script can make reachable from the
# SANDBOX side sits on one of the proxy's own subnets and is refused before
# any credential logic ever runs — the live in-sandbox hold is provably
# untestable here without either patching wardynd's runner or letting the
# sandbox dial a REAL AWS hostname (which this walk must never do).
#
# What it proves INSTEAD: this walk declares `agent_providers` (never
# `model_providers`), so the credential check CREATE RUN actually runs is
# internal/api/runs.go:264's `s.enforceCreateLLMMechanism(ctx, w, req, spec,
# bedrockRef, ssoSubject, &modelCred, true)` — the trailing `true` is
# `refresh`, meaning this call may redeem and rotate the captured SSO session,
# not merely read its cached state (runs_dispatch_llm_mechanism.go's own doc
# comment on resolveRunLLMLanes). That function calls resolveRunLLMLanes
# (runs_dispatch_llm_mechanism.go:439), which calls resolveBedrockAuth
# (runs_bedrock.go), which — when a captured SSO credential is on file — calls
# refreshAWSSSOBlob (internal/api/awssso_refresh.go) with refresh=true. This
# is wardynd's OWN direct outbound call, never proxy/sandbox-gated, and
# reliable here (container-to-container to the fake).
#
# On a refresh failure the SAME code path does two things worth reading back
# directly rather than inferring from a dispatch refusal alone
# (harnesscred.go:417 deleteSpentAWSSSOBlob, awssso_refresh.go:399
# markAWSSSOTokenSpent):
#   - the stored credential (Postgres `secrets` row named
#     "wardyn-harness-aws-oauth" for this shared-scope walk) is DELETED —
#     from then on every dispatch refuses as "no credential", not literally
#     "spent"; and
#   - the refresh token's fingerprint (8 bytes of its SHA-256, hex — the SAME
#     one-way truncated fingerprint this script computes below) is written to
#     the `aws_sso_spent_tokens` table (migration 0068), so a refresh token
#     AWS already retired is never treated as renewable again even after a
#     restart wipes wardynd's in-memory state.
#
# This walk arms the fake, watches CREATE RUN's own refresh attempt correctly
# refuse a new dispatch once the credential is spent, DISARMS THE FAKE (so
# only Postgres, not a still-armed fake, can explain what happens next),
# kills and restarts wardynd, and reads BOTH rows back directly from Postgres
# to confirm the delete and the spent-token mark actually persisted — then
# resolves with a fresh capture and confirms dispatch succeeds again. This is
# a real, live daemon-restart survival proof of wardynd's own
# credential-freshness state machine; it is just not the in-flight-request
# shape the kind walk already covers.
RUN_BODY='{"agent":"claude-code","repo":"local:survival","interactive":true,
  "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny",
  "min_confinement_class":"CC1","auto_stop_after_sec":-1}}'

# try_dispatch -> sets DISPATCH_OUTCOME to "live" (credential accepted; a real
# run was created, appended to DISPATCHED_RUN_IDS and killed immediately —
# this walk never needs it to do anything) or "dead" (create-run refused,
# reason=model_credential — wardynd's OWN CREATE-RUN-time refreshAWSSSOBlob
# call found the credential spent, or absent). Anything else (a different
# refusal, a different reason) is treated as a hard failure: this walk must
# not read an unrelated error as proof of anything.
try_dispatch() {
  code=$(api POST /api/v1/runs "${RUN_BODY}")
  if [[ "${code}" == "200" || "${code}" == "201" ]]; then
    local rid; rid="$(jq -r '.id' "${TMPDIR}/resp.json")"
    DISPATCHED_RUN_IDS+=("${rid}")
    curl -sS --max-time "${CURL_MAX_TIME}" -X POST "${BASE}/api/v1/runs/${rid}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1
    [[ $? -eq 0 ]] || echo "note: killing probe run ${rid} failed; teardown will retry it" >&2
    DISPATCH_OUTCOME="live"
    return 0
  fi
  local reason; reason="$(jq -r '.reason // empty' "${TMPDIR}/resp.json" 2>/dev/null)"
  if [[ "${code}" == "422" && "${reason}" == "model_credential" ]]; then
    DISPATCH_OUTCOME="dead"
    return 0
  fi
  cat "${TMPDIR}/resp.json" >&2
  die "create run answered ${code} (reason=${reason:-<none>}) — not the model_credential refusal this walk expects; see ${EVIDENCE_DIR}"
}

step "confirming the credential is accepted (a real dispatch succeeds before arming)"
try_dispatch
[[ "${DISPATCH_OUTCOME}" == "live" ]] || die "dispatch was refused before the fake was ever armed — something upstream is already wrong"
pass "credential accepted; a normal dispatch works"

step "arming the fake (POST /_control/reauth?after=1) — the NEXT refresh now answers invalid_grant"
arm_code="$(curl -sS --max-time "${CURL_MAX_TIME}" -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${FAKE_PORT}/_control/reauth?after=1")"
[[ "${arm_code}" == "200" ]] || die "POST /_control/reauth answered ${arm_code}"
pass "armed"

# wait_dispatch_dead MAX_TRIES -> 0 once try_dispatch reports "dead", else 1.
# wardynd's own CREATE-RUN-time check calls refreshAWSSSOBlob every time, so
# no forced sandbox action is needed here — only a poll for the moment the
# credential (already armed) actually needs a refresh and gets invalid_grant.
wait_dispatch_dead() {
  local tries="$1"
  for _ in $(seq 1 "${tries}"); do
    try_dispatch
    [[ "${DISPATCH_OUTCOME}" == "dead" ]] && return 0
    sleep 2
  done
  return 1
}

step "waiting for wardynd's own refresh check to find the (armed) credential spent"
wait_dispatch_dead 150 || {
  compose logs wardynd >"${EVIDENCE_DIR}/wardynd-diag.log" 2>&1
  tail -80 "${EVIDENCE_DIR}/wardynd-diag.log" >&2
  die "dispatch never started refusing with reason=model_credential — see ${EVIDENCE_DIR}/wardynd-diag.log"
}
pass "wardynd's own refresh check now refuses new dispatches — the credential is spent"

# NOT keying the Postgres read below on a client-computed fingerprint,
# DELIBERATELY: wardynd's own aws_sso_spent_tokens row is keyed by
# awsSSOTokenFingerprint(refreshToken) (awssso_refresh.go — 8 bytes of the
# refresh token's SHA-256, hex), but the ACTUAL refresh token spent is not
# necessarily this walk's own captured one. With TOKEN_TTL this short and
# reauth disabled at container start (see the fake-startup comment above),
# an EARLIER dispatch (this script's own "confirm the credential is
# accepted" probe, before arming) can already have triggered a real,
# transparent refresh — a harmless rotation the fake still answers
# successfully — silently swapping in a NEW refresh token before the fake
# was ever armed. A fingerprint computed from the ORIGINAL captured token
# then no longer matches whichever token actually got marked spent, and a
# real GREEN run was seen to die here for exactly that reason (evidence:
# EVIDENCE_DIR's wardynd.log, moved into the EXIT trap below so a run that
# dies keeps it). Since every run uses a FRESH, dedicated Postgres volume
# (this walk's own project, --volumes torn down and recreated each time),
# ANY row in aws_sso_spent_tokens can only be the one this walk's own spend
# wrote — a plain existence count is exact here without needing to predict
# which token generation it belongs to.

# DISARM BEFORE THE KILL, not after the restart check: left armed, a
# restarted wardynd that forgot everything (never persisted the delete or
# the spent mark) would simply ask the fake again and be refused again —
# the post-restart check would pass for the WRONG reason, proving nothing
# about persistence. Disarming here means only Postgres, never the fake,
# can explain a refusal after the restart.
step "disarming the fake (POST /_control/reauth?after=0) so only Postgres can explain what happens after the restart"
disarm_code="$(curl -sS --max-time "${CURL_MAX_TIME}" -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${FAKE_PORT}/_control/reauth?after=0")"
[[ "${disarm_code}" == "200" ]] || die "POST /_control/reauth?after=0 answered ${disarm_code}"
pass "disarmed"

step "confirming Postgres actually holds the spent state before the kill (not just wardynd's memory)"
secret_count="$(psql1 "SELECT count(*) FROM secrets WHERE name = '${SECRET_NAME}'")"
spent_count="$(psql1 "SELECT count(*) FROM aws_sso_spent_tokens")"
[[ "${secret_count}" == "0" ]] || die "expected the spent credential's secret row to be deleted; count=${secret_count}"
[[ "${spent_count}" == "1" ]] || die "expected exactly one aws_sso_spent_tokens row (this project's own fresh volume); count=${spent_count}"
pass "Postgres shows the secret deleted (count=0) and the spent-token row present (count=1) — before the kill"

# ── THE WALK: kill wardynd while the credential is spent, restart, verify ───
step "docker kill ${PROJECT}-api (wardynd) while the credential is spent"
docker kill "${PROJECT}-api" >/dev/null || die "docker kill ${PROJECT}-api failed"
sleep 3
[[ "$(docker inspect -f '{{.State.Running}}' "${PROJECT}-api" 2>/dev/null)" == "false" ]] \
  || die "${PROJECT}-api is still running after docker kill"
pass "${PROJECT}-api is dead"

step "restarting wardynd (compose up -d wardynd)"
compose up -d wardynd || die "compose up -d wardynd (restart) failed"
healthy=""
for _ in $(seq 1 60); do
  curl -sf --max-time "${CURL_MAX_TIME}" "${BASE}/healthz" >/dev/null 2>&1 && { healthy=1; break; }
  sleep 2
done
[[ -n "${healthy}" ]] || { compose logs wardynd | tail -80; die "wardynd did not come back healthy after restart"; }
pass "wardynd back up"

step "confirming the spent state survived the restart — read directly from Postgres, the fake is disarmed"
secret_count="$(psql1 "SELECT count(*) FROM secrets WHERE name = '${SECRET_NAME}'")"
spent_count="$(psql1 "SELECT count(*) FROM aws_sso_spent_tokens")"
[[ "${secret_count}" == "0" ]] || die "after restart, the deleted secret is BACK (count=${secret_count}) — the delete did not persist"
[[ "${spent_count}" == "1" ]] || die "after restart, the spent-token row is GONE (count=${spent_count}) — the mark did not persist"
pass "still deleted/spent after restart in Postgres itself — this is the persisted fact, not the fake's own memory"

step "confirming dispatch still refuses too (corroborating, with the fake disarmed)"
try_dispatch
[[ "${DISPATCH_OUTCOME}" == "dead" ]] || die "after restart, dispatch unexpectedly succeeded even though Postgres still shows the credential gone"
pass "dispatch still refuses, consistent with what Postgres holds"

step "signing in again (fresh capture) to clear the spent state"
capture_aws_credential || die "the post-restart capture never reached model_access.state=live"
pass "fresh capture landed"

step "confirming dispatch succeeds again after the resolve"
try_dispatch
if [[ "${DISPATCH_OUTCOME}" == "live" ]]; then
  pass "credential resolved — a fresh dispatch succeeds again"
else
  fail "expected dispatch to succeed again after the fresh capture; still ${DISPATCH_OUTCOME}"
fi

step "evidence (including wardynd.log, saved by teardown on every exit path) is in ${EVIDENCE_DIR}"

if [[ "${FAILED}" -ne 0 ]]; then
  echo "survival-walk: FAILED — see ${EVIDENCE_DIR}" >&2
  exit 1
fi
echo "survival-walk: PASS — evidence in ${EVIDENCE_DIR}"
