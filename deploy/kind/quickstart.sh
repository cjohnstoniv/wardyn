#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# quickstart.sh — one command from a bare host to a REAL Wardyn install on a
# throwaway kind cluster, with the KUBERNETES runner substrate on (sandboxes
# are pods in this cluster, not containers on a Docker daemon). Run it via
# `make kind-quickstart`; `make kind-down` deletes the cluster.
#
# This is the same shape CI already proves, in one script instead of two jobs:
# ci.yml's helm-install-test (build wardynd locally, `kind load`, `helm
# install`, wait for a healthy control plane) plus conformance-k8s (a
# disableDefaultCNI kind cluster + a version-pinned Calico, because the
# substrate refuses to boot on a CNI that does not enforce NetworkPolicy). The
# Calico version and the two rollout waits below are pinned to exactly what
# ci.yml pins, so a green CI and a working quickstart never diverge.
#
# It is NOT a production recipe. Demo-grade shortcuts, all deliberate:
#   - Postgres is a single plain-manifest pod with no PVC (mirrors
#     `make helm-install-test`); the cluster is disposable, so is the data.
#   - the admin token is passed inline, so it lands in the Helm release
#     history — fine on a laptop cluster, never beyond one.
#   - the age identity lives in the same external Secret as the DSN
#     (secrets.ageKeyFromSecret=true) so it survives pod restarts; the chart's
#     own default regenerates one per boot and the pod crash-loops on its
#     SECOND start, unable to decrypt what boot 1 wrote.
# For a real cluster, read deploy/helm/wardyn/README.md — it is canonical.
#
# NEVER auto-sets WARDYN_K8S_ALLOW_UNENFORCED_NETPOL: if this host's kind +
# Calico does not actually enforce NetworkPolicy, the substrate's refusal is
# surfaced VERBATIM and the script fails. An install that silently downgrades
# containment to make a quickstart succeed would be the worst thing this file
# could do.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"

# ── identity (single source of truth; `make kind-down` calls this script) ────
CLUSTER="wardyn-quickstart"
CONTEXT="kind-${CLUSTER}"
NAMESPACE="wardyn"
RELEASE="wardyn"
# Pinned exactly as ci.yml's conformance-k8s job installs it.
CALICO_VERSION="v3.28.0"
CALICO_MANIFEST="https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/calico.yaml"
# NodePorts published on the host by quickstart-kind-config.yaml's
# extraPortMappings — these must agree with that file.
#
# HTTP_PORT/SSH_PORT are each used TWICE on purpose: as the Service port (the
# chart's service.port / ssh.port) and as the host port the mapping publishes.
# Keeping them equal is what makes the printed URL match what the chart renders
# and what wardynd advertises over SSH.
NODE_HTTP_PORT=30080
HTTP_PORT=8080
NODE_SSH_PORT=30022
SSH_PORT=2222
# Locally built, `kind load`ed images. No registry is involved anywhere here.
WARDYND_IMAGE="wardyn/wardynd:quickstart"
PROXY_IMAGE="wardyn/wardyn-proxy:quickstart"
# The repo-convention tag `make agent-images-core` produces. Reused (not a
# :quickstart tag of its own) so an operator behind a corporate proxy can
# build it once through make's NPM_REGISTRY/HTTPS_PROXY pass-through and this
# script will find it and skip its own plain `docker build`.
AGENT_IMAGE="wardyn/agent-claude-code:local"

step() { printf '\n==> %s\n' "$*"; }
die() { printf 'quickstart: %s\n' "$*" >&2; exit 1; }

if [[ "${1:-}" == "--down" ]]; then
  step "deleting kind cluster ${CLUSTER}"
  kind delete cluster --name "${CLUSTER}"
  echo "Cluster gone. The locally built images remain on this host:"
  echo "  ${WARDYND_IMAGE}  ${PROXY_IMAGE}  ${AGENT_IMAGE}"
  echo "  (docker rmi them if you want the disk back)"
  exit 0
fi
if [[ -n "${1:-}" ]]; then
  die "unknown argument '${1}' (only --down is accepted)"
fi

for bin in docker kind kubectl helm curl openssl; do
  command -v "${bin}" >/dev/null 2>&1 || die "${bin} not found on PATH"
done
docker info >/dev/null 2>&1 || die "docker daemon unreachable"

# ── 1. images (BEFORE the cluster: a failed build leaves nothing behind) ─────
step "building wardynd (${WARDYND_IMAGE}) — -tags docker,k8s, same Dockerfile CI builds"
docker build -f deploy/compose/Dockerfile.wardynd -t "${WARDYND_IMAGE}" .

