#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# The keep-and-revive walk on KUBERNETES, through the real API, as a signed-in person.
#
# Question: on the agent-sandbox backend with keep on, does an interactive run with a kept work
# folder survive the end of its lease and come back, with its file, when its owner extends and
# revives it, while a run without a kept work folder is still torn down at its end? The driver
# pins and the conformance cases call the substrate the way wardynd does. Nothing else drives the
# path the person drives: create, end, read as kept, extend, revive.
#
# WHY A BROWSER SESSION. The kept work folder (/home/agent/work) is a managed member drive, and the
# API makes one only through keep_work_folder, which refuses every caller that is not a signed-in
# person who owns the run (internal/api/run_keep_work_folder.go, keepWorkFolderGates). A bearer
# token cannot make a run that can be kept. So the walk is a Playwright spec,
# ui/e2e/walk/agent-sandbox-keep.spec.ts, that signs in through Dex as member@wardyn.local and
# calls the API from the page's own session, the way sso-concurrency.spec.ts does. The spec also
# reads the cluster with kubectl (the gate on the held pod, the missing proxy pod).
#
# THE CLUSTER. The one `make kind-quickstart` and `make kind-sso` leave behind: Dex, the two
# principals and the k8s runner. This script NEVER creates or deletes a cluster. It does change
# it: it installs the agent-sandbox controller, upgrades the wardyn release with the backend and
# keep switched on, and turns on member drives with a kept main drive. So a hand run must set
# WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the cluster is disposable (kind-survival-walk.sh's rule).
#
# GUARD: self-skips unless WARDYN_TEST_K8S=1, the same knob every other cluster-dependent lane uses.
#
# NOT RUN OR PROVEN IN THE PASS THAT WROTE IT: no cluster was available there. The nightly job
# kind-agent-sandbox-keep-walk is what proves it. It stays out of release.yml's watched= until it
# has gone green on two scheduled runs.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/lib/common.sh
. "${ROOT}/scripts/lib/common.sh"

if [[ "${WARDYN_TEST_K8S:-}" != "1" ]]; then
  skip_lane "kind-agent-sandbox-keep-walk: set WARDYN_TEST_K8S=1 to run the cluster-dependent keep walk (skipping)."
fi
if [[ "${WARDYN_ACK_SHARED_QUICKSTART:-}" != "1" ]]; then
  skip_lane "kind-agent-sandbox-keep-walk: set WARDYN_ACK_SHARED_QUICKSTART=1 to confirm the wardyn-quickstart cluster this walk finds may be changed (a controller installed, the release upgraded) — refusing to run without it."
fi

wardyn_pick_docker_host

die() { echo "::error::kind-agent-sandbox-keep-walk: $*" >&2; exit 1; }
step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

for bin in kubectl helm curl jq sha256sum; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done

# Must agree with deploy/kind/quickstart.sh and deploy/kind/sso/overlay.sh.
CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
RUNS_NAMESPACE="wardyn-runs"
RELEASE="wardyn"
HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
BASE_URL="http://localhost:${HTTP_PORT}"
DEX_PORT="${WARDYN_KIND_SSO_DEX_PORT:-5557}"
DEX_PF_PIDFILE="${TMPDIR:-/tmp}/wardyn-kind-sso-dex-${CLUSTER}.pid"

# The agent-sandbox controller. MUST MATCH ci.yml, job conformance-k8s-agent-sandbox: the step "Fetch
# agent-sandbox v1.0.9 (pinned sha256, retried, cached)" for the url and the sha256, and that job's
# env AGENT_SANDBOX_CONTROLLER_IMAGE for the digest. Copied, not shared: a change to one is a change
# to the other, and docs/operations/kubernetes-agent-sandbox.md names the same digest.
AGENT_SANDBOX_URL="https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.9/sandbox.yaml"
AGENT_SANDBOX_SHA256="3eeb27fa821659593ba92c5454571f08c84abb07096bb9d372c1c6c750f6d7d2"
AGENT_SANDBOX_CONTROLLER_IMAGE="registry.k8s.io/agent-sandbox/agent-sandbox-controller@sha256:ef1a1a3076afc996e711c44b388fbb7ffb7cd6858953d7e685166b299d77bd38"

