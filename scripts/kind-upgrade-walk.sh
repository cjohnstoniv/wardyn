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
#   admin-namespace secret, the retired managed-harness credential seeded
#   where v0.7.11 lets it be (#677), and one interactive run that genuinely
#   reaches RUNNING -> upgrade to tip (Recreate, the chart's own default
#   strategy) -> assert:
#     - the boot-time secret conversion: the boot log's converted count, one
#       secret.read row (purpose boot, outcome success) for the seeded secret
#       and no failed secret.read row anywhere (the conversion decrypts every
#       row it converts and aborts boot on one that will not);
#     - the retired credential is gone from every namespace and one
#       model_credential.retire row names it (#677);
#     - the carried run keeps the documented pre-0.7.12 outcome (see "THE
#       CARRIED RUN" below), with the refusal proven rather than tailed: the
#       console answers /api/v1/internal/decisions 404, the carried proxy
#       denies a probe and its decision never reaches the audit trail, and
#       the operator's kill leaves a run.kill row;
#     - a fresh run reaches RUNNING with both pods Running over the tip image,
#       and /healthz.proxy_hop_tls=true;
#   -> helm upgrade --set secretFiles.enabled=true (#720): boots, carries zero
#   secretKeyRef entries, the pre-upgrade secret is still there and no read
#   failed;
#   -> helm rollback to v0.7.11, whose rollout must NOT become Ready (see "THE
#   ROLLBACK ASSERTION" below) and whose Deployment must really run the
#   v0.7.11 image;
#   -> then an age-key FILE holding a DIFFERENT key must refuse boot and
#   leave the store untouched, never come up empty. The rollback's logs and
#   the resolved chart and image digests are copied into this log (the
#   nightly uploads only this log).
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
#     - an in-flight model-provider run failing closed at its next MODEL call
#       after the retirement (MP-4b, shipped in 0.8.2): the carried run makes
#       no model call. What is asserted is the retirement's own effect on
#       stored state, the boot sweep (#677), not the run-time refusal.
#     - a person's namespace holding the credential through the public API:
#       v0.7.11 refuses a generic PUT of the name and a cross-owner write, and
#       a person's own capture needs a login sandbox and a signed-in person
#       this walk does not have. The person's row is therefore a copy of the
#       operator's sealed row made in Postgres, and the walk says so when it
#       cannot make it.
#
# THE ROLLBACK ASSERTION. The tip boot converts every legacy secret row to
# envelope v1 and logs that "an older wardynd can no longer read them". The
# v${FROM_VERSION} binary has no downgrade guard of any kind (db.Migrate's
# "cannot run under" refusal, #675 / PR #834, merged 2026-09-25, after both
# v0.7.11 and v0.7.12), so it does not refuse by name: it boots against the
# tip schema and meets rows it cannot open. Its boot keys (signing, session)
# live in that same table, and a boot-key read that fails decrypting fails
# closed (loadOrCreateSecret), so its rollout must NOT become Ready. This
# step hard-asserts exactly that, and copies the helm output and the pod log
# into this log so the cause is on record. It asserts no refusal TEXT, because
# no published binary can produce one; once a release ships #834's guard,
# FROM_VERSION should move to it and this step should assert that text.
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
# The digests this walk actually resolved, in the log the nightly uploads: a
# moved tag shows up as a different digest between two nights' runs.
mkdir -p "${WORK}/chart-pull"
chart_pull="$(helm pull "${CHART_REF}" --version "${FROM_VERSION}" --destination "${WORK}/chart-pull" 2>&1)" \
  || { printf '%s\n' "${chart_pull}" >&2; die "helm pull of the published v${FROM_VERSION} chart failed"; }
echo "evidence: chart ${CHART_REF}:${FROM_VERSION} $(grep -i '^Digest:' <<<"${chart_pull}" | head -1 || true)"
echo "evidence: image ghcr.io/cjohnstoniv/wardynd:${FROM_VERSION} $(docker image inspect "ghcr.io/cjohnstoniv/wardynd:${FROM_VERSION}" --format '{{join .RepoDigests ", "}}')"
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

