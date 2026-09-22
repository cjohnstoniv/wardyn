#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# The kind walk's AZURE DEVOPS PROFILE (WARDYN_KIND_SSO_PROFILE=ado), run by
# scripts/kind-sso-walk.sh. The question it answers is the one only a real
# cluster can: when a MEMBER signs in to the console through Entra, does the
# login itself capture THEIR Azure DevOps credential, and does THEIR run then
# reach Azure DevOps as THEM — REST and git — with nothing in the sandbox?
#
# It runs against the cluster `make kind-quickstart` + `WARDYN_KIND_SSO_PROFILE=ado
# make kind-sso` leave behind, and never creates or deletes one. Everything
# Microsoft is test/adofake/cmd, one pod reached as a FORWARD PROXY: wardynd
# dials it as WARDYN_DAEMON_PROXY_URL (values-ado.yaml), the run's proxy sidecar
# as site-config's upstream_proxy_url, and this script through a port-forward.
# Each then CONNECTs to login.microsoftonline.com or dev.azure.com by the real
# name and trusts the leaf through the walk CA (WARDYN_TRUSTED_CA_FILE). No
# product code is pointed at a test-only address.
#
# THE ORDER IS THE ASSERTION ABOUT WHO HOLDS A CREDENTIAL. The admin signs in
# FIRST, before any Azure DevOps row exists, then writes the row from that
# session — exactly how an organisation turns this on — and only then does the
# member sign in. So the member's login is the one widened, and the walk asserts
# the store holds a blob for the member and none for the admin, and that the
# fake Azure DevOps saw the member's subject and never the admin's.
#
# The browser is curl: the fake's /authorize is a picker (entrafake
# SetIdentities), so each sign-in is three GETs and nothing is typed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"
. "${ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
for bin in kubectl helm curl jq; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
KIND_NODE="${WARDYN_KIND_SSO_NODE:-${CLUSTER}-control-plane}"
NAMESPACE="wardyn"
RUNS_NAMESPACE="wardyn-runs"
RELEASE="wardyn"
# Pinned: the fake's one accepted redirect and values-ado.yaml's OIDC callback.
HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8580}"
[[ "${HTTP_PORT}" == "8580" ]] || die "the ado profile's callback is pinned to http://localhost:8580 (WARDYN_QUICKSTART_HTTP_PORT=${HTTP_PORT})"
BASE_URL="http://localhost:${HTTP_PORT}"
WARDYND_IMAGE="wardyn/wardynd:quickstart"
PROXY_IMAGE="wardyn/wardyn-proxy:quickstart"
FAKE_IMAGE="wardyn/test-adofake:local"
FAKE_SVC="wardyn-test-adofake"
FAKE_HOST="${FAKE_SVC}.${NAMESPACE}.svc.cluster.local"
FAKE_PROXY="http://${FAKE_HOST}:3128"
FAKE_PF_PORT="${WARDYN_KIND_SSO_SEEN_PORT:-8590}"
FAKE_LOCAL="http://127.0.0.1:${FAKE_PF_PORT}"
TENANT="11111111-2222-3333-4444-555555555555"
CLIENT_ID="aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
ADMIN_USER="admin@wardyn.test"
MEMBER_USER="member@wardyn.test"
ADMIN_SUB="entra-sub-admin-0001"
MEMBER_SUB="entra-sub-member-0001"
ORG="contoso"
REPO_URL="https://dev.azure.com/${ORG}/proj/_git/app"
ROW_ID="ado-walk"
EVIDENCE_DIR="${WARDYN_KIND_SSO_EVIDENCE:-${ROOT}/local/evidence/kind-ado}"
mkdir -p "${EVIDENCE_DIR}"
WORK="$(mktemp -d)"
pf_pid=""
cleanup() {
  [[ -n "${pf_pid}" ]] && kill "${pf_pid}" 2>/dev/null
  rm -rf "${WORK}"
  return 0
}
trap cleanup EXIT

