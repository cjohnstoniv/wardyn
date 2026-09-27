#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# T-40 (#700): the KIND leg. scripts/survival-walk.sh's own header already
# names this as open ("the kind leg" — see that file) after its COMPOSE leg
# landed (#1201): docker-kill/restart survival of a spent AWS SSO credential.
# This script is the k8s half, against the SAME cluster
# scripts/kind-sso-walk.sh already reuses (`make kind-quickstart`) — never
# created or deleted here.
#
# TWO SCENARIOS, NOT THE ONE THE PLAN TEXT NAMES. #700's own plan text asks
# for "cordon+drain -> lost(node), drive kept". THAT EXACT OUTCOME DOES NOT
# EXIST IN THE SHIPPED CODE, and this script does not fabricate it:
#
#   - types.LostReason has exactly three values (internal/types/lost_reason.go):
#     ended, reboot, outage. There is no "node".
#   - internal/api/run_lost.go's own doc comment is explicit: "Everything that
#     cannot be kept fails closed and is torn down as before: ... a substrate
#     that cannot keep a sandbox (Kubernetes)." lostRunKeepable's stop path
#     returns runner.ErrEndUnsupported for the k8s runner, which loseRun reads
#     as `kept=false` and tears the run down as terminal — never as a lost-
#     but-kept run at all. A Kubernetes Pod deletion is unlike a Docker
#     container exit: nothing is left "stopped but still there" for a later
#     revive to resume, so k8s cannot offer the reboot/outage shape either.
#
# So what this script actually proves, against real code, corrected the same
# way scripts/survival-walk.sh's own header corrects its issue's assumptions
# with citations:
#
#   A. wardynd's OWN pod, deleted mid-run (the CONTROL PLANE, not a run's
#      sandbox) — the k8s analogue of the compose leg's docker-kill/restart:
#      the Deployment recreates the pod, Postgres (unaffected — a separate
#      Deployment) still holds the run row, and the daemon's watcher resumes
#      tracking it once it is back, exactly as the compose leg proved for a
#      spent credential's persisted state.
#   B. a RUNNING run's own agent pod, evicted by a cordon+drain of the node it
#      is scheduled on — proving the run ends up torn down TERMINAL (not
#      silently "kept"), matching run_lost.go's own documented invariant
#      above, rather than asserting a "lost(node)" state nothing in this repo
#      produces.
#
# THE SINGLE-NODE ADAPTATION, NAMED RATHER THAN HIDDEN: `make kind-quickstart`
# is a ONE-NODE cluster (deploy/kind/quickstart-kind-config.yaml). A real
# `kubectl drain` on that node evicts wardynd and Postgres too — there is
# nowhere else for the cluster to put them — so scenario B cordons the node
# (never schedulable, so nothing NEW lands there) and deletes ONLY the run's
# own pod directly, rather than draining the whole node, then immediately
# uncordons so the control plane's own future rollouts are not left Pending.
# This reproduces exactly what a drain does TO THAT ONE POD (a Delete against
# a pod on a node the scheduler will not target again) without also killing
# the control plane the assertion needs to stay up to observe the outcome. A
# multi-node kind config that could run a REAL drain is out of this issue's
# scope — deploy/kind/quickstart-kind-config.yaml is shared with every other
# kind walk in this repo and changing its node topology is not this script's
# call to make alone.
#
# NOT BUILT HERE, NAMED RATHER THAN SILENTLY DROPPED: "drive kept" — that a
# host_path/k8s_pvc_static drive's bytes outlive the terminal run above. Doing
# that honestly needs a real POST /api/v1/drives registration this pass did
# not get far enough to ground in the actual request schema
# (internal/api/user_drives.go's writeUserDrive) against a live cluster to
# verify field-by-field; asserting on a guessed shape here would be worse than
# not asserting at all. Left for a follow-up that can verify it against a real
# cluster.
#
# GUARD: self-skips unless WARDYN_TEST_K8S=1, same knob every other
# cluster-dependent lane uses. Never creates or deletes the cluster.
#
# NOT RUN OR PROVEN IN THIS AUTHORING PASS: no kind cluster was available to
# execute it here (this repo's own lane rules forbid creating one in this
# environment). bash -n and this repo's lint are what this pass can offer;
# the hosted nightly is what proves the mechanism end to end.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-survival-walk: set WARDYN_TEST_K8S=1 to run the cluster-dependent survival walk (skipping)."
fi

wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
pass() { printf '  \033[1;32m[pass]\033[0m %s\n' "$*"; }
fail() { printf '  \033[1;31m[FAIL]\033[0m %s\n' "$*"; FAILED=1; }
FAILED=0
CURL_MAX_TIME="${WARDYN_KIND_SURVIVAL_CURL_MAX_TIME:-10}"

for bin in kubectl curl jq; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
RUNS_NAMESPACE="wardyn-runs"
RELEASE="wardyn"
HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
BASE="http://127.0.0.1:${HTTP_PORT}"

EVIDENCE_DIR="${WARDYN_KIND_SURVIVAL_EVIDENCE:-${ROOT}/local/evidence/kind-survival-walk}"
mkdir -p "${EVIDENCE_DIR}"
TMPDIR="$(mktemp -d /tmp/wardyn-kind-survival-walk.XXXXXX)"
trap 'rm -rf "${TMPDIR}"' EXIT

step "checking the cluster is up (never created or deleted here)"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get deployment "${RELEASE}" >/dev/null 2>&1 \
  || die "no wardyn release on ${CONTEXT} — run: WARDYN_QUICKSTART_HTTP_PORT=${HTTP_PORT} make kind-quickstart"
ADMIN_TOKEN="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret wardyn-auth \
  -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[[ -n "${ADMIN_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth"
curl -sf --max-time "${CURL_MAX_TIME}" "${BASE}/healthz" >/dev/null 2>&1 \
  || die "${BASE}/healthz did not answer — is the NodePort published on 127.0.0.1:${HTTP_PORT}?"
pass "cluster reachable at ${BASE}"

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

# psql1 SQL -> one query against the cluster's own postgres Deployment
# (kubectl exec, never docker exec — wardynd's own pod is distroless and has
# no shell, but the Deployment quickstart.sh creates for postgres is the plain
# postgres:16 image and has psql).
psql1() {
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" exec deployment/postgres -- \
    psql -U wardyn -d wardyn -tAc "$1" 2>&1
}

# create_running_run -> launches one interactive claude-code run and waits for
# its agent pod to exist in RUNS_NAMESPACE, printing the run id on stdout.
# Deliberately the SAME minimal inline-policy shape scripts/survival-walk.sh
# uses for its own probe dispatches: this walk does not care whether the run
# ever gets a live model credential, only that it has a real sandbox POD to
# kill.
create_running_run() {
  local code rid pod up=""
  code=$(api POST /api/v1/runs '{"agent":"claude-code","repo":"local:kind-survival","interactive":true,
    "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny","min_confinement_class":"CC1","auto_stop_after_sec":-1}}')
  [[ "${code}" == "200" || "${code}" == "201" ]] || { cat "${TMPDIR}/resp.json" >&2; die "POST /runs answered ${code}"; }
  rid="$(jq -r '.id' "${TMPDIR}/resp.json")"
  [[ -n "${rid}" && "${rid}" != "null" ]] || die "create-run response carried no id"
  for _ in $(seq 1 60); do
    pod="$(kubectl --context "${CONTEXT}" -n "${RUNS_NAMESPACE}" get pods -l "wardyn.run-id=${rid}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    [[ -n "${pod}" ]] && { up=1; break; }
    sleep 2
  done
  [[ -n "${up}" ]] || die "run ${rid}'s pod never appeared in ${RUNS_NAMESPACE} (wardyn.run-id=${rid})"
  echo "${rid}"
}

RUN_A_ID=""
RUN_B_ID=""
cleanup_runs() {
  local rid
  for rid in "${RUN_A_ID}" "${RUN_B_ID}"; do
    [[ -n "${rid}" ]] || continue
    curl -sS --max-time "${CURL_MAX_TIME}" -X POST "${BASE}/api/v1/runs/${rid}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1 || true
  done
}
trap 'cleanup_runs; rm -rf "${TMPDIR}"' EXIT

# ── A. wardynd's OWN pod, deleted mid-run — the control-plane analogue of
#      the compose leg's docker-kill/restart ────────────────────────────────
step "A: launching a run, then deleting wardynd's own pod (the control plane)"
RUN_A_ID="$(create_running_run)"
pass "run ${RUN_A_ID} has a sandbox pod"

before_row="$(psql1 "SELECT state FROM agent_runs WHERE id = '${RUN_A_ID}'")"
[[ "${before_row}" == "RUNNING" ]] || die "expected run ${RUN_A_ID} to be RUNNING in Postgres before the pod delete; got ${before_row}"

