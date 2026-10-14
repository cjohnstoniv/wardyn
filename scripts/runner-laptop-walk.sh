#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Laptop-runner walk: a real wardyn-runnerd on Docker Desktop (in WSL2 here,
# the VM-backed engine an adopter's laptop has) against a real control plane
# built from this tree. It proves, in order:
#   1. a member without the self_host_runner grant is refused 403 on self-mint;
#      after an admin allow for `all` the mint succeeds and carries proxy_image;
#   2. `register` + `wardyn runner claim --pool Laptop --default` with the
#      member's own token registers, claims, makes the pool and its default;
#   3. `serve --relay-host-target 192.0.2.1` (TEST-NET-1, never routed) exits
#      non-zero before it dials, naming the target (the runner is never seen);
#   4. `serve` with no relay flags goes online (Docker Desktop is classified
#      and probed, so it needs none), and a local exec run on it completes; its
#      proxy's calls to the org arrive over the relay (audit data.relay =
#      runner:<id>) and its egress decisions over the runner's session (actor
#      runner:<id>, data.spooled);
#   4b. in that run the agent is denied host.docker.internal:8443 and the relay
#      target's IP: the relay is the proxy's route to the org, never the agent's;
#   5. GET /runs/{id} and the run.create audit row carry placement local,
#      runner_name and runner_pool_name;
#   7. the member's bedrock_sso agent run, against test/awsssofake: with AWS
#      SSO proxy-inject on it is admitted local and starts, the sandbox holds
#      only the placeholder, and the fake's portal accepts the bearer the proxy
#      injected (the model call then fails on the fake's canned Bedrock body,
#      which is not a model reply); with proxy-inject off, preflight refuses the
#      same local run 422 placement_credential before any run exists;
#   6. (last) with serve stopped, the owner's revoke answers 200 state revoked,
#      and GET /me/runner-pools answers 200 with Laptop.
#
# WHY NOT `setup`: it installs and starts a systemd user service and has no
# way to skip that. This walk runs the three steps setup chains (register,
# claim, serve) itself and keeps serve in the foreground under its own control.
#
# TOPOLOGY. The org is a dedicated compose project on a SECOND, native engine
# (WARDYN_LAPTOP_WALK_ORG_DOCKER), published only on that engine's bridge
# gateway, a non-loopback address of this host, with built-in TLS from a CA
# this walk mints (SSL_CERT_FILE/CURL_CA_BUNDLE trust it and nothing else). The
# org's WARDYN_CONTROL_PLANE_URL stays compose's https://wardynd:8443, so the
# relay binds 127.0.0.1:8443 here, which nothing else may hold. The run's proxy
# reaches the org only through that relay.
#
# GUARD: self-skips (77) unless WARDYN_TEST_DOCKER=1, Docker Desktop answers at
# WARDYN_LAPTOP_WALK_RUNNER_DOCKER and a second engine at
# WARDYN_LAPTOP_WALK_ORG_DOCKER. Removes everything it creates on both engines.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

[[ "${WARDYN_TEST_DOCKER:-}" == "1" ]] || skip_lane "runner-laptop-walk: set WARDYN_TEST_DOCKER=1 to run it (skipping)."
for bin in docker curl jq go openssl ss; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

RUNNER_DOCKER="${WARDYN_LAPTOP_WALK_RUNNER_DOCKER:-unix:///var/run/docker.sock}"
ORG_DOCKER="${WARDYN_LAPTOP_WALK_ORG_DOCKER:-unix:///var/run/wardyn-docker.sock}"
rd() { DOCKER_HOST="${RUNNER_DOCKER}" docker "$@"; }
od() { DOCKER_HOST="${ORG_DOCKER}" docker "$@"; }
[[ "$(rd info --format '{{.OperatingSystem}}' 2>/dev/null)" == "Docker Desktop" ]] \
  || skip_lane "runner-laptop-walk: no Docker Desktop at ${RUNNER_DOCKER} (skipping)."
od info >/dev/null 2>&1 || skip_lane "runner-laptop-walk: no org engine at ${ORG_DOCKER} (skipping)."

PROJECT="${WARDYN_LAPTOP_WALK_PROJECT:-wardyn-laptop-walk}"
ORG_HOST="$(od network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
ORG_PORT="${WARDYN_LAPTOP_WALK_ORG_PORT:-18543}"
ORG_URL="https://${ORG_HOST}:${ORG_PORT}"
RELAY_PORT=8443 # the port of compose's WARDYN_CONTROL_PLANE_URL
[[ -n "${ORG_HOST}" && "${ORG_HOST}" != 127.* ]] || die "the org engine's bridge gateway '${ORG_HOST}' is not a non-loopback address"
if ss -Hltn "sport = :${RELAY_PORT}" | grep -q .; then die "something already listens on :${RELAY_PORT}, the relay's port"; fi

