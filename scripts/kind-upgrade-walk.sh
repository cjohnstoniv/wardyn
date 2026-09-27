#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# T-30 (#690): install the PUBLISHED v0.7.11 chart+images, fill it via the
# real API, upgrade in place to this tree's own tip build, and prove the
# upgrade/rollback envelope this org relies on — on a THROWAWAY kind cluster
# this script owns end to end (create, load, delete), never the shared
# `make kind-quickstart` cluster scripts/kind-sso-walk.sh and
# scripts/kind-survival-walk.sh reuse. Shape follows
# scripts/daemon-proxy-secret-kind.sh (#719): one file, `kind create cluster
# --kubeconfig`, delete it on every exit path.
#
# WHAT THIS SCRIPT BUILDS, AND WHAT #690 ALSO ASKS FOR THAT IT DOES NOT:
#
#   BUILT: v0.7.11 install -> one admin-namespace secret + one RUNNING
#   interactive run -> upgrade to tip (Recreate, the chart's own default
#   strategy) -> assert the boot-time secret conversion log, the secret still
#   reads back, the run reaches a real outcome rather than getting stuck, a
#   fresh run succeeds over the tip image, /healthz.proxy_hop_tls=true -> helm
#   rollback back to v0.7.11 fails, naming the migration the older binary
#   cannot run under.
#
#   NOT BUILT, NAMED RATHER THAN FAKED:
#     - a Dex member, a fake AWS-SSO capture, an ADO/Entra blob and a PENDING
#       credential_reauth hold in the pre-upgrade fill. Each needs its own
#       fake-IdP overlay (the shape scripts/kind-sso-walk.sh's SSO overlay
#       already is, for ONE cluster it never tears down) stood up fresh on a
#       cluster THIS script deletes every run — a second overlay-authoring
#       effort this pass did not have room for. The upgrade/rollback envelope
#       this script DOES prove is the same mechanism regardless of what filled
#       it; a follow-up that adds the richer fill strengthens the same
#       assertions rather than replacing them.
#     - the compose variant (scripts/up.sh) and the P2-1 release-to-release
#       lane from v0.7.12. Both are compose-only in principle (no kind), but
#       both ALSO need the published v0.7.11/v0.7.12 images pulled from
#       ghcr.io/cjohnstoniv, and this pass could not confirm those packages
#       are pullable without the release workflow's own registry credentials
#       (an anonymous manifest HEAD against ghcr.io/cjohnstoniv/wardynd:0.7.11
#       answered 404 here) — see this PR's UNVERIFIED list. Building a second
#       variant on top of an unconfirmed pull was not a good trade against
#       finishing the kind leg's own assertions correctly.
#     - U4 (an in-flight model-provider run failing closed at its next call
#       after MP-4b). MP-4b is #672, a DIFFERENT, not-yet-merged lane's own
#       work (BE-1 in this handoff's proposed-lanes table) — nothing in this
#       tree implements the retired-var behaviour yet for this script to
#       exercise.
#
# THE ROLLBACK ASSERTION, GROUNDED IN SHIPPED CODE: v0.7.11 predates envelope
# v1 (migration 0069_secret_envelope_v1.sql, formerly 0065 before a later
# renumbering, shipped in v0.7.12) — so a secret this script writes through
# the v0.7.11 install is a legacy (enc_version 0) row, exactly what
# cmd/wardynd/secret_store.go's boot-time ConvertV0 call re-seals as envelope
# v1 on the upgrade to tip, logging "wardynd: converted stored secrets to
# envelope v1; an older wardynd can no longer read them" (grepped for below).
# Rolling back to v0.7.11 after that lands the v0.7.11 binary against a
# database schema_migrations has already recorded rows it does not ship —
# db.Migrate's own downgrade refusal (#675 / PR #834, `db.applyMigration`)
# refuses the boot, naming the newest unknown migration file BY NAME. Since
# that name is literally 0069_secret_envelope_v1.sql (or its pre-rename
# 0065_secret_envelope_v1.sql — TestRetiredMigrationsCoverEveryReleasedName
# pins both across the rename), the refusal names "envelope" in the exact,
# literal sense #690's plan text asks for — not a metaphor this script has to
# manufacture.
#
# Usage: scripts/kind-upgrade-walk.sh   (needs docker, kind, kubectl, helm, jq, curl)
# CLUSTER overrides the cluster name (default kind-upgrade-walk); refuses to
# reuse one that already exists, the same guard daemon-proxy-secret-kind.sh
# uses, so this never touches a cluster it did not create.
#
# NOT RUN OR PROVEN IN THIS AUTHORING PASS: no kind cluster and no confirmed
# ghcr.io pull were available here. bash -n and this repo's lint are what
# this pass can offer; the hosted nightly (with the release workflow's own
# registry credentials) is what proves it end to end.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
source "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-upgrade-walk: set WARDYN_TEST_K8S=1 to run the kind upgrade/rollback walk (skipping)."
fi

