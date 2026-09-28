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
#   BUILT: v0.7.11 install WITH the k8s substrate (its proxy and agent images
#   built from the v0.7.11 tag: only wardynd is published) -> one
#   admin-namespace secret + one interactive run that genuinely reaches
#   RUNNING -> upgrade to tip (Recreate, the chart's own default strategy) ->
#   assert the boot-time secret conversion log, the secret still reads back,
#   the carried run keeps the documented pre-0.7.12 outcome (see "THE CARRIED
#   RUN" below), a fresh run succeeds over the tip image,
#   /healthz.proxy_hop_tls=true -> helm
#   rollback back to v0.7.11 recorded as evidence (informational — no
#   published release yet carries the downgrade refusal that would let this
#   assert a specific pass/fail outcome; see "THE ROLLBACK ASSERTION" below).
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
# THE ROLLBACK ASSERTION, CORRECTED AGAINST RELEASE DATES: an earlier version
# of this script asserted that rolling back to v${FROM_VERSION} would refuse
# to boot, naming db.Migrate's downgrade guard (#675 / PR #834,
# `db.applyMigration`'s "this wardynd does not ship ... cannot run under"
# error). That guard was merged 2026-09-25 — AFTER v0.7.11 (tagged
# 2026-09-22) AND v0.7.12 (tagged 2026-09-23). `git show v0.7.11:internal/
# db/db.go` and the v0.7.12 equivalent both have zero matches for "cannot run
# under" — neither published release this walk could use as FROM_VERSION
# contains the guard, so no published baseline can exercise this refusal
# today, and asserting specific refusal text no shipped binary can produce
# would be exactly the kind of unmeasured claim this handoff's own rules
# forbid. (Confirmed 0069_secret_envelope_v1.sql — the migration the
# original assertion expected the refusal to name — is also absent from
# v0.7.11's tree, matching this same gap.)
#
# What the rollback step below asserts instead, and why it is INFORMATIONAL
# rather than pass/fail: v${FROM_VERSION} has NO downgrade guard of any kind
# (confirmed by the git-show above), so it will attempt to boot against
# whatever the tip upgrade above left in the schema — real, unrelated schema
# changes shipped since v0.7.11 (e.g. 0074_user_tier_rename.sql's role-value
# rewrite) mean this is not guaranteed to be clean, but nothing in this binary
# checks for that either way, so this script cannot predict Ready vs.
# crash-loop from reading the code alone, and did not run it to find out (the
# lane rules this pass operates under forbid creating a kind cluster to
# check). The step records what actually happened — Ready, or not, with logs
# saved as evidence — without asserting a specific outcome it cannot prove.
# Follow-up: once a release ships PR #834's guard, FROM_VERSION should move to
# it and this step should go back to a hard pass/fail assertion on the
# refusal text.
#
# THE CARRIED RUN, AND WHY IT IS NOT ASSERTED TERMINAL. A v0.7.11 proxy
# predates the TLS control-plane hop (0.7.12, #561): it dials wardynd's console
# listener in plaintext. The documented outcome after an upgrade past #1263 is
# CHANGELOG.md [Unreleased] "Once the control-plane hop is TLS, the console
# listener refuses /api/v1/internal/*" and docs/OPERATIONS.md's proxy-facing
# TLS section: such a proxy "gets 404 on every call after an upgrade (on the
# chart it has no route back at all ...)"; on Kubernetes the operator stops
# such runs and starts new ones, because the restart refuses them ("stop any
# run dispatched before 0.7.12 before you upgrade", CHANGELOG.md [Unreleased],
# the #606 entry; #1342). Nothing documents the upgrade
# itself ending the run, and its token
# only lapses an hour on (runTokenLapseAfter). So the walk asserts: the run
# still reads RUNNING on the SAME two pods (neither ended, lost nor
# re-dispatched by the upgrade), GET /admin/runs/proxy-window lists it outside
# the supported window (proxy_release '' — started before migration 0084),
# the documented refusal holds: on Kubernetes the substrate implements no
# runner.ProxyReviver, so the single-run revive answers 409 and the bulk
# restart's result for the run is not ok, both with reason revive_unsupported
# and runner.ErrReviveUnsupported's text, and neither touches the pods; and
# the operator's stop ends it cleanly: KILLED, both pods gone.
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
# The runs namespace, before BOTH installs: the chart never creates one.
kubectl create namespace wardyn-runs >/dev/null

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