# ── 1. the cluster and the profile are up ───────────────────────────────────
step "checking the cluster and the Azure DevOps profile"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get deployment "${RELEASE}" >/dev/null 2>&1 \
  || die "no wardyn release on ${CONTEXT} — run: WARDYN_QUICKSTART_CLUSTER=${CLUSTER} WARDYN_QUICKSTART_HTTP_PORT=8580 WARDYN_QUICKSTART_SSH_PORT=2522 make kind-quickstart"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get svc "${FAKE_SVC}" >/dev/null 2>&1 \
  || die "the fake Microsoft is not installed — run: WARDYN_KIND_SSO_PROFILE=ado WARDYN_QUICKSTART_CLUSTER=${CLUSTER} WARDYN_QUICKSTART_HTTP_PORT=8580 make kind-sso"
health="$(curl -s "${BASE_URL}/healthz" || true)"
[[ "${health}" == *'"runner":"k8s"'* ]] || die "${BASE_URL}/healthz did not answer with runner=k8s. Body: ${health}"

if [[ "${WARDYN_KIND_SSO_REBUILD:-}" == "1" ]]; then
  command -v kind >/dev/null 2>&1 || die "kind not found on PATH (needed for WARDYN_KIND_SSO_REBUILD=1)"
  step "rebuilding wardynd, wardyn-proxy and the fake from this tree (no agent image is retagged)"
  for spec in "deploy/compose/Dockerfile.wardynd ${WARDYND_IMAGE} wardynd" \
              "deploy/compose/Dockerfile.proxy ${PROXY_IMAGE} proxy" \
              "test/adofake/cmd/Dockerfile ${FAKE_IMAGE} adofake"; do
    read -r df tag name <<<"${spec}"
    docker build -f "${df}" -t "${tag}" . >"${EVIDENCE_DIR}/rebuild-${name}.log" 2>&1 \
      || { tail -30 "${EVIDENCE_DIR}/rebuild-${name}.log" >&2; die "${tag} build failed"; }
    kind load docker-image "${tag}" --name "${CLUSTER}" >/dev/null || die "kind load ${tag} failed"
  done
fi

step "recording the image provenance into ${EVIDENCE_DIR}/images.txt"
NODE_IMAGES="$(docker exec "${KIND_NODE}" ctr -n k8s.io images ls 2>/dev/null || true)"
{
  echo "walk tree:       $(git -C "${ROOT}" rev-parse HEAD 2>/dev/null)"
  echo "walk tree dirty: $(git -C "${ROOT}" status --porcelain 2>/dev/null | wc -l) file(s)"
  echo "rebuilt:         ${WARDYN_KIND_SSO_REBUILD:-0}"
  for img in "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${FAKE_IMAGE}"; do
    h="$(docker image inspect "${img}" --format '{{.Id}}' 2>/dev/null)"
    n="$(printf '%s\n' "${NODE_IMAGES}" | awk -v r="docker.io/${img}" '$1==r {print $3}' | head -1)"
    a="NO"; [[ -n "${h}" && "${h}" == "${n}" ]] && a="yes"
    printf '%-34s host %-72s node %-72s agree %s\n' "${img}" "${h:-(absent)}" "${n:-(absent)}" "${a}"
  done
} | tee "${EVIDENCE_DIR}/images.txt"

# ── 2. a fresh install, every run, with an admin token as the control channel ─
# The same reasoning as the default profile's step 3b: the token only reaches a
# pod that restarts, and a clean database is the honest precondition for "the
# member's login is what stored the credential". The fake restarts too, so its
# /_seen means THIS walk.
ADMIN_TOKEN="walk-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
step "setting the walk's admin token (helm upgrade --reuse-values)"
helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn -n "${NAMESPACE}" --reuse-values \
  --set "auth.adminToken.value=${ADMIN_TOKEN}" >"${EVIDENCE_DIR}/helm-upgrade.log" 2>&1 \
  || { tail -30 "${EVIDENCE_DIR}/helm-upgrade.log" >&2; die "helm upgrade failed"; }
step "resetting to a fresh install (postgres, the fake, then wardynd)"
for dep in postgres "${FAKE_SVC}" "${RELEASE}"; do
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout restart "deployment/${dep}" >/dev/null \
    || die "could not restart ${dep}"
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${dep}" --timeout=300s \
    || { kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs "deployment/${dep}" --tail=40 >&2; die "${dep} did not come back"; }
done

# ── 3. the org's egress goes through the same middlebox ─────────────────────
step "seeding site-config upstream_proxy_url (${FAKE_PROXY})"
current=""
for _ in $(seq 1 30); do
  body="$(curl -s -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE_URL}/api/v1/site-config")"
  if jq -e 'type == "object" and (has("error") | not)' >/dev/null 2>&1 <<<"${body}"; then current="${body}"; break; fi
  sleep 2