# GET /api/v1/audit with a query string; the JSON array lands in resp.json.
audit() { api GET "/api/v1/audit?$1"; }
# One psql statement or script (stdin) against the walk's own Postgres.
psql_q() { kubectl -n "${NS}" exec -i deployment/postgres -- psql -U wardyn -d wardyn -v ON_ERROR_STOP=1 -qAt "$@"; }
secret_rows() { psql_q -c "SELECT count(*) FROM secrets WHERE name = '$1'"; }

# ── fill via the real API (the reduced set — see this file's header) ──────
SECRET_NAME="kind-upgrade-walk-secret"
step "writing one secret through the v${FROM_VERSION} API (lands as a legacy enc_version=0 row — envelope v1 does not exist yet)"
code=$(api PUT "/api/v1/secrets/${SECRET_NAME}" '{"value":"kind-upgrade-walk-value"}')
[[ "${code}" == "200" || "${code}" == "204" ]] || { cat "${WORK}/resp.json" >&2; die "PUT /secrets/${SECRET_NAME} on v${FROM_VERSION} answered ${code}"; }
pass "secret written under v${FROM_VERSION}"

# #677: the retired managed-harness credential, in the operator's namespace
# (the paste door is the only way v0.7.11 stores that name) and in one
# person's. v0.7.11 refuses the name on the generic secrets API and refuses a
# cross-owner write, so the person's row is the operator's sealed row copied
# under another owner in Postgres: a v0 row is age-sealed with nothing bound to
# its owner, so the copy opens like the original.
RETIRED_NAME="wardyn-harness-anthropic-oauth"
step "seeding the retired managed-harness credential ${RETIRED_NAME} (#677)"
code=$(api PUT /api/v1/setup/harness-credential/anthropic '{"token":"sk-ant-oat01-kind-upgrade-walk-retired-credential"}')
PASTE_CODE="${code}"
if [[ "${code}" == "200" ]]; then
  if psql_q -c "CREATE TEMP TABLE walk_copy AS SELECT * FROM secrets WHERE owned_by = '' AND name = '${RETIRED_NAME}';
    UPDATE walk_copy SET owned_by = 'kind-upgrade-walk-person';
    INSERT INTO secrets SELECT * FROM walk_copy;" >/dev/null; then
    pass "seeded ${RETIRED_NAME} in the operator namespace and in one person's"
  else
    echo "evidence: could not copy ${RETIRED_NAME} into a person's namespace; the operator's row is the only one seeded"
  fi
else
  echo "evidence: v${FROM_VERSION} cannot seed ${RETIRED_NAME} (PUT answered ${code}); asserting absence after the upgrade only"
fi
RETIRED_SEEDED="$(secret_rows "${RETIRED_NAME}")" || die "could not count ${RETIRED_NAME} rows in the walk's Postgres"
echo "evidence: ${RETIRED_SEEDED} row(s) named ${RETIRED_NAME} before the upgrade"
# A paste that answered 200 stored a row; none there means the seed is broken,
# and carrying on would assert only absence of something never present.
[[ "${PASTE_CODE}" != "200" || "${RETIRED_SEEDED}" -ge 1 ]] \
  || fail "the paste of ${RETIRED_NAME} answered 200 but no row with that name exists"

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

# Every tip upgrade below shares one argument set; a later --set wins, so a
# caller's extra flags override the defaults here.
tip_helm_upgrade() {
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
    "$@" || return 1
  kubectl -n "${NS}" patch service "${RELEASE}" -p \
    "{\"spec\":{\"ports\":[{\"port\":${HTTP_PORT},\"nodePort\":${NODE_HTTP_PORT}}]}}"
}

step "helm upgrade --install to tip (the chart's own Recreate strategy tears the old pod down first)"
tip_helm_upgrade || die "helm upgrade to tip failed"
kubectl -n "${NS}" rollout status "deployment/${RELEASE}" --timeout=180s \
  || { kubectl -n "${NS}" logs "deployment/${RELEASE}" --tail=150 --all-containers >&2 || true; die "the tip upgrade never became ready"; }
