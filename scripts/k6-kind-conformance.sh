#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/k6-kind-conformance.sh — lane K6's kind conformance harness for the Kubernetes runner
# (design K1 §8). For each kubelet version (the 1.31 floor and the latest kind node image) it:
#   - creates its own cluster, wardyn-k6-<pid>, one control plane and two workers, on Calico
#     (a CNI that enforces NetworkPolicy);
#   - builds and loads the runnerd and proxy images (make runnerd-image, the compose proxy-image
#     target) and test/k6/fakeorg, a minimal organisation endpoint;
#   - installs deploy/helm/wardyn-runnerd with its rendered Role against the fake org, and checks
#     the real runnerd's boot (access review, foreign objects, kubelet floor, relay source probe);
#   - runs test/k6 under runnerd's own ServiceAccount token, never cluster-admin;
#   - deletes the cluster, and every image it built, on any exit (trap).
# It refuses to start below 8 GB of available memory, and never touches another kind cluster.
# The release gate for the unbound-pod arm is bind-delete-serial and bind-delete-overlap passing
# on both versions.
#
# Usage: WARDYN_TEST_K8S=1 scripts/k6-kind-conformance.sh   (needs docker, kind, kubectl, helm,
# go, openssl, jq). K6_NODE_IMAGES overrides the node images; K6_CALICO_VERSION the Calico pin.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/common.sh
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

[ "${WARDYN_TEST_K8S:-}" = "1" ] || skip_lane "k6-kind-conformance: WARDYN_TEST_K8S=1 not set; skipping"
for tool in docker kind kubectl helm go openssl jq; do
  command -v "$tool" >/dev/null || die "k6-kind-conformance needs $tool"
done

NODE_IMAGES="${K6_NODE_IMAGES:-kindest/node:v1.31.4 kindest/node:v1.35.0}"
CALICO_VERSION="${K6_CALICO_VERSION:-v3.28.0}"
CLUSTER="wardyn-k6-$$"
TAG="k6-$$"
RUNNERD_IMAGE="wardyn/wardyn-runnerd:$TAG"
PROXY_IMAGE="wardyn/wardyn-proxy:$TAG"
ORG_IMAGE="wardyn/k6-fakeorg:$TAG"
AGENT_IMAGE="wardyn/k6-agent:$TAG"
ORG_NS=wardyn-k6-org
OWN_NS=wardyn-k6-runner
RUNS_NS=wardyn-k6-runs
RELEASE=k6
ORG_HOST="fake-org.$ORG_NS.svc.cluster.local"
ORG_URL="https://$ORG_HOST"
WORK="$(mktemp -d)"
CREATED=""
START=$SECONDS

# Images this script may create; a :local tag or the agent image that existed before it ran is kept.
had() { docker image inspect "$1" >/dev/null 2>&1 && echo yes || echo no; }
HAD_RUNNERD_LOCAL="$(had wardyn/wardyn-runnerd:local)"
HAD_PROXY_LOCAL="$(had wardyn/wardyn-proxy:local)"
HAD_BUSYBOX="$(had busybox:1.36)"

# shellcheck disable=SC2317  # reached through the EXIT trap
cleanup() {
  local rc=$?
  set +e
  [ -n "${SAMPLER:-}" ] && kill "$SAMPLER" 2>/dev/null
  [ -n "$CREATED" ] && kind delete cluster --name "$CREATED" >/dev/null 2>&1
  docker rmi "$RUNNERD_IMAGE" "$PROXY_IMAGE" "$ORG_IMAGE" "$AGENT_IMAGE" >/dev/null 2>&1
  [ "$HAD_RUNNERD_LOCAL" = no ] && docker rmi wardyn/wardyn-runnerd:local >/dev/null 2>&1
  [ "$HAD_PROXY_LOCAL" = no ] && docker rmi wardyn/wardyn-proxy:local >/dev/null 2>&1
  [ "$HAD_BUSYBOX" = no ] && docker rmi busybox:1.36 >/dev/null 2>&1
  rm -rf "$WORK"
  return "$rc"
}

kind get clusters 2>/dev/null | grep -qx "$CLUSTER" && die "a kind cluster named $CLUSTER already exists"
# Installed only after the guard, so a pre-existing cluster of this name is never deleted.
trap cleanup EXIT

mem_check() {
  local avail
  avail="$(free -g | awk '/^Mem:/{print $7}')"
  [ "$avail" -ge 8 ] || die "only ${avail} GB of memory available; the harness needs 8"
}