# The v0.7.11 proxy and agent images, built from the tag's own tree: only
# wardynd is published, and a run carried across the upgrade has to have been
# dispatched by v0.7.11 with v0.7.11's sidecar for the upgrade to mean anything.
FROM_PROXY_IMAGE="wardyn/wardyn-proxy:kind-upgrade-from"
FROM_AGENT_IMAGE="wardyn/agent-claude-code:kind-upgrade-from"
step "building the v${FROM_VERSION} proxy and claude-code agent images from the v${FROM_VERSION} tag"
git rev-parse -q --verify "refs/tags/v${FROM_VERSION}" >/dev/null \
  || git fetch -q --depth 1 origin "refs/tags/v${FROM_VERSION}:refs/tags/v${FROM_VERSION}" \
  || die "could not fetch tag v${FROM_VERSION}"
mkdir -p "${WORK}/from-src"
git archive "v${FROM_VERSION}" | tar -x -C "${WORK}/from-src" || die "could not export the v${FROM_VERSION} tree"
docker build -f "${WORK}/from-src/deploy/compose/Dockerfile.proxy" -t "${FROM_PROXY_IMAGE}" "${WORK}/from-src" >"${WORK}/build-from-proxy.log" 2>&1 \
  || { tail -40 "${WORK}/build-from-proxy.log" >&2; die "building the v${FROM_VERSION} proxy image failed"; }
docker build -f "${WORK}/from-src/deploy/images/claude-code/Dockerfile" -t "${FROM_AGENT_IMAGE}" "${WORK}/from-src" >"${WORK}/build-from-agent.log" 2>&1 \
  || { tail -40 "${WORK}/build-from-agent.log" >&2; die "building the v${FROM_VERSION} claude-code agent image failed"; }
kind load docker-image "${FROM_PROXY_IMAGE}" --name "${CLUSTER}" >/dev/null
kind load docker-image "${FROM_AGENT_IMAGE}" --name "${CLUSTER}" >/dev/null

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
env:
  # The agent image is loaded into this cluster, not pullable from ghcr.
  WARDYN_AGENT_IMAGES: '{"claude-code":"${FROM_AGENT_IMAGE}"}'
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
  --set k8s.enabled=true \
  --set k8s.proxyImage="${FROM_PROXY_IMAGE}" \
  --set k8s.runsNamespace=wardyn-runs \
  --set serviceAccount.automount=true \
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
carried_state=""
for _ in $(seq 1 120); do
  code=$(api GET "/api/v1/runs/${CARRIED_RUN_ID}")
  [[ "${code}" == "200" ]] && carried_state="$(jq -r '.state' "${WORK}/resp.json")"
  case "${carried_state}" in RUNNING|COMPLETED|FAILED|KILLED|STOPPED|ARCHIVED) break ;; esac
  sleep 2
done
[[ "${carried_state}" == "RUNNING" ]] || {
  cat "${WORK}/resp.json" >&2
  kubectl -n wardyn-runs describe pods >&2 || true
  die "run ${CARRIED_RUN_ID} never reached RUNNING on v${FROM_VERSION} (last read: ${carried_state:-<unreadable>})"
}
AGENT_POD="wardyn-agent-${CARRIED_RUN_ID}"
PROXY_POD="wardyn-proxy-${CARRIED_RUN_ID}"
pod_uid() { kubectl -n wardyn-runs get pod "$1" -o jsonpath='{.metadata.uid}/{.status.phase}' 2>/dev/null; }
AGENT_UID_BEFORE="$(pod_uid "${AGENT_POD}")"
PROXY_UID_BEFORE="$(pod_uid "${PROXY_POD}")"
[[ "${AGENT_UID_BEFORE}" == */Running && "${PROXY_UID_BEFORE}" == */Running ]] \
  || die "run ${CARRIED_RUN_ID} reads RUNNING but its pods are not both Running (agent=${AGENT_UID_BEFORE:-absent} proxy=${PROXY_UID_BEFORE:-absent})"
pass "run ${CARRIED_RUN_ID} RUNNING on v${FROM_VERSION}, agent and proxy pods Running"