done
[[ -n "${current}" ]] || die "GET /site-config never answered with this run's admin token (last body: ${body:-<empty>})"
merged="$(jq --arg u "${FAKE_PROXY}" '.upstream_proxy_url = $u' <<<"${current}")"
code="$(curl -s -o "${EVIDENCE_DIR}/site-config-put.json" -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" -H 'Content-Type: application/json' -d "${merged}" "${BASE_URL}/api/v1/site-config")"
[[ "${code}" == "200" ]] || { cat "${EVIDENCE_DIR}/site-config-put.json" >&2; die "PUT /site-config answered ${code}"; }

step "opening the port-forward to the fake (${FAKE_LOCAL}: the browser's route to Microsoft, and /_seen)"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" port-forward "svc/${FAKE_SVC}" "${FAKE_PF_PORT}:3128" >/dev/null 2>&1 &
pf_pid=$!
for _ in $(seq 1 30); do curl -sf "${FAKE_LOCAL}/healthz" >/dev/null 2>&1 && break; sleep 1; done
curl -sf "${FAKE_LOCAL}/healthz" >/dev/null 2>&1 || die "the fake never answered on ${FAKE_LOCAL} (is ${FAKE_PF_PORT} taken?)"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret wardyn-test-adofake-ca -o jsonpath='{.data.ca\.crt}' \
  | base64 -d > "${WORK}/ca.crt" || die "could not read the walk CA"

# signin <username> <cookie jar>: the console's own OIDC flow, driven by curl.
# The console hop is direct; the Microsoft hop goes through the fake's proxy and
# trusts the walk CA, exactly as wardynd's own leg does.
signin() {
  local user="$1" jar="$2" loc page pick cb final
  loc="$(curl -s -o /dev/null -c "${jar}" -b "${jar}" -w '%{redirect_url}' "${BASE_URL}/auth/login")"
  [[ "${loc}" == "https://login.microsoftonline.com/${TENANT}/oauth2/v2.0/authorize?"* ]] \
    || die "${user}: /auth/login did not redirect to the Entra tenant (got: ${loc:-<none>})"
  printf '%s\n' "${loc}" >"${EVIDENCE_DIR}/authorize-${user%%@*}.url"
  page="$(curl -s --proxy "${FAKE_LOCAL}" --cacert "${WORK}/ca.crt" "${loc}")"
  pick="$(grep -o 'href="[^"]*"' <<<"${page}" | sed 's/^href="//; s/"$//; s/&amp;/\&/g' | grep -F "login_hint=${user/@/%40}" | head -1)"
  [[ -n "${pick}" ]] || die "${user}: the picker offered no link for this person. Page: ${page}"
  cb="$(curl -s -o /dev/null --proxy "${FAKE_LOCAL}" --cacert "${WORK}/ca.crt" -w '%{redirect_url}' "https://login.microsoftonline.com${pick}")"
  [[ "${cb}" == "${BASE_URL}/auth/callback?"* ]] || die "${user}: the picker did not return to the console callback (got: ${cb:-<none>})"
  final="$(curl -s -o "${EVIDENCE_DIR}/callback-${user%%@*}.body" -c "${jar}" -b "${jar}" -w '%{http_code} %{redirect_url}' "${cb}")"
  echo "${user}: callback answered ${final}"
  [[ "${final}" == 30* ]] || die "${user}: the console callback refused the sign-in (${final}; body in ${EVIDENCE_DIR}/callback-${user%%@*}.body)"
}
me_role() { curl -s -b "$1" "${BASE_URL}/api/v1/me" | tee "${EVIDENCE_DIR}/me-$2.json" | jq -r '.role // empty'; }

# ── 4. the admin signs in, BEFORE any Azure DevOps row exists ───────────────
step "the admin signs in through the fake Entra tenant"
signin "${ADMIN_USER}" "${WORK}/admin.jar"
[[ "$(me_role "${WORK}/admin.jar" admin)" == "admin" ]] || die "the admin's session is not role admin (see ${EVIDENCE_DIR}/me-admin.json)"

step "the admin configures the per-person Azure DevOps row and onboards the repository, from that session"
providers="$(jq -n --arg id "${ROW_ID}" --arg org "https://dev.azure.com/${ORG}" --arg t "${TENANT}" --arg c "${CLIENT_ID}" '{
  git: [{id: $id, kind: "azure_devops", base_urls: [$org], lanes: ["entra"], credential_source: "per_user",
         entra: {tenant_id: $t, client_id: $c, capability_ceiling: ["read"], default_profile: ["read"]}}]}')"
