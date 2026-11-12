#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# The Kubernetes ground-truth sensor, joined: the chart's DaemonSet (groundtruth.k8s.enabled) on the
# `make kind-quickstart` cluster, fed a synthetic Tetragon export line that names a live run's agent pod.
#
#   1. turns the sensor on with helm upgrade, pointing it at a directory this script makes on the kind node
#      (the export a Tetragon would write), and waits for the DaemonSet.
#   2. starts a long exec run and reads its agent pod's name.
#   3. appends one Tetragon-shaped exec line for that pod to the node's export file, and asserts the run's
#      audit rows hold a kernel.process.exec row, and that /healthz ebpf_groundtruth.sensors[] names the kind node.
#   4. renames the export file away and creates a new one (a rotation), appends a second line, and asserts it
#      arrives too: the DaemonSet mounts the DIRECTORY, so the new file is followed.
#   5. restarts wardynd, which mints and patches a new token at boot, asserts the Secret's token changed, then
#      appends a third line and asserts it still arrives.
#
# This proves the join (export line -> run -> audit row), the rotation through the chart's mount, node
# health, that wardynd updates the Secret, and that delivery continued after it did. It does NOT prove that
# the sensor picked up the new token through the mounted Secret: the sensor re-reads its token only after
# wardynd rejects the one it holds, so the third line can arrive on the cached token. That 401, re-read and
# retry path is covered by a unit test (TestEventSink_RotatedFileIsReadAfterRejection), and the mounted
# projection refreshing in a live pod is not proven by this script.
# It does NOT prove a real sensor either: no Tetragon runs here (that needs a kernel with BTF), so the line
# is made up and its fields follow Tetragon's gRPC reference.
#
# GUARD: self-skips unless WARDYN_TEST_K8S=1 (exit 77, common.sh's skip_lane). It runs helm upgrade on the
# cluster it finds, so a hand run must also set WARDYN_ACK_SHARED_QUICKSTART=1 to say that cluster is
# disposable. Run by hand or from the nightly; it is not part of any PR gate.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-groundtruth: set WARDYN_TEST_K8S=1 to run the cluster-dependent ground-truth sensor join (skipping)."
fi
if [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" != "1" ]]; then
  skip_lane "kind-groundtruth: set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the quickstart cluster this script finds may be mutated — refusing to run against a cluster it did not create otherwise."
fi

wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

for bin in docker kind kubectl helm curl jq go; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
# Refuse a cluster kind does not list: a stale kubeconfig context can point at a real cluster, and this
# script runs helm upgrade against whatever it finds.
if ! kind get clusters 2>/dev/null | grep -x -- "${CLUSTER}" >/dev/null; then
  skip_lane "kind-groundtruth: kind has no cluster named ${CLUSTER} (kind get clusters); set WARDYN_QUICKSTART_CLUSTER to a kind cluster that exists (skipping)."
fi
NAMESPACE="wardyn"
RELEASE="wardyn"
RUNS_NAMESPACE="wardyn-runs"
HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
BASE_URL="http://127.0.0.1:${HTTP_PORT}"
EXPORT_DIR="/var/lib/wardyn-gt-job"
EXPORT_FILE="tetragon.log"
# The sensor objects are named from the release name and namespace; read the name from the cluster.
SENSOR_DS=""
SECRET=""
CURL_MAX_TIME=15
EVIDENCE_DIR="${WARDYN_KIND_GROUNDTRUTH_EVIDENCE:-${ROOT}/local/evidence/kind-groundtruth}"
mkdir -p "${EVIDENCE_DIR}"
TMPDIR="$(mktemp -d /tmp/wardyn-kind-groundtruth.XXXXXX)"
RUN_PID=""
trap '[[ -n "${RUN_PID}" ]] && kill "${RUN_PID}" 2>/dev/null; rm -rf "${TMPDIR}"' EXIT
kc() { kubectl --context "${CONTEXT}" "$@"; }
fail=0
check() { # WHAT CONDITION-EXIT-CODE
  if [[ "$2" -eq 0 ]]; then echo "  ok: $1"; else echo "  FAIL: $1" >&2; fail=1; fi
}

step "checking the cluster is up (never created or deleted here)"
kc -n "${NAMESPACE}" get deployment "${RELEASE}" >/dev/null 2>&1 \
  || die "no wardyn release on ${CONTEXT} — run: WARDYN_QUICKSTART_HTTP_PORT=${HTTP_PORT} make kind-quickstart"
ADMIN_TOKEN="$(kc -n "${NAMESPACE}" get secret wardyn-auth -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[[ -n "${ADMIN_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth"
NODE="$(kc get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
[[ -n "${NODE}" ]] || die "could not read the kind node's name"

api() { # PATH -> body on stdout, nonzero on a non-2xx
  curl -sf --max-time "${CURL_MAX_TIME}" -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE_URL}$1"
}

# ── 1. the sensor, on ───────────────────────────────────────────────────────
step "making the export directory ${EXPORT_DIR} on node ${NODE}"
docker exec "${NODE}" sh -c "mkdir -p ${EXPORT_DIR} && : > ${EXPORT_DIR}/${EXPORT_FILE}" || die "could not make the export file on the node"

step "turning the sensor on (helm upgrade --reuse-values groundtruth.k8s.enabled=true, hostPath ${EXPORT_DIR})"
helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn -n "${NAMESPACE}" --reuse-values \
  --set groundtruth.k8s.enabled=true \
  --set "groundtruth.k8s.exportDirectory=${EXPORT_DIR}" \
  --set "groundtruth.k8s.exportFilename=${EXPORT_FILE}" >"${EVIDENCE_DIR}/helm-upgrade.log" 2>&1 \
  || { tail -30 "${EVIDENCE_DIR}/helm-upgrade.log" >&2; die "helm upgrade failed"; }
kc -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=240s || die "wardynd did not come back after the upgrade"

# The sensor waits at start (up to 10 minutes) until wardynd has patched the CA into the Secret and the
# kubelet has synced it, so the DaemonSet can be up before the Secret is filled.
step "waiting for the sensor DaemonSet and the Secret's token"
SENSOR_DS="$(kc -n "${NAMESPACE}" get daemonset -l app.kubernetes.io/component=groundtruth -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
[[ -n "${SENSOR_DS}" ]] || die "the upgrade created no sensor DaemonSet in ${NAMESPACE}"
SECRET="${SENSOR_DS}"
kc -n "${NAMESPACE}" rollout status "daemonset/${SENSOR_DS}" --timeout=300s \
  || { kc -n "${NAMESPACE}" describe "daemonset/${SENSOR_DS}" >&2; kc -n "${NAMESPACE}" logs "daemonset/${SENSOR_DS}" --tail=50 >&2; die "the sensor DaemonSet did not become ready"; }
secret_token() { kc -n "${NAMESPACE}" get secret "${SECRET}" -o jsonpath='{.data.token}' 2>/dev/null; }
token0=""
for _ in $(seq 1 60); do token0="$(secret_token)"; [[ -n "${token0}" ]] && break; sleep 2; done
[[ -n "${token0}" ]]; check "wardynd patched a token into Secret ${SECRET}" $?
[[ -n "$(kc -n "${NAMESPACE}" get secret "${SECRET}" -o jsonpath='{.data.control-plane-ca\.pem}' 2>/dev/null)" ]]
check "wardynd patched control-plane-ca.pem into the Secret" $?

# ── 2. a live run ───────────────────────────────────────────────────────────
step "building the wardyn CLI and starting a long exec run"
go build -o "${TMPDIR}/bin/wardyn" ./cmd/wardyn || die "go build ./cmd/wardyn failed"
env PATH="${TMPDIR}/bin:${PATH}" WARDYN_URL="${BASE_URL}" WARDYN_CI_TOKEN="${ADMIN_TOKEN}" WARDYN_CI_TIMEOUT=15m \
  WARDYN_CI_OUT="${EVIDENCE_DIR}/ci-artifacts-run" WARDYN_CI_TASK_MODE=exec WARDYN_CI_TASK="sleep 600" \
  WARDYN_CI_POLICY_FILE=examples/policies/ci.json ./scripts/ci-run.sh >"${EVIDENCE_DIR}/ci-run.log" 2>&1 &
RUN_PID=$!
pod=""
for _ in $(seq 1 90); do
  pod="$(kc -n "${RUNS_NAMESPACE}" get pods -l wardyn.managed=true,wardyn.component=agent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
  [[ -n "${pod}" ]] && break
  sleep 2
done
[[ -n "${pod}" ]] || die "no agent pod appeared in ${RUNS_NAMESPACE}"
RUN_ID="${pod#wardyn-agent-}"
echo "  run ${RUN_ID} (pod ${pod})"

# ── 3. an export line for that pod ──────────────────────────────────────────
line() { # TAG -> one Tetragon-shaped exec line for the agent pod, binary named by TAG
  printf '{"node_name":"%s","process_exec":{"process":{"binary":"/usr/bin/%s","arguments":"job","docker":"c0ffee000000","pod":{"namespace":"%s","name":"%s","container":{"id":"c0ffee000000","name":"agent"}}}}}\n' \
    "${NODE}" "$1" "${RUNS_NAMESPACE}" "${pod}"
}
append() { docker exec "${NODE}" sh -c "cat >> ${EXPORT_DIR}/${EXPORT_FILE}" <<<"$(line "$1")"; }
audit_has() { # TAG -> 0 when the run has a kernel.process.exec row naming /usr/bin/TAG (waits up to 60 s)
  local i
  for i in $(seq 1 30); do
    api "/api/v1/audit?run_id=${RUN_ID}" 2>/dev/null | jq -e --arg b "/usr/bin/$1" \
      '(.events // .) | any(.[]; .action == "kernel.process.exec" and ((.data | tostring) | contains($b)))' >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}

step "appending an export line for ${pod}"
append gtjob-first
audit_has gtjob-first; check "audit: run ${RUN_ID} has a kernel.process.exec row from the export line" $?

hz="$(api /healthz 2>/dev/null || true)"
printf '%s\n' "${hz}" >"${EVIDENCE_DIR}/healthz.json"
jq -e --arg n "${NODE}" '.ebpf_groundtruth.sensors[0].node == $n' <<<"${hz}" >/dev/null 2>&1
check "/healthz ebpf_groundtruth.sensors[0].node is ${NODE}" $?

# ── 4. a rotation: rename the file away, create a new one ───────────────────
step "rotating the export (rename, then create) and appending a second line"
docker exec "${NODE}" sh -c "mv ${EXPORT_DIR}/${EXPORT_FILE} ${EXPORT_DIR}/${EXPORT_FILE}.1 && : > ${EXPORT_DIR}/${EXPORT_FILE}" || die "could not rotate the export on the node"
append gtjob-second
audit_has gtjob-second; check "audit: a line written after the rename-and-create rotation arrived" $?

# ── 5. a new token ──────────────────────────────────────────────────────────
step "restarting wardynd (it mints and patches a new token at boot)"
kc -n "${NAMESPACE}" rollout restart "deployment/${RELEASE}" >/dev/null || die "could not restart wardynd"
kc -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=240s || die "wardynd did not come back"
token1="${token0}"
for _ in $(seq 1 60); do token1="$(secret_token)"; [[ "${token1}" != "${token0}" && -n "${token1}" ]] && break; sleep 2; done
[[ "${token1}" != "${token0}" ]]; check "Secret ${SECRET} holds a new token after the restart" $?
append gtjob-third
audit_has gtjob-third; check "audit: a line written after the Secret was updated still arrived (not proof the new token was used)" $?

kc -n "${NAMESPACE}" logs "daemonset/${SENSOR_DS}" --tail=200 >"${EVIDENCE_DIR}/sensor.log" 2>&1 || true
api "/api/v1/audit?run_id=${RUN_ID}" >"${EVIDENCE_DIR}/audit.json" 2>/dev/null || true

[[ "${fail}" -eq 0 ]] || die "the sensor join was not proven — see the FAIL lines above and ${EVIDENCE_DIR}"
echo "kind-groundtruth: PASS"
