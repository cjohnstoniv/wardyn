#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# An external AI gateway, run: the unmodified agentgateway image (pinned by digest in
# deploy/kind/gateway/gateway.yaml) in front of test/gatewayfake, on the `make kind-quickstart`
# cluster, with two real Wardyn runs through it.
#
#   1. a claude-code harness run on a custom_endpoint provider whose base_url is the gateway's
#      model hostname (llm-gw). The fake answers with a canned message.
#   2. an exec run that makes one MCP tool call through the gateway's MCP hostname (mcp-gw). The
#      call comes from a curl in the sandbox, not from a model.
#
# The model route also verifies Wardyn's run identity: the provider has gateway_identity on, the run's
# proxy sends the signed token in X-Wardyn-Identity, and the gateway (jwtAuth, strict) checks it against
# the key set wardynd serves. The gateway's access log carries the token's subject and run id and the
# call id Wardyn sent in X-Wardyn-Call-Id. The MCP route reads no token.
#
# Why two hostnames: a component may not reach a host that serves a model
# (internal/api/components_run.go), so MCP federation lives on a host of its own.
#
# What the asserts read, never a run's exit code alone (claude-code's exit says nothing about the
# gateway): the runs' audit rows (the proxy let each call through), the gateway's own access log
# (a line for each route, the model line naming the run's subject and run id, and the call id the
# audit row carries), and the fake's /_seen (the made-up credential arrived on the model route, the
# identity header did not, and the tool was called once). Two direct calls to the model route, one with
# no token and one with a token another key signed, must be answered 401.
#
# No provider key exists anywhere in this job: the fake sits behind the gateway, and the one
# credential is a value this script makes up. The gateway image is public and pulled anonymously.
#
# The model route reaches the gateway on the proxy's brokered lane, which needs no internal_hosts
# entry (docs/OPERATIONS.md, "Internal model gateway"). The MCP host is an ordinary egress
# destination on a private address, so it has an entry and the run's policy allows it.
#
# GUARD: self-skips unless WARDYN_TEST_K8S=1 (exit 77, common.sh's skip_lane). It mutates the
# cluster it finds (helm upgrade, site-config, and it REPLACES the model-provider block and
# claude-code's default provider), so a hand run must also set WARDYN_ACK_SHARED_QUICKSTART=1 to say
# that cluster is disposable; nightly.yml sets it for the hosted run, which creates its own.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-gateway-run: set WARDYN_TEST_K8S=1 to run the cluster-dependent AI gateway run (skipping)."
fi
if [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" != "1" ]]; then
  skip_lane "kind-gateway-run: set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the quickstart cluster this script finds may be mutated — refusing to run against a cluster it did not create otherwise."
fi

wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

for bin in docker kind kubectl helm curl jq go openssl python3; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
RELEASE="wardyn"
HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
BASE_URL="http://127.0.0.1:${HTTP_PORT}"
MANIFEST="deploy/kind/gateway/gateway.yaml"
FAKE_IMAGE="wardyn/gatewayfake:local"
FAKE_SVC="gatewayfake"
GATEWAY="ai-gateway"
# Service names, never a pod IP: the proxy refuses to lift its own pod subnet.
LLM_HOST="llm-gw.${NAMESPACE}.svc.cluster.local"
MCP_HOST="mcp-gw.${NAMESPACE}.svc.cluster.local"
GATEWAY_PORT=8443
AUDIENCE="wardyn-ai-gateway-job"
PROVIDER_ID="ai-gateway"
MODEL="claude-sonnet-4-5"
CREDENTIAL="made-up-gateway-credential-not-a-secret"
CURL_MAX_TIME=15
EVIDENCE_DIR="${WARDYN_KIND_GATEWAY_EVIDENCE:-${ROOT}/local/evidence/kind-gateway-run}"
mkdir -p "${EVIDENCE_DIR}"
TMPDIR="$(mktemp -d /tmp/wardyn-kind-gateway-run.XXXXXX)"
PF_PID=""
GW_PF_PID=""
trap 'for p in "${PF_PID}" "${GW_PF_PID}"; do [[ -n "${p}" ]] && kill "${p}" 2>/dev/null; done; rm -rf "${TMPDIR}"' EXIT

step "checking the cluster is up (never created or deleted here)"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get deployment "${RELEASE}" >/dev/null 2>&1 \
  || die "no wardyn release on ${CONTEXT} — run: WARDYN_QUICKSTART_HTTP_PORT=${HTTP_PORT} make kind-quickstart"
ADMIN_TOKEN="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret wardyn-auth \
  -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[[ -n "${ADMIN_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth"

api() { # METHOD PATH [BODY] -> HTTP code; body in ${TMPDIR}/resp.json
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "${body}" ]]; then
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE_URL}/api/v1${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" -H 'Content-Type: application/json' -d "${body}"
  else
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${TMPDIR}/resp.json" -w '%{http_code}' -X "${method}" "${BASE_URL}/api/v1${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}"
  fi
}
must() { # WANT_CODE WHAT METHOD PATH [BODY]
  local want="$1" what="$2" code
  shift 2
  code="$(api "$@")"
  [[ "${code}" == "${want}" ]] || { cat "${TMPDIR}/resp.json" >&2; die "${what} answered ${code}, want ${want}"; }
}