code="$(curl -s -o "${EVIDENCE_DIR}/workspace-providers-put.json" -w '%{http_code}' -X PUT -b "${WORK}/admin.jar" \
  -H 'Content-Type: application/json' -d "${providers}" "${BASE_URL}/api/v1/workspace-providers")"
[[ "${code}" == "200" ]] || { cat "${EVIDENCE_DIR}/workspace-providers-put.json" >&2; die "PUT /workspace-providers answered ${code}"; }
code="$(curl -s -o "${EVIDENCE_DIR}/workspace-create.json" -w '%{http_code}' -X POST -b "${WORK}/admin.jar" \
  -H 'Content-Type: application/json' -d "$(jq -n --arg r "${REPO_URL}" '{name: "ado-walk", sources: [{type: "repo", source: $r}]}')" \
  "${BASE_URL}/api/v1/workspaces")"
[[ "${code}" == 20* ]] || { cat "${EVIDENCE_DIR}/workspace-create.json" >&2; die "POST /workspaces answered ${code}"; }
WORKSPACE_ID="$(jq -r '.id // empty' "${EVIDENCE_DIR}/workspace-create.json")"
[[ -n "${WORKSPACE_ID}" ]] || die "POST /workspaces returned no id (see ${EVIDENCE_DIR}/workspace-create.json)"

# ── 5. the member signs in: the login IS the capture ────────────────────────
step "the member signs in through the fake Entra tenant (the login asks for the row's Azure DevOps scopes)"
signin "${MEMBER_USER}" "${WORK}/member.jar"
grep -q 'vso.code' "${EVIDENCE_DIR}/authorize-member.url" \
  || die "the member's authorization request carried no Azure DevOps scope — the login was not widened"
grep -q 'vso.code' "${EVIDENCE_DIR}/authorize-admin.url" \
  && die "the admin's authorization request carried an Azure DevOps scope before any row existed"
[[ "$(me_role "${WORK}/member.jar" member)" == "member" ]] || die "the member's session is not role member (see ${EVIDENCE_DIR}/me-member.json)"

step "asserting the capture: one blob, the member's, and none for the admin"
curl -s -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE_URL}/api/v1/audit?action=scm.ado.signin.captured" \
  >"${EVIDENCE_DIR}/audit-capture.json"
jq -e --arg m "${MEMBER_SUB}" '[.. | objects | select(.action? == "scm.ado.signin.captured" and .outcome? == "success") | .actor] == [$m]' \
  "${EVIDENCE_DIR}/audit-capture.json" >/dev/null \
  || die "the capture audit is not exactly one success for the member (see ${EVIDENCE_DIR}/audit-capture.json)"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" exec deployment/postgres -- psql -U wardyn -d wardyn -tAc \
  "select owned_by from secrets where name = 'wardyn-harness-ado-${ROW_ID}-oauth' order by owned_by" \
  >"${EVIDENCE_DIR}/blob-owners.txt" 2>&1 || die "could not read the secret store (see ${EVIDENCE_DIR}/blob-owners.txt)"
[[ "$(tr -d '[:space:]' <"${EVIDENCE_DIR}/blob-owners.txt")" == "${MEMBER_SUB}" ]] \
  || die "the stored Azure DevOps blob's owners are not exactly the member (got: $(cat "${EVIDENCE_DIR}/blob-owners.txt"))"

# ── 6. the member's run redeems it ──────────────────────────────────────────
# An exec run: a plain command in the governed sandbox. The sandbox holds only
# the placeholder; the proxy attaches the member's bearer on the wire. On the
# claude-code image, not agent-base: agent-base's agent-run stub goes straight
# to the command without configuring the git broker's insteadOf, so its git
# would dial dev.azure.com through the REST gate, which refuses git.
step "the member launches a run on ${REPO_URL} (a REST read and git ls-remote)"
task="set -e
curl -sS -f -o /dev/null -w 'ado-rest %{http_code}\n' 'https://dev.azure.com/${ORG}/_apis/projects?api-version=7.1'
git ls-remote '${REPO_URL}' > /tmp/ls-remote.txt
grep refs/heads/main /tmp/ls-remote.txt
echo ADO-WALK-OK"
run_body="$(jq -n --arg task "${task}" --arg ws "${WORKSPACE_ID}" '{
  agent: "claude-code", task_mode: "exec", task: $task, title: "kind ado walk", workspace_id: $ws}')"
