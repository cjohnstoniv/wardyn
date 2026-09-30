#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# T-21 (#681): ci-mode-dogfood's MODEL-RUN variant. scripts/ci-run.sh in its
# "existing control plane" mode (WARDYN_URL + a CI principal's own token +
# WARDYN_CI_MODEL_PROVIDER, docs/CI.md "CI's identity"), driving a real HARNESS
# run (claude-code) whose one model call lands on a fake, so the whole
# model-provider path — the provider row, the caller's own stored credential,
# dispatch resolving the lane, the proxy dialing the provider's base_url — is
# proven nightly with no vendor credential and no internet.
#
# WHY KIND AND NOT COMPOSE. A single-host compose stack cannot host this proof:
# the fake would have to sit on a control-plane network the run's proxy refuses
# to dial (onOwnSubnetOrControlPlane), or be exposed on a host address. On the
# kind cluster the fake is an ordinary in-cluster Service on the SERVICE CIDR,
# which an operator-declared site-config `internal_hosts` rule can lift
# (scripts/kind-sso-walk.sh's fourth precondition says why), and nothing is
# published on the host.
#
# NO NEW SERVER. The fake is test/awsssofake's bedrock-runtime stub, the same
# one the kind SSO walk spends its role credential on, deployed by
# deploy/kind/sso/awsssofake.yaml. The provider is a bedrock_bearer row whose
# bedrock.base_url points at it (plain http, which the daemon accepts only under
# WARDYN_ALLOW_TEST_ENDPOINTS — the same hatch the SSO walk sets).
#
# NO SSO. This cluster is the plain `make kind-quickstart` one: no OIDC, so the
# admin token resolves to the fixed subject "admin-token" (runIdentitySubject,
# pinned by internal/api/run_identity_subject_test.go) and CAN hold a model
# credential of its own — which is what lets the same token be the CI
# principal here, exactly as scripts/test-kind-sso-provider-seed.sh's
# PUT /model-providers/{id}/credential does. Under OIDC the same token is
# not_applicable and ci-run.sh refuses it by name.
#
# The assertions are the run's audit rows plus the fake's own counter (/_seen),
# never the run's exit code alone: a run that exits 0 without calling the model
# proves nothing (and against this fake the agent exits 1 anyway, see step 6).
#
# GUARD: self-skips unless WARDYN_TEST_K8S=1 (exit 77, common.sh's skip_lane).
# It mutates the cluster it finds (helm upgrade, site-config, and it REPLACES
# the model-provider block and claude-code's default provider),
# so a hand run must also set WARDYN_ACK_SHARED_QUICKSTART=1 to say that cluster
# is disposable; nightly.yml sets it for the hosted run, which creates its own.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-ci-model-run: set WARDYN_TEST_K8S=1 to run the cluster-dependent CI model run (skipping)."
fi
if [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" != "1" ]]; then
  skip_lane "kind-ci-model-run: set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the quickstart cluster this script finds may be mutated — refusing to run against a cluster it did not create otherwise."
fi

wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

for bin in docker kind kubectl helm curl jq go; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
RELEASE="wardyn"
HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
BASE_URL="http://127.0.0.1:${HTTP_PORT}"
FAKE_IMAGE="wardyn/awsssofake:local"
FAKE_SVC="wardyn-awsssofake"
# Service names, never a pod IP: the proxy refuses to lift its own pod subnet.
FAKE_BEDROCK_HOST="${FAKE_SVC}-bedrock.${NAMESPACE}.svc.cluster.local"
PROVIDER_ID="ci-model-fake"
MODEL="us.anthropic.claude-sonnet-4-5-20250929-v1:0"
CURL_MAX_TIME=15
EVIDENCE_DIR="${WARDYN_KIND_CI_MODEL_EVIDENCE:-${ROOT}/local/evidence/kind-ci-model-run}"
mkdir -p "${EVIDENCE_DIR}"
TMPDIR="$(mktemp -d /tmp/wardyn-kind-ci-model-run.XXXXXX)"
PF_PID=""
trap '[[ -n "${PF_PID}" ]] && kill "${PF_PID}" 2>/dev/null; rm -rf "${TMPDIR}"' EXIT

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

# ── 1. the fake, on the cluster ─────────────────────────────────────────────
step "building and loading the fake (${FAKE_IMAGE}, test/awsssofake's bedrock-runtime stub)"
docker build -f test/awsssofake/cmd/Dockerfile -t "${FAKE_IMAGE}" . >"${EVIDENCE_DIR}/fake-build.log" 2>&1 \
  || { tail -20 "${EVIDENCE_DIR}/fake-build.log" >&2; die "docker build of the fake failed"; }
kind load docker-image "${FAKE_IMAGE}" --name "${CLUSTER}" >/dev/null || die "kind load of the fake failed"
kubectl --context "${CONTEXT}" apply -f deploy/kind/sso/awsssofake.yaml >/dev/null || die "could not apply deploy/kind/sso/awsssofake.yaml"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${FAKE_SVC}" --timeout=180s || die "the fake did not become ready"

# ── 2. the test hatch the http:// base_url needs ────────────────────────────
step "enabling WARDYN_ALLOW_TEST_ENDPOINTS (helm upgrade --reuse-values)"
helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn -n "${NAMESPACE}" --reuse-values \
  --set "env.WARDYN_ALLOW_TEST_ENDPOINTS=true" >"${EVIDENCE_DIR}/helm-upgrade.log" 2>&1 \
  || { tail -30 "${EVIDENCE_DIR}/helm-upgrade.log" >&2; die "helm upgrade failed"; }
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=240s || die "wardynd did not come back after the upgrade"

# ── 3. internal_hosts: the proxy's private-address refusal, lifted for the fake
SERVICE_CIDR="$(kubectl --context "${CONTEXT}" -n kube-system get pod \
  -l component=kube-apiserver -o jsonpath='{.items[0].spec.containers[0].command}' 2>/dev/null \
  | jq -r '.[] | select(startswith("--service-cluster-ip-range=")) | sub("^--service-cluster-ip-range=";"")' 2>/dev/null | head -1)"
[[ -n "${SERVICE_CIDR}" ]] || die "could not read --service-cluster-ip-range from the apiserver — do NOT substitute a pod IP or the pod CIDR: the proxy refuses to lift its own subnet"
step "seeding site-config internal_hosts (${FAKE_BEDROCK_HOST} scoped to ${SERVICE_CIDR})"
current=""
for _ in $(seq 1 30); do
  if [[ "$(api GET /site-config)" == "200" ]] && jq -e 'type == "object"' "${TMPDIR}/resp.json" >/dev/null 2>&1; then current="$(cat "${TMPDIR}/resp.json")"; break; fi
  sleep 2
done
[[ -n "${current}" ]] || die "GET /site-config never answered 200"
merged="$(jq --arg h "${FAKE_BEDROCK_HOST}" --arg c "${SERVICE_CIDR}" \
  '.internal_hosts = ((.internal_hosts // []) | map(select(.host_suffix != $h)) + [{host_suffix:$h, cidrs:[$c]}])' <<<"${current}")"
must 200 "PUT /site-config" PUT /site-config "${merged}"

# ── 4. the provider, and the caller's own credential ────────────────────────
step "seeding the model provider (${PROVIDER_ID}: bedrock_bearer -> the fake); no credential stored yet"
must 200 "PUT /model-providers" PUT /model-providers "$(jq -cn --arg id "${PROVIDER_ID}" --arg u "http://${FAKE_BEDROCK_HOST}:8091" --arg m "${MODEL}" \
  '{providers:[{id:$id,name:"CI model fake",kind:"bedrock_bearer",bedrock:{region:"us-east-1",base_url:$u},
    harnesses:[{harness:"claude-code",model:$m}]}]}')"