# ── 0. the gateway is a pinned image ────────────────────────────────────────
step "checking every image in ${MANIFEST} is pinned by digest (or is the locally built fake)"
unpinned="$(grep -E '^[[:space:]]*image:' "${MANIFEST}" | grep -v '@sha256:[0-9a-f]\{64\}' | grep -v "${FAKE_IMAGE}" || true)"
[[ -z "${unpinned}" ]] || die "an image in ${MANIFEST} floats on a tag: ${unpinned}"
grep -q 'image: ghcr.io/agentgateway/agentgateway:v[0-9.]*@sha256:' "${MANIFEST}" || die "${MANIFEST} does not run the pinned agentgateway image"

# ── 1. a throwaway CA, and the trust wardynd needs to reach the gateway ─────
# The server certificate names both hostnames. The CA reaches wardynd through the chart's trustedCA
# value, and from there the run's proxy and the sandbox's own TLS clients (WARDYN_TRUSTED_CA_FILE).
step "making a throwaway CA and a server certificate for ${LLM_HOST} and ${MCP_HOST}"
CERTS="${TMPDIR}/certs"
mkdir -p "${CERTS}"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 -subj "/CN=wardyn-gateway-job-ca" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign" \
  -keyout "${CERTS}/ca.key" -out "${CERTS}/ca.crt" >/dev/null 2>&1 || die "could not make the CA"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=${LLM_HOST}" \
  -keyout "${CERTS}/tls.key" -out "${CERTS}/tls.csr" >/dev/null 2>&1 || die "could not make the server key"
openssl x509 -req -in "${CERTS}/tls.csr" -CA "${CERTS}/ca.crt" -CAkey "${CERTS}/ca.key" -CAcreateserial -days 1 \
  -extfile <(printf 'subjectAltName=DNS:%s,DNS:%s\nextendedKeyUsage=serverAuth\n' "${LLM_HOST}" "${MCP_HOST}") \
  -out "${CERTS}/tls.crt" >/dev/null 2>&1 || die "could not sign the server certificate"

step "installing the CA as wardynd's trusted bundle (helm upgrade --reuse-values --set-file trustedCA)"
helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn -n "${NAMESPACE}" --reuse-values \
  --set-file "trustedCA=${CERTS}/ca.crt" >"${EVIDENCE_DIR}/helm-upgrade.log" 2>&1 \
  || { tail -30 "${EVIDENCE_DIR}/helm-upgrade.log" >&2; die "helm upgrade failed"; }
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=240s || die "wardynd did not come back after the upgrade"

# ── 2. internal_hosts: the private-address refusal, lifted for the MCP host only ─
SERVICE_CIDR="$(kubectl --context "${CONTEXT}" -n kube-system get pod \
  -l component=kube-apiserver -o jsonpath='{.items[0].spec.containers[0].command}' 2>/dev/null \
  | jq -r '.[] | select(startswith("--service-cluster-ip-range=")) | sub("^--service-cluster-ip-range=";"")' 2>/dev/null | head -1)"
