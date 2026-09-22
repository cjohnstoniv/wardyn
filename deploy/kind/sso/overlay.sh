#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# overlay.sh — the multi-user (SSO) overlay for the kind quickstart cluster, as
# one command instead of four hand-typed ones. Run it via `make kind-sso`;
# `make kind-sso-down` removes the overlay.
#
# It is the SAME four commands README.md documents, in the same order, with one
# addition: `kind load` of the locally built images, so the overlay runs THIS
# tree's wardynd rather than whatever `make kind-quickstart` loaded earlier.
# Thin on purpose, for the reason quickstart.sh gives: the cluster name, the
# ports and every install flag live in ONE place, so the down path can never
# drift from what the up path created.
#
# IT NEVER CREATES OR DELETES A CLUSTER. The overlay runs on the cluster `make
# kind-quickstart` leaves behind (deploy/kind/quickstart.sh owns that lifecycle,
# and this box may hold other clusters); a missing cluster is a loud failure
# naming the command that makes one, never an implicit `kind create`.
#
# Everything it installs is demo-grade and public by design — the bcrypt literal
# is the word `password`, the client secret is a fixed demo string, and
# awsssofake impersonates AWS with no signing at all. Do not reuse any of it
# outside a throwaway cluster.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "${ROOT}"

# ONE DAEMON EVERYWHERE. On a dual-daemon box the kind cluster's node container
# lives on the dedicated wardyn daemon, and `kind get clusters` / `kind load` /
# `docker build` all read DOCKER_HOST — so without this the cluster is invisible
# and the overlay refuses a cluster that is running. Same picker every other
# script uses (scripts/lib/common.sh, honoured by setup/up/e2e-backend).
. "${ROOT}/scripts/lib/common.sh"
wardyn_pick_docker_host

# ── identity (single source of truth; the down path reads the same values) ───
# Must agree with deploy/kind/quickstart.sh — this overlay upgrades that release.
CLUSTER="${WARDYN_QUICKSTART_CLUSTER:-wardyn-quickstart}"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
RELEASE="wardyn"
# README.md's ports: 8280 so the cluster coexists with a compose stack on :8080,
# and 5557 for the browser-facing Dex (split horizon — the cluster reaches Dex
# by its Service, your browser reaches it through this port-forward).
HTTP_PORT="${WARDYN_QUICKSTART_HTTP_PORT:-8280}"
DEX_PORT="${WARDYN_KIND_SSO_DEX_PORT:-5557}"
# The port-forward is a long-lived child; its pid lives beside the repo's other
# scratch state so `make kind-sso-down` can stop exactly the one it started.
PF_PIDFILE="${TMPDIR:-/tmp}/wardyn-kind-sso-dex-${CLUSTER}.pid"
# Locally built images, `kind load`ed. No registry is involved anywhere here.
# Tagged per cluster (deploy/kind/quickstart.sh's WARDYN_QUICKSTART_IMAGE_TAG);
# the Azure DevOps profile defaults to its own tag so it never retags the one
# another cluster's overlay reloads.
if [[ "${WARDYN_KIND_SSO_PROFILE:-default}" == "ado" ]]; then
  IMAGE_TAG="${WARDYN_QUICKSTART_IMAGE_TAG:-kind-ado}"
else
  IMAGE_TAG="${WARDYN_QUICKSTART_IMAGE_TAG:-quickstart}"
fi
WARDYND_IMAGE="wardyn/wardynd:${IMAGE_TAG}"
PROXY_IMAGE="wardyn/wardyn-proxy:${IMAGE_TAG}"
FAKE_IMAGE="wardyn/awsssofake:local"
# The AWS SSO LOGIN sandbox's image. `make kind-quickstart` loads agent-base and
# agent-claude-code; neither is what a "Sign in to AWS" run boots. Without this
# the login pod resolves agentImage("aws-sso") to the PUBLISHED
# ghcr.io/cjohnstoniv/agent-aws-sso:<version> ref and sits in ImagePullBackOff
# on a cluster with no pull path — which the console shows as a login pane that
# never reaches its terminal, i.e. exactly the P1 symptom this overlay exists to
# let you disprove.
AWS_SSO_IMAGE="wardyn/agent-aws-sso:local"
# WHICH OVERLAY. `default` is Dex + the fake AWS endpoints. `ado` is the Azure
# DevOps profile: the console signs in against a fake Entra tenant, and a fake
# Azure DevOps serves the run (deploy/kind/sso/adofake.yaml, values-ado.yaml).
# Two profiles, not one overlay with two issuers: the console has ONE issuer,
# and the Azure DevOps sign-in only binds to an Entra one.
PROFILE="${WARDYN_KIND_SSO_PROFILE:-default}"
case "${PROFILE}" in default|ado) ;; *) echo "ERROR: WARDYN_KIND_SSO_PROFILE must be default or ado (got ${PROFILE})" >&2; exit 1 ;; esac
# The ado profile's TEST image and the Secret holding the CA its TLS front
# signs leaves with. Never published: built here, `kind load`ed, nothing else.
ADO_FAKE_IMAGE="wardyn/test-adofake:local"
ADO_CA_SECRET="wardyn-test-adofake-ca"