code="$(curl -s -o "${EVIDENCE_DIR}/run-create.json" -w '%{http_code}' -X POST -b "${WORK}/member.jar" \
  -H 'Content-Type: application/json' -d "${run_body}" "${BASE_URL}/api/v1/runs")"
[[ "${code}" == 20* ]] || { cat "${EVIDENCE_DIR}/run-create.json" >&2; die "POST /runs answered ${code}"; }
RUN_ID="$(jq -r '.id // .run.id // empty' "${EVIDENCE_DIR}/run-create.json")"
[[ -n "${RUN_ID}" ]] || die "POST /runs returned no run id (see ${EVIDENCE_DIR}/run-create.json)"
echo "run ${RUN_ID}"

# The run's pods do not outlive it for long, so their logs are read WHILE it
# runs: the agent container's output is the only place the task's own lines
# (the REST status, the ls-remote refs) are visible.
status=""
for _ in $(seq 1 150); do
  status="$(curl -s -b "${WORK}/member.jar" "${BASE_URL}/api/v1/runs/${RUN_ID}" | tee "${EVIDENCE_DIR}/run.json" | jq -r '.state // empty')"
  for pod in $(kubectl --context "${CONTEXT}" -n "${RUNS_NAMESPACE}" get pods -o name 2>/dev/null | grep "${RUN_ID:0:8}"); do
    logs="$(kubectl --context "${CONTEXT}" -n "${RUNS_NAMESPACE}" logs "${pod}" --all-containers 2>/dev/null)"
    [[ -n "${logs}" ]] && printf '%s\n' "${logs}" >"${EVIDENCE_DIR}/run-${pod#pod/}.log"
  done
  case "${status}" in COMPLETED|FAILED|KILLED|STOPPED|ARCHIVED) break ;; esac
  sleep 4
done
echo "run status: ${status}"
curl -s -b "${WORK}/member.jar" "${BASE_URL}/api/v1/runs/${RUN_ID}/recording" >"${EVIDENCE_DIR}/run-recording.txt" 2>/dev/null || true
curl -s -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE_URL}/api/v1/audit?run_id=${RUN_ID}" >"${EVIDENCE_DIR}/audit-run.json"
[[ "${status}" == "COMPLETED" ]] \
  || die "the member's run ended ${status:-<unknown>} (see ${EVIDENCE_DIR}/run.json, run-recording.txt, audit-run.json)"

step "asserting the redemption: the injection resolved for this run"
jq -e '[.. | objects | select(.action? == "secret.read" and .target? == "azure-devops-entra-access-token" and .outcome? == "success")] | length >= 1' \
  "${EVIDENCE_DIR}/audit-run.json" >/dev/null \
  || die "no successful Azure DevOps injection resolve is on the run's audit trail (see ${EVIDENCE_DIR}/audit-run.json)"

# ── 7. what Azure DevOps saw: the one observation that is not Wardyn's ──────
step "reading the fake's /_seen"
curl -s "${FAKE_LOCAL}/_seen" | tee "${EVIDENCE_DIR}/seen.json"; echo
jq -e --arg m "${MEMBER_SUB}" '.callers[$m].endpoints["projects.get"] >= 1 and .callers[$m].endpoints["git.advertise"] >= 1 and .callers[$m].authorized == .callers[$m].requests' \
  "${EVIDENCE_DIR}/seen.json" >/dev/null \
  || die "the fake Azure DevOps did not see the member's REST read AND git advertisement, all authorized (see ${EVIDENCE_DIR}/seen.json)"
jq -e --arg a "${ADMIN_SUB}" '.callers | has($a) | not' "${EVIDENCE_DIR}/seen.json" >/dev/null \
  || die "the admin's subject reached the fake Azure DevOps (see ${EVIDENCE_DIR}/seen.json)"

echo
echo "kind-sso-walk (ado): PASS — evidence in ${EVIDENCE_DIR}"