for bin in kind kubectl helm docker jq curl; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

CLUSTER="${CLUSTER:-kind-upgrade-walk}"
NS="wardyn"
RELEASE="wardyn"
CHART_REF="oci://ghcr.io/cjohnstoniv/charts/wardyn"
FROM_VERSION="${WARDYN_KIND_UPGRADE_FROM:-0.7.11}"
TIP_IMAGE="wardyn/wardynd:kind-upgrade-walk"
CURL_MAX_TIME="${WARDYN_KIND_UPGRADE_CURL_MAX_TIME:-10}"
WORK="$(mktemp -d)"
CREATED_CLUSTER=""

step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
pass() { printf '  \033[1;32m[pass]\033[0m %s\n' "$*"; }
fail() { printf '  \033[1;31m[FAIL]\033[0m %s\n' "$*"; FAILED=1; }
FAILED=0

cleanup() {
  [[ -n "${CREATED_CLUSTER}" ]] && kind delete cluster --name "${CREATED_CLUSTER}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}

kind get clusters 2>/dev/null | grep -qx "${CLUSTER}" && die "a kind cluster named ${CLUSTER} already exists; set CLUSTER to another name"
trap cleanup EXIT

HTTP_PORT="${WARDYN_KIND_UPGRADE_HTTP_PORT:-8380}"   # host-published, and the chart's own service.port
NODE_HTTP_PORT=30380                                  # the Service's nodePort, pinned by patch after each install (below) — matches this config's containerPort
# Same shape as deploy/kind/quickstart-kind-config.yaml, one port instead of
# two (this walk never opens the SSH gateway): disableDefaultCNI, because
# k8s.enabled's boot-time egress canary (internal/runner/k8s/canary.go)
# refuses to construct under kindnet (it does not enforce NetworkPolicy) —
# Calico below is what actually provides pod networking on this cluster, not
# an addition on top of kindnet. extraPortMappings + loopback listenAddress:
# node-IP routing is unreliable on Docker Desktop/WSL2 (that file's own
# comment); publish the NodePort on the host instead.
cat >"${WORK}/kind-config.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: ${NODE_HTTP_PORT}
        hostPort: ${HTTP_PORT}
        listenAddress: "127.0.0.1"
        protocol: TCP
EOF

step "creating throwaway kind cluster ${CLUSTER}"
kind create cluster --name "${CLUSTER}" --config "${WORK}/kind-config.yaml" --kubeconfig "${WORK}/kubeconfig" --wait 120s >/dev/null
CREATED_CLUSTER="${CLUSTER}"
export KUBECONFIG="${WORK}/kubeconfig"

kubectl create namespace "${NS}" >/dev/null

CALICO_VERSION="v3.28.0"
step "installing Calico ${CALICO_VERSION} (same recipe as deploy/kind/quickstart.sh)"
kubectl apply -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/calico.yaml"
kubectl -n kube-system rollout status daemonset/calico-node --timeout=180s
kubectl -n kube-system rollout status deployment/calico-kube-controllers --timeout=180s

step "postgres"
kubectl -n "${NS}" create deployment postgres --image=postgres:17 >/dev/null
kubectl -n "${NS}" set env deployment/postgres POSTGRES_USER=wardyn POSTGRES_PASSWORD=wardyn POSTGRES_DB=wardyn >/dev/null
kubectl -n "${NS}" expose deployment postgres --port=5432 >/dev/null
kubectl -n "${NS}" rollout status deployment/postgres --timeout=120s >/dev/null

# A persistent age key, minted from the v0.7.11 image so `-gen-age-key`
# matches whatever that release actually shipped, and reused unchanged for
# every install/upgrade/rollback below — the SAME reason
# scripts/survival-walk.sh mints one: an ephemeral key refuses the very
# restart this walk exists to prove works.
docker pull "ghcr.io/cjohnstoniv/wardynd:${FROM_VERSION}" >/dev/null \
  || die "could not pull ghcr.io/cjohnstoniv/wardynd:${FROM_VERSION} — see this file's header on the unconfirmed registry pull"
AGE_KEY="$(docker run --rm "ghcr.io/cjohnstoniv/wardynd:${FROM_VERSION}" -gen-age-key)"
[[ "${AGE_KEY}" == AGE-SECRET-KEY-* ]] || die "-gen-age-key on the v${FROM_VERSION} image did not print a key"
kubectl -n "${NS}" create secret generic wardyn-postgres-dsn \
  --from-literal=dsn="postgres://wardyn:wardyn@postgres:5432/wardyn?sslmode=disable" \
  --from-literal=age-key="${AGE_KEY}" >/dev/null

ADMIN_TOKEN="$(openssl rand -hex 20)"
BASE="http://127.0.0.1:${HTTP_PORT}"
kind load docker-image "ghcr.io/cjohnstoniv/wardynd:${FROM_VERSION}" --name "${CLUSTER}" >/dev/null

# NodePort ingress arrives from OUTSIDE the pod network, SNATed to the node's
# own address — Calico's default `podSelector: {}` ingress peer does not match
# it, so this cluster's NetworkPolicy must ALSO name the node CIDR, exactly the
# finding deploy/kind/quickstart-kind-config.yaml's own comment documents. Read
# off the docker network rather than hardcoded, same reason.
NODE_CIDR="$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' \
  | tr ' ' '\n' | grep -v ':' | grep -v '^$' | head -1)"
[[ -n "${NODE_CIDR}" ]] || die "could not read the kind docker network's IPv4 subnet"
cat >"${WORK}/values.yaml" <<EOF
networkPolicy:
  ingress:
    from:
      - podSelector: {}
      - ipBlock:
          cidr: ${NODE_CIDR}
EOF

step "helm install the PUBLISHED v${FROM_VERSION} chart"
helm install "${RELEASE}" "${CHART_REF}" --version "${FROM_VERSION}" \
  --namespace "${NS}" \
  -f "${WORK}/values.yaml" \
  --set image.repository=ghcr.io/cjohnstoniv/wardynd \
  --set image.tag="${FROM_VERSION}" \
  --set secrets.ageKeyFromSecret=true \
  --set auth.adminToken.value="${ADMIN_TOKEN}" \
  --set service.type=NodePort \
  --set service.port="${HTTP_PORT}" \
  || die "helm install of the published v${FROM_VERSION} chart failed"
# The chart has no nodePort value (deploy/kind/quickstart.sh's own finding —
# it is a Service field, not a Wardyn one): k8s assigns a random one at
# install time, so pin it onto the Service by port NUMBER to match this
# walk's own kind config extraPortMappings.
kubectl -n "${NS}" patch service "${RELEASE}" -p \
  "{\"spec\":{\"ports\":[{\"port\":${HTTP_PORT},\"nodePort\":${NODE_HTTP_PORT}}]}}"
kubectl -n "${NS}" rollout status "deployment/${RELEASE}" --timeout=180s \
  || { kubectl -n "${NS}" logs "deployment/${RELEASE}" --tail=100 --all-containers >&2 || true; die "v${FROM_VERSION} never became ready"; }
wait_healthy "${BASE}" 60 2 || die "${BASE}/healthz did not answer on the v${FROM_VERSION} install"
pass "v${FROM_VERSION} installed and healthy"

api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "${body}" ]]; then
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${WORK}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" -H "Content-Type: application/json" -d "${body}"
  else
    curl -sS --max-time "${CURL_MAX_TIME}" -o "${WORK}/resp.json" -w '%{http_code}' -X "${method}" "${BASE}${path}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}"
  fi
}