step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
die() { echo "ERROR: $*" >&2; exit 1; }

stop_port_forward() {
  if [[ -f "${PF_PIDFILE}" ]]; then
    local pid; pid="$(cat "${PF_PIDFILE}" 2>/dev/null || true)"
    if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || true
    fi
    rm -f "${PF_PIDFILE}"
  fi
}

if [[ "${1:-}" == "--down" && "${PROFILE}" == "ado" ]]; then
  step "deleting the Azure DevOps profile's objects (the fake + its CA Secret)"
  kubectl --context "${CONTEXT}" delete -f deploy/kind/sso/adofake.yaml --ignore-not-found=true || true
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" delete secret "${ADO_CA_SECRET}" --ignore-not-found=true || true
  echo "Overlay removed. The cluster and the helm release are untouched."
  exit 0
fi
if [[ "${1:-}" == "--down" ]]; then
  step "stopping the Dex port-forward"
  stop_port_forward
  step "deleting the overlay's own objects (Dex + the fake AWS endpoints)"
  # The CLUSTER stays: `make kind-down` owns that, and this overlay is not the
  # only thing that may be installed on it. The helm release stays on OIDC too —
  # reverting it to the admin token is `make kind-quickstart` again, which is
  # both shorter and honest about what it does.
  kubectl --context "${CONTEXT}" delete -f deploy/kind/sso/awsssofake.yaml --ignore-not-found=true || true
  kubectl --context "${CONTEXT}" delete -f deploy/kind/sso/dex.yaml --ignore-not-found=true || true
  echo
  echo "Overlay removed. The cluster and the helm release are untouched:"
  echo "  make kind-quickstart   # re-render the release on the admin token"
  echo "  make kind-down         # delete the cluster"
  exit 0
fi
[[ -z "${1:-}" ]] || die "unknown argument '${1}' (only --down is accepted)"

for bin in docker kind kubectl helm curl; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done
kind get clusters 2>/dev/null | grep -qx "${CLUSTER}" || die \
  "no kind cluster named ${CLUSTER}. This overlay never creates one — run:
    WARDYN_QUICKSTART_HTTP_PORT=${HTTP_PORT} WARDYN_QUICKSTART_SSH_PORT=2322 make kind-quickstart"

# K-01: the cluster-name check above says nothing about the PORT this overlay
# is about to print as the console URL. A plain `make kind-quickstart`
# publishes :8080; this overlay's default is :8280 (README.md's split with a
# compose stack) — install on that mismatch and it prints a URL nothing
# answers, the OIDC callback 404s, and the walk dies on a misleading "another
# daemon on that port?". Fail loudly here instead, before touching the cluster.
health="$(curl -sf --max-time 3 "http://localhost:${HTTP_PORT}/healthz" || true)"
[[ "${health}" == *'"runner":"k8s"'* ]] || die \
  "http://localhost:${HTTP_PORT}/healthz did not answer with runner=k8s (nothing there, or a
compose stack on that port? Body: ${health}) — this overlay is about
to print that as the console URL. Re-run the quickstart on the SAME port:
    WARDYN_QUICKSTART_HTTP_PORT=${HTTP_PORT} WARDYN_QUICKSTART_SSH_PORT=2322 make kind-quickstart"