must 200 "PUT /agent-providers" PUT /agent-providers "$(jq -cn --arg id "${PROVIDER_ID}" '{agents:[{id:"claude-code",default_provider:$id}]}')"

# ── 5. the CLI ci-run.sh drives, and the run itself ─────────────────────────
step "building the wardyn CLI"
go build -o "${TMPDIR}/bin/wardyn" ./cmd/wardyn || die "go build ./cmd/wardyn failed"

# WARDYN_CI_TOKEN is the admin token ON PURPOSE: with no OIDC it IS the
# principal that will hold the credential. ci-run.sh clears WARDYN_ADMIN_TOKEN
# before every call, so this is the only identity it uses.
ci_run() { # LOG -> ci-run.sh's exit code
  env PATH="${TMPDIR}/bin:${PATH}" \
    WARDYN_URL="${BASE_URL}" \
    WARDYN_CI_TOKEN="${ADMIN_TOKEN}" \
    WARDYN_CI_MODEL_PROVIDER="${PROVIDER_ID}" \
    WARDYN_CI_TASK="say hello" \
    WARDYN_CI_TIMEOUT="${WARDYN_CI_TIMEOUT:-10m}" \
    WARDYN_CI_OUT="${CI_OUT}" \
    ./scripts/ci-run.sh 2>&1 | tee "$1"
  return "${PIPESTATUS[0]}"
}
# Never an ambient WARDYN_CI_OUT: this directory is deleted below.
CI_OUT="${EVIDENCE_DIR}/ci-artifacts"
rm -rf "${CI_OUT}"
# A credential left by an earlier run on this cluster survives the provider PUT
# (the UID is kept for the same id), so clear it: 5a needs "no credential".
code="$(api DELETE "/model-providers/${PROVIDER_ID}/credential")"
[[ "${code}" == "204" || "${code}" == "404" ]] || die "could not clear a stale credential for ${PROVIDER_ID} (DELETE answered ${code})"

