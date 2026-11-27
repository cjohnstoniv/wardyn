#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# The server cluster's long control-plane outage (#4085), on the `make kind-quickstart` cluster, which this walk never
# creates or deletes. Postgres is frozen for KIND_DB_STOP_MIN minutes (default 70, past the proxy's renew window) with
# two interactive runs up, each with a file written before the freeze:
#
#   A. its proxy lives through: once Postgres is back, the proxy recovers its token (run.token.recover) within 120 s,
#      the agent pod keeps its UID, no run.lost row is written, and the file is unchanged.
#   B. at minute 40 its proxy pod is deleted without its agent (--cascade=orphan), as the server's own
#      ServiceAccount, which may not patch pods: the server Role's `delete` alone must do it. Nothing is left to
#      recover B, so after the outage hold it is marked lost and kept: run.lost with kept true, the agent pod with the
#      same UID, Running and with no owner, the file unchanged and no network. Its revive then brings the network
#      back, and the agent pod keeps its UID.
#
# Needs: the run namespace's NetworkPolicy verdict proven (Calico, deploy/kind/quickstart.sh); a leg B run.lost row
# with kept false fails fast as "namespace verdict not proven". Wall time about 110 minutes.
#
# GUARD: self-skips unless WARDYN_TEST_K8S=1, and a hand run must set WARDYN_ACK_SHARED_QUICKSTART=1: it freezes the
# cluster's Postgres and deletes a run's proxy pod.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/lib/walk-api.sh
source "${ROOT}/scripts/lib/walk-api.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-server-outage-walk: set WARDYN_TEST_K8S=1 to run the cluster-dependent walk (skipping)."
fi
if [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" != "1" ]]; then
  skip_lane "kind-server-outage-walk: set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the wardyn-quickstart cluster may have its Postgres frozen and a run's proxy pod deleted."
fi

wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
pass() { printf '  \033[1;32m[pass]\033[0m %s\n' "$*"; }
fail() { printf '  \033[1;31m[FAIL]\033[0m %s\n' "$*"; FAILED=1; }
FAILED=0

for bin in kubectl curl jq docker; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

STOP_MIN="${KIND_DB_STOP_MIN:-70}"
DELETE_AT_MIN="${KIND_PROXY_DELETE_AT_MIN:-40}"
[[ "${STOP_MIN}" =~ ^[0-9]+$ && "${DELETE_AT_MIN}" =~ ^[0-9]+$ && "${DELETE_AT_MIN}" -lt "${STOP_MIN}" ]] \
  || die "KIND_DB_STOP_MIN (${STOP_MIN}) and KIND_PROXY_DELETE_AT_MIN (${DELETE_AT_MIN}) must be whole minutes, the delete first"
CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
KIND_NODE="${CLUSTER}-control-plane"
NAMESPACE="wardyn"
RUNS_NAMESPACE="wardyn-runs"
RELEASE="wardyn"
BASE="http://127.0.0.1:${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
PROOF="/tmp/outage-walk-proof"
TMP="$(mktemp -d /tmp/wardyn-kind-server-outage.XXXXXX)"

k() { kubectl --context "${CONTEXT}" "$@"; }
psql1() { k -n "${NAMESPACE}" exec deployment/postgres -- psql -U wardyn -d wardyn -tAc "$1" 2>&1; }
pg_signal() { docker exec "${KIND_NODE}" pkill "-$1" -x postgres; }
agent_exec() { k -n "${RUNS_NAMESPACE}" exec "wardyn-agent-$1" -c agent -- sh -c "$2"; }
pod_field() { k -n "${RUNS_NAMESPACE}" get pod "$1" -o jsonpath="$2" 2>/dev/null; }
audit_n() { psql1 "SELECT count(*) FROM audit_events WHERE run_id = '$1' AND action = '$2' $3"; }
reaches_proxy() { agent_exec "$1" 'nc -z -w2 wardyn-proxy 3128' >/dev/null 2>&1; }

FROZEN=""
RUN_A=""
RUN_B=""
cleanup() {
  [[ -n "${FROZEN}" ]] && pg_signal CONT >/dev/null 2>&1
  for rid in "${RUN_A}" "${RUN_B}"; do
    [[ -n "${rid}" ]] && curl -sS --max-time 10 -X POST "${BASE}/api/v1/runs/${rid}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN:-}" >/dev/null 2>&1
  done
  rm -rf "${TMP}"
}
trap cleanup EXIT

step "checking the cluster is up (never created or deleted here)"
k -n "${NAMESPACE}" get deployment "${RELEASE}" >/dev/null 2>&1 || die "no wardyn release on ${CONTEXT}: run make kind-quickstart"
ADMIN_TOKEN="$(k -n "${NAMESPACE}" get secret wardyn-auth -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null)"
[[ -n "${ADMIN_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth"
api() { walk_api "${BASE}" "${ADMIN_TOKEN}" "${TMP}/resp.json" 10 "$@"; }
wait_healthy "${BASE}" 30 2 || die "${BASE}/healthz did not answer"
pass "cluster reachable at ${BASE}"

step "the server's ServiceAccount may delete pods but not patch them"
SA="$(k -n "${NAMESPACE}" get deployment "${RELEASE}" -o jsonpath='{.spec.template.spec.serviceAccountName}')"
[[ -n "${SA}" ]] || die "could not read the server Deployment's ServiceAccount"
AS="--as=system:serviceaccount:${NAMESPACE}:${SA}"
if k "${AS}" -n "${RUNS_NAMESPACE}" auth can-i patch pods >/dev/null 2>&1; then
  die "${SA} may patch pods in ${RUNS_NAMESPACE}: the orphan delete would not show that delete alone suffices"
fi
k "${AS}" -n "${RUNS_NAMESPACE}" auth can-i delete pods >/dev/null 2>&1 || die "${SA} may not delete pods in ${RUNS_NAMESPACE}"
pass "${SA}: patch pods no, delete pods yes"

# start_run -> an interactive run with its agent and proxy Running and the proof file written; prints the run id.
start_run() {
  local code rid
  code=$(api POST /api/v1/runs '{"agent":"claude-code","repo":"local:kind-server-outage","interactive":true,
    "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny","min_confinement_class":"CC1","auto_stop_after_sec":-1,
    "resources":{"cpu_millis":250,"memory_mib":256}}}')
  [[ "${code}" == "200" || "${code}" == "201" ]] || { cat "${TMP}/resp.json" >&2; die "POST /runs answered ${code}"; }
  rid="$(jq -r '.id' "${TMP}/resp.json")"
  [[ -n "${rid}" && "${rid}" != "null" ]] || die "create-run response carried no id"
  for pod in "wardyn-agent-${rid}" "wardyn-proxy-${rid}"; do
    for _ in $(seq 1 90); do
      [[ "$(pod_field "${pod}" '{.status.phase}')" == "Running" ]] && break
      sleep 2
    done
    [[ "$(pod_field "${pod}" '{.status.phase}')" == "Running" ]] || die "pod ${pod} never reached Running"
  done
  agent_exec "${rid}" "echo ${rid} > ${PROOF}" || die "could not write the proof file in run ${rid}"
  echo "${rid}"
}

proof_unchanged() { [[ "$(agent_exec "$1" "cat ${PROOF}" 2>/dev/null)" == "$1" ]]; }

step "starting runs A and B"
RUN_A="$(start_run)"
RUN_B="$(start_run)"
UID_A="$(pod_field "wardyn-agent-${RUN_A}" '{.metadata.uid}')"
UID_B="$(pod_field "wardyn-agent-${RUN_B}" '{.metadata.uid}')"
pass "run A ${RUN_A} (agent ${UID_A}), run B ${RUN_B} (agent ${UID_B})"

step "freezing Postgres for ${STOP_MIN} minutes"
pg_signal STOP || die "could not freeze Postgres on ${KIND_NODE}"
FROZEN=1
T0="$(date +%s)"
until (( $(date +%s) - T0 >= DELETE_AT_MIN * 60 )); do sleep 30; done

step "minute ${DELETE_AT_MIN}: deleting B's proxy pod without its agent, as ${SA}"
k "${AS}" -n "${RUNS_NAMESPACE}" delete pod "wardyn-proxy-${RUN_B}" --cascade=orphan --wait \
  || die "the orphan delete of B's proxy pod as ${SA} was refused"
[[ "$(pod_field "wardyn-agent-${RUN_B}" '{.metadata.uid}')" == "${UID_B}" ]] || die "B's agent pod went with its proxy pod"
[[ -z "$(pod_field "wardyn-agent-${RUN_B}" '{.metadata.ownerReferences}')" ]] || fail "B's agent pod still has an owner after the orphan delete"
pass "B's proxy pod gone; its agent pod ${UID_B} is left with no owner"

until (( $(date +%s) - T0 >= STOP_MIN * 60 )); do sleep 30; done
step "thawing Postgres after ${STOP_MIN} minutes"
pg_signal CONT || die "could not thaw Postgres on ${KIND_NODE}"
FROZEN=""
BACK="$(date +%s)"
wait_healthy "${BASE}" 60 2 || die "${BASE}/healthz did not answer after the thaw"

step "A: its proxy recovers its token within 120 s"
recovered=""
for _ in $(seq 1 60); do
  [[ "$(audit_n "${RUN_A}" run.token.recover "AND time >= to_timestamp(${BACK})")" -ge 1 ]] 2>/dev/null && { recovered=1; break; }
  sleep 2
done
[[ -n "${recovered}" ]] && pass "run A recovered its token" || fail "run A wrote no run.token.recover row within 120 s of the thaw"
[[ "$(pod_field "wardyn-agent-${RUN_A}" '{.metadata.uid}')" == "${UID_A}" ]] && pass "A's agent pod kept its UID" || fail "A's agent pod changed"
[[ "$(audit_n "${RUN_A}" run.lost "")" == "0" ]] && pass "no run.lost row for A" || fail "run A was marked lost"
proof_unchanged "${RUN_A}" && pass "A's file is unchanged" || fail "A's file changed or is gone"

step "B: marked lost and kept once the outage hold ends"
kept=""
for _ in $(seq 1 120); do
  if [[ "$(audit_n "${RUN_B}" run.lost "AND data->>'kept' = 'false'")" -ge 1 ]] 2>/dev/null; then
    die "namespace verdict not proven: run B's run.lost row says kept false"
  fi
  [[ "$(audit_n "${RUN_B}" run.lost "AND data->>'kept' = 'true'")" -ge 1 ]] 2>/dev/null && { kept=1; break; }
  sleep 30
done
[[ -n "${kept}" ]] || die "run B was not marked lost and kept within an hour of the thaw"
pass "run.lost kept:true for B"
[[ "$(pod_field "wardyn-agent-${RUN_B}" '{.metadata.uid}')" == "${UID_B}" ]] || fail "B's agent pod changed"
[[ "$(pod_field "wardyn-agent-${RUN_B}" '{.status.phase}')" == "Running" ]] || fail "B's agent pod is not Running"
[[ -z "$(pod_field "wardyn-agent-${RUN_B}" '{.metadata.ownerReferences}')" ]] || fail "B's agent pod has an owner"
proof_unchanged "${RUN_B}" && pass "B's file is unchanged" || fail "B's file changed or is gone"
reaches_proxy "${RUN_B}" && fail "B's agent reaches a proxy while kept" || pass "B's agent has no network"

step "B: its revive brings the network back"
code=$(api POST "/api/v1/runs/${RUN_B}/revive" '')
[[ "${code}" == "200" ]] || { cat "${TMP}/resp.json" >&2; fail "revive of B answered ${code}"; }
back=""
for _ in $(seq 1 60); do
  reaches_proxy "${RUN_B}" && { back=1; break; }
  sleep 2
done
[[ -n "${back}" ]] && pass "B's agent reaches its new proxy" || fail "B's agent did not reach a proxy within 120 s of the revive"
[[ "$(pod_field "wardyn-agent-${RUN_B}" '{.metadata.uid}')" == "${UID_B}" ]] && pass "B's agent pod kept its UID" || fail "B's agent pod changed"

if [[ "${FAILED}" -ne 0 ]]; then
  echo "kind-server-outage-walk: FAILED" >&2
  exit 1
fi
echo "kind-server-outage-walk: PASS"
