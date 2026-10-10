#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# 05-kind-deploy.sh <git-ref> — deploy ONE commit to the Entra kind cluster,
# the way an organisation runs it: Entra sign-in, the per-user Azure DevOps
# lane, and Bedrock through each person's AWS IAM Identity Center sign-in.
#
# It builds from `git archive` of the ref (the checkout is never touched), tags
# every image with the commit (so a re-deploy never reuses a stale `:local`
# agent image another tree built), loads them into the cluster, and makes ONE
# `helm upgrade --reuse-values` carrying the Entra overlay. It does not re-run quickstart.sh:
# quickstart's own upgrade carries no overlay, so every deploy through it would roll the cluster through a
# revision with no OIDC and no egress to the issuer before this restored it.
# quickstart.sh stays the first install (README.md Step 3); this is every
# deploy after it.
#
# It touches only the local cluster. The tenant side (02-app.sh, consent) and
# the console-side rows (Azure DevOps provider, the Bedrock model provider
# with its region and model) are separate.
#
# Needs TENANT_ID and CLIENT_ID (not secrets). The client secret stays in the
# cluster's wardyn-entra-oidc Secret, which a first deploy creates from
# deploy/azure-entra-sso/.env.local's CLIENT_SECRET (README.md, Step 4).
set -euo pipefail

REF="${1:?usage: 05-kind-deploy.sh <git-ref>}"
REPO="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
SHA="$(git -C "${REPO}" rev-parse --verify "${REF}^{commit}")"
TAG="c-${SHA:0:12}"
CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-entra}"
CTX="kind-${CLUSTER}"
HTTP_PORT="${HTTP_PORT:-8480}"
: "${TENANT_ID:?TENANT_ID is required}" "${CLIENT_ID:?CLIENT_ID is required}"

# The default daemon, where Step 3 put the cluster — named explicitly, because
# the repo's scripts prefer the wardyn-docker daemon when DOCKER_HOST is unset
# (wardyn_pick_docker_host), and this cluster is not there.
export DOCKER_HOST="${WARDYN_ENTRA_DOCKER_HOST:-unix:///var/run/docker.sock}"
helm --kube-context "${CTX}" -n wardyn status wardyn >/dev/null \
  || { echo "no wardyn release on ${CTX} — run README.md Steps 3 and 4 first" >&2; exit 1; }
kubectl --context "${CTX}" -n wardyn get secret wardyn-entra-oidc >/dev/null \
  || { echo "Secret wardyn-entra-oidc is missing on ${CTX} — README.md Step 4 creates it" >&2; exit 1; }

BUILD="$(mktemp -d)"
trap 'rm -rf "${BUILD}"' EXIT
git -C "${REPO}" archive "${SHA}" | tar -x -C "${BUILD}"
cd "${BUILD}"
echo "==> ${SHA} -> ${CTX} (images :${TAG})"

WARDYND_IMAGE="wardyn/wardynd:${TAG}"
PROXY_IMAGE="wardyn/wardyn-proxy:${TAG}"
AGENT_IMAGE="wardyn/agent-claude-code:${TAG}"
BASE_IMAGE="wardyn/agent-base:${TAG}"
docker build -q -f deploy/compose/Dockerfile.wardynd -t "${WARDYND_IMAGE}" .
docker build -q -f deploy/compose/Dockerfile.proxy -t "${PROXY_IMAGE}" .
docker build -q -f deploy/images/base/Dockerfile -t "${BASE_IMAGE}" .
docker build -q -f deploy/images/claude-code/Dockerfile -t "${AGENT_IMAGE}" .
kind load docker-image "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${AGENT_IMAGE}" "${BASE_IMAGE}" --name "${CLUSTER}"

KIT=deploy/azure-entra-sso
# shellcheck source=../../scripts/lib/common.sh
. "${BUILD}/scripts/lib/common.sh" # wait_healthy

# quickstart.sh's demo Postgres has no volume (it is the same bare
# `kubectl create deployment` `make helm-install-test` uses, and that
# cluster is disposable by construction). This estate is not: it is walked
# for hours, and a `docker restart` of the kind node — the kindest thing
# anyone does to a box — takes every run, policy and console row with it.
# So the claim is created here, not in quickstart.sh, which every quickstart
# would then carry. Re-running is a no-op: `apply` on an unchanged object.
#
# FIRST adoption costs the database's current contents: mounting a PVC where
# there was none restarts the pod against an empty volume. That is the walk's
# setup to lose, once, before the walk — never a steady-state deploy, where
# the claim already exists and the patch below changes nothing.
kubectl --context "${CTX}" -n wardyn apply -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-data
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 2Gi
YAML
kubectl --context "${CTX}" -n wardyn patch deployment postgres --type=strategic -p '{
  "spec": {"template": {"spec": {
    "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "postgres-data"}}],
    "containers": [{"name": "postgres", "volumeMounts": [{"name": "data", "mountPath": "/var/lib/postgresql/data"}]}]
  }}}}'
# A cluster with no provisioner for the claim leaves the pod Pending on the
# mount forever, so name that state rather than letting the timeout read as a
# Postgres crash: it is the only way this rollout hangs.
if ! kubectl --context "${CTX}" -n wardyn rollout status deployment/postgres --timeout=300s; then
  kubectl --context "${CTX}" -n wardyn get pvc postgres-data >&2 || true
  die "deployment/postgres never rolled out — if pvc/postgres-data is Pending, this cluster has no StorageClass to bind it (kind's default local-path one does)"
fi

printf 'TENANT_ID=%s\nCLIENT_ID=%s\nHTTP_PORT=%s\n' "${TENANT_ID}" "${CLIENT_ID}" "${HTTP_PORT}" >"${KIT}/.env.local"
"${KIT}/04-values.sh" >/dev/null
cat >"${BUILD}/org.yaml" <<EOF
image:
  repository: ${WARDYND_IMAGE%:*}
  tag: ${TAG}
k8s:
  proxyImage: ${PROXY_IMAGE}
env:
  WARDYN_AGENT_IMAGES: '{"base":"${BASE_IMAGE}","claude-code":"${AGENT_IMAGE}"}'
EOF
helm --kube-context "${CTX}" upgrade wardyn deploy/helm/wardyn -n wardyn --reuse-values \
  -f "${KIT}/values-entra.yaml" -f "${BUILD}/org.yaml" \
  --set-file defaultPolicy=deploy/kind/sso/default-policy.json
kubectl --context "${CTX}" -n wardyn rollout status deployment/wardyn --timeout=300s

# The NodePort resets connections for a few seconds after the rollout reports
# done, so this polls for readiness. `curl --retry` printed its own
# `curl: (56)` on each failed attempt — an expected first failure that reads
# like a deploy failure in the log a 06 run tees; wait_healthy polls quietly and
# only the FINAL failure is reported, with the pod state that explains it.
# -m 3 bounds each attempt so a port that accepts and never replies still
# reaches the next one (common.sh's own note on the ceiling).
if ! wait_healthy "http://localhost:${HTTP_PORT}" 30 2; then
  kubectl --context "${CTX}" -n wardyn get pods >&2 || true
  die "/healthz never answered on http://localhost:${HTTP_PORT} after ${SHA} — the deploy above did not come up"
fi
echo "==> deployed ${SHA} to ${CTX}: http://localhost:${HTTP_PORT}"