ADMIN_TOKEN="demo-admin-token" # compose's own default
MEMBER="laptop-walk-member"
MEMBER_TOKEN="wdn_$(openssl rand -hex 32)"
RUNNER_NAME="laptop-walk"
RUNNER_NET="${PROJECT}-internal" # Docker Desktop may already hold another stack's wardyn-internal
WARDYND_IMAGE="wardyn/wardynd:${PROJECT}"
PROXY_IMAGE="wardyn/wardyn-proxy:${PROJECT}"
AGENT_IMAGE="${PROJECT}/agent-claude-code:pinned"
AWSSSO_IMAGE="${PROJECT}/agent-aws-sso:pinned"
# The AWS SSO fake for step 7, see "STEP 7" below for where it listens and why.
FAKE_CONTAINER="${PROJECT}-awsssofake"
FAKE_URL="http://172.17.0.1:${WARDYN_LAPTOP_WALK_FAKE_PORT:-18391}"
PIN_ACCOUNT="222222222222" PIN_ROLE="WardynLaptopWalk" SSO_REGION="us-east-1"
SSO_START_URL="https://wardyn-laptop-walk.awsapps.com/start"
BEDROCK_MODEL="arn:aws:bedrock:${SSO_REGION}:${PIN_ACCOUNT}:inference-profile/us.anthropic.claude-sonnet-4-5-20250929-v1:0"
PROVIDER_ID="laptop-walk-bedrock"
PG_CONTAINER="${PROJECT}-postgres"
EVIDENCE_DIR="${WARDYN_LAPTOP_WALK_EVIDENCE:-${ROOT}/local/evidence/runner-laptop-walk}"
mkdir -p "${EVIDENCE_DIR}"
TMP="$(mktemp -d /tmp/wardyn-laptop-walk.XXXXXX)"
STATE="${TMP}/runner-state"
export SSL_CERT_FILE="${TMP}/tls/ca.crt" CURL_CA_BUNDLE="${TMP}/tls/ca.crt"
SERVE_PID="" RID="" RUN_ID="" SSO_RUN_ID="" LOGIN_RUN_ID=""
FAILED=0
pass() { printf '  [pass] %s\n' "$*"; }
fail() { printf '  [FAIL] %s\n' "$*"; FAILED=1; }

compose() {
  DOCKER_HOST="${ORG_DOCKER}" COMPOSE_PROJECT_NAME="${PROJECT}" WARDYN_NS="${PROJECT}" \
    WARDYN_UP_PORT=0 WARDYN_PG_PORT=0 WARDYN_REGISTRY_PORT=0 WARDYN_SSH_PORT=0 WARDYN_UI_SANDBOX_PORT=0 \
    WARDYN_DOCKER_SOCK="${ORG_DOCKER#unix://}" WARDYN_WARDYND_IMAGE="${WARDYND_IMAGE}" WARDYN_PROXY_IMAGE="${PROXY_IMAGE}" \
    WARDYN_AGENT_IMAGES="$(jq -nc --arg cc "${AGENT_IMAGE}" --arg aws "${AWSSSO_IMAGE}" '{"claude-code":$cc,"aws-sso":$aws}')" \
    docker compose -p "${PROJECT}" -f deploy/compose/docker-compose.yaml -f "${TMP}/override.yaml" "$@"
}

# api TOKEN METHOD PATH [BODY] -> body in $TMP/resp.json, status on stdout.
api() {
  local args=(-sS --max-time 15 -o "${TMP}/resp.json" -w '%{http_code}' -X "$2" "${ORG_URL}$3" -H "Authorization: Bearer $1")
  [[ -n "${4:-}" ]] && args+=(-H "Content-Type: application/json" -d "$4")
  curl "${args[@]}"
}
psql1() { od exec "${PG_CONTAINER}" psql -U wardyn -d wardyn -tAc "$1"; }
w() { WARDYN_URL="${ORG_URL}" WARDYN_TOKEN="${MEMBER_TOKEN}" "${TMP}/wardyn" "$@"; }
runnerd() { DOCKER_HOST="${RUNNER_DOCKER}" "${TMP}/wardyn-runnerd" "$@"; }

stop_serve() {
  [[ -n "${SERVE_PID}" ]] || return 0
  kill -TERM "${SERVE_PID}" 2>/dev/null
  wait "${SERVE_PID}"
  echo "serve stopped (exit $?)"
  SERVE_PID=""
}

cleanup() {
  stop_serve
  [[ -f "${TMP}/override.yaml" ]] && compose logs wardynd >"${EVIDENCE_DIR}/wardynd.log" 2>&1
  local label
  [[ -n "${LOGIN_RUN_ID}" ]] && api "${MEMBER_TOKEN}" POST "/api/v1/runs/${LOGIN_RUN_ID}/kill" >/dev/null 2>&1
  rd rm -f "${FAKE_CONTAINER}" >/dev/null 2>&1
  for label in ${RUN_ID:+wardyn.run-id=${RUN_ID}} ${SSO_RUN_ID:+wardyn.run-id=${SSO_RUN_ID}} ${RID:+wardyn.runner-id=${RID}}; do
    rd ps -aq --filter "label=${label}" | xargs -r docker -H "${RUNNER_DOCKER}" rm -f >/dev/null 2>&1
    rd volume ls -q --filter "label=${label}" | xargs -r docker -H "${RUNNER_DOCKER}" volume rm >/dev/null 2>&1
  done
  [[ -n "${RUN_ID}" ]] && rd network rm "wardyn-int-${RUN_ID}" >/dev/null 2>&1
  [[ -n "${SSO_RUN_ID}" ]] && rd network rm "wardyn-int-${SSO_RUN_ID}" >/dev/null 2>&1
  rd network rm "${RUNNER_NET}" >/dev/null 2>&1
  [[ -f "${TMP}/override.yaml" ]] && compose down --volumes >/dev/null 2>&1
  if [[ "${WARDYN_LAPTOP_WALK_KEEP_IMAGES:-}" != "1" ]]; then
    rd rmi "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${AGENT_IMAGE}" "${AWSSSO_IMAGE}" >/dev/null 2>&1
    od rmi "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${AWSSSO_IMAGE}" >/dev/null 2>&1
  fi
  rm -rf "${TMP}"
  echo "cleaned up ${PROJECT} on both engines"
}
trap cleanup EXIT