wait_healthy "${BASE}" 60 2 || die "${BASE}/healthz did not answer after the tip upgrade"
pass "upgraded to tip and healthy"

step "asserting the boot-time secret conversion (v0->envelope v1) and that it decrypted"
tip_pod="$(kubectl -n "${NS}" get pods -l app.kubernetes.io/name=wardyn -o jsonpath='{.items[0].metadata.name}')"
logs="$(kubectl -n "${NS}" logs "${tip_pod}" --tail=-1 2>/dev/null)"
# The line carries its count as an attribute (secrets=N in text, "secrets":N in
# JSON); the conversion opens every row it converts and aborts boot on one that
# will not decrypt, so a boot that logged a count decrypted that many rows.
converted_n="$(grep 'converted stored secrets to envelope v1' <<<"${logs}" | grep -oE 'secrets[=":]+ *[0-9]+' | tail -1 | grep -oE '[0-9]+$')"
[[ "${converted_n:-0}" -ge 1 ]] \
  && pass "boot logged the v0->envelope-v1 conversion of ${converted_n} secret(s)" \
  || fail "expected the tip pod's boot log to name the envelope v1 conversion with a count of at least 1; did not find it"
code=$(audit "action=secret.read&limit=500")
[[ "${code}" == "200" ]] || { cat "${WORK}/resp.json" >&2; die "GET /audit?action=secret.read under tip answered ${code}"; }
jq -e --arg n "${SECRET_NAME}" '[.[] | select(.target == $n and .outcome == "success" and .data.purpose == "boot")] | length == 1' "${WORK}/resp.json" >/dev/null \
  && pass "the audit trail holds one secret.read row (purpose boot, outcome success) for ${SECRET_NAME}" \
  || fail "expected exactly one successful boot secret.read row for ${SECRET_NAME}: $(jq -c '[.[] | {target, outcome, purpose: .data.purpose}]' "${WORK}/resp.json")"
jq -e '[.[] | select(.outcome != "success")] | length == 0' "${WORK}/resp.json" >/dev/null \
  && pass "no secret.read row failed: every converted secret decrypted" \
  || fail "a secret.read row failed during the conversion: $(jq -c '[.[] | select(.outcome != "success") | {target, outcome}]' "${WORK}/resp.json")"

step "confirming the secret written under v${FROM_VERSION} is still listed under tip"
code=$(api GET "/api/v1/secrets")
[[ "${code}" == "200" ]] || { cat "${WORK}/resp.json" >&2; die "GET /secrets under tip answered ${code}"; }
# GET /secrets answers {"names": [...], "mine": [...]} (handleListSecrets): the
# admin's own namespace is `mine`, a list of bare names.
jq -e --arg n "${SECRET_NAME}" 'any(.mine[]; . == $n)' "${WORK}/resp.json" >/dev/null \
  && pass "secret ${SECRET_NAME} still present after the conversion" \
  || fail "expected ${SECRET_NAME} to still be listed after the upgrade"

step "the retired managed-harness credential is gone from every namespace (#677)"
retired_after="$(secret_rows "${RETIRED_NAME}")"
[[ "${retired_after}" == "0" ]] \
  && pass "no row named ${RETIRED_NAME} is left in any namespace (${RETIRED_SEEDED} before)" \
  || fail "expected the boot sweep to delete ${RETIRED_NAME} everywhere; ${retired_after} row(s) remain"
code=$(audit "action=model_credential.retire&limit=500")
if [[ "${RETIRED_SEEDED}" -ge 1 ]]; then
  [[ "${code}" == "200" ]] && jq -e --arg n "${RETIRED_NAME}" --argjson c "${RETIRED_SEEDED}" \
    '[.[] | select(.outcome == "success" and .data.count == $c and ((.data.names // []) | index($n)))] | length == 1' "${WORK}/resp.json" >/dev/null \
    && pass "one model_credential.retire row names ${RETIRED_NAME} and counts ${RETIRED_SEEDED} row(s)" \
    || fail "expected one model_credential.retire row naming ${RETIRED_NAME} with count ${RETIRED_SEEDED}; answered ${code}: $(cat "${WORK}/resp.json")"