# ── the Azure DevOps profile ────────────────────────────────────────────────
if [[ "${PROFILE}" == "ado" ]]; then
  # The fake's one accepted redirect and values-ado.yaml's OIDC callback are
  # both http://localhost:8580 — a demo literal, like dex.yaml's 8280.
  [[ "${HTTP_PORT}" == "8580" ]] || die "the ado profile's callback is pinned to http://localhost:8580 (adofake.yaml, values-ado.yaml); run the quickstart and this overlay with WARDYN_QUICKSTART_HTTP_PORT=8580"
  command -v openssl >/dev/null 2>&1 || die "openssl not found on PATH (the ado profile mints its walk CA with it)"
  step "building the fake Microsoft image (${ADO_FAKE_IMAGE}) — TEST ONLY, never published"
  docker build -f test/adofake/cmd/Dockerfile -t "${ADO_FAKE_IMAGE}" .
  # ALWAYS built here, under this profile's own tag: the release is pinned to
  # these below, so what runs is this tree whatever the quickstart loaded.
  step "building wardynd + wardyn-proxy from this tree (${WARDYND_IMAGE}, ${PROXY_IMAGE})"
  docker build -f deploy/compose/Dockerfile.wardynd -t "${WARDYND_IMAGE}" .
  docker build -f deploy/compose/Dockerfile.proxy   -t "${PROXY_IMAGE}"   .
  step "loading images into ${CLUSTER} (no registry pull)"
  for img in "${ADO_FAKE_IMAGE}" "${WARDYND_IMAGE}" "${PROXY_IMAGE}"; do
    kind load docker-image "${img}" --name "${CLUSTER}"
  done
  # THE WALK CA, minted once per cluster and kept in a Secret, so a restart of
  # the fake (which every walk does) signs with the same CA wardynd trusts.
  ca_dir="$(mktemp -d)"
  trap 'rm -rf "${ca_dir}"' EXIT
  if ! kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret "${ADO_CA_SECRET}" >/dev/null 2>&1; then
    step "minting the walk CA (Secret ${ADO_CA_SECRET})"
    openssl ecparam -name prime256v1 -genkey -noout 2>/dev/null | openssl pkcs8 -topk8 -nocrypt -out "${ca_dir}/ca.key"
    openssl req -x509 -new -key "${ca_dir}/ca.key" -subj "/CN=wardyn kind ado walk CA" -days 30 \
      -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign -out "${ca_dir}/ca.crt"
    kubectl --context "${CONTEXT}" -n "${NAMESPACE}" create secret generic "${ADO_CA_SECRET}" \
      --from-file=ca.crt="${ca_dir}/ca.crt" --from-file=ca.key="${ca_dir}/ca.key"
  fi
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret "${ADO_CA_SECRET}" \
    -o jsonpath='{.data.ca\.crt}' | base64 -d > "${ca_dir}/ca.crt"
  step "applying the fake Microsoft"
  kubectl --context "${CONTEXT}" apply -f deploy/kind/sso/adofake.yaml
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status deployment/wardyn-test-adofake --timeout=180s
  step "helm upgrade ${RELEASE} onto the fake Entra tenant (trusting the walk CA)"
  helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn \
    -n "${NAMESPACE}" --reuse-values -f deploy/kind/sso/values-ado.yaml \
    --set "image.repository=${WARDYND_IMAGE%:*}" --set "image.tag=${IMAGE_TAG}" \
    --set "k8s.proxyImage=${PROXY_IMAGE}" \
    --set-file trustedCA="${ca_dir}/ca.crt" \
    --set-file defaultPolicy=deploy/kind/sso/default-policy.json
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=300s
  cat <<EOF

The Azure DevOps profile is up.

  URL:     http://localhost:${HTTP_PORT}
  Sign in: admin@wardyn.test or member@wardyn.test (a picker, no password)
  Fake Microsoft: wardyn-test-adofake.${NAMESPACE}.svc.cluster.local:3128 (a forward proxy, in-cluster only)

  WARDYN_TEST_K8S=1 WARDYN_KIND_SSO_PROFILE=ado WARDYN_QUICKSTART_CLUSTER=${CLUSTER} scripts/kind-sso-walk.sh   # the walk
  WARDYN_KIND_SSO_PROFILE=ado make kind-sso-down          # remove the overlay (the cluster stays)
EOF
  exit 0
fi

# ── 0. images ───────────────────────────────────────────────────────────────
# awsssofake is built every time: it is small, it is this tree's own code, and a
# stale fake is the hardest failure here to read (the walk fails inside a
# sandbox, on a host that looks right). It is NOT a product image — never
# published, never built by `make agent-images`.
step "building the fake AWS endpoints image (${FAKE_IMAGE}) — TEST ONLY, never published"
docker build -f test/awsssofake/cmd/Dockerfile -t "${FAKE_IMAGE}" .

# wardynd/proxy are REUSED unless asked otherwise: the quickstart just built
# them, and rebuilding on every overlay run would make a two-minute command a
# ten-minute one. Set WARDYN_KIND_SSO_REBUILD=1 after changing daemon code.
if [[ "${WARDYN_KIND_SSO_REBUILD:-}" == "1" ]]; then
  step "rebuilding wardynd + wardyn-proxy from this tree"
  docker build -f deploy/compose/Dockerfile.wardynd -t "${WARDYND_IMAGE}" .
  docker build -f deploy/compose/Dockerfile.proxy   -t "${PROXY_IMAGE}"   .