log "building the CLI, wardyn-runnerd and the images from this tree"
go build -o "${TMP}/wardyn" ./cmd/wardyn || die "go build wardyn failed"
CGO_ENABLED=0 go build -tags docker -o "${TMP}/wardyn-runnerd" ./cmd/wardyn-runnerd || die "go build wardyn-runnerd failed"
rd build -q -f deploy/compose/Dockerfile.wardynd -t "${WARDYND_IMAGE}" . >"${EVIDENCE_DIR}/build-wardynd.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/build-wardynd.log"; die "wardynd image build failed"; }
rd build -q -f deploy/compose/Dockerfile.proxy -t "${PROXY_IMAGE}" . >"${EVIDENCE_DIR}/build-proxy.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/build-proxy.log"; die "proxy image build failed"; }
rd build -q -f deploy/images/claude-code/Dockerfile -t "${AGENT_IMAGE}" . >"${EVIDENCE_DIR}/build-agent.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/build-agent.log"; die "agent image build failed"; }
rd build -q -f deploy/images/aws-sso/Dockerfile -t "${AWSSSO_IMAGE}" . >"${EVIDENCE_DIR}/build-aws-sso.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/build-aws-sso.log"; die "aws-sso image build failed"; }
rd save "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${AWSSSO_IMAGE}" | od load -q || die "loading the org images into ${ORG_DOCKER} failed"
CGO_ENABLED=0 go build -o "${TMP}/awsssofake" ./test/awsssofake/cmd || die "go build awsssofake failed"
# Persistent, so the restart in step 7b can still read the captured session.
WARDYN_AGE_KEY="$(od run --rm "${WARDYND_IMAGE}" -gen-age-key 2>/dev/null)"
[[ "${WARDYN_AGE_KEY}" == AGE-SECRET-KEY-* ]] || die "-gen-age-key printed no key"
export WARDYN_AGE_KEY

log "minting the walk CA and the org's certificate for ${ORG_HOST}"
mkdir -p "${TMP}/tls" && cd "${TMP}/tls" || die "no tls dir"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=laptop-walk CA" \
  -keyout ca.key -out ca.crt 2>/dev/null || die "CA mint failed"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=${ORG_HOST}" -keyout tls.key -out tls.csr 2>/dev/null || die "CSR failed"
printf 'subjectAltName=IP:%s,DNS:localhost\n' "${ORG_HOST}" >san.ext
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 -extfile san.ext -out tls.crt 2>/dev/null || die "leaf mint failed"
chmod 600 tls.key && cd "${ROOT}" || die "cd back failed"
cat >"${TMP}/override.yaml" <<EOF
services:
  wardynd:
    ports: ["${ORG_HOST}:${ORG_PORT}:8080"]
    environment:
      WARDYN_TLS_CERT: /walk-tls/tls.crt
      WARDYN_TLS_KEY: /walk-tls/tls.key
      WARDYN_RUNNER_ORG_URL: "${ORG_URL}"
      WARDYN_ALLOW_TEST_ENDPOINTS: "true"
      WARDYN_AWS_SSO_ENDPOINT_OVERRIDE: "${FAKE_URL}"
    volumes: ["${TMP}/tls:/walk-tls:ro"]
    healthcheck: {disable: true}
EOF

log "bringing up the org (${PROJECT} on ${ORG_DOCKER}) at ${ORG_URL}"
compose up -d wardynd >"${EVIDENCE_DIR}/compose-up.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/compose-up.log"; die "compose up failed"; }
wait_healthy "${ORG_URL}" 90 2 || die "the org never answered ${ORG_URL}/healthz"
pass "org healthy over TLS at ${ORG_URL}"
api "${ADMIN_TOKEN}" POST /api/v1/setup/onboarding-complete >/dev/null
[[ "$(api "${ADMIN_TOKEN}" PUT /api/v1/runners/settings '{"enabled":true}')" == 200 ]] || die "enabling runners failed: $(cat "${TMP}/resp.json")"
psql1 "INSERT INTO api_tokens (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256)
  VALUES (gen_random_uuid(), '${MEMBER}', '${MEMBER}@walk.wardyn.invalid', 'user', 'standard', '[]', false, 'laptop walk',
  '$(printf '%s' "${MEMBER_TOKEN}" | sha256sum | cut -d' ' -f1)')" >/dev/null || die "seeding the member's token failed"