EVIDENCE_DIR="${WARDYN_KIND_AGENT_SANDBOX_KEEP_EVIDENCE:-${ROOT}/local/evidence/kind-agent-sandbox-keep}"
mkdir -p "${EVIDENCE_DIR}"
WORK_DIR="$(mktemp -d /tmp/wardyn-kind-agent-sandbox-keep.XXXXXX)"
dex_pf_pid=""
cleanup_walk() {
  [[ -n "${dex_pf_pid}" ]] && kill "${dex_pf_pid}" 2>/dev/null
  rm -rf "${WORK_DIR}"
  return 0
}
trap cleanup_walk EXIT

kc() { kubectl --context "${CONTEXT}" "$@"; }

# ── 1. the cluster and the SSO overlay are up ───────────────────────────────
step "checking the cluster and the SSO overlay (never created or deleted here)"
kc -n "${NAMESPACE}" get deployment "${RELEASE}" >/dev/null 2>&1 \
  || die "no wardyn release on ${CONTEXT} — run: WARDYN_QUICKSTART_HTTP_PORT=${HTTP_PORT} make kind-quickstart"
kc -n "${NAMESPACE}" get svc wardyn-dex >/dev/null 2>&1 \
  || die "the Dex overlay is not installed — run: make kind-sso"
health="$(curl -s "${BASE_URL}/healthz" || true)"
[[ "${health}" == *'"runner":"k8s"'* ]] \
  || die "${BASE_URL}/healthz did not answer with runner=k8s (another daemon on that port?). Body: ${health}"
server_version="$(kc version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion // empty')"
echo "kubernetes server: ${server_version:-<unreadable>}"
# The backend's docs claim v1.30 only. A later minor can make the start check refuse a field the API
# server now fills in, which this walk would then report as a keep failure.
[[ "${server_version}" == v1.30.* ]] \
  || echo "::warning::the cluster runs ${server_version:-an unreadable version}, not the v1.30 the backend's docs name"

# ── 2. Dex: the browser leg signs in here, so its forward must be up ────────
dex_up() { curl -sf --max-time 3 "http://localhost:${DEX_PORT}/.well-known/openid-configuration" >/dev/null 2>&1; }
if ! dex_up; then
  step "re-opening the Dex port-forward (the overlay's forward ended with its own step)"
  kc -n "${NAMESPACE}" port-forward svc/wardyn-dex "${DEX_PORT}:5556" >/dev/null 2>&1 &
  dex_pf_pid=$!
  echo "${dex_pf_pid}" > "${DEX_PF_PIDFILE}"
  for _ in $(seq 1 30); do
    dex_up && break
    sleep 1
  done
fi
dex_up || die "Dex never answered on http://localhost:${DEX_PORT} (is ${DEX_PORT} taken?)"
dex_iss="$(kc -n "${NAMESPACE}" get configmap wardyn-dex -o jsonpath='{.data.config\.yaml}' | sed -n 's/^issuer: *//p')"
[[ "${dex_iss}" == "http://localhost:${DEX_PORT}" ]] \
  || die "this cluster's Dex issuer is '${dex_iss}', not http://localhost:${DEX_PORT} — the browser would sign in elsewhere"

# ── 3. the agent-sandbox controller ─────────────────────────────────────────
# The release's chart never installs it (docs/operations/kubernetes-agent-sandbox.md). Same steps as
# ci.yml: the pinned download, the image line replaced by the digest, no image left unpinned, then
# apply and wait.
step "installing agent-sandbox v1.0.9 (pinned sha256 and controller digest)"
manifest="${WORK_DIR}/agent-sandbox.yaml"
curl -fsSL --retry 20 --retry-all-errors --retry-max-time 300 --retry-delay 10 --connect-timeout 20 \
  -o "${manifest}" "${AGENT_SANDBOX_URL}" || die "could not download ${AGENT_SANDBOX_URL}"
echo "${AGENT_SANDBOX_SHA256}  ${manifest}" | sha256sum --strict -c - \
  || die "sandbox.yaml does not match the pinned sha256 ${AGENT_SANDBOX_SHA256}"
tagged='registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.9'
[[ "$(grep -cF "image: ${tagged}" "${manifest}")" == 1 ]] \
  || die "expected exactly one '${tagged}' image line in sandbox.yaml"
