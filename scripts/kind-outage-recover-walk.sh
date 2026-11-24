#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# kind-outage-recover-walk.sh: through a control-plane outage longer than a run token's hour, a run's network
# pauses at its outage allowance and comes back by itself once Wardyn can check the run again (#4082). Two legs,
# one after the other, under the one kind lock the caller holds:
#
#   A. The database stop, on the cluster `make kind-quickstart` made, reused as scripts/kind-survival-walk.sh
#      reuses it and never created or deleted here. The quickstart runs no runner agent, so these are the server's
#      own Kubernetes runs, whose proxies take the plain gate (open from the start, paused at the allowance). Run A
#      has the default outage terms, run B a policy `outage: {allowance_minutes: 30, strict: true}`. Postgres is
#      scaled to 0 for KIND_OUTAGE_MIN minutes. At minute 35 A's allowed host answers and B's does not; at minute
#      65 A's proxy answers 503 with the paused body. Within 120 s of Postgres coming back, A's host answers again
#      and A has a run.token.recover audit row, and B is FAILED with the strict reason.
#   B. The runner-link cut, runner-mode, on a throwaway kind cluster of its own: a unique name, deleted by a trap
#      on every exit, stood up as scripts/k6-kind-conformance.sh does it (scripts/lib/kind-org.sh: an organisation
#      over TLS, a runner agent registered with an org token and claimed by its fingerprint). The runner reaches the
#      organisation through a TCP forwarder, outage-walk-fwd (a Deployment and Service in the walk's namespace).
#      runnerd keeps the organisation's URL, which its handshake binds, and a host alias points that name at the
#      forwarder. Run C (default terms) runs on the runner; the forwarder is scaled to 0 for KIND_OUTAGE_MIN minutes.
#      The runner goes offline, C's egress pauses by minute 65, C is not marked lost while the link is cut, and
#      within 120 s of the forwarder coming back C's host answers again, with a run.token.recover row whose
#      window_kind is runner_link.
#
# Exits 77 unless WARDYN_TEST_K8S=1, and refuses (exit 2) unless WARDYN_ACK_SHARED_QUICKSTART=1: leg A scales the
# shared quickstart's Postgres to 0 and puts it back, on every exit path. Leg B never touches the quickstart cluster
# or a compose stack: `kind get clusters` is read before and after it, and the wardyn-runnerd and wardyn-proxy :local
# tags its image builds overwrite are put back. Before creating its cluster it waits up to 30 minutes for 12 GB of
# available memory, then fails.
# Knobs: KIND_OUTAGE_MIN (70; at least 66, since the checks fall at minutes 35 and 65), KIND_OUTAGE_HOST (example.com,
# the allowed host the agents reach), KIND_OUTAGE_LEGS ("A B"), KIND_OUTAGE_NODE_IMAGE (leg B's kind node image).
# Needs kubectl, kind, helm, docker, go, openssl, jq and curl. Exit 0 when every assertion passed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"
# shellcheck source=scripts/lib/image-build.sh
source "${ROOT}/scripts/lib/image-build.sh"
# shellcheck source=scripts/lib/kind-org.sh
source "${ROOT}/scripts/lib/kind-org.sh"
# shellcheck source=scripts/lib/walk-api.sh
source "${ROOT}/scripts/lib/walk-api.sh"