log "STEP 2 (grant): a member without self_host_runner is refused; an all allow admits them"
code="$(api "${MEMBER_TOKEN}" POST /api/v1/me/runners/tokens '{}')"
if [[ "${code}" == 403 ]] && jq -e '.reason == "capability_feature"' "${TMP}/resp.json" >/dev/null; then
  pass "self-mint without the grant: 403 $(jq -c '{reason,error}' "${TMP}/resp.json")"
else fail "self-mint without the grant answered ${code} $(cat "${TMP}/resp.json")"; fi
code="$(api "${ADMIN_TOKEN}" POST /api/v1/permissions/grants '{"subject_type":"all","subject":"","capability":"feature","value":"self_host_runner","effect":"allow"}')"
[[ "${code}" == 20[01] ]] || die "the all allow was refused: ${code} $(cat "${TMP}/resp.json")"
code="$(api "${MEMBER_TOKEN}" POST /api/v1/me/runners/tokens '{}')"
PROXY_REF="$(jq -r '.proxy_image // empty' "${TMP}/resp.json")"
if [[ "${code}" == 201 && "${PROXY_REF}" == "${PROXY_IMAGE}" ]]; then pass "self-mint with the grant: 201, proxy_image ${PROXY_REF}"
else fail "self-mint with the grant answered ${code} $(jq -c 'del(.token)' "${TMP}/resp.json")"; fi
jq -r .token "${TMP}/resp.json" >"${TMP}/reg-token" && chmod 600 "${TMP}/reg-token"

log "STEP 3 (register + claim): the member's own token claims into Laptop as default"
runnerd register --org "${ORG_URL}" --token-file "${TMP}/reg-token" --state-dir "${STATE}" --name "${RUNNER_NAME}" \
  --total-disk 20 </dev/null || die "register failed"
RID="$(jq -r .runner_id "${STATE}/runner.json")"
w runner claim --url "${ORG_URL}" --state-dir "${STATE}" --pool Laptop --default || die "claim failed"
api "${MEMBER_TOKEN}" GET /api/v1/me/runner-pools >/dev/null
POOL_ID="$(jq -r '[.. | objects | select(.name? == "Laptop" and .scope? == "user")][0].id // empty' "${TMP}/resp.json")"
api "${MEMBER_TOKEN}" GET /api/v1/me/runner-pool-defaults >/dev/null
DEFAULT_ID="$(jq -r '.pool_id // empty' "${TMP}/resp.json")"
api "${MEMBER_TOKEN}" GET "/api/v1/runner-pools/${POOL_ID}/members" >/dev/null
if [[ -n "${POOL_ID}" && "${DEFAULT_ID}" == "${POOL_ID}" ]] && grep -q "${RID}" "${TMP}/resp.json"; then
  pass "runner ${RID} claimed into user pool Laptop ${POOL_ID}, which is the member's default"
else fail "after claim: Laptop ${POOL_ID:-missing}, default ${DEFAULT_ID:-none}, members $(cat "${TMP}/resp.json")"; fi

log "STEP 1 (baseline guard): serve with an unreachable --relay-host-target"
WARDYN_RELAY_LISTEN="" WARDYN_RELAY_HOST_TARGET="" runnerd serve --org "${ORG_URL}" --state-dir "${STATE}" --proxy-image "${PROXY_IMAGE}" \
  --internal-network "${RUNNER_NET}" --relay-host-target 192.0.2.1 >"${TMP}/serve-bare.log" 2>&1
rc=$?
api "${MEMBER_TOKEN}" GET "/api/v1/me/runners/${RID}" >/dev/null
if [[ "${rc}" -ne 0 ]] && grep -q -- "192.0.2.1" "${TMP}/serve-bare.log" && jq -e '.online == false and .last_seen_at == null' "${TMP}/resp.json" >/dev/null; then
  pass "serve exited ${rc} before dialing: $(tail -1 "${TMP}/serve-bare.log"); runner never seen"
else fail "serve with --relay-host-target 192.0.2.1: exit ${rc}, $(tail -1 "${TMP}/serve-bare.log"), runner $(jq -c '{online,last_seen_at}' "${TMP}/resp.json")"; fi

log "STEP 4 (run): serve with no relay flags, then a local exec run"
# The binary itself, not the runnerd function: a backgrounded function is a
# subshell, and stopping its pid would leave serve running.
DOCKER_HOST="${RUNNER_DOCKER}" "${TMP}/wardyn-runnerd" serve --org "${ORG_URL}" --state-dir "${STATE}" --proxy-image "${PROXY_IMAGE}" \
  --internal-network "${RUNNER_NET}" >"${EVIDENCE_DIR}/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 30); do
  api "${MEMBER_TOKEN}" GET "/api/v1/me/runners/${RID}" >/dev/null
  jq -e '.online == true' "${TMP}/resp.json" >/dev/null && break
  kill -0 "${SERVE_PID}" 2>/dev/null || break
  sleep 2