[[ -n "${SERVICE_CIDR}" ]] || die "could not read --service-cluster-ip-range from the apiserver — do NOT substitute a pod IP or the pod CIDR: the proxy refuses to lift its own subnet"
step "seeding site-config internal_hosts (${MCP_HOST} scoped to ${SERVICE_CIDR}; the model host gets none)"
current=""
for _ in $(seq 1 30); do
  if [[ "$(api GET /site-config)" == "200" ]] && jq -e 'type == "object"' "${TMPDIR}/resp.json" >/dev/null 2>&1; then current="$(cat "${TMPDIR}/resp.json")"; break; fi
  sleep 2
done
[[ -n "${current}" ]] || die "GET /site-config never answered 200"
merged="$(jq --arg h "${MCP_HOST}" --arg c "${SERVICE_CIDR}" \
  '.internal_hosts = ((.internal_hosts // []) | map(select(.host_suffix != $h)) + [{host_suffix:$h, cidrs:[$c]}])' <<<"${current}")"
must 200 "PUT /site-config" PUT /site-config "${merged}"

# ── 3. the provider, with run identity on, and the caller's own credential ────────────────────────
step "seeding the model provider (${PROVIDER_ID}: custom_endpoint -> https://${LLM_HOST}:${GATEWAY_PORT}, gateway identity audience ${AUDIENCE}) and its made-up credential"
must 200 "PUT /model-providers" PUT /model-providers "$(jq -cn --arg id "${PROVIDER_ID}" --arg u "https://${LLM_HOST}:${GATEWAY_PORT}" --arg m "${MODEL}" --arg a "${AUDIENCE}" \
  '{providers:[{id:$id,name:"AI gateway",kind:"custom_endpoint",base_url:$u,gateway_identity:{audience:$a},harnesses:[{harness:"claude-code",model:$m}]}]}')"
must 200 "PUT /agent-providers" PUT /agent-providers "$(jq -cn --arg id "${PROVIDER_ID}" '{agents:[{id:"claude-code",default_provider:$id}]}')"
code="$(api DELETE "/model-providers/${PROVIDER_ID}/credential")"
[[ "${code}" == "204" || "${code}" == "404" ]] || die "could not clear a stale credential for ${PROVIDER_ID} (DELETE answered ${code})"
must 204 "PUT /model-providers/${PROVIDER_ID}/credential" PUT "/model-providers/${PROVIDER_ID}/credential" "$(jq -cn --arg v "${CREDENTIAL}" '{value:$v}')"

# ── 4. the fake and the gateway, on the cluster ─────────────────────────────
# After the provider: saving it with gateway_identity on is what makes wardynd create the signing key
# and serve the key set, and the gateway does not start without the key set.
step "reading the issuer and key set address wardynd reports"
must 200 "GET /model-providers" GET /model-providers
ISSUER="$(jq -r '.gateway_identity_issuer // empty' "${TMPDIR}/resp.json")"
JWKS_PATH="$(jq -r '.gateway_jwks_path // empty' "${TMPDIR}/resp.json")"
[[ -n "${ISSUER}" && -n "${JWKS_PATH}" ]] || die "GET /model-providers did not report gateway_identity_issuer and gateway_jwks_path: $(cat "${TMPDIR}/resp.json")"
CP_PORT="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get svc "${RELEASE}" -o jsonpath='{.spec.ports[?(@.name=="http")].port}' 2>/dev/null)"
[[ -n "${CP_PORT}" ]] || die "could not read the http port of Service ${RELEASE}"
# The in-cluster address the gateway's pod uses. The console shows the same path under the browser's
# own origin; a gateway that reaches wardynd elsewhere uses that address with the same path.
JWKS_URL="http://${RELEASE}.${NAMESPACE}.svc.cluster.local:${CP_PORT}${JWKS_PATH}"
step "waiting for wardynd to serve the key set (${JWKS_PATH})"
jwks_ok=0
for _ in $(seq 1 40); do
  if curl -sf --max-time "${CURL_MAX_TIME}" -o "${EVIDENCE_DIR}/jwks.json" "${BASE_URL}${JWKS_PATH}" && jq -e '(.keys | length) >= 1' "${EVIDENCE_DIR}/jwks.json" >/dev/null 2>&1; then
    jwks_ok=1
    break
  fi
  sleep 3
