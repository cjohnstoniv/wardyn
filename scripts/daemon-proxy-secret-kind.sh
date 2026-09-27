#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/daemon-proxy-secret-kind.sh — the Kubernetes leg of
# WARDYN_DAEMON_PROXY_SECRET (T-59, #719): a throwaway kind cluster, a
# throwaway Postgres, and TWO helm installs of the SAME chart with
# daemonProxySecret.existingSecret pointed at an operator-managed Secret —
# once with the chart's own default defaultMode (0440, the kubelet's
# fsGroup-added group-read shape) and once with defaultMode: 0400 (no group
# bit, root-owned) — asserting both boot: the pod becomes Ready and its own
# log names WARDYN_DAEMON_PROXY_SECRET as the source (installDaemonProxySecret,
# cmd/wardynd/daemon_proxy.go, TestInstallDaemonProxySecret_GroupReadableUnderFsGroupAccepted
# is this same rule's unit proof — this script proves the CHART wiring reaches
# it end to end, not just the Go rule in isolation).
#
# The Secret carries a syntactically valid but NEVER-DIALED proxy URL:
# daemonProxySecretMode only inspects the mounted file's mode and parses the
# URL's shape at boot — nothing here ever connects to it.
#
# Usage: scripts/daemon-proxy-secret-kind.sh   (needs docker, kind, kubectl, helm)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

CLUSTER="${CLUSTER:-daemon-proxy-secret-k8s}"
NS=wardyn-test
IMAGE_REPO=wardyn/wardynd
IMAGE_TAG="daemon-proxy-secret-$$"
WORK="$(mktemp -d)"
CREATED_CLUSTER=""

cleanup() {
  [ -n "$CREATED_CLUSTER" ] && kind delete cluster --name "$CREATED_CLUSTER" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}

kind get clusters 2>/dev/null | grep -qx "$CLUSTER" && die "a kind cluster named $CLUSTER already exists; set CLUSTER to another name"
# The trap is installed only after the guard passes, so a pre-existing
# cluster of the same name is never torn down by this script exiting.
trap cleanup EXIT
log "creating kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --kubeconfig "$WORK/kubeconfig" --wait 120s >/dev/null
CREATED_CLUSTER="$CLUSTER"
export KUBECONFIG="$WORK/kubeconfig"

log "building $IMAGE_REPO:$IMAGE_TAG"
docker build -f deploy/compose/Dockerfile.wardynd -t "$IMAGE_REPO:$IMAGE_TAG" . >/dev/null
kind load docker-image "$IMAGE_REPO:$IMAGE_TAG" --name "$CLUSTER" >/dev/null

log "postgres ($NS)"
kubectl create namespace "$NS" >/dev/null
kubectl -n "$NS" create secret generic wardyn-postgres-dsn \
  --from-literal=dsn="postgres://wardyn:wardyn@postgres:5432/wardyn?sslmode=disable" \
  --from-literal=age-key="$(docker run --rm "$IMAGE_REPO:$IMAGE_TAG" -gen-age-key)" >/dev/null
kubectl -n "$NS" create deployment postgres --image=postgres:17 >/dev/null
kubectl -n "$NS" set env deployment/postgres POSTGRES_USER=wardyn POSTGRES_PASSWORD=wardyn POSTGRES_DB=wardyn >/dev/null
kubectl -n "$NS" expose deployment postgres --port=5432 >/dev/null
kubectl -n "$NS" rollout status deployment/postgres --timeout=120s >/dev/null

rc=0
for mode in 0440 0400; do
  release="wardyn-dps-$mode"
  secret="wardyn-daemon-proxy-$mode"
  log "mode $mode: Secret $secret + helm install $release"
  kubectl -n "$NS" create secret generic "$secret" \
    --from-literal=proxy-url="http://alice:s3cr3t-token@proxy.invalid:3128" >/dev/null
  # The chart's own daemonProxySecret.defaultMode renders the Secret volume's
  # defaultMode; --set here is the ONLY thing that varies between the two runs.
  if ! helm install "$release" ./deploy/helm/wardyn \
      --namespace "$NS" \
      --set image.repository="$IMAGE_REPO" \
      --set image.tag="$IMAGE_TAG" \
      --set secrets.ageKeyFromSecret=true \
      --set auth.adminToken.value="$(openssl rand -hex 20)" \
      --set daemonProxySecret.existingSecret="$secret" \
      --set daemonProxySecret.defaultMode="$mode" >/dev/null; then
    warn "mode $mode: helm install failed"
    rc=1
    continue
  fi
  if ! kubectl -n "$NS" rollout status "deployment/$release" --timeout=120s >/dev/null; then
    warn "mode $mode: rollout never converged — pod state + logs follow"
    kubectl -n "$NS" describe pod -l "app.kubernetes.io/instance=$release" || true
    kubectl -n "$NS" logs -l "app.kubernetes.io/instance=$release" --tail=200 --all-containers || true
    rc=1
  elif ! kubectl -n "$NS" logs -l "app.kubernetes.io/instance=$release" --all-containers 2>/dev/null \
      | grep -q "daemon egress proxy configured (WARDYN_DAEMON_PROXY_SECRET)"; then
    warn "mode $mode: pod booted but never logged wiring WARDYN_DAEMON_PROXY_SECRET — the Secret may not have reached the mount"
    rc=1
  else
    log "mode $mode: booted and wired WARDYN_DAEMON_PROXY_SECRET"
  fi
  helm uninstall "$release" --namespace "$NS" >/dev/null 2>&1 || true
done

exit "$rc"