done
jq -e '.online == true' "${TMP}/resp.json" >/dev/null || { tail -20 "${EVIDENCE_DIR}/serve.log"; die "the runner never came online"; }
pass "runner online with no relay flags: $(grep -m1 'run relay reachable' "${EVIDENCE_DIR}/serve.log" || echo 'no "run relay reachable" log line')"
grep -q 'run relay reachable' "${EVIDENCE_DIR}/serve.log" || fail "serve.log has no 'run relay reachable' line"
# The relay target as a run's proxy sees it: what host-gateway resolves to on
# this engine, read by a throwaway container (the agent image carries getent; ahostsv4, since Docker Desktop maps host-gateway to an IPv6 address too).
RELAY_TARGET_IP="$(rd run --rm --entrypoint getent --add-host walk-relay-target:host-gateway "${AGENT_IMAGE}" ahostsv4 walk-relay-target 2>/dev/null | awk '{print $1; exit}')"
[[ "${RELAY_TARGET_IP}" =~ ^[0-9.]+$ ]] || { fail "could not resolve host-gateway to an IP on the runner engine: '${RELAY_TARGET_IP}'"; RELAY_TARGET_IP="192.0.2.1"; }
cat >"${TMP}/policy.json" <<'EOF'
{"allowed_domains": [], "denied_domains": [], "first_use_approval": "always_deny", "allowed_methods": [],
 "min_confinement_class": "CC1", "auto_stop_after_sec": 0}
EOF
w run --agent claude-code --task-mode exec --placement local --policy-file "${TMP}/policy.json" --wait --timeout 8m --json \
  --task "echo laptop-walk-marker; curl -sS -m 10 -o /dev/null https://denied.laptop-walk.invalid/ || true; curl -sS -m 5 -o /dev/null https://host.docker.internal:${RELAY_PORT}/ || true; curl -sS -m 5 -o /dev/null https://${RELAY_TARGET_IP}:${RELAY_PORT}/ || true" \
  >"${TMP}/run.json" 2>"${TMP}/run.err"
rc=$?
RUN_ID="$(jq -r '.id // empty' "${TMP}/run.json" 2>/dev/null)"
[[ -n "${RUN_ID}" ]] || { cat "${TMP}/run.err"; die "no run id (exit ${rc})"; }
api "${MEMBER_TOKEN}" GET "/api/v1/runs/${RUN_ID}" >/dev/null
cp "${TMP}/resp.json" "${EVIDENCE_DIR}/run.json"
if [[ "${rc}" -eq 0 ]] && jq -e '.state == "COMPLETED"' "${TMP}/resp.json" >/dev/null; then pass "run ${RUN_ID} COMPLETED on the runner"
else fail "run ${RUN_ID}: exit ${rc}, state $(jq -r .state "${TMP}/resp.json"); $(tail -3 "${TMP}/run.err")"; fi
# Two hops, both proven from the run's audit: the proxy's own calls to the org
# (the recording upload always; a token renewal when the run outlives one)
# arrived over the 127.0.0.1 relay
# (data.relay, stamped only on a relayed connection), and the agent's egress
# through that proxy was decided and shipped over the runner's session spool
# (actor runner:<id>, data.spooled), never over a direct path to the org.
q="SELECT action || ' ' || target FROM audit_events WHERE run_id = '${RUN_ID}'"
relayed="$(psql1 "${q} AND data->>'relay' = 'runner:${RID}' ORDER BY seq" | tr '\n' ';')"
spooled="$(psql1 "${q} AND action LIKE 'egress.%' AND actor = 'runner:${RID}' AND (data->>'spooled')::boolean ORDER BY seq" | tr '\n' ';')"
if [[ -n "${relayed}" ]]; then pass "proxy-to-org calls arrived over the relay (data.relay=runner:${RID}): ${relayed}"
else fail "no audit row for ${RUN_ID} arrived over the relay"; fi
if [[ "${spooled}" == *"egress.deny denied.laptop-walk.invalid"* ]]; then pass "egress decided by the run's proxy, shipped by runner:${RID}: ${spooled}"
else fail "no spooled egress.deny for the denied host: spooled=[${spooled}]"; fi

log "STEP 4b (the relay is the proxy's, not the agent's): the agent is denied the relay's names"
for t in "host.docker.internal" "${RELAY_TARGET_IP}"; do
  denied="$(psql1 "${q} AND action = 'egress.deny' AND target LIKE '${t}%' ORDER BY seq" | tr '\n' ';')"
  allowed="$(psql1 "${q} AND action = 'egress.allow' AND target LIKE '${t}%' ORDER BY seq" | tr '\n' ';')"
  if [[ -n "${denied}" && -z "${allowed}" ]]; then pass "agent denied ${t}: ${denied}"
  else fail "agent and ${t}: deny rows [${denied}], allow rows [${allowed}]"; fi
done

log "STEP 5 (where the run ran): the run read and its run.create row"
want='.placement == "local" and .runner_name == $n and .runner_pool_name == "Laptop"'
if jq -e --arg n "${RUNNER_NAME}" "${want}" "${TMP}/resp.json" >/dev/null; then pass "GET /runs/{id}: $(jq -c '{placement, runner_id, runner_name, runner_pool_name}' "${TMP}/resp.json")"
else fail "GET /runs/{id}: $(jq -c '{placement, runner_id, runner_name, runner_pool_name}' "${TMP}/resp.json")"; fi
psql1 "SELECT data FROM audit_events WHERE run_id = '${RUN_ID}' AND action = 'run.create' AND outcome = 'success'" >"${TMP}/create.json"
if jq -e --arg n "${RUNNER_NAME}" "${want}" "${TMP}/create.json" >/dev/null; then pass "run.create row: $(jq -c '{placement, runner_id, runner_name, runner_pool_name}' "${TMP}/create.json")"
else fail "run.create row: $(cat "${TMP}/create.json")"; fi