done
[[ "${jwks_ok}" -eq 1 ]] || die "wardynd never served a key set at ${JWKS_PATH} — saving the provider with gateway_identity should have created the key"

step "building and loading the fake (${FAKE_IMAGE}, test/gatewayfake)"
docker build -f test/gatewayfake/Dockerfile -t "${FAKE_IMAGE}" . >"${EVIDENCE_DIR}/fake-build.log" 2>&1 \
  || { tail -20 "${EVIDENCE_DIR}/fake-build.log" >&2; die "docker build of the fake failed"; }
kind load docker-image "${FAKE_IMAGE}" --name "${CLUSTER}" >/dev/null || die "kind load of the fake failed"

step "deploying the gateway and the fake (${MANIFEST})"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" create secret tls ai-gateway-tls \
  --cert="${CERTS}/tls.crt" --key="${CERTS}/tls.key" --dry-run=client -o yaml \
  | kubectl --context "${CONTEXT}" apply -f - >/dev/null || die "could not create Secret ai-gateway-tls"
# The manifest names its three run-specific values as placeholders; fill them in as it is applied.
RENDERED="${TMPDIR}/gateway.rendered.yaml"
sed -e "s|__GATEWAY_ISSUER__|${ISSUER}|g" -e "s|__GATEWAY_AUDIENCE__|${AUDIENCE}|g" -e "s|__GATEWAY_JWKS_URL__|${JWKS_URL}|g" "${MANIFEST}" >"${RENDERED}"
! grep -q '__GATEWAY_' "${RENDERED}" || die "a placeholder in ${MANIFEST} was left unfilled"
cp "${RENDERED}" "${EVIDENCE_DIR}/gateway.rendered.yaml"
kubectl --context "${CONTEXT}" apply -f "${RENDERED}" >/dev/null || die "could not apply ${MANIFEST}"
for d in "${FAKE_SVC}" "${GATEWAY}"; do
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${d}" --timeout=240s \
    || {
      kubectl --context "${CONTEXT}" -n "${NAMESPACE}" describe "deployment/${d}" >&2
      kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs "deployment/${d}" --tail=100 >"${EVIDENCE_DIR}/${d}-rollout.log" 2>&1
      die "${d} did not become ready (its last 100 log lines are in ${EVIDENCE_DIR}/${d}-rollout.log)"
    }
done

# ── 5. the runs ─────────────────────────────────────────────────────────────
step "building the wardyn CLI"
go build -o "${TMPDIR}/bin/wardyn" ./cmd/wardyn || die "go build ./cmd/wardyn failed"

policy_file() { # HOST -> a ci.json-shaped policy path allowing only HOST
  local f="${TMPDIR}/policy-$1.json"
  jq --arg h "$1" '.allowed_domains = [$h]' examples/policies/ci.json >"${f}" || die "could not write a policy for $1"
  echo "${f}"
}
# WARDYN_CI_TOKEN is the admin token ON PURPOSE: with no OIDC it IS the principal that holds the
# credential. ci-run.sh clears WARDYN_ADMIN_TOKEN before every call.
ci_run() { # LOG OUT_DIR VAR=value... -> ci-run.sh's exit code
  local log="$1" out="$2"
  shift 2
  env PATH="${TMPDIR}/bin:${PATH}" \
    WARDYN_URL="${BASE_URL}" \
    WARDYN_CI_TOKEN="${ADMIN_TOKEN}" \
    WARDYN_CI_TIMEOUT="${WARDYN_CI_TIMEOUT:-10m}" \
    WARDYN_CI_OUT="${out}" \
    "$@" ./scripts/ci-run.sh 2>&1 | tee "${log}"
  return "${PIPESTATUS[0]}"
}
# Never an ambient WARDYN_CI_OUT: these directories are deleted below.
MODEL_OUT="${EVIDENCE_DIR}/ci-artifacts-model"
MCP_OUT="${EVIDENCE_DIR}/ci-artifacts-mcp"
rm -rf "${MODEL_OUT}" "${MCP_OUT}"