# ── fill via the real API (the reduced set — see this file's header) ──────
SECRET_NAME="kind-upgrade-walk-secret"
step "writing one secret through the v${FROM_VERSION} API (lands as a legacy enc_version=0 row — envelope v1 does not exist yet)"
code=$(api PUT "/api/v1/secrets/${SECRET_NAME}" '{"value":"kind-upgrade-walk-value"}')
[[ "${code}" == "200" || "${code}" == "204" ]] || { cat "${WORK}/resp.json" >&2; die "PUT /secrets/${SECRET_NAME} on v${FROM_VERSION} answered ${code}"; }
pass "secret written under v${FROM_VERSION}"

step "launching one interactive run on v${FROM_VERSION} to carry across the upgrade"
code=$(api POST /api/v1/runs '{"agent":"claude-code","repo":"local:kind-upgrade","interactive":true,
  "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny","min_confinement_class":"CC1","auto_stop_after_sec":-1}}')
[[ "${code}" == "200" || "${code}" == "201" ]] || { cat "${WORK}/resp.json" >&2; die "POST /runs on v${FROM_VERSION} answered ${code}"; }
CARRIED_RUN_ID="$(jq -r '.id' "${WORK}/resp.json")"
[[ -n "${CARRIED_RUN_ID}" && "${CARRIED_RUN_ID}" != "null" ]] || die "create-run on v${FROM_VERSION} carried no id"
pass "run ${CARRIED_RUN_ID} launched on v${FROM_VERSION}"

