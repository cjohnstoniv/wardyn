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
# agent image another tree built), runs quickstart.sh against the existing
# cluster, and then re-applies the Entra and Bedrock overlay with
# --reuse-values — quickstart's own `helm upgrade` carries no overlay, so
# without that second upgrade every deploy would drop the OIDC settings.
#
# It touches only the local cluster. The tenant side (02-app.sh, consent) and
# the console-side rows (Azure DevOps provider, Bedrock roster) are separate.
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
BEDROCK_REGION="${WARDYN_BEDROCK_REGION:-us-east-1}"
BEDROCK_MODEL="${WARDYN_BEDROCK_MODEL:-us.anthropic.claude-haiku-4-5-20251001-v1:0}"
: "${TENANT_ID:?TENANT_ID is required}" "${CLIENT_ID:?CLIENT_ID is required}"

# The default daemon, where Step 3 put the cluster — named explicitly, because
# quickstart.sh's wardyn_pick_docker_host prefers the wardyn-docker daemon when
# DOCKER_HOST is unset, and then fails to find this cluster and tries to create
# a second one on the same host ports.
export DOCKER_HOST="${WARDYN_ENTRA_DOCKER_HOST:-unix:///var/run/docker.sock}"
kubectl --context "${CTX}" -n wardyn get secret wardyn-entra-oidc >/dev/null \
  || { echo "Secret wardyn-entra-oidc is missing on ${CTX} — README.md Step 4 creates it" >&2; exit 1; }

BUILD="$(mktemp -d)"
trap 'rm -rf "${BUILD}"' EXIT
git -C "${REPO}" archive "${SHA}" | tar -x -C "${BUILD}"
cd "${BUILD}"
echo "==> ${SHA} -> ${CTX} (images :${TAG})"

AGENT_IMAGE="wardyn/agent-claude-code:${TAG}"
BASE_IMAGE="wardyn/agent-base:${TAG}"
docker build -q -f deploy/images/base/Dockerfile -t "${BASE_IMAGE}" .
docker build -q -f deploy/images/claude-code/Dockerfile -t "${AGENT_IMAGE}" .

WARDYN_QUICKSTART_CLUSTER="${CLUSTER}" \
WARDYN_QUICKSTART_HTTP_PORT="${HTTP_PORT}" \
WARDYN_QUICKSTART_SSH_PORT="${SSH_PORT:-2422}" \
WARDYN_QUICKSTART_IMAGE_TAG="${TAG}" \
  deploy/kind/quickstart.sh

kind load docker-image "${AGENT_IMAGE}" "${BASE_IMAGE}" --name "${CLUSTER}"

KIT=deploy/azure-entra-sso
printf 'TENANT_ID=%s\nCLIENT_ID=%s\nHTTP_PORT=%s\n' "${TENANT_ID}" "${CLIENT_ID}" "${HTTP_PORT}" >"${KIT}/.env.local"
"${KIT}/04-values.sh" >/dev/null
cat >"${BUILD}/org.yaml" <<EOF
env:
  WARDYN_AGENT_IMAGES: '{"base":"${BASE_IMAGE}","claude-code":"${AGENT_IMAGE}"}'
  WARDYN_BEDROCK_REGION: "${BEDROCK_REGION}"
  WARDYN_BEDROCK_MODEL: "${BEDROCK_MODEL}"
EOF
helm --kube-context "${CTX}" upgrade wardyn deploy/helm/wardyn -n wardyn --reuse-values \
  -f "${KIT}/values-entra.yaml" -f "${BUILD}/org.yaml" \
  --set-file defaultPolicy=deploy/kind/sso/default-policy.json
kubectl --context "${CTX}" -n wardyn rollout status deployment/wardyn --timeout=300s

# The NodePort resets connections for a few seconds after the rollout reports done.
curl -fsS --retry 10 --retry-all-errors --retry-delay 3 "http://localhost:${HTTP_PORT}/healthz" >/dev/null
echo "==> deployed ${SHA} to ${CTX}: http://localhost:${HTTP_PORT}"