step "run 1: a claude-code run on ${PROVIDER_ID}; its model call goes through ${LLM_HOST}"
ci_run "${EVIDENCE_DIR}/ci-run-model.log" "${MODEL_OUT}" \
  WARDYN_CI_MODEL_PROVIDER="${PROVIDER_ID}" WARDYN_CI_TASK="say hello" \
  WARDYN_CI_POLICY_FILE="$(policy_file "${LLM_HOST}")"
model_rc=$?

# The task runs under sh -c in the sandbox: initialize, keep the session id the gateway hands back,
# say the session is initialized, then one tools/call. The echo tool answers "echo: <text>".
MCP_URL="https://${MCP_HOST}:${GATEWAY_PORT}/mcp"
MCP_TASK="U='${MCP_URL}'
$(cat <<'EOF'
set -e
J='Content-Type: application/json'
A='Accept: application/json, text/event-stream'
sid=$(curl -sS -D - -o /dev/null -H "$J" -H "$A" -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"gateway-job","version":"0"}}}' "$U" | tr -d '\r' | awk 'tolower($1)=="mcp-session-id:"{print $2}')
test -n "$sid"
curl -sS -o /dev/null -H "$J" -H "$A" -H "Mcp-Session-Id: $sid" -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$U"
curl -sS -H "$J" -H "$A" -H "Mcp-Session-Id: $sid" -H 'MCP-Protocol-Version: 2025-06-18' -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"from-the-job"}}}' "$U" | grep 'echo: from-the-job' >/dev/null
EOF
)"
step "run 2: an exec run whose one MCP tool call goes through ${MCP_HOST}"
ci_run "${EVIDENCE_DIR}/ci-run-mcp.log" "${MCP_OUT}" \
  WARDYN_CI_TASK_MODE=exec WARDYN_CI_TASK="${MCP_TASK}" \
  WARDYN_CI_POLICY_FILE="$(policy_file "${MCP_HOST}")"
mcp_rc=$?

# ── 6. what the runs prove ──────────────────────────────────────────────────
# The fake is not a model, and a claude-code run's exit code says nothing about the gateway: the
# model run only has to have launched and ended. The exec run's task fails unless the tool answered,
# so its exit code is part of the proof.
step "asserting the runs' audit rows (model run exit ${model_rc}, MCP run exit ${mcp_rc})"
fail=0
check() { # WHAT CONDITION-EXIT-CODE
  if [[ "$2" -eq 0 ]]; then echo "  ok: $1"; else echo "  FAIL: $1" >&2; fail=1; fi
}
[[ -s "${MODEL_OUT}/run.json" && -s "${MODEL_OUT}/audit.json" ]]; check "the model run launched and its record was collected" $?
jq -e --arg p "${PROVIDER_ID}" '.model_provider_id == $p and (.state == "COMPLETED" or .state == "FAILED")' "${MODEL_OUT}/run.json" >/dev/null 2>&1
check "the model run used ${PROVIDER_ID} and reached a terminal state" $?
jq -e --arg h "${LLM_HOST}" 'any(.[]; .action == "egress.allow" and .data.host == $h and ((.data.path // "") | endswith("/v1/messages")))' "${MODEL_OUT}/audit.json" >/dev/null 2>&1
check "audit: the proxy let the model call through to ${LLM_HOST}" $?
[[ -s "${MCP_OUT}/run.json" && -s "${MCP_OUT}/audit.json" ]]; check "the MCP run launched and its record was collected" $?
jq -e '.state == "COMPLETED"' "${MCP_OUT}/run.json" >/dev/null 2>&1
check "the MCP run completed (its task fails unless the tool answered)" $?
[[ "${mcp_rc}" -eq 0 ]]; check "the MCP run exited 0" $?
jq -e --arg h "${MCP_HOST}" 'any(.[]; .action == "egress.allow" and .data.host == $h)' "${MCP_OUT}/audit.json" >/dev/null 2>&1
check "audit: the proxy let the tool call through to ${MCP_HOST}" $?

step "reading the gateway's access log"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs "deployment/${GATEWAY}" >"${EVIDENCE_DIR}/gateway.log" 2>&1 \
  || die "could not read the gateway's log"