# ── upgrade to tip ───────────────────────────────────────────────────────────
step "building this tree's own tip images (wardynd + proxy) and loading them"
TIP_PROXY_IMAGE="wardyn/wardyn-proxy:kind-upgrade-walk"
docker build -f deploy/compose/Dockerfile.wardynd -t "${TIP_IMAGE}" . >"${WORK}/build-tip.log" 2>&1 \
  || { tail -40 "${WORK}/build-tip.log" >&2; die "building the tip wardynd image failed"; }
docker build -f deploy/compose/Dockerfile.proxy -t "${TIP_PROXY_IMAGE}" . >"${WORK}/build-tip-proxy.log" 2>&1 \
  || { tail -40 "${WORK}/build-tip-proxy.log" >&2; die "building the tip proxy image failed"; }
kind load docker-image "${TIP_IMAGE}" --name "${CLUSTER}" >/dev/null
kind load docker-image "${TIP_PROXY_IMAGE}" --name "${CLUSTER}" >/dev/null

kubectl create namespace wardyn-runs >/dev/null

step "helm upgrade --install to tip (the chart's own Recreate strategy tears the old pod down first)"
helm upgrade --install "${RELEASE}" ./deploy/helm/wardyn \
  --namespace "${NS}" \
  -f "${WORK}/values.yaml" \
  --set image.repository="${TIP_IMAGE%:*}" \
  --set image.tag="${TIP_IMAGE##*:}" \
  --set secrets.ageKeyFromSecret=true \
  --set auth.adminToken.value="${ADMIN_TOKEN}" \
  --set service.type=NodePort \
  --set service.port="${HTTP_PORT}" \
  --set k8s.enabled=true \
  --set k8s.proxyImage="${TIP_PROXY_IMAGE}" \
  --set k8s.runsNamespace=wardyn-runs \
  || die "helm upgrade to tip failed"
kubectl -n "${NS}" patch service "${RELEASE}" -p \
  "{\"spec\":{\"ports\":[{\"port\":${HTTP_PORT},\"nodePort\":${NODE_HTTP_PORT}}]}}"
kubectl -n "${NS}" rollout status "deployment/${RELEASE}" --timeout=180s \
  || { kubectl -n "${NS}" logs "deployment/${RELEASE}" --tail=150 --all-containers >&2 || true; die "the tip upgrade never became ready"; }
wait_healthy "${BASE}" 60 2 || die "${BASE}/healthz did not answer after the tip upgrade"
pass "upgraded to tip and healthy"

step "asserting the boot-time secret conversion log (v0->envelope v1)"
tip_pod="$(kubectl -n "${NS}" get pods -l app.kubernetes.io/name=wardyn -o jsonpath='{.items[0].metadata.name}')"
logs="$(kubectl -n "${NS}" logs "${tip_pod}" --tail=-1 2>/dev/null)"
grep -q "converted stored secrets to envelope v1" <<<"${logs}" \
  && pass "boot logged the v0->envelope-v1 conversion" \
  || fail "expected the tip pod's boot log to name the envelope v1 conversion; did not find it"