# ── upgrade to tip ───────────────────────────────────────────────────────────
step "building this tree's own tip images (wardynd + proxy) and loading them"
TIP_PROXY_IMAGE="wardyn/wardyn-proxy:kind-upgrade-walk"
docker build -f deploy/compose/Dockerfile.wardynd -t "${TIP_IMAGE}" . >"${WORK}/build-tip.log" 2>&1 \
  || { tail -40 "${WORK}/build-tip.log" >&2; die "building the tip wardynd image failed"; }
docker build -f deploy/compose/Dockerfile.proxy -t "${TIP_PROXY_IMAGE}" . >"${WORK}/build-tip-proxy.log" 2>&1 \
  || { tail -40 "${WORK}/build-tip-proxy.log" >&2; die "building the tip proxy image failed"; }
kind load docker-image "${TIP_IMAGE}" --name "${CLUSTER}" >/dev/null
kind load docker-image "${TIP_PROXY_IMAGE}" --name "${CLUSTER}" >/dev/null

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
  --set serviceAccount.automount=true \
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
# GET /secrets answers {"names": [...], "mine": [...]} (handleListSecrets): the
# admin's own namespace is `mine`, a list of bare names.
jq -e --arg n "${SECRET_NAME}" 'any(.mine[]; . == $n)' "${WORK}/resp.json" >/dev/null \
  && pass "secret ${SECRET_NAME} still present after the conversion" \
  || fail "expected ${SECRET_NAME} to still be listed after the upgrade"

step "the run carried across the upgrade keeps the documented pre-0.7.12 outcome (see THE CARRIED RUN above)"
# Sampled for 20 s, not read once: a boot sweep that ended or re-dispatched it
# a moment after the first read must still fail this.
carried_ok=1
for _ in $(seq 1 10); do
  code=$(api GET "/api/v1/runs/${CARRIED_RUN_ID}")
  carried_state="$([[ "${code}" == "200" ]] && jq -r '.state' "${WORK}/resp.json")"
  [[ "${carried_state}" == "RUNNING" ]] || { carried_ok=""; break; }
  sleep 2
done
[[ -n "${carried_ok}" ]] \
  && pass "run ${CARRIED_RUN_ID} still RUNNING after the upgrade (neither ended nor lost by it)" \
  || fail "run ${CARRIED_RUN_ID} left RUNNING across the upgrade (read: ${carried_state:-<unreadable>}: $(cat "${WORK}/resp.json"))"
agent_after="$(pod_uid "${AGENT_POD}")"
proxy_after="$(pod_uid "${PROXY_POD}")"
[[ "${agent_after}" == "${AGENT_UID_BEFORE}" && "${proxy_after}" == "${PROXY_UID_BEFORE}" ]] \
  && pass "the same v${FROM_VERSION} agent and proxy pods are still Running (not re-dispatched)" \
  || fail "the carried run's pods changed across the upgrade: agent ${AGENT_UID_BEFORE} -> ${agent_after:-absent}, proxy ${PROXY_UID_BEFORE} -> ${proxy_after:-absent}"
code=$(api GET /api/v1/admin/runs/proxy-window)
[[ "${code}" == "200" ]] && jq -e --arg id "${CARRIED_RUN_ID}" 'any(.outside[]; .run_id == $id and .proxy_release == "")' "${WORK}/resp.json" >/dev/null \
  && pass "GET /admin/runs/proxy-window lists it outside the supported window (proxy_release '')" \
  || fail "expected GET /admin/runs/proxy-window to list ${CARRIED_RUN_ID} outside the window; answered ${code}: $(cat "${WORK}/resp.json")"
# The documented refusal (docs/operations/kubernetes-known-gaps.md, #1342):
# status, reason and runner.ErrReviveUnsupported's text, on both doors.
revive_refusal="runner: this substrate cannot replace a sandbox's proxy"
code=$(api POST /api/v1/admin/runs/restart "{\"run_ids\":[\"${CARRIED_RUN_ID}\"]}")
[[ "${code}" == "200" ]] && jq -e --arg id "${CARRIED_RUN_ID}" --arg e "${revive_refusal}" \
  '.results | length == 1 and (.[0] | .run_id == $id and .ok == false and .reason == "revive_unsupported" and .error == $e and (.lost_again // false) == false)' \
  "${WORK}/resp.json" >/dev/null \
  && pass "POST /admin/runs/restart refuses it: ok=false, reason revive_unsupported" \
  || fail "expected POST /admin/runs/restart to refuse ${CARRIED_RUN_ID} with reason revive_unsupported; answered ${code}: $(cat "${WORK}/resp.json")"