# Host memory in use, sampled every 5 s, for the peak this run reports.
free -m | awk '/^Mem:/{print $3}' >"$WORK/mem.base"
( while true; do free -m | awk '/^Mem:/{print $3}'; sleep 5; done ) >"$WORK/mem.log" &
SAMPLER=$!

mem_check
log "building images ($TAG)"
make runnerd-image >"$WORK/build.log" 2>&1 || { tail -40 "$WORK/build.log"; die "make runnerd-image failed"; }
docker compose -f deploy/compose/docker-compose.yaml --profile build-only build proxy-image >>"$WORK/build.log" 2>&1 \
  || { tail -40 "$WORK/build.log"; die "proxy image build failed"; }
docker tag wardyn/wardyn-runnerd:local "$RUNNERD_IMAGE"
docker tag wardyn/wardyn-proxy:local "$PROXY_IMAGE"
docker build -q -f test/k6/fakeorg/Dockerfile -t "$ORG_IMAGE" . >>"$WORK/build.log" 2>&1 \
  || { tail -40 "$WORK/build.log"; die "fake org image build failed"; }
# The agent is busybox, rebuilt as a single-platform local image: kind load cannot import a pulled
# multi-platform index from Docker Desktop's containerd store (content digest not found).
printf 'FROM busybox:1.36\n' | docker build -q -t "$AGENT_IMAGE" - >>"$WORK/build.log" 2>&1 \
  || { tail -20 "$WORK/build.log"; die "agent image build failed"; }

log "fake org TLS ($ORG_HOST)"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=k6 org CA" \
  -keyout "$WORK/ca.key" -out "$WORK/ca.pem" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj "/CN=$ORG_HOST" -keyout "$WORK/tls.key" -out "$WORK/tls.csr" >/dev/null 2>&1
printf 'subjectAltName=DNS:%s\n' "$ORG_HOST" >"$WORK/san.ext"
openssl x509 -req -in "$WORK/tls.csr" -CA "$WORK/ca.pem" -CAkey "$WORK/ca.key" -CAcreateserial -days 1 \
  -extfile "$WORK/san.ext" -out "$WORK/tls.crt" >/dev/null 2>&1

cat >"$WORK/kind.yaml" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
nodes:
  - role: control-plane
  - role: worker
  - role: worker
EOF

cat >"$WORK/values.yaml" <<EOF
org: $ORG_URL
runsNamespace: $RUNS_NS
sandbox:
  arch: ARCH
image:
  repository: wardyn/wardyn-runnerd
  tag: $TAG
proxyImage:
  repository: wardyn/wardyn-proxy
registration:
  existingSecret: k6-registration
trustedCA:
  configMap: k6-org-ca
limits:
  maxRuns: 2
  totalCPU: 2
  totalMemory: 4
  totalDisk: 10
egress:
  extra:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: $ORG_NS
      ports:
        - protocol: TCP
          port: 8443
EOF

SUMMARY="$WORK/summary.tsv"
: >"$SUMMARY"
rc=0
case_line() { printf 'K6-CASE\t%s\t%s\t%s\n' "$1" "$2" "$3" | sed "s/^K6-CASE/$VERSION/" >>"$SUMMARY"; }

for image in $NODE_IMAGES; do
  VERSION="${image##*:}"
  mem_check
  log "[$VERSION] creating $CLUSTER"
  if ! kind create cluster --name "$CLUSTER" --image "$image" --config "$WORK/kind.yaml" \
      --kubeconfig "$WORK/admin.kc" --wait 180s >"$WORK/kind-$VERSION.log" 2>&1; then
    tail -20 "$WORK/kind-$VERSION.log"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    case_line cluster FAIL "kind could not create a $image cluster (see the kind output above)"
    rc=1
    continue
  fi
  CREATED="$CLUSTER"
  export KUBECONFIG="$WORK/admin.kc"
  kubectl apply -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/calico.yaml" >/dev/null
  kubectl -n kube-system rollout status daemonset/calico-node --timeout=300s >/dev/null
  kubectl -n kube-system rollout status deployment/calico-kube-controllers --timeout=300s >/dev/null
  kubectl wait --for=condition=Ready nodes --all --timeout=180s >/dev/null
  for img in "$RUNNERD_IMAGE" "$PROXY_IMAGE" "$ORG_IMAGE" "$AGENT_IMAGE"; do
    kind load docker-image "$img" --name "$CLUSTER" >/dev/null
  done
  ARCH="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')"
  RUNNER_ID="$(cat /proc/sys/kernel/random/uuid)"

  log "[$VERSION] fake org and the runnerd chart"
  kubectl create namespace "$ORG_NS" >/dev/null
  kubectl create namespace "$OWN_NS" >/dev/null
  kubectl create namespace "$RUNS_NS" >/dev/null
  kubectl label namespace "$RUNS_NS" pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/enforce-version=latest >/dev/null
  kubectl -n "$ORG_NS" create secret tls fake-org-tls --cert="$WORK/tls.crt" --key="$WORK/tls.key" >/dev/null
  kubectl -n "$ORG_NS" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: fake-org}