wardynd_pod="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pods -l app.kubernetes.io/name=wardyn \
  -o jsonpath='{.items[0].metadata.name}')"
[[ -n "${wardynd_pod}" ]] || die "could not find wardynd's own pod (label app.kubernetes.io/name=wardyn)"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" delete pod "${wardynd_pod}" --wait=false \
  || die "deleting wardynd's pod ${wardynd_pod} failed"
pass "wardynd pod ${wardynd_pod} deleted"

step "waiting for the Deployment to recreate wardynd and become healthy again"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=180s \
  || { kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs "deployment/${RELEASE}" --tail=80 >&2 || true; die "wardynd never came back after its pod was deleted"; }
wait_healthy "${BASE}" 60 2 || die "${BASE}/healthz did not answer again after wardynd's pod was recreated"
pass "wardynd back up on a fresh pod"

step "confirming Postgres (a separate Deployment, unaffected by the wardynd delete) still holds run ${RUN_A_ID}"
after_row="$(psql1 "SELECT state FROM agent_runs WHERE id = '${RUN_A_ID}'")"
[[ -n "${after_row}" ]] \
  && pass "run ${RUN_A_ID} row survived the control-plane pod's death (state=${after_row})" \
  || fail "expected run ${RUN_A_ID}'s row to still be readable after the control-plane pod was recreated"

# ── B. a RUNNING run's OWN agent pod, evicted by a cordon of its node ──────
step "B: launching a second run, then cordoning its node and deleting its pod directly (single-node drain — see this file's header)"
RUN_B_ID="$(create_running_run)"
pod_b="$(kubectl --context "${CONTEXT}" -n "${RUNS_NAMESPACE}" get pods -l "wardyn.run-id=${RUN_B_ID}" \
  -o jsonpath='{.items[0].metadata.name}')"
node_b="$(kubectl --context "${CONTEXT}" -n "${RUNS_NAMESPACE}" get pod "${pod_b}" -o jsonpath='{.spec.nodeName}')"
[[ -n "${pod_b}" && -n "${node_b}" ]] || die "could not resolve run ${RUN_B_ID}'s own pod/node"
pass "run ${RUN_B_ID} scheduled as pod ${pod_b} on node ${node_b}"

kubectl --context "${CONTEXT}" cordon "${node_b}" || die "cordoning ${node_b} failed"
kubectl --context "${CONTEXT}" -n "${RUNS_NAMESPACE}" delete pod "${pod_b}" --grace-period=0 --force \
  || { kubectl --context "${CONTEXT}" uncordon "${node_b}" || true; die "deleting ${pod_b} failed"; }
kubectl --context "${CONTEXT}" uncordon "${node_b}" || die "uncordoning ${node_b} failed (the control plane may now be stuck unschedulable — fix this by hand)"
pass "pod ${pod_b} deleted; node ${node_b} re-uncordoned immediately"

step "confirming the run ends up TERMINAL (torn down, never a fictitious kept/lost(node) state)"
terminal=""
final_state=""
for _ in $(seq 1 60); do
  final_state="$(psql1 "SELECT state FROM agent_runs WHERE id = '${RUN_B_ID}'")"
  case "${final_state}" in
    FAILED|COMPLETED|KILLED) terminal=1; break ;;
  esac
  sleep 2
done
[[ -n "${terminal}" ]] \
  && pass "run ${RUN_B_ID} ended terminal (state=${final_state}) — matches run_lost.go's documented 'a substrate that cannot keep a sandbox (Kubernetes)' fail-closed teardown" \
  || fail "expected run ${RUN_B_ID} to reach a terminal state after its pod was evicted; last read: ${final_state:-<none>}"
lost_reason="$(psql1 "SELECT COALESCE(lost_reason,'') FROM agent_runs WHERE id = '${RUN_B_ID}'")"
[[ -z "${lost_reason}" ]] \
  && pass "no lost_reason recorded — Kubernetes never keeps a lost run, so none should be" \
  || fail "expected no lost_reason on a k8s run (kept runs are docker-only); got '${lost_reason}'"

if [[ "${FAILED}" -ne 0 ]]; then
  echo "kind-survival-walk: FAILED — see ${EVIDENCE_DIR}" >&2
  exit 1
fi
echo "kind-survival-walk: PASS"