# STEP 7: a member's bedrock_sso run on the laptop, against test/awsssofake.
# WHERE THE FAKE LISTENS. A run's proxy on Docker Desktop reaches beyond its two
# networks only through host-gateway (the org's control-plane address there,
# which liftInternalHost never lifts) or Docker Desktop's VM itself. So the fake
# runs on the VM's host network bound to its docker0 address 172.17.0.1: no
# subnet of the proxy, not the control plane, and not published to this host or
# beyond. The org never calls it: the session is captured once with a long TTL,
# so no refresh comes due, and the walk talks to it with `docker exec`.
fake() { rd exec "${FAKE_CONTAINER}" wget -qO- -T 10 "$@"; }
fake_post() { fake --header 'Content-Type: application/json' --post-data "$2" "${FAKE_URL}$1"; }
# A real RegisterClient + StartDeviceAuthorization + CreateToken against the
# fake, printed as the session blob internal/api/harnesscred.go stores.
fake_device_login() {
  local reg cid csec dc tok
  reg="$(fake_post /client/register '{"clientName":"laptop-walk","clientType":"public"}')" || return 1
  cid="$(jq -r .clientId <<<"${reg}")"; csec="$(jq -r .clientSecret <<<"${reg}")"
  dc="$(fake_post /device_authorization "$(jq -nc --arg c "${cid}" --arg s "${csec}" --arg u "${SSO_START_URL}" '{clientId:$c,clientSecret:$s,startUrl:$u}')" | jq -r .deviceCode)" || return 1
  tok="$(fake_post /token "$(jq -nc --arg c "${cid}" --arg s "${csec}" --arg d "${dc}" '{clientId:$c,clientSecret:$s,grantType:"urn:ietf:params:oauth:grant-type:device_code",deviceCode:$d}')")" || return 1
  jq -c --arg cid "${cid}" --arg cs "${csec}" --arg u "${SSO_START_URL}" --arg r "${SSO_REGION}" --arg a "${PIN_ACCOUNT}" --arg role "${PIN_ROLE}" \
    --arg exp "$(date -u -d "+$(jq -r .expiresIn <<<"${tok}") seconds" +%Y-%m-%dT%H:%M:%SZ)" --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{access_token:.accessToken,refresh_token:.refreshToken,client_id:$cid,client_secret:$cs,start_url:$u,region:$r,account_id:$a,role_name:$role,expires_at:$exp,captured_at:$now}' <<<"${tok}"
}

log "STEP 7 (bedrock_sso): the fake, the provider, and the member's captured session"
FAKE_BASE="alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc" # scripts/survival-walk.sh's
ensure_image "${FAKE_BASE}" || die "no ${FAKE_BASE} on Docker Desktop"
rd create --name "${FAKE_CONTAINER}" --network host -e "AWSSSOFAKE_ADDR=${FAKE_URL#http://}" \
  -e "AWSSSOFAKE_ACCOUNTS=$(jq -nc --arg a "${PIN_ACCOUNT}" --arg r "${PIN_ROLE}" '[{account_id:$a,roles:[$r]}]')" \
  -e AWSSSOFAKE_TOKEN_TTL=2h -e AWSSSOFAKE_REAUTH_AFTER=0 "${FAKE_BASE}" /awsssofake >/dev/null || die "creating the fake failed"
rd cp "${TMP}/awsssofake" "${FAKE_CONTAINER}:/awsssofake" && rd start "${FAKE_CONTAINER}" >/dev/null || die "starting the fake failed"
for _ in $(seq 1 15); do fake "${FAKE_URL}/_seen" >/dev/null 2>&1 && break; sleep 1; done
fake "${FAKE_URL}/_seen" >/dev/null || die "the fake never answered at ${FAKE_URL}"
code="$(api "${ADMIN_TOKEN}" PUT /api/v1/model-providers "$(jq -nc --arg id "${PROVIDER_ID}" --arg r "${SSO_REGION}" --arg b "${FAKE_URL}" \
  --arg u "${SSO_START_URL}" --arg a "${PIN_ACCOUNT}" --arg role "${PIN_ROLE}" --arg m "${BEDROCK_MODEL}" \
  '{providers:[{id:$id,kind:"bedrock_sso",bedrock:{region:$r,base_url:$b,sso_start_url:$u,sso_account_id:$a,sso_role_name:$role},harnesses:[{harness:"claude-code",model:$m}]}]}')")"
