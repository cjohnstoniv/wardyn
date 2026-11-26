#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/test-kind-kata-place.sh: shows that a RuntimeClass's scheduling puts only a CC3 run's agent pod
# on an isolated pool of nodes. It creates its own three-node kind cluster (a control plane and two
# workers, on Calico), labels one worker pool=isolated and taints it isolated=true:NoSchedule, then runs
# TestConformanceK8sIsolatedPool (test/conformance). That test creates a RuntimeClass whose scheduling
# selects and tolerates the pool, starts a CC3 and a CC1 run, and asserts where the pods land: the CC3
# agent pod on the isolated worker, its proxy pod and the CC1 run's pods elsewhere. It asserts scheduling
# only: kind has no Kata, so no agent container starts. The cluster, the two images it builds and the
# work directory are removed on exit; it never touches another kind cluster.
#
# Usage: WARDYN_TEST_K8S=1 scripts/test-kind-kata-place.sh   (needs docker, kind, kubectl, go)
# KATA_CALICO_VERSION overrides the Calico pin. It is run by hand, not by a workflow.
set -euo pipefail

if [ "${WARDYN_TEST_K8S:-}" != "1" ]; then
  echo "test-kind-kata-place: set WARDYN_TEST_K8S=1 to run the isolated-pool kind case (skipping)"
  exit 77
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
die() { echo "test-kind-kata-place: $*" >&2; exit 1; }
for tool in docker kind kubectl go make; do
  command -v "$tool" >/dev/null || die "needs $tool"
done
avail="$(free -g | awk '/^Mem:/{print $7}')"
[ "$avail" -ge 8 ] || die "only ${avail} GB of memory available; the case needs 8"

CALICO_VERSION="${KATA_CALICO_VERSION:-v3.28.0}"
CLUSTER="wardyn-kata-$$"
PROXY_IMAGE="wardyn/wardyn-proxy:kata-place"
AGENT_IMAGE="wardyn/conformance-agent:kata-place"
WORK="$(mktemp -d)"
cleanup() {
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  docker rmi -f "$PROXY_IMAGE" "$AGENT_IMAGE" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

cat >"$WORK/kind.yaml" <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
nodes:
  - role: control-plane
  - role: worker
  - role: worker
YAML

echo "test-kind-kata-place: creating $CLUSTER"
kind create cluster --name "$CLUSTER" --config "$WORK/kind.yaml" --kubeconfig "$WORK/admin.kc" --wait 180s >"$WORK/kind.log" 2>&1 \
  || { tail -20 "$WORK/kind.log"; die "kind could not create the cluster"; }
export KUBECONFIG="$WORK/admin.kc"
kubectl apply -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/calico.yaml" >/dev/null
kubectl -n kube-system rollout status daemonset/calico-node --timeout=300s >/dev/null
kubectl -n kube-system rollout status deployment/calico-kube-controllers --timeout=300s >/dev/null
kubectl wait --for=condition=Ready nodes --all --timeout=180s >/dev/null

ISOLATED="$CLUSTER-worker2"
kubectl label node "$ISOLATED" pool=isolated >/dev/null
kubectl taint node "$ISOLATED" isolated=true:NoSchedule >/dev/null

docker build -q -t "$PROXY_IMAGE" -f deploy/compose/Dockerfile.proxy . >/dev/null
make build-conformance-agent-image CONFORMANCE_AGENT_IMAGE="$AGENT_IMAGE" >/dev/null
kind load docker-image "$PROXY_IMAGE" "$AGENT_IMAGE" --name "$CLUSTER" >/dev/null

kubectl create namespace wardyn-conformance >/dev/null
kubectl label namespace wardyn-conformance \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/enforce-version=v1.30 >/dev/null

WARDYN_TEST_K8S=1 WARDYN_K8S_NAMESPACE=wardyn-conformance \
WARDYN_PROXY_IMAGE="$PROXY_IMAGE" WARDYN_TEST_K8S_AGENT_IMAGE="$AGENT_IMAGE" \
WARDYN_TEST_K8S_ISOLATED_NODE="$ISOLATED" \
  go test -v -tags k8s -run '^TestConformanceK8sIsolatedPool$' -count=1 -timeout 15m ./test/conformance/ 2>&1 | tee "$WORK/test.log" || true

if grep -q -- '--- PASS: TestConformanceK8sIsolatedPool' "$WORK/test.log" \
  && ! grep -qE -- '--- (SKIP|FAIL): TestConformanceK8sIsolatedPool' "$WORK/test.log"; then
  echo "test-kind-kata-place: PASS"
  exit 0
fi
echo "test-kind-kata-place: FAIL"
tail -40 "$WORK/test.log"
exit 1
