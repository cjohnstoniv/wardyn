#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# The live AWS SSO walk on KUBERNETES, with two real principals and no AWS
# tenant. The question it answers is the one nothing else could: does "a MEMBER
# signs in to AWS through Wardyn, and THEIR OWN Bedrock run gets THEIR OWN role
# credentials" actually hold on a real multi-user install?
#
# It runs against the cluster `make kind-quickstart` + `make kind-sso` leave
# behind (deploy/kind/quickstart.sh owns the cluster lifecycle; this script
# NEVER creates or deletes one — the box holds other clusters). Naming and shape
# follow scripts/run-e2e-ssh-k8s.sh and scripts/run-e2e-byoi.sh: a self-skipping
# manual proof, not a CI job. It is deliberately NOT scripts/run-e2e-live.sh,
# which is the Docker host-mode Go harness hard-wired to `demo-admin-token` and
# knows nothing of kind, OIDC or Playwright.
#
# ── THE FOUR PRECONDITIONS, AND WHY THE FOURTH FAILS FIRST ──────────────────
#
#  1. a kind cluster with the SSO overlay (Dex + the fake AWS endpoints) — the
#     two principals and the fake both come from `make kind-sso`;
#  2. the daemon pointed at the fake: WARDYN_AWS_SSO_ENDPOINT_OVERRIDE (gated by
#     WARDYN_ALLOW_TEST_ENDPOINTS) moves the login sandbox env, the ssoInject
#     sandbox env, the SSO egress entries and the CreateToken URL together;
#  3. a Bedrock lane to spend the minted credential on: WARDYN_BEDROCK_BASE_URL
#     points the data plane at the same fake's bedrock-runtime stub, and
#     WARDYN_BEDROCK_MODEL is an ARN naming the PINNED account, so the
#     account-pin check has both halves;
#  4. site-config `internal_hosts` seeded with the fake's SERVICE host AND the
#     Service CIDR, BEFORE the first sign-in.
#
# The fourth is the one that fails first if forgotten, and it fails as a deny
# inside a sandbox that looks like the fake being down. The sandbox's egress
# goes through the proxy sidecar, whose vetHostLift
# (internal/egress/proxy/policy.go) DENIES any host resolving to a private
# address unless an operator-declared `internal_hosts` rule lifts it
# (internal/egress/proxy/proxy.go) — and liftInternalHost REFUSES to lift the
# proxy's own interface subnets or the control plane's addresses. A pod IP is on
# the pod CIDR, which IS the proxy's own subnet, so a pod IP can never be
# lifted no matter what is declared. A ClusterIP is on the SERVICE CIDR, which
# is a different range and can be. That is why the fake is addressed by its
# Service name throughout and why this script reads the Service CIDR off the
# apiserver rather than guessing it.
#
# The walk itself is a Playwright project (ui/e2e/live/sso-member.spec.ts),
# driven through scripts/run-ui-e2e.sh in its LIVE mode — same runner, same
# per-spec reporting and the same zero-executed check, pointed at this cluster
# instead of the hermetic backend it otherwise boots.
#
# GUARD: self-skips unless WARDYN_TEST_K8S=1, the same knob every other
# cluster-dependent lane uses.
set -uo pipefail

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  echo "kind-sso-walk: set WARDYN_TEST_K8S=1 to run the cluster-dependent AWS SSO walk (skipping)."
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

# One daemon everywhere (see deploy/kind/sso/overlay.sh): the cluster's node
# container and every image this walk touches live on the daemon this picker
# chooses, and run-ui-e2e.sh picks the same one for its own children.
. "${ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