else
  echo "evidence: nothing was seeded under v${FROM_VERSION}, so no model_credential.retire row is expected; absence asserted above"
fi

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

step "the carried run fails closed: its proxy cannot reach the console's internal routes (#1263)"
# The console listener answers InternalPathPrefix 404 once the hop is TLS
# (internal/api/base_path.go), which is where a v${FROM_VERSION} proxy dials.
code=$(curl -sS --max-time "${CURL_MAX_TIME}" -o /dev/null -w '%{http_code}' -X POST "${BASE}/api/v1/internal/decisions" \
  -H "Content-Type: application/json" -d '{}')
[[ "${code}" == "404" ]] \
  && pass "the console answers /api/v1/internal/decisions 404" \
  || fail "expected the console's /api/v1/internal/decisions to answer 404; answered ${code}"
# A v${FROM_VERSION} proxy logs nothing when a decision post is refused (it counts the drop and
# retries silently), so the refusal is proven from both ends: the proxy denies a
# probe and mirrors the decision on its own stdout, and that decision never
# reaches the audit trail.
probe_host="walk-denied.kind-upgrade.invalid"
probe_out="$(kubectl -n wardyn-runs exec "${AGENT_POD}" -- curl -sS -m 10 -o /dev/null -w '%{http_code}' "https://${probe_host}/" 2>&1)"
# curl's stderr ("CONNECT tunnel failed, response 403") lands before the -w code, so
# the refusal is either a 000 code anywhere in the output or the proxy's 403 on CONNECT.
[[ "${probe_out}" == *000* || "${probe_out}" == *"CONNECT tunnel failed, response 403"* ]] \
  && pass "the carried proxy refused the probe to ${probe_host}: no response came back through it" \
  || fail "expected the probe through the carried proxy to be refused; got: ${probe_out}"
probe_logged=""
for _ in $(seq 1 10); do
  proxy_log="$(kubectl -n wardyn-runs logs "${PROXY_POD}" --all-containers 2>&1)"
  grep -q "${probe_host}" <<<"${proxy_log}" && { probe_logged=1; break; }
  sleep 2
done
[[ -n "${probe_logged}" ]] \
  && pass "the carried proxy's own log records its decision on ${probe_host}" \
  || fail "expected the carried proxy's log to record its decision on ${probe_host}"
sleep 10
code=$(audit "run_id=${CARRIED_RUN_ID}&limit=500")
[[ "${code}" == "200" ]] && jq -e --arg h "${probe_host}" '[.[] | select(tostring | contains($h))] | length == 0' "${WORK}/resp.json" >/dev/null \
  && pass "no audit row for run ${CARRIED_RUN_ID} names ${probe_host}: the proxy's decision never landed" \
  || fail "expected the refused decision to be absent from the audit trail; answered ${code}: $(jq -c --arg h "${probe_host}" '[.[] | select(tostring | contains($h)) | {action, outcome}]' "${WORK}/resp.json" 2>/dev/null)"

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
code=$(audit "run_id=${CARRIED_RUN_ID}&action=run.kill")
[[ "${code}" == "200" ]] && jq -e '[.[] | select(.outcome == "success")] | length >= 1' "${WORK}/resp.json" >/dev/null \
  && pass "the stop is audited: a run.kill row for ${CARRIED_RUN_ID}" \
  || fail "expected a successful run.kill row for ${CARRIED_RUN_ID}; answered ${code}: $(cat "${WORK}/resp.json")"

step "confirming /healthz.proxy_hop_tls=true and a FRESH run is really running on tip"
h="$(curl -sf --max-time "${CURL_MAX_TIME}" "${BASE}/healthz")"
[[ "$(jq -r '.proxy_hop_tls // false' <<<"${h}")" == "true" ]] \
  && pass "proxy_hop_tls=true after the upgrade" \
  || fail "expected /healthz.proxy_hop_tls=true after the upgrade to tip"