[[ "$(grep 'route=default/llm ' "${EVIDENCE_DIR}/gateway.log" | grep -E 'http\.path=/v1/messages(\?[^ ]*)?( |$)' | grep -c 'http.status=200')" -ge 1 ]]
check "gateway log: the model route served /v1/messages with 200" $?
[[ "$(grep 'route=default/mcp ' "${EVIDENCE_DIR}/gateway.log" | grep 'mcp.method.name=tools/call' | grep -c 'http.status=200')" -ge 1 ]]
check "gateway log: the MCP route served a tools/call with 200" $?

# The fields wardyn_sub, wardyn_run and wardyn_call are added by the gateway's config
# (deploy/kind/gateway/gateway.yaml) from the validated token and the request. A value may print
# quoted or bare, so both are accepted.
RUN_ID="$(jq -r '.id // empty' "${MODEL_OUT}/run.json" 2>/dev/null)"
[[ -n "${RUN_ID}" ]]; check "the model run has an id to look for in the gateway's log" $?
model_lines="$(grep 'route=default/llm ' "${EVIDENCE_DIR}/gateway.log" | grep -E 'http\.path=/v1/messages(\?[^ ]*)?( |$)' | grep 'http.status=200' || true)"
[[ -n "${RUN_ID}" ]] && grep -E "wardyn_sub=\"?admin-token\"?([[:space:]]|\$)" <<<"${model_lines}" | grep -E "wardyn_run=\"?${RUN_ID}\"?([[:space:]]|\$)" >/dev/null
check "gateway log: the model line carries the validated token's subject (admin-token) and the run id ${RUN_ID}" $?

# The call id: Wardyn made it, sent it as X-Wardyn-Call-Id and recorded it on the audit row, and the
# gateway logged the header it received. At least one id must be on both.
call_ids="$(jq -r --arg h "${LLM_HOST}" '[.[] | select(.action == "egress.allow" and .data.host == $h) | .data.gateway_call_id // empty] | unique | .[]' "${MODEL_OUT}/audit.json" 2>/dev/null)"
[[ -n "${call_ids}" ]]; check "audit: the model run's egress.allow rows carry a gateway_call_id ($(wc -l <<<"${call_ids}" | tr -d ' ') distinct)" $?
joined=0
while IFS= read -r id; do
  [[ -n "${id}" ]] || continue
  if grep 'route=default/llm ' "${EVIDENCE_DIR}/gateway.log" | grep -E "wardyn_call=\"?${id}\"?([[:space:]]|\$)" >/dev/null; then joined=$((joined + 1)); fi
done <<<"${call_ids}"
[[ "${joined}" -ge 1 ]]; check "the gateway's X-Wardyn-Call-Id equals the audit row's gateway_call_id (${joined} id(s) on both)" $?

step "reading the fake's counters (/_seen)"
port_forward PF_PID PF_PORT "${TMPDIR}/pf-fake.log" --context "${CONTEXT}" -n "${NAMESPACE}" port-forward "svc/${FAKE_SVC}" :8080 \
  || die "the port-forward to the fake named no local port: $(cat "${TMPDIR}/pf-fake.log")"
seen=""
for _ in $(seq 1 15); do
  seen="$(curl -sf --max-time 3 "http://127.0.0.1:${PF_PORT}/_seen" 2>/dev/null)" && break
  sleep 1
done
[[ -n "${seen}" ]] || die "the fake's /_seen never answered"
printf '%s\n' "${seen}" >"${EVIDENCE_DIR}/fake-seen.json"
# The gateway moves a bearer token to the x-api-key header an Anthropic upstream expects, so the
# value is looked for in every header of a model request rather than in one.
jq -e --arg c "${CREDENTIAL}" '.model_calls >= 1 and any(.requests[]; .route == "model" and .path == "/v1/messages" and any(.headers[][]; contains($c)))' <<<"${seen}" >/dev/null
check "the fake received a model call carrying the made-up credential ($(jq -c '{model_calls}' <<<"${seen}"))" $?
jq -e '.tool_calls == 1' <<<"${seen}" >/dev/null
check "the fake's tool was called exactly once ($(jq -c '{tool_calls}' <<<"${seen}"))" $?
jq -e '[.requests[] | select(.route == "model")] | length >= 1 and all(.headers | has("x-wardyn-identity") | not)' <<<"${seen}" >/dev/null
check "the fake received no x-wardyn-identity header: the gateway consumed it" $?
# The paths a harness reached beyond the model route's own: the evidence for what the gateway
# does with them (docs/operations/ai-gateway.md).
echo "  paths the fake saw outside its two routes: $(jq -c '[.requests[] | select(.route == "other") | .path] | unique' <<<"${seen}")"