step "building wardyn-proxy (${PROXY_IMAGE}) — the k8s substrate's sidecar AND its boot-time egress canary"
docker build -f deploy/compose/Dockerfile.proxy -t "${PROXY_IMAGE}" .

if docker image inspect "${AGENT_IMAGE}" >/dev/null 2>&1; then
  echo "agent image ${AGENT_IMAGE} already present — reusing it"
else
  step "building the claude-code agent image (${AGENT_IMAGE}) — carries wardyn-rec, which k8s Exec requires"
  docker build -f deploy/images/claude-code/Dockerfile -t "${AGENT_IMAGE}" .
fi

# ── 2. cluster + a CNI that actually enforces NetworkPolicy ─────────────────
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  step "kind cluster ${CLUSTER} already exists — reusing it"
else
  step "creating kind cluster ${CLUSTER}"
  kind create cluster --name "${CLUSTER}" \
    --config deploy/kind/quickstart-kind-config.yaml --wait 90s
fi

step "installing Calico ${CALICO_VERSION} (kind's default CNI does not enforce NetworkPolicy)"
kubectl --context "${CONTEXT}" apply -f "${CALICO_MANIFEST}"
kubectl --context "${CONTEXT}" -n kube-system rollout status daemonset/calico-node --timeout=180s
kubectl --context "${CONTEXT}" -n kube-system rollout status deployment/calico-kube-controllers --timeout=180s

step "loading images into the cluster (no registry pull)"
for img in "${WARDYND_IMAGE}" "${PROXY_IMAGE}" "${AGENT_IMAGE}"; do
  kind load docker-image "${img}" --name "${CLUSTER}"
done

# ── 3. namespace, DSN + age Secret, Postgres ────────────────────────────────
kubectl --context "${CONTEXT}" create namespace "${NAMESPACE}" \
  --dry-run=client -o yaml | kubectl --context "${CONTEXT}" apply -f -

# Created ONCE and never rotated on a re-run: the age identity in it is what
# decrypts everything a previous boot wrote to the secret store. Regenerating
# it would crash-loop wardynd with "no identity matched any of the recipients".
if kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret wardyn-postgres-dsn >/dev/null 2>&1; then
  echo "Secret wardyn-postgres-dsn exists — keeping its age identity"
else
  step "creating Secret wardyn-postgres-dsn (DSN + a fresh age identity)"
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" create secret generic wardyn-postgres-dsn \
    --from-literal=dsn="postgres://wardyn:wardyn@postgres:5432/wardyn?sslmode=disable" \
    --from-literal=age-key="$(docker run --rm "${WARDYND_IMAGE}" -gen-age-key)"
fi

if kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get deployment postgres >/dev/null 2>&1; then
  echo "Postgres already deployed"
else
  step "deploying Postgres (demo-grade: one pod, no PVC — same as make helm-install-test)"
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" create deployment postgres --image=postgres:16
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" set env deployment/postgres \
    POSTGRES_USER=wardyn POSTGRES_PASSWORD=wardyn POSTGRES_DB=wardyn
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" expose deployment postgres --port=5432
fi
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status deployment/postgres --timeout=180s

# ── 4. helm install ─────────────────────────────────────────────────────────
# Re-runs must not rotate the admin token out from under a printed URL.
TOKEN="$(kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get secret wardyn-auth \
  -o jsonpath='{.data.admin-token}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[[ -n "${TOKEN}" ]] || TOKEN="$(openssl rand -hex 20)"

# NodePort ingress arrives from OUTSIDE the pod network: kube-proxy SNATs it to
# the node's own address, which is not a pod and so matches neither the chart's
# default `podSelector: {}` ingress peer nor any other. Without the node
# network's CIDR as a second peer, Calico denies the very request this
# quickstart tells you to make. Read it off the docker network rather than
# hardcoding 172.18.0.0/16 — a pre-existing `kind` network may use another.
NODE_CIDR="$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' \
  | tr ' ' '\n' | grep -v ':' | grep -v '^$' | head -1)"
[[ -n "${NODE_CIDR}" ]] || die "could not read the kind docker network's IPv4 subnet"