sed -i "s|image: ${tagged}\$|image: ${AGENT_SANDBOX_CONTROLLER_IMAGE}|" "${manifest}"
images="$(grep -E '^[[:space:]]*(- )?image:[[:space:]]*[^[:space:]]' "${manifest}")"
echo "${images}"
if grep -v '@sha256:[0-9a-f]\{64\}$' <<<"${images}"; then die "an image above is not pinned by digest"; fi
grep -qF "image: ${AGENT_SANDBOX_CONTROLLER_IMAGE}" "${manifest}" || die "the controller image was not replaced by its digest"
kc apply -f "${manifest}" || die "could not apply the agent-sandbox manifest"
kc -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s \
  || die "the agent-sandbox controller did not roll out"
got="$(kc -n agent-sandbox-system get deployment agent-sandbox-controller -o jsonpath='{.spec.template.spec.containers[0].image}')"
[[ "${got}" == "${AGENT_SANDBOX_CONTROLLER_IMAGE}" ]] || die "the controller runs ${got}, not the pinned digest"

# ── 4. the release: backend on, keep on ─────────────────────────────────────
# --reuse-values keeps what quickstart.sh and overlay.sh set (the images, the Dex issuer, the role
# map). The chart renders the admission policies and the keep Role rules itself when these two
# switches are on, so nothing is applied beside it. The admin token is the walk's control channel for
# the one site-config write below; the person-facing calls all go through the member's own session.
step "helm upgrade --reuse-values (k8s.agentSandbox.enabled, k8s.agentSandbox.keep.enabled)"
ADMIN_TOKEN="${WARDYN_KIND_SSO_ADMIN_TOKEN:-walk-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')}"
helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn \
  -n "${NAMESPACE}" --reuse-values \
  --set "env.WARDYN_OIDC_ISSUER=http://localhost:${DEX_PORT}" \
  --set "env.WARDYN_OIDC_REDIRECT_URL=http://localhost:${HTTP_PORT}/auth/callback" \
  --set "auth.adminToken.value=${ADMIN_TOKEN}" \
  --set "k8s.agentSandbox.enabled=true" \
  --set "k8s.agentSandbox.keep.enabled=true" \
  >"${EVIDENCE_DIR}/helm-upgrade.log" 2>&1 \
  || { tail -30 "${EVIDENCE_DIR}/helm-upgrade.log" >&2; die "helm upgrade failed (see ${EVIDENCE_DIR}/helm-upgrade.log)"; }
if ! kc -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=300s; then
  kc -n "${NAMESPACE}" logs "deployment/${RELEASE}" --previous --tail=50 >&2 2>/dev/null \
    || kc -n "${NAMESPACE}" logs "deployment/${RELEASE}" --tail=50 >&2 || true
  die "wardynd did not become ready with the agent-sandbox backend on (its own refusal is above)"
fi
for var in WARDYN_K8S_AGENT_SANDBOX WARDYN_K8S_AGENT_SANDBOX_KEEP; do
  val="$(kc -n "${NAMESPACE}" get deployment "${RELEASE}" \
    -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name==\"${var}\")].value}")"
  [[ "${val}" == "true" ]] || die "the deployment carries ${var}='${val}', not 'true': the upgrade did not reach the pod"
done

# The policies come with the release. Same read as ci.yml's: each is observed once and its CEL
# type-checks clean, or a held pod could be admitted ungated.
step "checking the release's admission policies are observed and type-check clean"
policies="$(kc get validatingadmissionpolicy -l app.kubernetes.io/part-of=wardyn -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}')"
echo "policies: ${policies}"
for want in sandbox-pod-gate sandbox-pod-ungate keep; do
  grep -q -- "${want}" <<<"${policies}" || die "no admission policy named for '${want}' in: ${policies:-<none>}"
done
for p in ${policies}; do
  for _ in $(seq 30); do
    [[ "$(kc get validatingadmissionpolicy "${p}" -o jsonpath='{.status.observedGeneration}')" -ge 1 ]] 2>/dev/null && break
    sleep 1
  done
  gen="$(kc get validatingadmissionpolicy "${p}" -o jsonpath='{.status.observedGeneration}')"
  warn="$(kc get validatingadmissionpolicy "${p}" -o jsonpath='{.status.typeChecking.expressionWarnings}')"
  echo "${p}: observedGeneration=${gen} typeChecking warnings=[${warn}]"
  { [[ -n "${gen}" && "${gen}" -ge 1 ]] && [[ -z "${warn}" ]]; } 2>/dev/null || die "policy ${p} is not observed and type-checked clean"
done