fi

step "loading images into ${CLUSTER} (no registry pull)"
for img in "${FAKE_IMAGE}" "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${AWS_SSO_IMAGE}"; do
  docker image inspect "${img}" >/dev/null 2>&1 || die "image ${img} not present locally (run \`make kind-quickstart\` first, \`make agent-images\` for ${AWS_SSO_IMAGE}, or WARDYN_KIND_SSO_REBUILD=1)"
  kind load docker-image "${img}" --name "${CLUSTER}"
done

# ── 1. Dex + the fake AWS endpoints ─────────────────────────────────────────
step "applying Dex and the fake AWS endpoints"
# dex.yaml carries the two browser-facing URLs as its defaults (issuer
# localhost:5557, callback localhost:8280). Rendered onto THIS overlay's ports so
# a second SSO cluster can sit beside the first; with the defaults the render is
# the file byte for byte. A changed issuer only reaches a Dex that restarts
# (it reads its config once), so a ConfigMap that moved restarts it.
dex_out="$(sed -e "s#http://localhost:5557#http://localhost:${DEX_PORT}#g" \
               -e "s#http://localhost:8280/auth/callback#http://localhost:${HTTP_PORT}/auth/callback#g" \
               deploy/kind/sso/dex.yaml | kubectl --context "${CONTEXT}" apply -f -)"
echo "${dex_out}"
if grep -q "configmap/wardyn-dex configured" <<<"${dex_out}"; then
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout restart deployment/wardyn-dex
fi
kubectl --context "${CONTEXT}" apply -f deploy/kind/sso/awsssofake.yaml
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status deployment/wardyn-dex --timeout=180s
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status deployment/wardyn-awsssofake --timeout=180s

# ── 2. the chart, re-rendered on OIDC ───────────────────────────────────────
# --reuse-values so the quickstart's image/substrate values persist; the default
# policy rides along floored to CC1 — redundant since 0.7.8 (the baked default
# floors at CC1 now), kept so this Fence-only cluster states its own floor
# rather than inheriting the image's.
step "helm upgrade ${RELEASE} onto Dex SSO"
helm --kube-context "${CONTEXT}" upgrade "${RELEASE}" deploy/helm/wardyn \
  -n "${NAMESPACE}" --reuse-values -f deploy/kind/sso/values.yaml \
  --set "env.WARDYN_OIDC_ISSUER=http://localhost:${DEX_PORT}" \
  --set "env.WARDYN_OIDC_REDIRECT_URL=http://localhost:${HTTP_PORT}/auth/callback" \
  --set-file defaultPolicy=deploy/kind/sso/default-policy.json
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status "deployment/${RELEASE}" --timeout=300s

# ── 3. the browser-facing issuer ────────────────────────────────────────────
# Backgrounded rather than run in the foreground as README.md types it: this
# script has to return so `make kind-sso` can finish and scripts/kind-sso-walk.sh
# can drive the cluster. Same port, same command, one pidfile so the down path
# stops exactly this one.
step "port-forwarding Dex to localhost:${DEX_PORT} (split horizon — the browser's issuer)"
stop_port_forward
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" port-forward svc/wardyn-dex "${DEX_PORT}:5556" \
  >/dev/null 2>&1 &
echo $! > "${PF_PIDFILE}"

for _ in $(seq 1 30); do
  curl -sf "http://localhost:${DEX_PORT}/dex/.well-known/openid-configuration" >/dev/null 2>&1 && break
  curl -sf "http://localhost:${DEX_PORT}/.well-known/openid-configuration" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "http://localhost:${DEX_PORT}/.well-known/openid-configuration" >/dev/null 2>&1 || die \
  "Dex never answered on http://localhost:${DEX_PORT} — the port-forward did not come up (is ${DEX_PORT} already taken?)"

cat <<EOF

The SSO overlay is up.

  URL:     http://localhost:${HTTP_PORT}
  Admin:   admin@wardyn.local  / password
  Member:  member@wardyn.local / password
  Dex:     http://localhost:${DEX_PORT}   (port-forward pid $(cat "${PF_PIDFILE}"))
  Fake AWS endpoints: http://wardyn-awsssofake.${NAMESPACE}.svc.cluster.local:8090 (in-cluster only)

  scripts/kind-sso-walk.sh          # the live AWS SSO walk against this cluster
  make kind-sso-down                # remove the overlay (the cluster stays)
EOF