# ── 5a. no credential yet: ci-run.sh must refuse, naming the provider ────────
# (#681 item 1, live: the script test pins the message against a stub; this is
# the real control plane's answer.) A refusal launches nothing, so no record.
step "scripts/ci-run.sh with NO credential stored — it must refuse, naming ${PROVIDER_ID}, and launch nothing"
ci_run "${EVIDENCE_DIR}/ci-run-no-credential.log"; neg_rc=$?
if [[ "${neg_rc}" -ne 0 ]] && grep -q "${PROVIDER_ID}.*not connected" "${EVIDENCE_DIR}/ci-run-no-credential.log" && [[ ! -e "${CI_OUT}/run.json" ]]; then
  echo "  ok: refused (exit ${neg_rc}) naming ${PROVIDER_ID}, no run launched"
else
  die "with no credential stored ci-run.sh did not refuse naming ${PROVIDER_ID} (exit ${neg_rc}; see ${EVIDENCE_DIR}/ci-run-no-credential.log)"
fi

# ── 5b. the credential, stored as the CI principal; the run ──────────────────
step "storing ${PROVIDER_ID}'s credential as the CI principal, then scripts/ci-run.sh"
must 204 "PUT /model-providers/${PROVIDER_ID}/credential" PUT "/model-providers/${PROVIDER_ID}/credential" '{"value":"fake-bearer-token-not-a-real-secret"}'
ci_run "${EVIDENCE_DIR}/ci-run.log"
ci_rc=$?

# ── 6. what the run proves ──────────────────────────────────────────────────
# The fake is not a model: it answers a streaming call with canned JSON, and
# claude-code refuses that ("expected application/vnd.amazon.eventstream") and
# exits 1. So ci-run.sh's exit code is NOT the assertion — a launched run that
# ended (COMPLETED, or FAILED on that refusal) is. What is asserted instead is
# the path #681 is about: ci-run.sh cleared its provider check, the run was
# launched as the CI principal on that provider, dispatch read THAT principal's
# own stored credential, and the proxy spent it on the fake.
step "asserting the model-provider path (ci-run.sh exit ${ci_rc}; the fake's reply is not a model's)"
fail=0
check() { # WHAT CONDITION-EXIT-CODE
  if [[ "$2" -eq 0 ]]; then echo "  ok: $1"; else echo "  FAIL: $1" >&2; fail=1; fi
}
[[ -s "${CI_OUT}/run.json" && -s "${CI_OUT}/audit.json" ]]; check "ci-run.sh launched a run and collected its record (a setup failure collects none)" $?
grep -q "model provider ${PROVIDER_ID}: connected" "${EVIDENCE_DIR}/ci-run.log"; check "ci-run.sh's provider check passed, naming ${PROVIDER_ID}" $?
jq -e --arg p "${PROVIDER_ID}" '.created_by == "admin-token" and .model_provider_id == $p and (.state == "COMPLETED" or .state == "FAILED")' "${CI_OUT}/run.json" >/dev/null 2>&1
check "the run was launched as the CI principal on ${PROVIDER_ID} and reached a terminal state" $?
jq -e --arg p "${PROVIDER_ID}" 'any(.[]; .action == "secret.read" and .data.provider == $p and .data.owner == "admin-token" and .data.purpose == "proxy-injection")' "${CI_OUT}/audit.json" >/dev/null 2>&1
check "dispatch read the CI principal's own stored credential for ${PROVIDER_ID}, proxy-side" $?
jq -e --arg h "${FAKE_BEDROCK_HOST}" 'any(.[]; .action == "egress.allow" and .data.host == $h and (.data.path | startswith("/model/")))' "${CI_OUT}/audit.json" >/dev/null 2>&1
check "the proxy let a model call through to the fake" $?

step "reading the fake's counter (/_seen)"
PF_PORT="$(pick_free_port)"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" port-forward "svc/${FAKE_SVC}-bedrock" "${PF_PORT}:8091" >/dev/null 2>&1 &
PF_PID=$!
seen=""
for _ in $(seq 1 15); do
  seen="$(curl -sf --max-time 3 "http://127.0.0.1:${PF_PORT}/_seen" 2>/dev/null)" && break
  sleep 1
done
[[ -n "${seen}" ]] || die "the fake's /_seen never answered"
printf '%s\n' "${seen}" >"${EVIDENCE_DIR}/fake-seen.json"
# auth:Bearer means only "an Authorization: Bearer header", which the sandbox's
# own placeholder would also be; that the real key was injected is what the
# secret.read row above proves. $? is captured BEFORE the label's command
# substitution, which would otherwise overwrite it.
jq -e --arg m "${MODEL}" '.bedrock_calls >= 1 and (.bedrock_unattributed["auth:Bearer"] // 0) >= 1 and any(.bedrock_models[]?; . == $m)' <<<"${seen}" >/dev/null
seen_rc=$?
check "the fake received a model call for ${MODEL} with a Bearer header ($(jq -c '{bedrock_calls, bedrock_models}' <<<"${seen}"))" "${seen_rc}"

[[ "${fail}" -eq 0 ]] || die "the model-provider path was not proven — see the FAIL lines above and ${EVIDENCE_DIR}"
echo "kind-ci-model-run: PASS"