[[ "${code}" == 200 ]] || die "PUT /model-providers answered ${code}: $(cat "${TMP}/resp.json")"
code="$(api "${ADMIN_TOKEN}" PUT /api/v1/agent-providers "$(jq -nc --arg id "${PROVIDER_ID}" '{agents:[{id:"claude-code",default_provider:$id}]}')")"
[[ "${code}" == 200 ]] || die "PUT /agent-providers answered ${code}: $(cat "${TMP}/resp.json")"
# The sign-in run is placement-exempt: it lands on the org's own Docker runner, which needs limits first.
wardyn() { WARDYN_URL="${ORG_URL}" WARDYN_ADMIN_TOKEN="${ADMIN_TOKEN}" "${TMP}/wardyn" "$@"; }
# shellcheck source=scripts/lib/runner-limits.sh
source "${ROOT}/scripts/lib/runner-limits.sh"
WARDYN_SETUP_RUNNER_LIMITS="--max-runs 2 --total-cpu 4 --total-memory 8 --total-disk 40 --per-run-cpu 2 --per-run-memory 4 --per-run-disk 20" \
  wardyn_ensure_runner_limits </dev/null || die "could not give the org's own runner its limits"
code="$(api "${MEMBER_TOKEN}" POST "/api/v1/model-providers/${PROVIDER_ID}/sign-in")"
LOGIN_RUN_ID="$(jq -r '.run_id // empty' "${TMP}/resp.json")"
[[ "${code}" == 20[01] && -n "${LOGIN_RUN_ID}" ]] || die "member sign-in answered ${code}: $(cat "${TMP}/resp.json")"
for _ in $(seq 1 60); do [[ "$(od inspect -f '{{.State.Running}}' "wardyn-agent-${LOGIN_RUN_ID}" 2>/dev/null)" == true ]] && break; sleep 2; done
BLOB="$(fake_device_login)" || die "the device login against the fake failed"
REAL_TOKEN="$(jq -r .access_token <<<"${BLOB}")"
code="$(od exec "wardyn-agent-${LOGIN_RUN_ID}" sh -c "curl -sS -m 15 -o /dev/null -w '%{http_code}' -X PUT \"\${WARDYN_PROXY_URL}/wardyn/v1/sso-token/\${WARDYN_RUN_ID}\" -H 'Content-Type: application/json' -d '${BLOB}'")"
[[ "${code}" == 204 ]] || die "capturing the member's session into sign-in run ${LOGIN_RUN_ID} answered ${code}"
api "${MEMBER_TOKEN}" POST "/api/v1/runs/${LOGIN_RUN_ID}/kill" >/dev/null
api "${MEMBER_TOKEN}" GET /api/v1/setup/status >/dev/null
jq -e --arg id "${PROVIDER_ID}" '.provider_access[]? | select(.provider == $id) | .state == "live"' "${TMP}/resp.json" >/dev/null \
  || die "the member's session is not live: $(jq -c '.provider_access' "${TMP}/resp.json")"
pass "member's AWS SSO session captured (sign-in run ${LOGIN_RUN_ID} on the org's runner), provider ${PROVIDER_ID} live"

log "STEP 7a (proxy inject on): the member's bedrock_sso agent run on the laptop"
# A real agent run: an exec command is not a model run and gets no model access.
# Claude Code's AWS SDK presents the placeholder its SSO cache holds to the portal,
# the proxy swaps in the member's session on that one GetRoleCredentials, and the
# SDK spends the role credential on the fake's Bedrock stub, whose canned answer
# is not a model reply.
w run --agent claude-code --placement local --policy-file "${TMP}/policy.json" --wait --timeout 8m --json \
  --task "Reply with the single word laptop-walk." >"${TMP}/sso-run.json" 2>"${TMP}/sso-run.err" &
cli=$!
agent=""
for _ in $(seq 1 90); do
  agent="$(rd ps -q --filter label=wardyn.managed=true --filter name=wardyn-agent- --filter label=wardyn.runner-id="${RID}" | head -1)"
  [[ -n "${agent}" ]] && break
  sleep 2
done
[[ -n "${agent}" ]] || die "no agent container started on the runner"
SSO_RUN_ID="$(rd inspect -f '{{index .Config.Labels "wardyn.run-id"}}' "${agent}")"
rd inspect "${agent}" >"${EVIDENCE_DIR}/sso-agent-inspect.json"
wait "${cli}"; rc=$?
api "${MEMBER_TOKEN}" GET "/api/v1/runs/${SSO_RUN_ID}" >/dev/null
cp "${TMP}/resp.json" "${EVIDENCE_DIR}/sso-run.json"
if jq -e --arg n "${RUNNER_NAME}" '.placement == "local" and .runner_name == $n' "${TMP}/resp.json" >/dev/null; then
  pass "bedrock_sso run ${SSO_RUN_ID} admitted local on ${RUNNER_NAME}, sandbox started (agent container ${agent:0:12}); end state $(jq -r .state "${TMP}/resp.json") $(jq -c '.error // .failure // empty' "${TMP}/resp.json" | head -c 200), CLI exit ${rc}"