code=$(api POST /api/v1/runs '{"agent":"claude-code","repo":"local:kind-upgrade-fresh","interactive":true,
  "inline_policy":{"allowed_domains":[],"first_use_approval":"always_deny","min_confinement_class":"CC1","auto_stop_after_sec":-1}}')
if [[ "${code}" == "200" || "${code}" == "201" ]]; then
  fresh_rid="$(jq -r '.id' "${WORK}/resp.json")"
  fresh_state=""
  for _ in $(seq 1 120); do
    code=$(api GET "/api/v1/runs/${fresh_rid}")
    [[ "${code}" == "200" ]] && fresh_state="$(jq -r '.state' "${WORK}/resp.json")"
    case "${fresh_state}" in RUNNING|COMPLETED|FAILED|KILLED|STOPPED|ARCHIVED) break ;; esac
    sleep 2
  done
  fresh_agent="$(pod_uid "wardyn-agent-${fresh_rid}")"
  fresh_proxy="$(pod_uid "wardyn-proxy-${fresh_rid}")"
  [[ "${fresh_state}" == "RUNNING" && "${fresh_agent}" == */Running && "${fresh_proxy}" == */Running ]] \
    && pass "a fresh run over the TLS hop reaches RUNNING on tip with both pods Running" \
    || fail "expected a fresh run RUNNING with both pods Running on tip (state=${fresh_state:-<unreadable>} agent=${fresh_agent:-absent} proxy=${fresh_proxy:-absent})"
  curl -sS --max-time "${CURL_MAX_TIME}" -X POST "${BASE}/api/v1/runs/${fresh_rid}/kill" -H "Authorization: Bearer ${ADMIN_TOKEN}" >/dev/null 2>&1 || true
else
  fail "expected a fresh run to succeed on tip; POST /runs answered ${code}: $(cat "${WORK}/resp.json")"
fi

# ── #720: the same age key as files ────────────────────────────────────────
# Every local row's kek_id is "local[/purpose]:<key fingerprint>"; the
# fingerprint is the age key's. Taken before and after, one identical value
# shows the rows are still under the key that sealed them.
kek_fingerprints() { psql_q -c "SELECT string_agg(DISTINCT split_part(kek_id, ':', 2), ',') FROM secrets WHERE kek_id LIKE 'local%'"; }
KEK_FP_BEFORE="$(kek_fingerprints)" || die "could not read the secrets' key fingerprints"
step "helm upgrade --set secretFiles.enabled=true: the same age key, delivered as files (#720)"
tip_helm_upgrade --set secretFiles.enabled=true || die "helm upgrade with secretFiles.enabled=true failed"
kubectl -n "${NS}" rollout status "deployment/${RELEASE}" --timeout=180s \
  || { kubectl -n "${NS}" logs "deployment/${RELEASE}" --tail=150 --all-containers >&2 || true; die "the secretFiles upgrade never became ready"; }
wait_healthy "${BASE}" 60 2 || die "${BASE}/healthz did not answer after the secretFiles upgrade"
dep_json="$(kubectl -n "${NS}" get "deployment/${RELEASE}" -o json)"
n_ref="$(grep -c secretKeyRef <<<"${dep_json}")"
[[ "${n_ref}" == "0" ]] \
  && pass "the Deployment carries 0 secretKeyRef entries under secretFiles.enabled" \
  || fail "expected 0 secretKeyRef entries under secretFiles.enabled; found ${n_ref}"
jq -e '[.spec.template.spec.containers[0].env[] | select(.name == "WARDYN_AGE_KEY_FILE")] | length == 1' <<<"${dep_json}" >/dev/null \
  && pass "wardynd reads its age key from WARDYN_AGE_KEY_FILE" \
  || fail "expected WARDYN_AGE_KEY_FILE on the wardynd container under secretFiles.enabled"