[[ "${WARDYN_TEST_K8S:-}" == "1" ]] || skip_lane "kind-outage-recover-walk: set WARDYN_TEST_K8S=1 to run it (skipping)."
if [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" != "1" ]]; then
  echo "kind-outage-recover-walk: set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the wardyn-quickstart cluster may be mutated (its Postgres is scaled to 0 for over an hour); refusing." >&2
  exit 2
fi
OUTAGE_MIN="${KIND_OUTAGE_MIN:-70}"
[[ "${OUTAGE_MIN}" =~ ^[0-9]+$ ]] && ((OUTAGE_MIN >= 66)) || { echo "KIND_OUTAGE_MIN must be a whole number of at least 66, got '${OUTAGE_MIN}'" >&2; exit 2; }
HOST="${KIND_OUTAGE_HOST:-example.com}"
LEGS="${KIND_OUTAGE_LEGS:-A B}"
for bin in kubectl kind helm docker go openssl jq curl; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done
wardyn_pick_docker_host

step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
pass() { printf '  \033[1;32m[pass]\033[0m %s\n' "$*"; }
fail() { printf '  \033[1;31m[FAIL]\033[0m %s\n' "$*"; FAILED=1; }
FAILED=0
PAUSED_TEXT="network is paused until Wardyn can check the run again"
POLICY_BASE="\"allowed_domains\":[\"${HOST}\"],\"first_use_approval\":\"always_deny\",\"min_confinement_class\":\"CC1\",\"auto_stop_after_sec\":-1"
# wait_minute N T0 sleeps until N minutes after T0 (a $SECONDS reading).
wait_minute() { local t=$(($2 + $1 * 60 - SECONDS)); ((t <= 0)) || sleep "${t}"; }

# ── Leg A's cluster: the quickstart, through the caller's own kubeconfig ──────
QS_KUBECONFIG="${KUBECONFIG:-}"
QS_CONTEXT="kind-${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
QS_BASE="http://127.0.0.1:${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
TMPDIR="$(mktemp -d /tmp/wardyn-kind-outage-walk.XXXXXX)"
qk() { KUBECONFIG="${QS_KUBECONFIG}" kubectl --context "${QS_CONTEXT}" "$@"; }
qs_api() { walk_api "${QS_BASE}" "${QS_TOKEN}" "${TMPDIR}/qs.json" 20 "$@"; }
qs_psql() { qk -n wardyn exec deployment/postgres -- psql -U wardyn -d wardyn -tAc "$1" 2>&1; }
# qs_https RUN: the agent's HTTPS request to HOST through its proxy, as "<status> <CONNECT status>" (000 when none).
qs_https() {
  qk -n wardyn-runs exec "wardyn-agent-$1" -c agent -- curl -sS -o /dev/null -w '%{http_code} %{http_connect}' \
    --max-time 20 "https://${HOST}/" 2>/dev/null || true
}
# qs_forward RUN: a plain forward request to HOST through the agent's proxy: its body, then its status on the last line.
qs_forward() {
  qk -n wardyn-runs exec "wardyn-agent-$1" -c agent -- sh -c \
    'curl -sS --max-time 20 -x "${HTTPS_PROXY:-${https_proxy:-}}" -w "\n%{http_code}" "http://$0/"' "${HOST}" 2>/dev/null || true
}

QS_PG_DOWN=0
RUN_A="" RUN_B=""
# Leg B's state, read by cleanup.
CREATED="" KEPT=0 WORK="" TAG="outage-walk-$$"
LOCAL_TAGS=(wardyn/wardyn-runnerd:local wardyn/wardyn-proxy:local)
# restore_local_tags puts back the :local tags build_org_images overwrote (or removes one that did not exist before).
restore_local_tags() {
  local t
  ((KEPT)) || return 0
  for t in "${LOCAL_TAGS[@]}"; do
    if docker image inspect "${t%:*}:kept-${TAG}" >/dev/null 2>&1; then
      docker tag "${t%:*}:kept-${TAG}" "${t}" && docker rmi "${t%:*}:kept-${TAG}" >/dev/null 2>&1
    else
      docker rmi "${t}" >/dev/null 2>&1
    fi
  done
  KEPT=0
}
cleanup() {
  local rc=$? id
  set +e
  if ((QS_PG_DOWN)); then
    qk -n wardyn scale deployment/postgres --replicas=1 >/dev/null 2>&1
    qk -n wardyn rollout status deployment/postgres --timeout=180s >/dev/null 2>&1
  fi
  for id in "${RUN_A}" "${RUN_B}"; do
    [[ -n "${id}" ]] && qs_api POST "/api/v1/runs/${id}/kill" >/dev/null 2>&1
  done
  [[ -n "${CREATED}" ]] && kind delete cluster --name "${CREATED}" >/dev/null 2>&1
  restore_local_tags
  [[ -n "${WORK}" ]] && docker rmi "${RUNNERD_IMAGE}" "${PROXY_IMAGE}" "${WARDYND_IMAGE}" "${PG_IMAGE}" "${AGENT_IMAGE}" "${RELAY_IMAGE}" >/dev/null 2>&1
  [[ -n "${WORK}" ]] && rm -rf "${WORK}"
  rm -rf "${TMPDIR}"
  return "${rc}"
}
trap cleanup EXIT

# qs_create NAME OUTAGE: a server-mode run (interactive, idle) with OUTAGE ("" or ',"outage":{...}'); prints its id.
qs_create() {
  local code id
  code="$(qs_api POST /api/v1/runs "{\"agent\":\"claude-code\",\"repo\":\"local:kind-outage-$1\",\"interactive\":true,
    \"inline_policy\":{${POLICY_BASE},\"resources\":{\"cpu_millis\":250,\"memory_mib\":256}$2}}")"
  [[ "${code}" == 200 || "${code}" == 201 ]] || { cat "${TMPDIR}/qs.json" >&2; die "POST /runs for run $1 answered ${code}"; }
  id="$(jq -r '.id // empty' "${TMPDIR}/qs.json")"
  [[ -n "${id}" ]] || die "the create response for run $1 carried no id"
  echo "${id}"
}
# qs_wait_renewed RUN...: waits up to 20 minutes for each RUN's token_renewed_at to move past its reading at RUNNING.
qs_wait_renewed() {
  local id state r0 i
  for id in "$@"; do
    for i in $(seq 1 90); do
      qs_api GET "/api/v1/runs/${id}" >/dev/null && state="$(jq -r '.state // ""' "${TMPDIR}/qs.json")"
      [[ "${state:-}" == RUNNING ]] && break
      sleep 2
    done
    [[ "${state:-}" == RUNNING ]] || die "run ${id} did not reach RUNNING (state '${state:-}')"
    r0="$(qs_psql "SELECT COALESCE(extract(epoch FROM token_renewed_at)::bigint, 0) FROM agent_runs WHERE id = '${id}'")"
    for i in $(seq 1 120); do
      [[ "$(qs_psql "SELECT COALESCE(extract(epoch FROM token_renewed_at)::bigint, 0) FROM agent_runs WHERE id = '${id}'")" != "${r0}" ]] && break
      ((i == 120)) && die "run ${id}'s token was not renewed within 20 minutes of RUNNING"
      sleep 10
    done
    pass "run ${id} is RUNNING and its proxy renewed its token"
  done
}

leg_a() {
  local t0 t1 out code i ok
  step "A: the quickstart cluster (never created or deleted here)"
  qk -n wardyn get deployment wardyn >/dev/null 2>&1 || die "no wardyn release on ${QS_CONTEXT}: run make kind-quickstart first"
  QS_TOKEN="$(qk -n wardyn get secret wardyn-auth -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
  [[ -n "${QS_TOKEN}" ]] || die "could not read the admin token from Secret wardyn-auth"
  wait_healthy "${QS_BASE}" 10 2 || die "${QS_BASE}/healthz did not answer"

  step "A: run A (default outage terms) and run B (strict, a 30-minute allowance), server-mode Kubernetes runs"
  RUN_A="$(qs_create a "")"
  RUN_B="$(qs_create b ',"outage":{"allowance_minutes":30,"strict":true}')"
  qs_wait_renewed "${RUN_A}" "${RUN_B}"
  for id in "${RUN_A}" "${RUN_B}"; do
    out="$(qs_https "${id}")"
    [[ "${out}" == 2* || "${out}" == 3* ]] || die "run ${id} cannot reach ${HOST} before the outage (got '${out}'): the walk can prove nothing"
  done
  pass "both agents reach ${HOST} before the outage"

  step "A: Postgres scaled to 0 for ${OUTAGE_MIN} minutes"
  QS_PG_DOWN=1
  qk -n wardyn scale deployment/postgres --replicas=0 >/dev/null || die "scaling Postgres to 0 failed"
  t0=${SECONDS}
  wait_minute 35 "${t0}"
  out="$(qs_https "${RUN_A}")"
  [[ "${out}" == 2* || "${out}" == 3* ]] && pass "minute 35: run A's ${HOST} answers (${out})" || fail "minute 35: run A's ${HOST} should answer inside its allowance, got '${out}'"
  out="$(qs_https "${RUN_B}")"
  [[ "${out}" == 000* ]] && pass "minute 35: run B's ${HOST} does not answer past its strict allowance (${out})" || fail "minute 35: run B's ${HOST} should not answer past its strict allowance, got '${out}'"
  wait_minute 65 "${t0}"
  out="$(qs_forward "${RUN_A}")"
  [[ "${out##*$'\n'}" == 503 && "${out}" == *"${PAUSED_TEXT}"* ]] \
    && pass "minute 65: run A's proxy answers 503 with the paused body" \
    || fail "minute 65: run A's proxy should answer 503 with the paused body, got '${out//$'\n'/ | }'"
  wait_minute "${OUTAGE_MIN}" "${t0}"

  step "A: Postgres back; run A recovers within 120 s and run B is ended"
  qk -n wardyn scale deployment/postgres --replicas=1 >/dev/null || die "scaling Postgres back failed"
  qk -n wardyn rollout status deployment/postgres --timeout=180s >/dev/null || die "Postgres did not come back"
  QS_PG_DOWN=0
  t1=${SECONDS}
  ok=""
  while ((SECONDS - t1 <= 120)); do
    out="$(qs_https "${RUN_A}")"
    [[ "${out}" == 2* || "${out}" == 3* ]] && { ok=1; break; }
    sleep 5
  done
  [[ -n "${ok}" ]] && pass "run A's ${HOST} answers again $((SECONDS - t1)) s after Postgres came back" || fail "run A's ${HOST} did not answer within 120 s of Postgres coming back (last '${out}')"
  ok=""
  for i in $(seq 1 24); do
    qs_api GET "/api/v1/audit?run_id=${RUN_A}" >/dev/null
    jq -e '[.[] | select(.action == "run.token.recover")] | length > 0' "${TMPDIR}/qs.json" >/dev/null 2>&1 && { ok=1; break; }
    sleep 5
  done
  [[ -n "${ok}" ]] && pass "run A has a run.token.recover audit row ($(jq -c '[.[] | select(.action == "run.token.recover")][0].data' "${TMPDIR}/qs.json"))" || fail "run A has no run.token.recover audit row"
  ok=""
  for i in $(seq 1 24); do
    code="$(qs_api GET "/api/v1/runs/${RUN_B}")"
    [[ "${code}" == 200 ]] && jq -e '.state == "FAILED" and (.failure_hint // "" | contains("strict outage terms"))' "${TMPDIR}/qs.json" >/dev/null && { ok=1; break; }
    sleep 5
  done
  [[ -n "${ok}" ]] && pass "run B is FAILED with the strict reason ($(jq -r '.failure_hint' "${TMPDIR}/qs.json"))" || fail "run B should be FAILED with the strict reason; last read $(jq -c '{state, failure_hint}' "${TMPDIR}/qs.json" 2>/dev/null)"
  for id in "${RUN_A}" "${RUN_B}"; do qs_api POST "/api/v1/runs/${id}/kill" >/dev/null; done
  RUN_A="" RUN_B=""
}

# ── Leg B: runner-mode, on a cluster of its own ───────────────────────────────
CLUSTER="wardyn-outage-walk-$$"
VERSION="outage-walk"
CALICO_VERSION="${K6_CALICO_VERSION:-v3.28.0}"
RUNNERD_IMAGE="wardyn/wardyn-runnerd:${TAG}"
PROXY_IMAGE="wardyn/wardyn-proxy:${TAG}"
WARDYND_IMAGE="wardyn/wardynd:${TAG}"
PG_IMAGE="wardyn/k6-postgres:${TAG}"
AGENT_IMAGE="wardyn/k6-agent:${TAG}"
RELAY_IMAGE="wardyn/hybrid-relay:${TAG}"
ORG_NS=outage-walk-org
ORG_RUNS_NS=outage-walk-orgruns
OWN_NS=outage-walk-runner
RUNS_NS=outage-walk-runs
WALK_NS=outage-walk
RELEASE=outage-walk
ORG_RELEASE=outage-walk-org
ORG_FULL=wardyn-org
ORG_HOST="${ORG_FULL}.${ORG_NS}.svc.cluster.local"
ORG_URL="https://${ORG_HOST}:8080"
SCHED_LABEL="wardyn-outage-walk/sched"
ORG_OIDC=()
DEPLOY=""
CP_PLACE='nodeSelector: {node-role.kubernetes.io/control-plane: ""}
tolerations: [{key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}]'
want() { return 1; } # no k6 suite runs here
# c_connect: agentprobe connect HOST:443 from run C's agent: the proxy's status line and body.
c_connect() { kubectl -n "${RUNS_NS}" exec "wardyn-agent-${RUN_C}" -c agent -- timeout 30 agentprobe connect "${HOST}:443" 2>&1 || true; }
c_psql() { kubectl -n "${ORG_NS}" exec deploy/postgres -- psql -U wardyn -d wardyn -tAc "$1" 2>&1; }
# wait_memory waits up to 30 minutes for 12 GB of available memory.
wait_memory() {
  local avail i
  for i in $(seq 0 60); do
    avail="$(free -g | awk '/^Mem:/{print $7}')"
    ((avail >= 12)) && return 0
    ((i == 0)) && echo "  waiting for 12 GB of available memory before creating ${CLUSTER} (now ${avail} GB; up to 30 minutes)"
    ((i < 60)) && sleep 30
  done
  die "leg B needs 12 GB of available memory before it creates its cluster; ${avail} GB were available after 30 minutes"
}

write_leg_b_values() {
  cat >"${WORK}/kind.yaml" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
nodes:
  - role: control-plane
  - role: worker
EOF
  cat >"${WORK}/org-values.yaml" <<EOF
fullnameOverride: ${ORG_FULL}
serviceAccount: {automount: true}
image: {repository: ${WARDYND_IMAGE%:*}, tag: ${TAG}, pullPolicy: IfNotPresent}
auth: {adminToken: {value: ${ADMIN_TOKEN}}}
secrets: {ageKeyFromSecret: true}
service: {type: NodePort}
${CP_PLACE}
k8s: {enabled: true, proxyImage: ${PROXY_IMAGE}, runsNamespace: ${ORG_RUNS_NS}}
networkPolicy:
  ingress:
    from:
      - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ${OWN_NS}}}
      - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ${WALK_NS}}}
      - ipBlock: {cidr: NODE_CIDR}