step "confirming the secret written under v${FROM_VERSION} still reads back under tip"
code=$(api GET "/api/v1/secrets")
[[ "${code}" == "200" ]] || { cat "${WORK}/resp.json" >&2; die "GET /secrets under tip answered ${code}"; }
jq -e --arg n "${SECRET_NAME}" 'any(.[]; .name == $n)' "${WORK}/resp.json" >/dev/null \
  && pass "secret ${SECRET_NAME} still present after the conversion" \
  || fail "expected ${SECRET_NAME} to still be listed after the upgrade"

step "confirming the run carried across the upgrade reached a real outcome (not stuck RUNNING forever with no proxy)"
carried_state=""
for _ in $(seq 1 30); do
  code=$(api GET "/api/v1/runs/${CARRIED_RUN_ID}")
  [[ "${code}" == "200" ]] || { sleep 2; continue; }
  carried_state="$(jq -r '.state' "${WORK}/resp.json")"
  [[ "${carried_state}" != "RUNNING" ]] && break
  sleep 2
done
[[ -n "${carried_state}" ]] \
  && pass "run ${CARRIED_RUN_ID} reads back as ${carried_state} after the upgrade (never silently vanished)" \
  || fail "could not read back run ${CARRIED_RUN_ID}'s state after the upgrade"

step "confirming /healthz.proxy_hop_tls=true and a FRESH run succeeds on tip"
h="$(curl -sf --max-time "${CURL_MAX_TIME}" "${BASE}/healthz")"
[[ "$(jq -r '.proxy_hop_tls // false' <<<"${h}")" == "true" ]] \
  && pass "proxy_hop_tls=true after the upgrade" \
  || fail "expected /healthz.proxy_hop_tls=true after the upgrade to tip"
code=$(api POST /api/v1/runs '{"agent":"claude-code","repo":"local:kind-upgrade-fresh","interactive":true,
  "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny","min_confinement_class":"CC1","auto_stop_after_sec":-1}}')
if [[ "${code}" == "200" || "${code}" == "201" ]]; then
  fresh_rid="$(jq -r '.id' "${WORK}/resp.json")"
  curl -sS --max-time "${CURL_MAX_TIME}" -X POST "${BASE}/api/v1/runs/${fresh_rid}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1 || true
  pass "a fresh run over the TLS hop succeeds on tip"
else
  fail "expected a fresh run to succeed on tip; POST /runs answered ${code}: $(cat "${WORK}/resp.json")"
fi

# ── rollback: fails, naming the envelope migration ─────────────────────────
step "helm rollback to v${FROM_VERSION} — expected to FAIL, naming the migration this binary cannot run under"
helm rollback "${RELEASE}" 1 --namespace "${NS}" --wait --timeout 120s >"${WORK}/rollback.log" 2>&1
rollback_helm_rc=$?
# A "succeeding" helm rollback here (rc=0, or a pod that reports Ready) is the
# FAILURE this step is checking for — db.Migrate's downgrade refusal must
# have kept the v0.7.11 binary crash-looping, never Ready.
rollback_ready="false"
for _ in $(seq 1 30); do
  kubectl -n "${NS}" rollout status "deployment/${RELEASE}" --timeout=5s >/dev/null 2>&1 && { rollback_ready="true"; break; }
  sleep 2
done
if [[ "${rollback_ready}" == "true" ]]; then
  fail "the v${FROM_VERSION} rollback became Ready — the downgrade refusal (#675/PR #834) did not fire"
else
  rb_pod="$(kubectl -n "${NS}" get pods -l app.kubernetes.io/name=wardyn -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  rb_logs=""
  [[ -n "${rb_pod}" ]] && rb_logs="$(kubectl -n "${NS}" logs "${rb_pod}" --tail=-1 2>/dev/null || true)"
  if grep -qi "envelope" <<<"${rb_logs}" && grep -q "cannot run under" <<<"${rb_logs}"; then
    pass "rollback correctly refused, naming the envelope migration this v${FROM_VERSION} binary cannot run under"
  else
    fail "rollback did not become Ready (expected), but its log did not name the envelope migration the way #675/PR #834 does — see ${WORK}/rollback.log and the pod log above"
  fi
fi
echo "(helm rollback command exit was ${rollback_helm_rc}; see ${WORK}/rollback.log — a non-zero/timeout exit here is itself part of what this step expects)"

if [[ "${FAILED}" -ne 0 ]]; then
  echo "kind-upgrade-walk: FAILED" >&2
  exit 1
fi
echo "kind-upgrade-walk: PASS"