die() { echo "ERROR: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

for bin in kubectl helm curl jq; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

# Must agree with deploy/kind/quickstart.sh and deploy/kind/sso/overlay.sh.
CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
RELEASE="wardyn"
HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
BASE_URL="http://localhost:${HTTP_PORT}"

# The fake, addressed as the sandbox will address it (see the fourth
# precondition above). ONE Service backs sso-oidc, the sso portal and the
# bedrock-runtime stub — their paths never collide (test/awsssofake's package
# doc), so one host and one internal_hosts entry cover all three.
FAKE_SVC="wardyn-awsssofake"
FAKE_HOST="${FAKE_SVC}.${NAMESPACE}.svc.cluster.local"
FAKE_PORT=8090
FAKE_URL="http://${FAKE_HOST}:${FAKE_PORT}"

# The PINNED pair. It is index 1 of the fake's entitlement fixture
# (deploy/kind/sso/awsssofake.yaml) on purpose: index 0 is always the wrong
# answer, which is the whole reason the account pin exists.
PIN_ACCOUNT="222222222222"
PIN_ROLE="WardynDev"
SSO_REGION="us-east-1"
# A full inference-profile ARN naming the PINNED account, so the model-account
# check (internal/api/awssso_pin.go) has both halves to compare.
BEDROCK_MODEL="arn:aws:bedrock:${SSO_REGION}:${PIN_ACCOUNT}:inference-profile/us.anthropic.claude-sonnet-4-5-20250929-v1:0"
SSO_START_URL="https://wardyn-dev.awsapps.com/start"

EVIDENCE_DIR="${WARDYN_KIND_SSO_EVIDENCE:-${ROOT}/local/v074/evidence/kind-sso}"
mkdir -p "${EVIDENCE_DIR}"

# ── 1. the cluster and the overlay are up ───────────────────────────────────
step "checking the cluster and the SSO overlay"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get deployment "${RELEASE}" >/dev/null 2>&1 \
  || die "no wardyn release on ${CONTEXT} — run: WARDYN_QUICKSTART_HTTP_PORT=${HTTP_PORT} WARDYN_QUICKSTART_SSH_PORT=2322 make kind-quickstart"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get svc "${FAKE_SVC}" >/dev/null 2>&1 \
  || die "the fake AWS endpoints are not installed — run: make kind-sso"
health="$(curl -s "${BASE_URL}/healthz" || true)"
[[ "${health}" == *'"runner":"k8s"'* ]] \
  || die "${BASE_URL}/healthz did not answer with runner=k8s (another daemon on that port?). Body: ${health}"

# ── 2. the Service CIDR (never a pod IP) ────────────────────────────────────
# Read off the apiserver's own flag rather than hardcoding kind's 10.96.0.0/16:
# a cluster created with another range would otherwise get an internal_hosts
# rule scoped to a range its Services are not in, and the lift would silently
# not apply — the same failure as forgetting the entry, with a config that
# looks correct.
step "reading the Service CIDR off the apiserver"
SERVICE_CIDR="$(kubectl --context "${CONTEXT}" -n kube-system get pod \
  -l component=kube-apiserver -o jsonpath='{.items[0].spec.containers[0].command}' 2>/dev/null \
  | tr ',' '\n' | sed -n 's/.*--service-cluster-ip-range=\([^"]*\).*/\1/p' | head -1)"
if [[ -z "${SERVICE_CIDR}" ]]; then
  die "could not read --service-cluster-ip-range from the apiserver. Set WARDYN_KIND_SSO_SERVICE_CIDR explicitly — do NOT substitute a pod IP or the pod CIDR: liftInternalHost refuses the proxy's own subnet, so the lift would never apply."
fi
SERVICE_CIDR="${WARDYN_KIND_SSO_SERVICE_CIDR:-${SERVICE_CIDR}}"
FAKE_CLUSTER_IP="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get svc "${FAKE_SVC}" -o jsonpath='{.spec.clusterIP}')"
echo "service CIDR: ${SERVICE_CIDR}   ${FAKE_SVC} ClusterIP: ${FAKE_CLUSTER_IP}"

# ── 3. point the daemon at the fake ─────────────────────────────────────────
# One helm upgrade sets all three of preconditions 2 and 3, plus an admin token.
# The token is the WALK'S CONTROL CHANNEL, nothing more: site-config is an
# operator-only write and the only other operator on this cluster is a browser
# session. It does not weaken what the walk proves — both principals still sign
# in through Dex, and every role assertion below is made against those sessions.
ADMIN_TOKEN="${WARDYN_KIND_SSO_ADMIN_TOKEN:-walk-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')}"
step "pointing wardynd at the fake AWS endpoints (helm upgrade --reuse-values)"
helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn \
  -n "${NAMESPACE}" --reuse-values \
  --set "auth.adminToken.value=${ADMIN_TOKEN}" \
  --set "env.WARDYN_ALLOW_TEST_ENDPOINTS=true" \
  --set "env.WARDYN_AWS_SSO_ENDPOINT_OVERRIDE=${FAKE_URL}" \
  --set "env.WARDYN_BEDROCK_REGION=${SSO_REGION}" \
  --set "env.WARDYN_BEDROCK_AWS_SSO_REGION=${SSO_REGION}" \
  --set "env.WARDYN_BEDROCK_BASE_URL=${FAKE_URL}" \
  --set "env.WARDYN_BEDROCK_MODEL=${BEDROCK_MODEL}" \
  >"${EVIDENCE_DIR}/helm-upgrade.log" 2>&1 \
  || { tail -30 "${EVIDENCE_DIR}/helm-upgrade.log" >&2; die "helm upgrade failed (see ${EVIDENCE_DIR}/helm-upgrade.log)"; }
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=300s \
  || die "wardynd did not become ready after the upgrade — check the boot log for the endpoint-override refusal (WARDYN_ALLOW_TEST_ENDPOINTS)"