# Boot opened every boot key under the file's key (an unreadable one refuses
# boot, see loadOrCreateSecret), so a healthy boot proves it is the key the
# store was written under; the negative below proves a different key is refused.
KEK_FP_AFTER="$(kek_fingerprints)"
[[ -n "${KEK_FP_BEFORE}" && "${KEK_FP_BEFORE}" != *,* && "${KEK_FP_AFTER}" == "${KEK_FP_BEFORE}" ]] \
  && pass "every locally sealed secret row is under one key fingerprint, unchanged by the move to secretFiles" \
  || fail "expected one unchanged key fingerprint across the secretFiles upgrade; before=${KEK_FP_BEFORE:-<none>} after=${KEK_FP_AFTER:-<none>}"
code=$(api GET "/api/v1/secrets")
[[ "${code}" == "200" ]] && jq -e --arg n "${SECRET_NAME}" 'any(.mine[]; . == $n)' "${WORK}/resp.json" >/dev/null \
  && [[ "$(secret_rows "${SECRET_NAME}")" == "1" ]] \
  && pass "secret ${SECRET_NAME} survived the move from secretKeyRef to secretFiles" \
  || fail "expected ${SECRET_NAME} to survive the move to secretFiles"
code=$(audit "action=secret.read&outcome=failure&limit=500")
[[ "${code}" == "200" ]] && jq -e 'length == 0' "${WORK}/resp.json" >/dev/null \
  && pass "still no failed secret.read row after the secretFiles boot" \
  || fail "a secret.read row failed after the secretFiles boot: $(cat "${WORK}/resp.json")"

# ── rollback: the old binary cannot read the converted store ────────────────
# See "THE ROLLBACK ASSERTION" in this file's header. The rollout must NOT
# become Ready; the cause goes into this log, which is all the nightly uploads.
# It runs from a Ready tip (the secretFiles revision) so that "not Ready" can
# only be v${FROM_VERSION}'s own doing, and it also proves the rollback applied.
step "helm rollback to v${FROM_VERSION}: its rollout must not become Ready"
helm rollback "${RELEASE}" 1 --namespace "${NS}" --wait --timeout 120s >"${WORK}/rollback.log" 2>&1
rollback_helm_rc=$?
rb_image="ghcr.io/cjohnstoniv/wardynd:${FROM_VERSION}"
rb_pod=""
dep_image=""
for _ in $(seq 1 30); do
  dep_image="$(kubectl -n "${NS}" get "deployment/${RELEASE}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
  rb_pod="$(kubectl -n "${NS}" get pods -l app.kubernetes.io/name=wardyn -o json 2>/dev/null \
    | jq -r --arg i "${rb_image}" '[.items[] | select(.spec.containers[0].image == $i)] | sort_by(.metadata.creationTimestamp) | last | .metadata.name // empty')"
  [[ "${dep_image}" == "${rb_image}" && -n "${rb_pod}" ]] && break
  sleep 2
done
rollback_ready="false"
for _ in $(seq 1 10); do
  kubectl -n "${NS}" rollout status "deployment/${RELEASE}" --timeout=5s >/dev/null 2>&1 && { rollback_ready="true"; break; }
  sleep 2
done
if [[ -n "${rb_pod}" ]]; then
  rb_restarts="$(kubectl -n "${NS}" get pod "${rb_pod}" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)"
  {
    kubectl -n "${NS}" logs "${rb_pod}" --tail=-1 2>&1
    if [[ "${rb_restarts:-0}" -gt 0 ]]; then
      echo "--- previous container ---"
      kubectl -n "${NS}" logs "${rb_pod}" --previous --tail=-1 2>&1
    fi
  } >"${WORK}/rollback-pod.log"
fi
echo "rollback: helm exit=${rollback_helm_rc}, deployment image=${dep_image:-<unreadable>}, pod=${rb_pod:-<none>}, rollout Ready=${rollback_ready}"
for f in rollback.log rollback-pod.log; do
  printf -- '--- %s ---\n' "${f}"
  cat "${WORK}/${f}" 2>/dev/null || echo "(not written)"
  printf -- '--- end %s ---\n' "${f}"
