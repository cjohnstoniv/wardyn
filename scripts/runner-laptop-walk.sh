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
#   3. `serve` without --relay-listen exits non-zero before it dials (the
#      runner is never seen);
#   4. `serve --relay-listen 127.0.0.1` goes online, and a local exec run on it
#      completes; its proxy's calls to the org arrive over the relay (audit
#      data.relay = runner:<id>) and its egress decisions over the runner's
#      session (actor runner:<id>, data.spooled);
#   5. GET /runs/{id} and the run.create audit row carry placement local,
#      runner_name and runner_pool_name;
#   6. with serve stopped, the owner's revoke answers 200 state revoked, and
#      GET /me/runner-pools answers 200 with Laptop.
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
PG_CONTAINER="${PROJECT}-postgres"
EVIDENCE_DIR="${WARDYN_LAPTOP_WALK_EVIDENCE:-${ROOT}/local/evidence/runner-laptop-walk}"
mkdir -p "${EVIDENCE_DIR}"
TMP="$(mktemp -d /tmp/wardyn-laptop-walk.XXXXXX)"
STATE="${TMP}/runner-state"
export SSL_CERT_FILE="${TMP}/tls/ca.crt" CURL_CA_BUNDLE="${TMP}/tls/ca.crt"
SERVE_PID="" RID="" RUN_ID=""
FAILED=0
pass() { printf '  [pass] %s\n' "$*"; }
fail() { printf '  [FAIL] %s\n' "$*"; FAILED=1; }

compose() {
  DOCKER_HOST="${ORG_DOCKER}" COMPOSE_PROJECT_NAME="${PROJECT}" WARDYN_NS="${PROJECT}" \
    WARDYN_UP_PORT=0 WARDYN_PG_PORT=0 WARDYN_REGISTRY_PORT=0 WARDYN_SSH_PORT=0 WARDYN_UI_SANDBOX_PORT=0 \
    WARDYN_DOCKER_SOCK="${ORG_DOCKER#unix://}" WARDYN_WARDYND_IMAGE="${WARDYND_IMAGE}" WARDYN_PROXY_IMAGE="${PROXY_IMAGE}" \
    WARDYN_AGENT_IMAGES="$(jq -nc --arg cc "${AGENT_IMAGE}" '{"claude-code":$cc}')" \
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
  for label in ${RUN_ID:+wardyn.run-id=${RUN_ID}} ${RID:+wardyn.runner-id=${RID}}; do
    rd ps -aq --filter "label=${label}" | xargs -r docker -H "${RUNNER_DOCKER}" rm -f >/dev/null 2>&1
    rd volume ls -q --filter "label=${label}" | xargs -r docker -H "${RUNNER_DOCKER}" volume rm >/dev/null 2>&1
  done
  [[ -n "${RUN_ID}" ]] && rd network rm "wardyn-int-${RUN_ID}" >/dev/null 2>&1
  rd network rm "${RUNNER_NET}" >/dev/null 2>&1
  [[ -f "${TMP}/override.yaml" ]] && compose down --volumes >/dev/null 2>&1
  if [[ "${WARDYN_LAPTOP_WALK_KEEP_IMAGES:-}" != "1" ]]; then
    rd rmi "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${AGENT_IMAGE}" >/dev/null 2>&1
    od rmi "${WARDYND_IMAGE}" "${PROXY_IMAGE}" >/dev/null 2>&1
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
rd save "${WARDYND_IMAGE}" "${PROXY_IMAGE}" | od load -q || die "loading the org images into ${ORG_DOCKER} failed"

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

log "STEP 1 (baseline guard): serve on Docker Desktop without --relay-listen"
WARDYN_RELAY_LISTEN="" runnerd serve --org "${ORG_URL}" --state-dir "${STATE}" --proxy-image "${PROXY_IMAGE}" \
  --internal-network "${RUNNER_NET}" >"${TMP}/serve-bare.log" 2>&1
rc=$?
api "${MEMBER_TOKEN}" GET "/api/v1/me/runners/${RID}" >/dev/null
if [[ "${rc}" -ne 0 ]] && grep -q -- "--relay-listen 127.0.0.1" "${TMP}/serve-bare.log" && jq -e '.online == false and .last_seen_at == null' "${TMP}/resp.json" >/dev/null; then
  pass "serve exited ${rc} before dialing: $(tail -1 "${TMP}/serve-bare.log"); runner never seen"
else fail "serve without --relay-listen: exit ${rc}, $(tail -1 "${TMP}/serve-bare.log"), runner $(jq -c '{online,last_seen_at}' "${TMP}/resp.json")"; fi

log "STEP 4 (run): serve --relay-listen 127.0.0.1, then a local exec run"
# The binary itself, not the runnerd function: a backgrounded function is a
# subshell, and stopping its pid would leave serve running.
DOCKER_HOST="${RUNNER_DOCKER}" "${TMP}/wardyn-runnerd" serve --org "${ORG_URL}" --state-dir "${STATE}" --proxy-image "${PROXY_IMAGE}" --relay-listen 127.0.0.1 \
  --internal-network "${RUNNER_NET}" >"${EVIDENCE_DIR}/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 30); do
  api "${MEMBER_TOKEN}" GET "/api/v1/me/runners/${RID}" >/dev/null
  jq -e '.online == true' "${TMP}/resp.json" >/dev/null && break
  kill -0 "${SERVE_PID}" 2>/dev/null || break
  sleep 2
done
jq -e '.online == true' "${TMP}/resp.json" >/dev/null || { tail -20 "${EVIDENCE_DIR}/serve.log"; die "the runner never came online"; }
pass "runner online with --relay-listen 127.0.0.1"
cat >"${TMP}/policy.json" <<'EOF'
{"allowed_domains": [], "denied_domains": [], "first_use_approval": "always_deny", "allowed_methods": [],
 "min_confinement_class": "CC1", "auto_stop_after_sec": 0}
EOF
w run --agent claude-code --task-mode exec --placement local --policy-file "${TMP}/policy.json" --wait --timeout 8m --json \
  --task "echo laptop-walk-marker; curl -sS -m 10 -o /dev/null https://denied.laptop-walk.invalid/ || true" \
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

log "STEP 5 (where the run ran): the run read and its run.create row"
want='.placement == "local" and .runner_name == $n and .runner_pool_name == "Laptop"'
if jq -e --arg n "${RUNNER_NAME}" "${want}" "${TMP}/resp.json" >/dev/null; then pass "GET /runs/{id}: $(jq -c '{placement, runner_id, runner_name, runner_pool_name}' "${TMP}/resp.json")"
else fail "GET /runs/{id}: $(jq -c '{placement, runner_id, runner_name, runner_pool_name}' "${TMP}/resp.json")"; fi
psql1 "SELECT data FROM audit_events WHERE run_id = '${RUN_ID}' AND action = 'run.create' AND outcome = 'success'" >"${TMP}/create.json"
if jq -e --arg n "${RUNNER_NAME}" "${want}" "${TMP}/create.json" >/dev/null; then pass "run.create row: $(jq -c '{placement, runner_id, runner_name, runner_pool_name}' "${TMP}/create.json")"
else fail "run.create row: $(cat "${TMP}/create.json")"; fi

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