# The boot WARN is itself an assertion: if the hatch had been refused or ignored,
# this line would be absent and every sandbox would dial the real AWS.
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs "deployment/${RELEASE}" --tail=500 \
  | grep -q "TEST HATCH ACTIVE" \
  || die "wardynd's boot log carries no 'TEST HATCH ACTIVE' warning — the endpoint override did not take effect"

# ── 4. THE FOURTH PRECONDITION ──────────────────────────────────────────────
# Seeded BEFORE the first sign-in, because the login sandbox is the first thing
# that dials the fake. GET-then-merge rather than a bare PUT: site-config is
# replace-semantics, and clobbering the rest of it would change what the walk is
# measuring.
step "seeding site-config internal_hosts (${FAKE_HOST} scoped to ${SERVICE_CIDR})"
current="$(curl -s -H "Authorization: Bearer ${ADMIN_TOKEN}" "${BASE_URL}/api/v1/site-config")" \
  || die "GET /site-config failed"
echo "${current}" | jq . >/dev/null 2>&1 || die "GET /site-config did not return JSON: ${current}"
merged="$(echo "${current}" | jq --arg h "${FAKE_HOST}" --arg c "${SERVICE_CIDR}" '
  .internal_hosts = ((.internal_hosts // []) | map(select(.host_suffix != $h)) + [{host_suffix:$h, cidrs:[$c]}])')"
code="$(curl -s -o "${EVIDENCE_DIR}/site-config-put.json" -w '%{http_code}' \
  -X PUT -H "Authorization: Bearer ${ADMIN_TOKEN}" -H 'Content-Type: application/json' \
  -d "${merged}" "${BASE_URL}/api/v1/site-config")"
[[ "${code}" == "200" ]] || { cat "${EVIDENCE_DIR}/site-config-put.json" >&2; die "PUT /site-config answered ${code}"; }

# ── 5. the walk ─────────────────────────────────────────────────────────────
# run-ui-e2e.sh in LIVE mode: it skips the hermetic backend entirely and points
# the `live` Playwright project at this cluster. One spec file, as always.
step "running the walk (ui/e2e/live/sso-member.spec.ts)"
export WARDYN_E2E_LIVE_BASE_URL="${BASE_URL}"
export WARDYN_TEST_K8S=1
export WARDYN_LIVE_ADMIN_TOKEN="${ADMIN_TOKEN}"
export WARDYN_LIVE_FAKE_URL="${FAKE_URL}"
export WARDYN_LIVE_PIN_ACCOUNT="${PIN_ACCOUNT}"
export WARDYN_LIVE_PIN_ROLE="${PIN_ROLE}"
export WARDYN_LIVE_SSO_START_URL="${SSO_START_URL}"
export WARDYN_LIVE_SSO_REGION="${SSO_REGION}"
./scripts/run-ui-e2e.sh sso-member 2>&1 | tee "${EVIDENCE_DIR}/walk.log"
walk_rc="${PIPESTATUS[0]}"

# /_seen is read from INSIDE the cluster (the fake has no host-side route by
# design), and it is the one observation that is not Wardyn asserting about
# itself: it is what the AWS SDK actually asked the portal to mint.
step "reading the fake's /_seen"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" exec "deployment/${RELEASE}" -- \
  wget -qO- "${FAKE_URL}/_seen" 2>/dev/null \
  | tee "${EVIDENCE_DIR}/seen.json" || echo '(could not read /_seen from the control-plane pod)'
echo

if [[ "${walk_rc}" -ne 0 ]]; then
  echo "kind-sso-walk: FAILED (see ${EVIDENCE_DIR}/walk.log)" >&2
  exit 1
fi
echo "kind-sso-walk: PASS — evidence in ${EVIDENCE_DIR}"