env:
  WARDYN_RUNNER_ORG_URL: "${ORG_URL}"
  WARDYN_TLS_CERT: /etc/wardyn/org-tls/tls.crt
  WARDYN_TLS_KEY: /etc/wardyn/org-tls/tls.key
  WARDYN_AGENT_IMAGES: '{"claude-code":"${AGENT_IMAGE}"}'
extraVolumes: [{name: org-tls, secret: {secretName: org-tls}}]
extraVolumeMounts: [{name: org-tls, mountPath: /etc/wardyn/org-tls, readOnly: true}]
EOF
  cat >"${WORK}/values.yaml" <<EOF
org: ${ORG_URL}
runsNamespace: ${RUNS_NS}
sandbox:
  arch: ARCH
image:
  repository: wardyn/wardyn-runnerd
  tag: ${TAG}
proxyImage:
  repository: wardyn/wardyn-proxy
registration:
  existingSecret: k6-registration
trustedCA:
  configMap: k6-org-ca
limits: {maxRuns: 2, totalCPU: 4, totalMemory: 8, totalDisk: 20}
relay:
  servicePort: 8443
${CP_PLACE}
k8s:
  sandbox:
    nodeSelector: {${SCHED_LABEL}: "on"}
egress:
  hardDenyCIDRs: [10.96.0.0/12, 192.168.0.0/16]
  extra:
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ${ORG_NS}}}}]
      ports: [{protocol: TCP, port: 8080}]
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ${WALK_NS}}}}]
      ports: [{protocol: TCP, port: 8080}]