done
[[ "${dep_image}" == "${rb_image}" && -n "${rb_pod}" ]] \
  && pass "the rollback applied: the Deployment runs ${rb_image} in pod ${rb_pod}" \
  || fail "expected the rollback to leave the Deployment on ${rb_image} with a pod running it (image=${dep_image:-<unreadable>} pod=${rb_pod:-<none>})"
[[ "${rollback_ready}" == "false" && -n "${rb_pod}" ]] \
  && pass "v${FROM_VERSION}'s rollout did not become Ready against the converted store" \
  || fail "expected v${FROM_VERSION}'s rollout to stay not-Ready against the converted store (Ready=${rollback_ready}, pod=${rb_pod:-<none>})"

# After the rollback, so that the crash-looping wrong-key tip pod cannot stand
# in for the v${FROM_VERSION} pod above. This upgrades the rolled-back release
# to tip again, with a Secret that holds some other age key.
step "negative: an age-key file holding a DIFFERENT key must refuse boot and leave the store alone (#720)"
WRONG_KEY="$(docker run --rm "${TIP_IMAGE}" -gen-age-key)"
[[ "${WRONG_KEY}" == AGE-SECRET-KEY-* && "${WRONG_KEY}" != "${AGE_KEY}" ]] || die "could not mint a second, different age key"
kubectl -n "${NS}" create secret generic wardyn-wrong-age-key --from-literal=age-key="${WRONG_KEY}" >/dev/null
rows_before="$(psql_q -c "SELECT count(*) FROM secrets")"
tip_helm_upgrade --set secretFiles.enabled=true --set secrets.ageKeyFromSecret=false --set secrets.ageKeySecretRef.name=wardyn-wrong-age-key \
  || die "helm upgrade naming the wrong age key failed to render"
# The pod that mounts the wrong key, whatever the old pod is doing meanwhile.
wrong_pod_json='[.items[] | select(any(.spec.volumes[]?; any(.projected.sources[]?; .secret.name == "wardyn-wrong-age-key")))]'
refused_pod=""
for _ in $(seq 1 90); do
  pods_json="$(kubectl -n "${NS}" get pods -l app.kubernetes.io/name=wardyn -o json)"
  refused_pod="$(jq -r "${wrong_pod_json} | map(select(any(.status.containerStatuses[]?; ((.lastState.terminated.exitCode // .state.terminated.exitCode // 0) != 0)))) | .[0].metadata.name // empty" <<<"${pods_json}")"
  [[ -n "${refused_pod}" ]] && break
  sleep 2
done
if [[ -n "${refused_pod}" ]]; then
  refusal_log="$(kubectl -n "${NS}" logs "${refused_pod}" --all-containers --tail=-1 2>&1; kubectl -n "${NS}" logs "${refused_pod}" --all-containers --previous --tail=-1 2>&1)"
  grep -qE 'load secret \\?"|not configured to reach' <<<"${refusal_log}" \
    && pass "the pod holding a different age key exited non-zero and said why" \
    || fail "the pod holding a different age key exited non-zero but its log carries no key refusal: $(tail -5 <<<"${refusal_log}")"
else
  fail "expected the pod holding a different age key to exit non-zero; it never did"
fi
ready_wrong="$(jq -r "${wrong_pod_json} | map(select(any(.status.containerStatuses[]?; .ready == true))) | length" <<<"$(kubectl -n "${NS}" get pods -l app.kubernetes.io/name=wardyn -o json)")"
[[ "${ready_wrong}" == "0" ]] \
  && pass "no pod holding a different age key is Ready" \
  || fail "a pod holding a different age key is Ready"
rows_after="$(psql_q -c "SELECT count(*) FROM secrets")"
[[ "${rows_after}" == "${rows_before}" && "$(secret_rows "${SECRET_NAME}")" == "1" ]] \
  && pass "the store is untouched (${rows_after} secret rows, ${SECRET_NAME} still there): it never came up empty" \
  || fail "the refused boot changed the store: ${rows_before} -> ${rows_after} secret rows"

if [[ "${FAILED}" -ne 0 ]]; then
  echo "kind-upgrade-walk: FAILED" >&2
  exit 1
fi
echo "kind-upgrade-walk: PASS"