# A values file, not a wall of --set: every override below is here because the
# chart REFUSES to render (or wardynd refuses to boot) without it, and each one
# gets to say why in place. It carries the token, so it lives in a 0700 mktemp
# dir and dies with the script.
values_dir="$(mktemp -d)"
trap 'rm -rf "${values_dir}"' EXIT
cat >"${values_dir}/values.yaml" <<EOF
# Generated by deploy/kind/quickstart.sh — not a template to copy into prod.
image:
  repository: ${WARDYND_IMAGE%:*}
  tag: ${WARDYND_IMAGE##*:}
auth:
  adminToken:
    # Inline mode: the chart refuses to render with neither a token nor an OIDC
    # issuer, because every route would 401 while /healthz still read Ready.
    value: ${TOKEN}
secrets:
  # The DSN Secret above also carries an "age-key" entry; inject it. Without a
  # STABLE identity wardynd generates a per-boot one and crash-loops on restart.
  ageKeyFromSecret: true
serviceAccount:
  # k8s.enabled refuses to render without this: the substrate drives the API
  # server with the pod's own projected ServiceAccount token.
  automount: true
service:
  # Reachable from the host through the kind config's extraPortMappings; the
  # node ports themselves are pinned onto the Service after install below.
  type: NodePort
k8s:
  enabled: true
  # Refused at render if empty, and it is a BOOT-time refusal in wardynd too
  # (errProxyImageUnset) — not a per-run failure.
  proxyImage: ${PROXY_IMAGE}
  # runsNamespace left empty => sandboxes run in this same namespace.
ssh:
  enabled: true
  port: ${SSH_PORT}
  # What agents/operators are told to connect back to. The host side of the
  # extraPortMapping, because that is the address a human here can reach.
  advertiseHost: 127.0.0.1
networkPolicy:
  ingress:
    from:
      # Keep the chart's default peer (this namespace: the UI, and every run's
      # proxy sidecar calling back for mints/approvals/recording uploads)...
      - podSelector: {}
      # ...and add the node network, or NodePort traffic is denied (see above).
      - ipBlock:
          cidr: ${NODE_CIDR}
env:
  # The agent image is loaded into this cluster, not pullable from ghcr — point
  # the claude-code harness at the local tag or every run ImagePullBackOffs.
  WARDYN_AGENT_IMAGES: '{"claude-code":"${AGENT_IMAGE}"}'
EOF

step "helm upgrade --install ${RELEASE} (namespace ${NAMESPACE})"
helm upgrade --install "${RELEASE}" ./deploy/helm/wardyn \
  --kube-context "${CONTEXT}" \
  --namespace "${NAMESPACE}" \
  -f "${values_dir}/values.yaml"

if ! kubectl --context "${CONTEXT}" -n "${NAMESPACE}" rollout status \
  "deployment/${RELEASE}" --timeout=300s; then
  echo "" >&2
  echo "FAIL: wardynd never became ready. Pod state and logs follow verbatim." >&2
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" describe pod \
    -l app.kubernetes.io/name=wardyn >&2 || true
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" logs \
    -l app.kubernetes.io/name=wardyn --tail=100 --all-containers >&2 || true
  echo "" >&2
  echo "If the logs above say 'refusing to boot' with 'NetworkPolicy is NOT enforced'," >&2
  echo "this host's kind+Calico is not actually enforcing policy. That refusal is" >&2
  echo "CORRECT and this script will not work around it: do NOT set" >&2
  echo "WARDYN_K8S_ALLOW_UNENFORCED_NETPOL — every sandbox would get unconfined" >&2
  echo "egress. Use the compose stack instead (make setup), or a cluster whose CNI" >&2
  echo "enforces NetworkPolicy (docs: deploy/helm/wardyn/README.md)." >&2
  exit 1
fi

# The chart has no nodePort value (a Service field, not a Wardyn one), so k8s
# assigns random ports at install time and the kind config's fixed
# extraPortMappings would map nothing. Patch them onto the Service by port
# number — Service.spec.ports merges on `port`, so this sets nothing else.
step "pinning the Service node ports to match the kind port mappings"
kubectl --context "${CONTEXT}" -n "${NAMESPACE}" patch service "${RELEASE}" -p \
  "{\"spec\":{\"ports\":[{\"port\":${HTTP_PORT},\"nodePort\":${NODE_HTTP_PORT}},{\"port\":${SSH_PORT},\"nodePort\":${NODE_SSH_PORT}}]}}"

step "proving /healthz through the published NodePort"
ok=0
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HTTP_PORT}/healthz" || true)"
  if [[ "${code}" == "200" ]]; then ok=1; break; fi
  sleep 2
done
[[ "${ok}" == "1" ]] || die "/healthz never returned 200 on http://127.0.0.1:${HTTP_PORT} (last code: ${code:-none})"

kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pods

cat <<EOF

Wardyn is up.

  URL:    http://127.0.0.1:${HTTP_PORT}
  Token:  ${TOKEN}
  SSH:    ssh -p ${SSH_PORT} <run-id>@127.0.0.1   (docs/SSH.md)

  kubectl --context ${CONTEXT} -n ${NAMESPACE} get pods
  make kind-down    # delete the cluster
EOF