# ── 5. member drives on, with a kept main drive ─────────────────────────────
# A bare PUT, as kind-sso-walk.sh makes: the cluster is fresh, so there is no other block to clobber.
# size_mib is required on Kubernetes (the claim's request); max_per_person bounds the walk's drives.
step "turning on member drives with keep_main_drive (storage.user_drive.member_create)"
code=""
for _ in $(seq 1 30); do
  code="$(curl -s -o "${EVIDENCE_DIR}/workspace-providers-put.json" -w '%{http_code}' \
    -X PUT -H "Authorization: Bearer ${ADMIN_TOKEN}" -H 'Content-Type: application/json' \
    -d '{"storage":{"user_drive":{"member_create":{"enabled":true,"max_per_person":3,"size_mib":512,"keep_main_drive":true}}}}' \
    "${BASE_URL}/api/v1/workspace-providers")"
  # 401 is the old pod still answering with the previous token while the new one takes over.
  [[ "${code}" == "401" ]] || break
  sleep 2
done
[[ "${code}" == "200" ]] || { cat "${EVIDENCE_DIR}/workspace-providers-put.json" >&2; die "PUT /workspace-providers answered ${code}"; }

# ── 6. the walk ─────────────────────────────────────────────────────────────
step "running the walk (ui/e2e/walk/agent-sandbox-keep.spec.ts, signed in as member@wardyn.local)"
export WARDYN_E2E_WALK_BASE_URL="${BASE_URL}"
export WARDYN_TEST_K8S=1
export WARDYN_WALK_ADMIN_TOKEN="${ADMIN_TOKEN}"
export WARDYN_WALK_KUBE_CONTEXT="${CONTEXT}"
export WARDYN_WALK_KUBE_NAMESPACE="${RUNS_NAMESPACE}"
# helpers.ts's SANDBOX_UP defaults to 300 s, tuned on a developer box. A hosted runner schedules the
# CNI, Postgres, the daemon, Dex and every sandbox pod at once on few vCPUs (kind-sso-walk.sh's note).
if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
  export WARDYN_WALK_SANDBOX_UP_MS="${WARDYN_WALK_SANDBOX_UP_MS:-720000}"
fi
walk_rc=0
./scripts/run-ui-e2e.sh agent-sandbox-keep 2>&1 | tee "${EVIDENCE_DIR}/walk.log"
spec_rc="${PIPESTATUS[0]}"
if [[ "${spec_rc}" -ne 0 ]]; then
  walk_rc=1
  if [[ -d "${ROOT}/ui/test-results" ]]; then
    rm -rf "${EVIDENCE_DIR}/test-results"
    cp -r "${ROOT}/ui/test-results" "${EVIDENCE_DIR}/test-results"
  fi
fi

# Evidence for a red walk, best effort and time-bounded so it never changes the walk's own exit code:
# the pods and their gates, the Sandbox objects, the events, and both daemons' logs.
if [[ "${walk_rc}" -ne 0 ]]; then
  step "walk failed: capturing pod, Sandbox, event and log evidence"
  timeout 60 kubectl --context "${CONTEXT}" get pods -A -o wide >"${EVIDENCE_DIR}/failure-pods-all.txt" 2>&1 || true
  timeout 60 kubectl --context "${CONTEXT}" -n "${RUNS_NAMESPACE}" get pods -o yaml >"${EVIDENCE_DIR}/failure-runs-pods.yaml" 2>&1 || true
  timeout 60 kubectl --context "${CONTEXT}" -n "${RUNS_NAMESPACE}" get sandboxes.agents.x-k8s.io -o yaml >"${EVIDENCE_DIR}/failure-sandboxes.yaml" 2>&1 || true
  timeout 60 kubectl --context "${CONTEXT}" -n "${RUNS_NAMESPACE}" get events --sort-by=.lastTimestamp >"${EVIDENCE_DIR}/failure-events-${RUNS_NAMESPACE}.txt" 2>&1 || true
  timeout 60 kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs "deployment/${RELEASE}" --all-containers --tail=5000 >"${EVIDENCE_DIR}/failure-wardynd-logs.txt" 2>&1 || true
  timeout 60 kubectl --context "${CONTEXT}" -n agent-sandbox-system logs deployment/agent-sandbox-controller --tail=2000 >"${EVIDENCE_DIR}/failure-controller-logs.txt" 2>&1 || true
  die "the keep walk failed (see ${EVIDENCE_DIR})"
fi

echo "kind-agent-sandbox-keep-walk: PASS"