spec:
  selector: {matchLabels: {app: fake-org}}
  template:
    metadata: {labels: {app: fake-org}}
    spec:
      containers:
        - name: fake-org
          image: $ORG_IMAGE
          env:
            - {name: K6_ORG_URL, value: "$ORG_URL"}
            - {name: K6_RUNNER_ID, value: "$RUNNER_ID"}
          ports: [{containerPort: 8443}]
          volumeMounts: [{name: tls, mountPath: /tls, readOnly: true}]
      volumes: [{name: tls, secret: {secretName: fake-org-tls}}]
---
apiVersion: v1
kind: Service
metadata: {name: fake-org}
spec:
  selector: {app: fake-org}
  ports: [{port: 443, targetPort: 8443}]
EOF
  kubectl -n "$ORG_NS" rollout status deployment/fake-org --timeout=120s >/dev/null
  kubectl -n "$OWN_NS" create configmap k6-org-ca --from-file=ca.pem="$WORK/ca.pem" >/dev/null
  kubectl -n "$OWN_NS" create secret generic k6-registration --from-literal=token="wdr_$(openssl rand -hex 32)" >/dev/null
  sed "s/arch: ARCH/arch: $ARCH/" "$WORK/values.yaml" >"$WORK/values-$VERSION.yaml"
  helm install "$RELEASE" deploy/helm/wardyn-runnerd -n "$OWN_NS" -f "$WORK/values-$VERSION.yaml" >/dev/null
  DEPLOY="$(kubectl -n "$OWN_NS" get deploy -l app.kubernetes.io/instance=$RELEASE -o jsonpath='{.items[0].metadata.name}')"
  # The chart's register init container does not mount trustedCA.configMap, so registration against
  # an org behind a private CA cannot verify it. The harness gives it the bundle through SSL_CERT_FILE
  # (reported as a chart follow-up); runnerd serve reads the bundle through the chart's own flag.
  kubectl -n "$OWN_NS" patch deployment "$DEPLOY" --type=json -p='[
    {"op":"add","path":"/spec/template/spec/initContainers/0/env","value":[{"name":"SSL_CERT_FILE","value":"/etc/wardyn/trusted-ca/ca.pem"}]},
    {"op":"add","path":"/spec/template/spec/initContainers/0/volumeMounts/-","value":{"name":"trusted-ca","mountPath":"/etc/wardyn/trusted-ca","readOnly":true}}]' >/dev/null
  boot=FAIL
  if kubectl -n "$OWN_NS" rollout status "deployment/$DEPLOY" --timeout=300s >/dev/null &&
     timeout 300 bash -c "until kubectl -n $OWN_NS logs deployment/$DEPLOY -c runnerd 2>/dev/null | grep -q 'k8s runner ready to connect'; do sleep 3; done"; then
    boot=PASS
  else
    rc=1
    kubectl -n "$OWN_NS" describe pods -l "app.kubernetes.io/instance=$RELEASE" | tail -40 || true
    kubectl -n "$OWN_NS" logs "deployment/$DEPLOY" --all-containers --tail=60 || true
  fi
  case_line in-cluster-runnerd-boot "$boot" "chart Role; SSAR, foreign objects, kubelet floor, relay listen and the relay source probe on Calico (log: k8s runner ready to connect)"
  timeout 60 bash -c "until kubectl -n $ORG_NS logs deployment/fake-org 2>/dev/null | grep -q 'stream attempt'; do sleep 2; done" || true
  orglog="$(kubectl -n "$ORG_NS" logs deployment/fake-org 2>/dev/null || true)"
  if grep -q 'registered runner' <<<"$orglog" && grep -q 'stream attempt' <<<"$orglog"; then
    case_line org-hub-session BLOCKED-ON-K3 "runnerd registered through the chart and dialled the stream; the fake org has no hub (org-runner sessions are K3's)"
  else
    case_line org-hub-session FAIL "the fake org saw no registration or no stream attempt"
    rc=1
  fi
  # runnerd stops here: the suite drives the same runner identity in process under its token.
  kubectl -n "$OWN_NS" scale "deployment/$DEPLOY" --replicas=0 >/dev/null
  kubectl -n "$OWN_NS" wait --for=delete pod -l "app.kubernetes.io/instance=$RELEASE" --timeout=120s >/dev/null 2>&1 || true

  SA="$(kubectl -n "$OWN_NS" get deployment "$DEPLOY" -o jsonpath='{.spec.template.spec.serviceAccountName}')"
  SELECTOR="$(kubectl -n "$OWN_NS" get deployment "$DEPLOY" -o jsonpath='{.spec.selector.matchLabels}')"
  RELAY_IP="$(kubectl -n "$OWN_NS" get svc -l app.kubernetes.io/instance=$RELEASE -o jsonpath='{.items[0].spec.clusterIP}')"
  TOKEN="$(kubectl -n "$OWN_NS" create token "$SA" --duration=4h)"
  SERVER="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
  CADATA="$(kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
  cat >"$WORK/runnerd.kc" <<EOF