code=$(api POST "/api/v1/runs/${CARRIED_RUN_ID}/revive")
[[ "${code}" == "409" ]] && jq -e --arg e "${revive_refusal}" '.reason == "revive_unsupported" and .error == $e' "${WORK}/resp.json" >/dev/null \
  && pass "POST /runs/{id}/revive refuses it: 409, reason revive_unsupported" \
  || fail "expected POST /runs/${CARRIED_RUN_ID}/revive to answer 409 revive_unsupported; answered ${code}: $(cat "${WORK}/resp.json")"
[[ "$(pod_uid "${AGENT_POD}")" == "${AGENT_UID_BEFORE}" && "$(pod_uid "${PROXY_POD}")" == "${PROXY_UID_BEFORE}" ]] \
  && pass "the refusals changed nothing: the same v${FROM_VERSION} pods are still there" \
  || fail "the refused restart changed the carried run's pods: agent=$(pod_uid "${AGENT_POD}") proxy=$(pod_uid "${PROXY_POD}")"
kubectl -n wardyn-runs logs "${PROXY_POD}" --all-containers --tail=5 2>&1 | sed 's/^/evidence: carried proxy log: /' || true
code=$(api POST "/api/v1/runs/${CARRIED_RUN_ID}/kill")
[[ "${code}" == "200" || "${code}" == "202" ]] || fail "POST /runs/${CARRIED_RUN_ID}/kill answered ${code}: $(cat "${WORK}/resp.json")"
carried_state=""
for _ in $(seq 1 60); do
  code=$(api GET "/api/v1/runs/${CARRIED_RUN_ID}")
  [[ "${code}" == "200" ]] && carried_state="$(jq -r '.state' "${WORK}/resp.json")"
  [[ "${carried_state}" == "KILLED" && -z "$(pod_uid "${AGENT_POD}")" && -z "$(pod_uid "${PROXY_POD}")" ]] && break
  sleep 2
done
[[ "${carried_state}" == "KILLED" && -z "$(pod_uid "${AGENT_POD}")" && -z "$(pod_uid "${PROXY_POD}")" ]] \
  && pass "the operator's stop ended it cleanly: KILLED, both v${FROM_VERSION} pods gone" \
  || fail "expected the stop to leave ${CARRIED_RUN_ID} KILLED with no pods (state=${carried_state:-<unreadable>} agent=$(pod_uid "${AGENT_POD}") proxy=$(pod_uid "${PROXY_POD}"))"

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

# ── rollback: recorded as evidence, not asserted on ────────────────────────
# See "THE ROLLBACK ASSERTION" in this file's header: no published release
# (v0.7.11 or v0.7.12) carries PR #834's downgrade guard, so this binary
# cannot be expected to refuse, and this walk did not execute the rollback
# to observe what it actually does instead (no kind cluster was available to
# this pass). This step is INFORMATIONAL — it records the outcome and saves
# logs as evidence, and does not call pass/fail on either Ready or not-Ready,
# since neither outcome is provable from reading the code alone and claiming
# one would be exactly the kind of unmeasured assertion the lane rules forbid.
step "helm rollback to v${FROM_VERSION} — outcome recorded as evidence, not asserted (see this file's header)"
helm rollback "${RELEASE}" 1 --namespace "${NS}" --wait --timeout 120s >"${WORK}/rollback.log" 2>&1
rollback_helm_rc=$?
rollback_ready="false"
for _ in $(seq 1 30); do
  kubectl -n "${NS}" rollout status "deployment/${RELEASE}" --timeout=5s >/dev/null 2>&1 && { rollback_ready="true"; break; }
  sleep 2
done
rb_pod="$(kubectl -n "${NS}" get pods -l app.kubernetes.io/name=wardyn -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
[[ -n "${rb_pod}" ]] && kubectl -n "${NS}" logs "${rb_pod}" --tail=-1 >"${WORK}/rollback-pod.log" 2>&1
echo "rollback: helm exit=${rollback_helm_rc}, rollout Ready=${rollback_ready} (evidence: ${WORK}/rollback.log, ${WORK}/rollback-pod.log — UNVERIFIED without a released downgrade guard to check against)"

if [[ "${FAILED}" -ne 0 ]]; then
  echo "kind-upgrade-walk: FAILED" >&2
  exit 1
fi
echo "kind-upgrade-walk: PASS"