else fail "bedrock_sso run ${SSO_RUN_ID}: $(jq -c '{state, placement, error}' "${TMP}/resp.json"); $(tail -3 "${TMP}/sso-run.err")"; fi
env_records="$(jq -r '.[0].Config.Env[] | select(startswith("WARDYN_AWS_SSO_CONFIG_B64=")) | sub("^[^=]*="; "")' "${EVIDENCE_DIR}/sso-agent-inspect.json")"
decoded="$(while IFS=$'\t' read -r _ b; do [[ -n "${b}" ]] && printf '%s' "${b}" | base64 -d; done <<<"${env_records}")"
if [[ -n "${decoded}" ]] && ! grep -qF "${REAL_TOKEN}" "${EVIDENCE_DIR}/sso-agent-inspect.json" && ! grep -qF "${REAL_TOKEN}" <<<"${decoded}"; then
  pass "docker inspect of the agent: no real access token, raw or in WARDYN_AWS_SSO_CONFIG_B64 (its cache holds $(grep -o '"accessToken" *: *"[^"]*"' <<<"${decoded}"))"
else fail "the agent container carries the real access token, or no SSO config at all"; fi
seen="$(fake "${FAKE_URL}/_seen")"
echo "${seen}" >"${EVIDENCE_DIR}/awsssofake-seen.json"
calls="$(jq -r --arg p "${PIN_ACCOUNT}/${PIN_ROLE}" '[.sessions[].role_cred_pairs[$p] // 0] | add // 0' <<<"${seen}")"
if [[ "${calls}" -ge 1 ]]; then
  pass "portal GetRoleCredentials for ${PIN_ACCOUNT}/${PIN_ROLE} accepted the bearer the proxy injected (${calls} authenticated call(s); the sandbox held only the placeholder)"
else fail "the fake saw no authenticated GetRoleCredentials: $(jq -c '{sessions, account_id, role_name}' <<<"${seen}"); egress: $(psql1 "SELECT action || ' ' || target || ' ' || coalesce(data->>'rule_source','') FROM audit_events WHERE run_id = '${SSO_RUN_ID}' AND action LIKE 'egress.%'" | tr '\n' ';')"; fi
echo "  fake bedrock stub: $(jq -c '{bedrock_calls, bedrock_model}' <<<"${seen}"); run output tail: $(w run output "${SSO_RUN_ID}" 2>&1 | tail -c 300 | tr '\n' ' ')"
echo "relayed rows for ${SSO_RUN_ID}: $(psql1 "SELECT action FROM audit_events WHERE run_id = '${SSO_RUN_ID}' AND data->>'relay' = 'runner:${RID}' ORDER BY seq" | tr '\n' ' ')"

log "STEP 7b (proxy inject off): the same local run is refused at preflight"
compose_env_off() { WARDYN_AWS_SSO_PROXY_INJECT=off compose "$@"; }
compose_env_off up -d wardynd >>"${EVIDENCE_DIR}/compose-up.log" 2>&1 || die "restarting the org with proxy-inject off failed"
wait_healthy "${ORG_URL}" 90 2 || die "the org did not come back after the restart"
for _ in $(seq 1 30); do
  api "${MEMBER_TOKEN}" GET "/api/v1/me/runners/${RID}" >/dev/null
  jq -e '.online == true' "${TMP}/resp.json" >/dev/null && break
  sleep 2
done
before="$(psql1 "SELECT count(*) FROM agent_runs")"
body="$(jq -nc --slurpfile p "${TMP}/policy.json" '{agent:"claude-code",placement:"local",task:"Reply with the single word laptop-walk.",inline_policy:$p[0]}')"
code="$(api "${MEMBER_TOKEN}" POST /api/v1/runs/preflight "${body}")"
after="$(psql1 "SELECT count(*) FROM agent_runs")"
if [[ "${code}" == 422 ]] && jq -e '.reason == "placement_credential"' "${TMP}/resp.json" >/dev/null \
  && grep -qF 'SandboxSpec.SecretEnv[WARDYN_AWS_SSO_CONFIG_B64]' "${TMP}/resp.json" && [[ "${before}" == "${after}" ]]; then
  pass "preflight with proxy-inject off: 422 placement_credential, no run created: $(jq -c . "${TMP}/resp.json" | head -c 400)"
else fail "preflight with proxy-inject off answered ${code} (runs ${before} -> ${after}): $(head -c 600 "${TMP}/resp.json")"; fi

log "STEP 6 (revoke): stop serve, revoke the dropped runner, read the pools"
stop_serve
for _ in $(seq 1 15); do
  api "${MEMBER_TOKEN}" GET "/api/v1/me/runners/${RID}" >/dev/null
  jq -e '.online == false' "${TMP}/resp.json" >/dev/null && break
  sleep 2
done
jq -e '.online == false' "${TMP}/resp.json" >/dev/null || fail "the runner still reads online after serve stopped"
code="$(api "${MEMBER_TOKEN}" DELETE "/api/v1/me/runners/${RID}")"
if [[ "${code}" == 200 ]] && jq -e '.state == "revoked"' "${TMP}/resp.json" >/dev/null; then pass "revoke after serve stopped: 200 state revoked"
else fail "revoke answered ${code} $(cat "${TMP}/resp.json")"; fi
code="$(api "${MEMBER_TOKEN}" GET /api/v1/me/runner-pools)"
if [[ "${code}" == 200 ]] && jq -e '[.. | objects | select(.name? == "Laptop")] | length > 0' "${TMP}/resp.json" >/dev/null; then pass "GET /me/runner-pools: 200 with Laptop"
else fail "GET /me/runner-pools answered ${code} $(cat "${TMP}/resp.json")"; fi

[[ "${FAILED}" -eq 0 ]] || die "runner-laptop-walk: at least one step FAILED (evidence in ${EVIDENCE_DIR})"
log "runner-laptop-walk: every step passed (evidence in ${EVIDENCE_DIR})"
