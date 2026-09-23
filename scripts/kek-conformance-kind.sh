#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/kek-conformance-kind.sh — the Vault Kubernetes-auth leg of the key
# service suite: Vault from its official Helm chart (dev mode) on a throwaway
# kind cluster, configured as docs/OPERATIONS.md gives it (Kubernetes auth,
# role "wardyn" bound to a service account, audience "vault", the KV policy
# templated on the service account's namespace, the two-path Transit policy),
# then TestLive_KubernetesAuth* with a real projected service-account token.
#
# Reports land in test/reports/go/kek-k8s/; the skip floor
# (scripts/test-report.sh) fails a run in which either test skipped.
#
# Usage: scripts/kek-conformance-kind.sh   (needs docker, kind, kubectl, helm, jq)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

CLUSTER="${CLUSTER:-kekconf-k8s}"
CHART_VERSION="${CHART_VERSION:-0.34.1}"
VAULT_TAG="${VAULT_TAG:-2.1.1}"
NS=wardyn-live
WORK="$(mktemp -d)"
PF_PID=""

cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

kind get clusters 2>/dev/null | grep -qx "$CLUSTER" && die "a kind cluster named $CLUSTER already exists; set CLUSTER to another name"
log "creating kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --kubeconfig "$WORK/kubeconfig" --wait 120s >/dev/null
export KUBECONFIG="$WORK/kubeconfig"

log "installing Vault (chart $CHART_VERSION, image $VAULT_TAG, dev mode)"
helm repo add hashicorp https://helm.releases.hashicorp.com --force-update >/dev/null
helm install vault hashicorp/vault --version "$CHART_VERSION" --namespace vault --create-namespace \
  --set server.dev.enabled=true --set server.dev.devRootToken=root --set injector.enabled=false \
  --set server.image.tag="$VAULT_TAG" >/dev/null
kubectl -n vault wait --for=condition=Ready pod/vault-0 --timeout=180s >/dev/null

vx() { kubectl -n vault exec -i vault-0 -- env VAULT_TOKEN=root "$@"; }
vx vault auth enable kubernetes >/dev/null
# Vault reviews tokens with its own service account (the chart's auth-delegator binding).
# shellcheck disable=SC2016 # expands inside the pod
vx sh -c 'vault write auth/kubernetes/config kubernetes_host=https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT' >/dev/null
ACCESSOR="$(vx vault auth list -format=json | jq -r '."kubernetes/".accessor')"
vx vault secrets enable -path=wardyn -version=2 kv >/dev/null
vx vault secrets enable transit >/dev/null
vx vault write -f transit/keys/wardyn type=aes256-gcm96 >/dev/null
vx vault policy write wardyn-kv - >/dev/null <<EOF
path "wardyn/data/{{identity.entity.aliases.$ACCESSOR.metadata.service_account_namespace}}/*" {
  capabilities = ["create", "update", "read"]
}
path "wardyn/metadata/{{identity.entity.aliases.$ACCESSOR.metadata.service_account_namespace}}/*" {
  capabilities = ["create", "update", "read", "delete", "list"]
}
EOF
vx vault policy write wardyn-transit - >/dev/null <<'EOF'
path "transit/encrypt/wardyn" { capabilities = ["update"] }
path "transit/decrypt/wardyn" { capabilities = ["update"] }
EOF
vx vault write auth/kubernetes/role/wardyn bound_service_account_names=wardyn bound_service_account_namespaces="$NS" \
  audience=vault policies=wardyn-kv,wardyn-transit token_ttl=1h >/dev/null

kubectl create namespace "$NS" >/dev/null
kubectl -n "$NS" create serviceaccount wardyn >/dev/null
kubectl -n "$NS" create token wardyn --audience vault --duration 1h >"$WORK/jwt"

kubectl -n vault port-forward svc/vault 0:8200 --address 127.0.0.1 >"$WORK/pf.log" 2>&1 &
PF_PID=$!
PORT=""
for _ in $(seq 1 30); do
  PORT="$(sed -n 's/^Forwarding from 127.0.0.1:\([0-9]*\) .*/\1/p' "$WORK/pf.log" | head -1)"
  [ -n "$PORT" ] && break
  sleep 1
done
[ -n "$PORT" ] || die "kubectl port-forward did not start: $(cat "$WORK/pf.log")"

log "live Kubernetes-auth suite against Vault at 127.0.0.1:$PORT"
WARDYN_TEST_VAULT="http://127.0.0.1:$PORT" WARDYN_TEST_VAULT_K8S_JWT_FILE="$WORK/jwt" \
  "$ROOT/scripts/test-report.sh" kek-k8s -count=1 -run '^TestLive_KubernetesAuth' ./internal/secretstore/vaultkv/
