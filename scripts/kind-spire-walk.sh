#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# The SPIRE identity provider, walked on a kind cluster of its own: upstream SPIRE (chart and images pinned
# in deploy/kind/spire/values.yaml), then Wardyn with spire.enabled and the ground-truth sensor on, then
# real runs. docs/operations/spire.md is what it proves.
#
#   step 1  /healthz reports identity spire, source configured.
#   step 2  an exec run reaches RUNNING and completes; its spiffe_id is the agent-run form, with at least two
#       identity.mint rows (the mint and the proxy's start renew).
#   step 3  while the run lives, exactly one SPIRE entry carries its ID, with k8s:ns:, k8s:sa: and both proxy labels.
#   step 4  probe pods with the CSI volume and the proxy's container name: (a) the proxy's labels and account in
#       the runs namespace get the run's X.509-SVID; (b) the same labels and account name in another namespace
#       get none; (c) the proxy's labels on default get none; (d) wardyn.component=agent gets none; (e) the
#       (a) probe gets no control-plane SVID, and that entry selects on wardynd's account. The agent pod has
#       no csi.spiffe.io volume and runs as default.
#   step 5  after the kill the entry is gone, and the (a) probe stops receiving the run's SVID.
#   step 6  20 back-to-back runs leave no entry; none carries the host sensor's nil run id; no binding row is past
#       its lapse.
#   step 7  a workload's two runs carry the workload form, with different IDs and the same workload; the probe's
#       URI SAN is the run's ID.
#
# Bounds are configured limits, overridable: SPIRE_WALK_NEGATIVE_WAIT (step 4b, step 4e, default 10 s: wardynd's boot
# wait for SPIRE) and SPIRE_WALK_REVOKE_BOUND (step 5, default 600 s: a run's X.509-SVID lifetime).
#
# GUARD: self-skips (exit 77) unless WARDYN_TEST_K8S=1 and WARDYN_ACK_SHARED_QUICKSTART=1. It creates and
# deletes its own cluster (never wardyn-quickstart), builds images and installs charts, so the second switch
# says this host may do that. It refuses, before touching anything, when deploy/kind/spire/values.yaml is
# missing or names an image without @sha256, or the chart pins below are empty.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/lib/walk-api.sh
source "${ROOT}/scripts/lib/walk-api.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-spire-walk: set WARDYN_TEST_K8S=1 to run the cluster-dependent SPIRE walk (skipping)."
fi
if [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" != "1" ]]; then
  skip_lane "kind-spire-walk: set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm this host may build images and create its own kind cluster (skipping)."
fi

die() { echo "::error::kind-spire-walk: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

VALUES="deploy/kind/spire/values.yaml"
[[ -f "${VALUES}" ]] || die "${VALUES} is missing: the walk installs SPIRE only from pinned values"
# Every image reference written on one line, as `image: ref` or as a chart's `tag: version@sha256:...`.
unpinned="$(grep -E '^[[:space:]]*(image|tag):[[:space:]]*[^[:space:]{#]' "${VALUES}" | grep -vE '@sha256:[0-9a-f]{64}' || true)"
[[ -z "${unpinned}" ]] || die "an image in ${VALUES} lacks @sha256: ${unpinned}"
grep -qE '^[[:space:]]*(image|tag):.*@sha256:[0-9a-f]{64}' "${VALUES}" || die "${VALUES} names no image pinned by @sha256"

# Upstream charts, by version and sha256 of the release archive. Empty refuses: there is no unpinned install.
SPIRE_CHART_VERSION="${SPIRE_WALK_CHART_VERSION:-0.31.0}"
SPIRE_CHART_SHA256="${SPIRE_WALK_CHART_SHA256:-}"
SPIRE_CRDS_VERSION="${SPIRE_WALK_CRDS_VERSION:-}"
SPIRE_CRDS_SHA256="${SPIRE_WALK_CRDS_SHA256:-}"
CHART_BASE="https://github.com/spiffe/helm-charts-hardened/releases/download"
[[ "${SPIRE_CHART_SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "SPIRE_WALK_CHART_SHA256 is not a sha256: the spire ${SPIRE_CHART_VERSION} chart is not pinned"
[[ -n "${SPIRE_CRDS_VERSION}" && "${SPIRE_CRDS_SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "SPIRE_WALK_CRDS_VERSION and SPIRE_WALK_CRDS_SHA256 are not set: the spire-crds chart is not pinned"

for bin in docker kind kubectl helm curl jq yq go openssl sha256sum; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done
step "host tools"
echo "kind:    $(command -v kind)  $(kind version 2>&1 | head -1)"
echo "helm:    $(command -v helm)  $(helm version --short 2>&1 | head -1)"
echo "kubectl: $(command -v kubectl)  $(kubectl version --client 2>&1 | head -1)"
wardyn_pick_docker_host

CLUSTER="${SPIRE_WALK_CLUSTER:-wardyn-spire-walk}"
[[ "${CLUSTER}" != "wardyn-quickstart" ]] || die "the walk never uses the wardyn-quickstart cluster; pick another SPIRE_WALK_CLUSTER"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
RUNS_NAMESPACE="wardyn-runs"
RELEASE="wardyn"
HTTP_PORT="${SPIRE_WALK_HTTP_PORT:-8380}"
BASE_URL="http://127.0.0.1:${HTTP_PORT}"
SERVER_NS="${SPIRE_WALK_SERVER_NAMESPACE:-spire-server}"
PROXY_SA="wardyn-proxy"
OTHER_NS="spire-walk-other"
NEGATIVE_WAIT="${SPIRE_WALK_NEGATIVE_WAIT:-10}"
REVOKE_BOUND="${SPIRE_WALK_REVOKE_BOUND:-600}"
CURL_MAX_TIME=15
TD="$(yq -r '.global.spire.trustDomain // ""' "${VALUES}")"
KCLUSTER="$(yq -r '.global.spire.clusterName // ""' "${VALUES}")"
[[ -n "${TD}" && -n "${KCLUSTER}" ]] || die "${VALUES} sets no global.spire.trustDomain or global.spire.clusterName"
PARENT_ID="spiffe://${TD}/k8s-cluster/${KCLUSTER}"
CP_ID="spiffe://${TD}/wardyn/control-plane"
NIL_RUN="00000000-0000-0000-0000-000000000000"
EVIDENCE_DIR="${SPIRE_WALK_EVIDENCE:-${ROOT}/local/evidence/kind-spire-walk}"
mkdir -p "${EVIDENCE_DIR}"
TMPDIR="$(mktemp -d /tmp/wardyn-kind-spire-walk.XXXXXX)"
cleanup() {
  if [[ "${SPIRE_WALK_KEEP_CLUSTER:-}" != "1" ]]; then kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1; fi
  rm -rf "${TMPDIR}"
}
trap cleanup EXIT

k() { kubectl --context "${CONTEXT}" "$@"; }
fail=0
check() { # WHAT EXIT-CODE
  if [[ "$2" == 0 ]]; then echo "  ok   $1"; else echo "  FAIL $1" >&2; fail=1; fi
}

# ── 1. cluster and Wardyn (the quickstart, on this walk's own cluster, images and port) ─
step "creating cluster ${CLUSTER} with Wardyn on the k8s runner"
WARDYN_QUICKSTART_CLUSTER="${CLUSTER}" WARDYN_QUICKSTART_IMAGE_TAG="spire-walk" \
  WARDYN_QUICKSTART_HTTP_PORT="${HTTP_PORT}" WARDYN_QUICKSTART_SSH_PORT="${SPIRE_WALK_SSH_PORT:-2322}" \
  ./deploy/kind/quickstart.sh >"${EVIDENCE_DIR}/quickstart.log" 2>&1 || { tail -40 "${EVIDENCE_DIR}/quickstart.log" >&2; die "quickstart failed"; }

# ── 2. SPIRE from pinned charts ──────────────────────────────────────────────
fetch_chart() { # NAME VERSION SHA256 -> path
  local f="${TMPDIR}/$1-$2.tgz"
  retry curl -fsSL -o "${f}" "${CHART_BASE}/$1-$2/$1-$2.tgz" || die "could not download chart $1 $2"
  echo "$3  ${f}" | sha256sum -c - >/dev/null 2>&1 || die "chart $1 $2 does not match its pinned sha256"
  echo "${f}"
}
step "installing spire-crds ${SPIRE_CRDS_VERSION} and spire ${SPIRE_CHART_VERSION} (pinned archives)"
crds="$(fetch_chart spire-crds "${SPIRE_CRDS_VERSION}" "${SPIRE_CRDS_SHA256}")" || exit 1
chart="$(fetch_chart spire "${SPIRE_CHART_VERSION}" "${SPIRE_CHART_SHA256}")" || exit 1
helm --kube-context "${CONTEXT}" upgrade --install spire-crds "${crds}" -n "${SERVER_NS}" --create-namespace --wait \
  >"${EVIDENCE_DIR}/helm-spire-crds.log" 2>&1 || die "spire-crds install failed"
helm --kube-context "${CONTEXT}" upgrade --install spire "${chart}" -n "${SERVER_NS}" -f "${VALUES}" --wait --timeout 10m \
  >"${EVIDENCE_DIR}/helm-spire.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/helm-spire.log" >&2; die "spire install failed"; }
SERVER_POD="$(k -n "${SERVER_NS}" get pods -l app.kubernetes.io/name=server -o jsonpath='{.items[0].metadata.name}')"
[[ -n "${SERVER_POD}" ]] || die "no SPIRE server pod in ${SERVER_NS}"
srv() { k -n "${SERVER_NS}" exec "${SERVER_POD}" -c spire-server -- /opt/spire/bin/spire-server "$@"; }
entries() { srv entry show -spiffeID "$1" -output json 2>/dev/null | jq -c '.entries // []'; } # ID -> JSON array
AGENT_IMAGE="$(k get ds -A -o json | jq -r '[.items[].spec.template.spec.containers[] | select(.name == "spire-agent") | .image][0] // ""')"
[[ "${AGENT_IMAGE}" == *@sha256:* ]] || die "the SPIRE agent DaemonSet does not run a digest-pinned image (${AGENT_IMAGE})"

step "registering the node alias ${PARENT_ID} and wardynd's admin entry ${CP_ID}"
srv entry create -node -spiffeID "${PARENT_ID}" -selector "k8s_psat:cluster:${KCLUSTER}" >/dev/null || die "node alias create failed"
WARDYN_SA="$(k -n "${NAMESPACE}" get deployment "${RELEASE}" -o jsonpath='{.spec.template.spec.serviceAccountName}')"
[[ -n "${WARDYN_SA}" && "${WARDYN_SA}" != "default" && "${WARDYN_SA}" != "${PROXY_SA}" ]] || die "wardynd's account (${WARDYN_SA}) is empty, default or the proxy's"
srv entry create -admin -spiffeID "${CP_ID}" -parentID "${PARENT_ID}" \
  -selector "k8s:ns:${NAMESPACE}" -selector "k8s:sa:${WARDYN_SA}" >/dev/null || die "admin entry create failed"

# ── 3. Wardyn with spire and the ground-truth sensor on ─────────────────────
step "upgrading ${RELEASE} with spire.enabled and groundtruth.k8s.enabled"
helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn -n "${NAMESPACE}" --reuse-values \
  --set spire.enabled=true --set "spire.server.address=spire-server.${SERVER_NS}.svc" --set spire.server.port=443 \
  --set spire.server.targetPort=8081 --set "spire.server.namespace=${SERVER_NS}" --set "spire.parentId=${PARENT_ID}" \
  --set "env.WARDYN_TRUST_DOMAIN=${TD}" --set groundtruth.k8s.enabled=true \
  >"${EVIDENCE_DIR}/helm-wardyn.log" 2>&1 || { tail -30 "${EVIDENCE_DIR}/helm-wardyn.log" >&2; die "wardyn upgrade failed"; }
k -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=300s || { k -n "${NAMESPACE}" logs "deployment/${RELEASE}" --tail=60 >&2; die "wardynd did not boot under spire"; }
if ! k -n "${NAMESPACE}" get ds -o name | grep -q groundtruth; then echo "  note: the ground-truth DaemonSet did not render"; fi
k -n "${NAMESPACE}" get ds -o wide >"${EVIDENCE_DIR}/groundtruth-ds.txt" 2>&1 || true

ADMIN_TOKEN="$(k -n "${NAMESPACE}" get secret wardyn-auth -o jsonpath='{.data.admin-token}' | base64 -d)"
[[ -n "${ADMIN_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth"
api() { walk_api "${BASE_URL}" "${ADMIN_TOKEN}" "${TMPDIR}/resp.json" "${CURL_MAX_TIME}" "$@"; }
go build -o "${TMPDIR}/bin/wardyn" ./cmd/wardyn || die "go build ./cmd/wardyn failed"
wardyn() { WARDYN_URL="${BASE_URL}" WARDYN_TOKEN="${ADMIN_TOKEN}" "${TMPDIR}/bin/wardyn" "$@"; }
POLICY="${TMPDIR}/policy.json"
jq '.allowed_domains = []' examples/policies/ci.json >"${POLICY}" || die "could not write the policy"
run_state() { wardyn run get "$1" --json 2>/dev/null | jq -r '.state // ""'; }
wait_state() { # RUN STATE SECONDS
  local i
  for ((i = 0; i < $3; i += 2)); do [[ "$(run_state "$1")" == "$2" ]] && return 0; sleep 2; done
  return 1
}
launch() { # TASK -> run id
  wardyn run --agent claude-code --task-mode exec --task "$1" --policy-file "${POLICY}" --json | jq -r '.id // empty'
}
# probe NAME NAMESPACE ACCOUNT COMPONENT RUN_ID WAIT -> the SPIFFE IDs the probe received, one per line
probe() {
  local name="$1" ns="$2" sa="$3" comp="$4" rid="$5" wait="$6"
  k -n "${ns}" delete pod "${name}" --ignore-not-found --wait=true >/dev/null 2>&1
  k -n "${ns}" apply -f - >/dev/null <<EOF || return 1
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  labels: {wardyn.component: "${comp}", wardyn.run-id: "${rid}"}
spec:
  serviceAccountName: ${sa}
  automountServiceAccountToken: false
  restartPolicy: Never
  containers:
    - name: proxy
      image: ${AGENT_IMAGE}
      command: ["/opt/spire/bin/spire-agent", "api", "fetch", "x509", "-socketPath", "/spiffe-workload-api/spire-agent.sock", "-timeout", "${wait}s"]
      volumeMounts: [{name: spiffe-workload-api, mountPath: /spiffe-workload-api, readOnly: true}]
  volumes: [{name: spiffe-workload-api, csi: {driver: csi.spiffe.io, readOnly: true}}]
EOF
  k -n "${ns}" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/${name}" --timeout="$((wait + 120))s" >/dev/null 2>&1 \
    || k -n "${ns}" wait --for=jsonpath='{.status.phase}'=Failed "pod/${name}" --timeout=5s >/dev/null 2>&1
  k -n "${ns}" logs "${name}" 2>/dev/null | tee "${EVIDENCE_DIR}/probe-${name}.log" | sed -n 's/^SPIFFE ID:[[:space:]]*//p'
}

# ── step 1 ──────────────────────────────────────────────────────────────────────
step "step 1: /healthz"
[[ "$(api GET /healthz)" == 200 ]] && jq -e '.components.identity.selected == "spire" and .components.identity.source == "configured"' "${TMPDIR}/resp.json" >/dev/null
check "step 1 identity selected spire, source configured" $?

# ── step 2 ──────────────────────────────────────────────────────────────────────
step "step 2: an exec run under spire"
R2="$(launch 'sleep 20')"
[[ -n "${R2}" ]] || die "the run did not launch"
wait_state "${R2}" RUNNING 300; check "step 2 run ${R2} reached RUNNING" $?
wait_state "${R2}" COMPLETED 300; check "step 2 run ${R2} completed" $?
wardyn run get "${R2}" --json >"${EVIDENCE_DIR}/run-w2.json"
jq -e --arg s "spiffe://${TD}/agent-run/${R2}" '.spiffe_id == $s' "${EVIDENCE_DIR}/run-w2.json" >/dev/null; check "step 2 spiffe_id is the agent-run form" $?
wardyn audit "${R2}" --json >"${EVIDENCE_DIR}/audit-w2.json" 2>/dev/null
jq -e '[.[] | select(.action == "identity.mint")] | length >= 2' "${EVIDENCE_DIR}/audit-w2.json" >/dev/null; check "step 2 at least two identity.mint rows" $?

# ── step 3-step 5: one live run ─────────────────────────────────────────────────────
RID="$(launch 'sleep 300')"
[[ -n "${RID}" ]] || die "the run did not launch"
wait_state "${RID}" RUNNING 300 || die "run ${RID} did not reach RUNNING"
RUN_ID_URI="spiffe://${TD}/agent-run/${RID}"

step "step 3: the run's entry"
e="$(entries "${RUN_ID_URI}")"; echo "${e}" >"${EVIDENCE_DIR}/entry.json"
sels='[.[0].selectors[] | .type + ":" + .value]'
jq -e --arg ns "${RUNS_NAMESPACE}" --arg sa "${PROXY_SA}" --arg r "${RID}" "length == 1 and (${sels} | index(\"k8s:ns:\" + \$ns) and index(\"k8s:sa:\" + \$sa) and index(\"k8s:pod-label:wardyn.component:proxy\") and index(\"k8s:pod-label:wardyn.run-id:\" + \$r))" <<<"${e}" >/dev/null
check "step 3 exactly one entry with k8s:ns:, k8s:sa: and both proxy labels" $?

step "step 4: attestation probes"
k create namespace "${OTHER_NS}" >/dev/null 2>&1; k -n "${OTHER_NS}" create serviceaccount "${PROXY_SA}" >/dev/null 2>&1
probe w4a "${RUNS_NAMESPACE}" "${PROXY_SA}" proxy "${RID}" 60 | grep -qxF "${RUN_ID_URI}"; check "step 4a proxy labels + account in the runs namespace get the run's SVID" $?
! probe w4b "${OTHER_NS}" "${PROXY_SA}" proxy "${RID}" "${NEGATIVE_WAIT}" | grep -qxF "${RUN_ID_URI}"; check "step 4b another namespace gets none within ${NEGATIVE_WAIT}s" $?
! probe w4c "${RUNS_NAMESPACE}" default proxy "${RID}" "${NEGATIVE_WAIT}" | grep -qxF "${RUN_ID_URI}"; check "step 4c the default account gets none" $?
! probe w4d "${RUNS_NAMESPACE}" "${PROXY_SA}" agent "${RID}" "${NEGATIVE_WAIT}" | grep -qxF "${RUN_ID_URI}"; check "step 4d wardyn.component=agent gets none" $?
! probe w4e "${RUNS_NAMESPACE}" "${PROXY_SA}" proxy "${RID}" "${NEGATIVE_WAIT}" | grep -qxF "${CP_ID}"; check "step 4e the proxy probe gets no control-plane SVID within ${NEGATIVE_WAIT}s" $?
jq -e --arg sa "${WARDYN_SA}" 'length >= 1 and all(.[]; [.selectors[] | .type + ":" + .value] | index("k8s:sa:" + $sa))' <<<"$(entries "${CP_ID}")" >/dev/null
check "step 4e the control-plane entry selects on wardynd's account" $?
agent_pod="$(k -n "${RUNS_NAMESPACE}" get pods -l "wardyn.run-id=${RID},wardyn.component=agent" -o json | jq -c '.items[0].spec')"
jq -e '((.serviceAccountName // "default") == "default") and ([.volumes[]? | select(.csi.driver == "csi.spiffe.io")] | length == 0)' <<<"${agent_pod}" >/dev/null
check "step 4 the agent pod runs as default with no csi.spiffe.io volume" $?

step "step 5: kill"
wardyn run kill "${RID}" >/dev/null 2>&1; wait_state "${RID}" KILLED 120 || true
gone=1
for ((t = 0; t < 60; t += 3)); do [[ "$(entries "${RUN_ID_URI}" | jq length)" == 0 ]] && { gone=0; break; }; sleep 3; done
check "step 5 the entry is gone after the kill" "${gone}"
stopped=1
for ((t = 0; t < REVOKE_BOUND; t += 15)); do
  if ! probe w5 "${RUNS_NAMESPACE}" "${PROXY_SA}" proxy "${RID}" 10 | grep -qxF "${RUN_ID_URI}"; then stopped=0; break; fi
done
check "step 5 the probe stops receiving the run's SVID within ${REVOKE_BOUND}s" "${stopped}"

# ── step 6: churn ───────────────────────────────────────────────────────────────
step "step 6: 20 back-to-back runs, with the ground-truth sensor's token in rotation"
for i in $(seq 1 20); do
  r="$(launch 'true')"; [[ -n "${r}" ]] || { check "step 6 run ${i} launched" 1; continue; }
  wait_state "${r}" COMPLETED 300 || check "step 6 run ${r} completed" 1
done
sleep 10
all="$(srv entry show -output json 2>/dev/null | jq -c '[.entries[]? | .spiffe_id | "spiffe://" + .trust_domain + .path]')"
echo "${all}" >"${EVIDENCE_DIR}/entries-after-churn.json"
jq -e '[.[] | select(test("/agent-run/"))] | length == 0' <<<"${all}" >/dev/null; check "step 6 no run entry left behind" $?
jq -e --arg n "/agent-run/${NIL_RUN}" '[.[] | select(endswith($n))] | length == 0' <<<"${all}" >/dev/null; check "step 6 no entry for the host sensor's nil run id" $?
stale="$(k -n "${NAMESPACE}" exec deploy/postgres -- psql -U wardyn -d wardyn -tAc "SELECT count(*) FROM identity_token_bindings WHERE expires_at < now() - interval '65 minutes'" 2>/dev/null | tr -d '[:space:]')"
[[ "${stale}" == 0 ]]; check "step 6 no binding row past its lapse (got '${stale}')" $?

# ── step 7: a workload's runs ───────────────────────────────────────────────────
step "step 7: two runs of one workload"
cat >"${TMPDIR}/workload.yaml" <<'EOF'
api_version: wardyn/v1
kind: Workload
name: spire-walk
intent:
  experience: background
  workload: {kind: command, command: sleep 240}
  no_repositories_or_drives: true
  inline_policy: {eligible_grants: []}
EOF
wardyn apply -f "${TMPDIR}/workload.yaml" >/dev/null || die "workload apply failed"
wl_run() { wardyn workload get spire-walk --json 2>/dev/null | jq -r '.run.id // .run_id // empty'; }
w1=""; for _ in $(seq 1 60); do w1="$(wl_run)"; [[ -n "${w1}" ]] && break; sleep 2; done
wait_state "${w1}" RUNNING 300; check "step 7 the workload's first run ${w1} reached RUNNING" $?
id1="$(wardyn run get "${w1}" --json | jq -r '.spiffe_id')"
[[ "${id1}" =~ ^spiffe://${TD}/workload/[^/]+/spire-walk/run/${w1}$ ]]; check "step 7 first run's ID is the workload form (${id1})" $?
probe w7 "${RUNS_NAMESPACE}" "${PROXY_SA}" proxy "${w1}" 60 | grep -qxF "${id1}"; check "step 7 the probe's URI SAN is the run's ID" $?
wardyn workload restart spire-walk >/dev/null || die "workload restart failed"
w2=""; for _ in $(seq 1 90); do w2="$(wl_run)"; [[ -n "${w2}" && "${w2}" != "${w1}" ]] && break; sleep 2; done
id2="$(wardyn run get "${w2}" --json | jq -r '.spiffe_id')"
wl() { sed -E 's#^spiffe://[^/]+/workload/(.+)/run/[^/]+$#\1#' <<<"$1"; }
[[ -n "${id2}" && "${id2}" != "${id1}" && "$(wl "${id1}")" == "$(wl "${id2}")" ]]; check "step 7 the second run has a different ID and the same workload ($(wl "${id2}"))" $?
wardyn workload delete spire-walk >/dev/null 2>&1 || true

[[ "${fail}" == 0 ]] || die "one or more assertions failed (evidence in ${EVIDENCE_DIR})"
echo "kind-spire-walk: PASS"