# ── 7. the gateway refuses a call that carries no valid token ───────────────
# Direct calls to the model route from this machine, through a port-forward: the Host header names
# the route, and the throwaway CA is the trust anchor.
step "calling the model route directly: no token, and a token another key signed"
port_forward GW_PF_PID GW_PF_PORT "${TMPDIR}/pf-gw.log" --context "${CONTEXT}" -n "${NAMESPACE}" port-forward "svc/llm-gw" ":${GATEWAY_PORT}" \
  || die "the port-forward to the gateway named no local port: $(cat "${TMPDIR}/pf-gw.log")"
b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
openssl ecparam -name prime256v1 -genkey -noout -out "${TMPDIR}/other.key" 2>/dev/null || die "could not make a second signing key"
# The header names the key set's real kid, so the gateway has the right public key to check against
# and the 401 comes from the signature, not from an unknown key.
real_kid="$(jq -r '.keys[0].kid // empty' "${EVIDENCE_DIR}/jwks.json")"
[[ -n "${real_kid}" ]] || die "the key set at ${JWKS_PATH} names no kid"
header="$(jq -cn --arg k "${real_kid}" '{alg:"ES256",typ:"JWT",kid:$k}' | b64url)"
payload="$(jq -cn --arg iss "${ISSUER}" --arg aud "${AUDIENCE}" --argjson exp "$(($(date +%s) + 300))" \
  '{iss:$iss,aud:$aud,sub:"admin-token",wardyn_run:"not-a-run",exp:$exp}' | b64url)"
# openssl signs in DER; a JWT's ES256 signature is r and s as two 32-byte integers.
sig="$(printf '%s.%s' "${header}" "${payload}" | openssl dgst -sha256 -sign "${TMPDIR}/other.key" \
  | python3 -I -c 'import sys; d=sys.stdin.buffer.read(); i=3 if d[1]<128 else 4; rl=d[i]; r=d[i+1:i+1+rl]; j=i+1+rl; sl=d[j+1]; s=d[j+2:j+2+sl]; sys.stdout.buffer.write(r.lstrip(b"\0").rjust(32,b"\0")+s.lstrip(b"\0").rjust(32,b"\0"))' \
  | b64url)"
[[ -n "${sig}" ]] || die "could not sign the second key's token"
direct() { # [HEADER...] -> HTTP code
  curl -sS --max-time "${CURL_MAX_TIME}" -o /dev/null -w '%{http_code}' --cacert "${CERTS}/ca.crt" \
    --resolve "${LLM_HOST}:${GW_PF_PORT}:127.0.0.1" -H "Host: ${LLM_HOST}" -H 'Content-Type: application/json' "$@" \
    -d '{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}' "https://${LLM_HOST}:${GW_PF_PORT}/v1/messages" 2>/dev/null
}
no_token=""
for _ in $(seq 1 15); do
  no_token="$(direct)" && [[ "${no_token}" != "000" ]] && break
  sleep 1
done
[[ "${no_token}" == "401" ]]; check "a direct call with no x-wardyn-identity header is answered 401 (got ${no_token})" $?
other_key="$(direct -H "x-wardyn-identity: ${header}.${payload}.${sig}")"
[[ "${other_key}" == "401" ]]; check "a direct call with a token another key signed is answered 401 (got ${other_key})" $?
kill "${GW_PF_PID}" 2>/dev/null

[[ "${fail}" -eq 0 ]] || die "the gateway path was not proven — see the FAIL lines above and ${EVIDENCE_DIR}"
echo "kind-gateway-run: PASS"