apiVersion: v1
kind: Config
clusters: [{name: k6, cluster: {server: "$SERVER", certificate-authority-data: "$CADATA"}}]
users: [{name: runnerd, user: {token: "$TOKEN"}}]
contexts: [{name: runnerd, context: {cluster: k6, user: runnerd}}]
current-context: runnerd
EOF

  log "[$VERSION] cases (test/k6, under runnerd's token)"
  set +e
  WARDYN_TEST_K6=1 KUBECONFIG="$WORK/runnerd.kc" K6_ADMIN_KUBECONFIG="$WORK/admin.kc" \
    K6_RUNS_NS="$RUNS_NS" K6_OWN_NS="$OWN_NS" K6_SA="$SA" K6_RUNNER_ID="$RUNNER_ID" K6_RELAY_IP="$RELAY_IP" \
    K6_ORG_URL="$ORG_URL" K6_ORG_CA="$WORK/ca.pem" K6_RUNNERD_SELECTOR="$SELECTOR" K6_ARCH="$ARCH" \
    K6_CLUSTER="$CLUSTER" K6_AGENT_IMAGE="$AGENT_IMAGE" WARDYN_PROXY_IMAGE="$PROXY_IMAGE" \
    go test -tags k8s -count=1 -v -timeout 90m -run '^TestK6$' ./test/k6/ >"$WORK/test-$VERSION.log" 2>&1
  trc=$?
  set -e
  grep -E '^\s*--- (FAIL|PASS|SKIP)|^(FAIL|ok) |panic:' "$WORK/test-$VERSION.log" | tail -60 || true
  grep '^K6-CASE' "$WORK/test-$VERSION.log" | sed "s/^K6-CASE/$VERSION/" >>"$SUMMARY" || true
  if [ "$trc" != 0 ]; then
    rc=1
    grep -B2 -A12 -E '^\s*--- FAIL|panic:' "$WORK/test-$VERSION.log" | head -150 || true
  fi
  if [ -n "${K6_KEEP_LOGS:-}" ]; then cp "$WORK/test-$VERSION.log" "$K6_KEEP_LOGS/" || true; fi

  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  CREATED=""
done

kill "$SAMPLER" 2>/dev/null
SAMPLER=""
base="$(cat "$WORK/mem.base")"
peak="$(sort -n "$WORK/mem.log" | tail -1)"
echo
printf '%-10s %-40s %-16s %s\n' VERSION CASE STATUS DETAIL
awk -F'\t' '{printf "%-10s %-40s %-16s %s\n", $1, $2, $3, $4}' "$SUMMARY"
echo
echo "commit $(git rev-parse --short HEAD); wall $((SECONDS - START)) s; host memory in use ${base} MiB at start, peak ${peak} MiB"
if [ "$rc" = 0 ] && ! awk -F'\t' '$3=="FAIL"{f=1} END{exit !f}' "$SUMMARY"; then
  echo "k6-kind-conformance: PASS"
else
  echo "k6-kind-conformance: FAIL"
  rc=1
fi
exit "$rc"