EOF
}

# deploy_forwarder: outage-walk-fwd, a socat TCP relay to the organisation, hardened as hybrid-walk.sh's relay is.
deploy_forwarder() {
  kubectl create namespace "${WALK_NS}" >/dev/null
  kubectl -n "${WALK_NS}" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: outage-walk-fwd}
spec:
  selector: {matchLabels: {app: outage-walk-fwd}}
  template:
    metadata: {labels: {app: outage-walk-fwd}}
    spec:
      ${CP_PLACE//$'\n'/$'\n'      }
      containers:
        - name: fwd
          image: ${RELAY_IMAGE}
          imagePullPolicy: IfNotPresent
          args: ["TCP-LISTEN:8080,fork,reuseaddr", "TCP:${ORG_HOST}:8080"]
          ports: [{containerPort: 8080}]
          securityContext: {runAsUser: 65532, runAsNonRoot: true, readOnlyRootFilesystem: true, allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
---
apiVersion: v1
kind: Service
metadata: {name: outage-walk-fwd}
spec:
  selector: {app: outage-walk-fwd}
  ports: [{port: 8080, targetPort: 8080}]
EOF
  kubectl -n "${WALK_NS}" rollout status deployment/outage-walk-fwd --timeout=120s >/dev/null
}

# runner_online WANT: waits up to 3 minutes for the org runner's online flag to read WANT (true or false).
runner_online() {
  local i
  for i in $(seq 1 36); do
    api "${SEC_TOKEN}" GET "/api/v1/runners/${RUNNER_ID}" >/dev/null
    jq -e ".online == $1" "${WORK}/resp.json" >/dev/null 2>&1 && return 0
    sleep 5
  done
  return 1
}

leg_b() {
  local before after t0 t1 out code pool i r0 ok fwd_ip
  step "B: a runner-mode run through a forwarder, on ${CLUSTER} (a cluster of its own)"
  before="$(kind get clusters 2>/dev/null | sort | tr '\n' ' ')"
  [[ " ${before} " != *" ${CLUSTER} "* ]] || die "a kind cluster named ${CLUSTER} already exists"
  echo "  kind clusters before: ${before:-none}"
  wait_memory
  WORK="$(mktemp -d)"
  log "building images (${TAG}); the :local tags they overwrite are put back after"
  local t
  for t in "${LOCAL_TAGS[@]}"; do
    docker image inspect "${t}" >/dev/null 2>&1 && docker tag "${t}" "${t%:*}:kept-${TAG}"
  done
  KEPT=1
  build_org_images
  restore_local_tags
  wardyn_docker_build -f test/hybrid-walk/Dockerfile.relay -t "${RELAY_IMAGE}" test/hybrid-walk || die "relay image build failed"
  mint_org_tls
  ADMIN_TOKEN="$(openssl rand -hex 24)"
  SEC_TOKEN="wdn_$(openssl rand -hex 32)"
  AGE_KEY="$(docker run --rm "${WARDYND_IMAGE}" -gen-age-key 2>/dev/null)"
  [[ "${AGE_KEY}" == AGE-SECRET-KEY-* ]] || die "wardynd -gen-age-key printed no key"
  write_leg_b_values
  kind create cluster --name "${CLUSTER}" --image "${KIND_OUTAGE_NODE_IMAGE:-kindest/node:v1.35.0}" --config "${WORK}/kind.yaml" \
    --kubeconfig "${WORK}/admin.kc" --wait 180s >"${WORK}/kind.log" 2>&1 || { tail -20 "${WORK}/kind.log"; die "kind could not create ${CLUSTER}"; }
  CREATED="${CLUSTER}"
  prepare_org_cluster
  for node in $(kind get nodes --name "${CLUSTER}"); do load_image "${RELAY_IMAGE}" "${node}"; done
  deploy_org || die "the organisation did not come up (see its pod state and log above)"
  org_runner_token
  install_runnerd "${RELEASE}" "${OWN_NS}" "${RUNS_NS}" "$(jq -r .token "${WORK}/resp.json")" || die "runnerd did not boot"
  DEPLOY="${INSTALLED}"
  claim_org_runner
  [[ "${hub}" == PASS ]] || die "the org runner did not come online: ${detail}"
  pass "runner ${RUNNER_ID} registered with an org token, claimed by its fingerprint, online"

  deploy_forwarder || die "the forwarder did not come up"
  fwd_ip="$(kubectl -n "${WALK_NS}" get svc outage-walk-fwd -o jsonpath='{.spec.clusterIP}')"
  kubectl -n "${OWN_NS}" patch deployment "${DEPLOY}" --type strategic -p \
    "{\"spec\":{\"template\":{\"spec\":{\"hostAliases\":[{\"ip\":\"${fwd_ip}\",\"hostnames\":[\"${ORG_HOST}\"]}]}}}}" >/dev/null \
    || die "pointing runnerd at the forwarder failed"
  kubectl -n "${OWN_NS}" rollout status "deployment/${DEPLOY}" --timeout=300s >/dev/null || die "runnerd did not come back after the host alias"
  runner_online true || die "the runner did not come back online through the forwarder"
  pass "runnerd reaches ${ORG_HOST} through outage-walk-fwd (${fwd_ip})"

  api "${SEC_TOKEN}" POST /api/v1/runner-pools '{"name":"outage-walk"}' >/dev/null
  pool="$(jq -r '.id // empty' "${WORK}/resp.json")"
  api "${SEC_TOKEN}" PUT "/api/v1/runner-pools/${pool}/runners/${RUNNER_ID}" >/dev/null
  code="$(api "${SEC_TOKEN}" POST /api/v1/runs "{\"agent\":\"claude-code\",\"task\":\"--idle\",\"placement\":\"local\",\"runner_pool_id\":\"${pool}\",\"inline_policy\":{${POLICY_BASE}}}")"
  [[ "${code}" == 200 || "${code}" == 201 ]] || die "creating run C answered ${code}: $(cat "${WORK}/resp.json")"
  RUN_C="$(jq -r '.id // empty' "${WORK}/resp.json")"
  for i in $(seq 1 90); do
    api "${SEC_TOKEN}" GET "/api/v1/runs/${RUN_C}" >/dev/null
    jq -e '.state == "RUNNING"' "${WORK}/resp.json" >/dev/null 2>&1 && break
    ((i == 90)) && die "run C did not reach RUNNING: $(jq -c '{state, failure_hint}' "${WORK}/resp.json")"
    sleep 4
  done
  r0="$(c_psql "SELECT COALESCE(extract(epoch FROM token_renewed_at)::bigint, 0) FROM agent_runs WHERE id = '${RUN_C}'")"
  for i in $(seq 1 120); do
    [[ "$(c_psql "SELECT COALESCE(extract(epoch FROM token_renewed_at)::bigint, 0) FROM agent_runs WHERE id = '${RUN_C}'")" != "${r0}" ]] && break
    ((i == 120)) && die "run C's token was not renewed within 20 minutes of RUNNING"
    sleep 10
  done
  out="$(c_connect)"
  [[ "${out}" == *" 200"* ]] || die "run C cannot reach ${HOST} before the cut (got '${out//$'\n'/ | }')"
  pass "run C ${RUN_C} is RUNNING on the runner, renewed, and reaches ${HOST}"

  step "B: the forwarder scaled to 0 for ${OUTAGE_MIN} minutes"
  kubectl -n "${WALK_NS}" scale deployment/outage-walk-fwd --replicas=0 >/dev/null || die "scaling the forwarder to 0 failed"
  t0=${SECONDS}
  runner_online false && pass "the organisation reports the runner offline after the cut" || fail "the runner still reads online 3 minutes after the cut: its link does not go through the forwarder"
  wait_minute 65 "${t0}"
  out="$(c_connect)"
  [[ "${out}" == *" 503"* && "${out}" == *"${PAUSED_TEXT}"* ]] \
    && pass "minute 65: run C's egress is paused" || fail "minute 65: run C's egress should be paused, got '${out//$'\n'/ | }'"
  for i in 65 $((OUTAGE_MIN - 1)); do
    wait_minute "${i}" "${t0}"
    api "${ADMIN_TOKEN}" GET "/api/v1/runs/${RUN_C}" >/dev/null
    jq -e '.state == "RUNNING" and (.lost_at == null)' "${WORK}/resp.json" >/dev/null 2>&1 \
      && pass "minute ${i}: run C is RUNNING and not marked lost" \
      || fail "minute ${i}: run C should not be marked lost while the link is cut: $(jq -c '{state, lost_at, lost_reason}' "${WORK}/resp.json")"
  done
  wait_minute "${OUTAGE_MIN}" "${t0}"

  step "B: the forwarder back; run C recovers within 120 s"
  kubectl -n "${WALK_NS}" scale deployment/outage-walk-fwd --replicas=1 >/dev/null || die "scaling the forwarder back failed"
  kubectl -n "${WALK_NS}" rollout status deployment/outage-walk-fwd --timeout=120s >/dev/null || die "the forwarder did not come back"
  t1=${SECONDS}
  ok=""
  while ((SECONDS - t1 <= 120)); do
    out="$(c_connect)"
    [[ "${out}" == *" 200"* ]] && { ok=1; break; }
    sleep 5
  done
  [[ -n "${ok}" ]] && pass "run C's ${HOST} answers again $((SECONDS - t1)) s after the forwarder came back" || fail "run C's ${HOST} did not answer within 120 s of the forwarder coming back (last '${out//$'\n'/ | }')"
  ok=""
  for i in $(seq 1 24); do
    api "${ADMIN_TOKEN}" GET "/api/v1/audit?run_id=${RUN_C}" >/dev/null
    jq -e '[.[] | select(.action == "run.token.recover" and .data.window_kind == "runner_link")] | length > 0' "${WORK}/resp.json" >/dev/null 2>&1 && { ok=1; break; }
    sleep 5
  done
  [[ -n "${ok}" ]] && pass "run C has a run.token.recover row with window_kind runner_link" || fail "run C has no run.token.recover row with window_kind runner_link: $(jq -c '[.[] | select(.action == "run.token.recover")]' "${WORK}/resp.json" 2>/dev/null)"

  api "${ADMIN_TOKEN}" POST "/api/v1/runs/${RUN_C}/kill" >/dev/null
  kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1
  CREATED=""
  after="$(kind get clusters 2>/dev/null | sort | tr '\n' ' ')"
  echo "  kind clusters after: ${after:-none}"
  [[ "${after}" == "${before}" ]] && pass "kind clusters are as before leg B (${CLUSTER} deleted, the others untouched)" || fail "kind clusters changed across leg B: before '${before}', after '${after}'"
}

RUN_C=""
case " ${LEGS} " in *" A "*) leg_a ;; esac
case " ${LEGS} " in *" B "*) leg_b ;; esac
if ((FAILED)); then
  echo "kind-outage-recover-walk: FAILED" >&2
  exit 1
fi
echo "kind-outage-recover-walk: PASS"
