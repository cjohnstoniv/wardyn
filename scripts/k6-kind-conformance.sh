#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# scripts/k6-kind-conformance.sh — the kind conformance harness for the Kubernetes runner. For
# each kubelet version (the 1.31 floor and the latest kind node image) it:
#   - creates its own cluster, wardyn-k6-<pid>, one control plane and two workers, on Calico
#     (a CNI that enforces NetworkPolicy);
#   - builds and loads the wardynd, runnerd and proxy images and the harness's agent image
#     (busybox, wardyn-rec and test/k6/agentprobe);
#   - stands up a real organisation on the control-plane node: wardynd from deploy/helm/wardyn
#     (with its own Kubernetes substrate, as `make kind-quickstart` installs it) over a postgres:17,
#     serving its runner URL over TLS from a CA this script mints;
#   - enrols an organisation runner the production way: an org token minted by the org, the
#     deploy/helm/wardyn-runnerd chart registering with it, a security operator claiming it by its
#     fingerprint, then runnerd dialling the org's hub outbound;
#   - enrols a second, personal runner (a person's token, a second chart release) for the trust
#     placement case;
#   - runs TestK6Org (test/k6) against that org and the live runnerd, then scales runnerd down and
#     runs TestK6, which drives the same runner identity's Driver in process under runnerd's own
#     ServiceAccount token, never cluster-admin;
#   - deletes the cluster, and every image it built, on any exit (trap).
# It refuses to start below 8 GB of available memory, and never touches another kind cluster.
# The release gate for the unbound-pod arm is bind-delete-serial and bind-delete-overlap passing
# on both versions.
#
# Usage: WARDYN_TEST_K8S=1 scripts/k6-kind-conformance.sh   (needs docker, kind, kubectl, helm,
# go, openssl, jq, curl). The suites run in a docker:cli container on the kind network, with the docker
# socket for their node-level steps. K6_NODE_IMAGES overrides the node images; K6_CALICO_VERSION the
# Calico pin. K6_ORG_RUN (development only) narrows the org suite's -test.run;
# K6_KEEP=1 (development only) leaves the last cluster, its images and the work directory
# up and prints how to remove them; K6_SKIP_INPROC=1 skips TestK6.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/common.sh disable=SC1091  # followed under shellcheck -x
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

[ "${WARDYN_TEST_K8S:-}" = "1" ] || skip_lane "k6-kind-conformance: WARDYN_TEST_K8S=1 not set; skipping"
for tool in docker kind kubectl helm go openssl jq curl; do
  command -v "$tool" >/dev/null || die "k6-kind-conformance needs $tool"
done

NODE_IMAGES="${K6_NODE_IMAGES:-kindest/node:v1.31.4 kindest/node:v1.35.0}"
CALICO_VERSION="${K6_CALICO_VERSION:-v3.28.0}"
CLUSTER="wardyn-k6-$$"
TAG="k6-$$"
RUNNERD_IMAGE="wardyn/wardyn-runnerd:$TAG"
PROXY_IMAGE="wardyn/wardyn-proxy:$TAG"
WARDYND_IMAGE="wardyn/wardynd:$TAG"
PG_IMAGE="wardyn/k6-postgres:$TAG"
AGENT_IMAGE="wardyn/k6-agent:$TAG"
ORG_NS=wardyn-k6-org
ORG_RUNS_NS=wardyn-k6-orgruns
OWN_NS=wardyn-k6-runner
RUNS_NS=wardyn-k6-runs
RELEASE=k6
P_RELEASE=k6p
P_OWN_NS=wardyn-k6-prunner
P_RUNS_NS=wardyn-k6-pruns
ORG_RELEASE=k6org
ORG_FULL=wardyn-org
ORG_HOST="$ORG_FULL.$ORG_NS.svc.cluster.local"
ORG_URL="https://$ORG_HOST:8080"
SCHED_LABEL="wardyn-k6/sched"
WORK="$(mktemp -d)"
CREATED=""
START=$SECONDS

# Images this script may create; a :local tag or a base image that existed before it ran is kept.
had() { docker image inspect "$1" >/dev/null 2>&1 && echo yes || echo no; }
HAD_RUNNERD_LOCAL="$(had wardyn/wardyn-runnerd:local)"
HAD_PROXY_LOCAL="$(had wardyn/wardyn-proxy:local)"
HAD_BUSYBOX="$(had busybox:1.36)"
HAD_POSTGRES="$(had postgres:17)"
RUNNER_IMAGE="docker:27.3.1-cli"
HAD_RUNNER="$(had "$RUNNER_IMAGE")"

# shellcheck disable=SC2317,SC2329  # reached through the EXIT trap
cleanup() {
  local rc=$?
  set +e
  [ -n "${SAMPLER:-}" ] && kill "$SAMPLER" 2>/dev/null
  if [ "${K6_KEEP:-}" = 1 ]; then
    echo "K6_KEEP=1: kept cluster '${CREATED:-none}', work dir $WORK and the $TAG images; remove them with:"
    echo "  kind delete cluster --name $CLUSTER; docker rmi $RUNNERD_IMAGE $PROXY_IMAGE $WARDYND_IMAGE $PG_IMAGE $AGENT_IMAGE; rm -rf $WORK"
    return "$rc"
  fi
  [ -n "$CREATED" ] && kind delete cluster --name "$CREATED" >/dev/null 2>&1
  docker rmi "$RUNNERD_IMAGE" "$PROXY_IMAGE" "$WARDYND_IMAGE" "$PG_IMAGE" "$AGENT_IMAGE" >/dev/null 2>&1
  [ "$HAD_RUNNERD_LOCAL" = no ] && docker rmi wardyn/wardyn-runnerd:local >/dev/null 2>&1
  [ "$HAD_PROXY_LOCAL" = no ] && docker rmi wardyn/wardyn-proxy:local >/dev/null 2>&1
  [ "$HAD_BUSYBOX" = no ] && docker rmi busybox:1.36 >/dev/null 2>&1
  [ "$HAD_POSTGRES" = no ] && docker rmi postgres:17 >/dev/null 2>&1
  [ "$HAD_RUNNER" = no ] && docker rmi "$RUNNER_IMAGE" >/dev/null 2>&1
  rm -rf "$WORK"
  return "$rc"
}

kind get clusters 2>/dev/null | grep -x "$CLUSTER" >/dev/null && die "a kind cluster named $CLUSTER already exists"
# Installed only after the guard, so a pre-existing cluster of this name is never deleted.
trap cleanup EXIT

# load_image IMAGE NODE imports a local image into a kind node's containerd directly. kind load
# cannot detect the snapshotter of newer node images (containerd 2) on older kind releases.
load_image() {
  docker save "$1" | docker exec -i "$2" ctr --namespace=k8s.io images import --digests - >/dev/null
}

mem_check() {
  local avail
  avail="$(free -g | awk '/^Mem:/{print $7}')"
  [ "$avail" -ge 8 ] || die "only ${avail} GB of memory available; the harness needs 8"
}

# Host memory in use, sampled every 5 s, for the peak this run reports.
free -m | awk '/^Mem:/{print $3}' >"$WORK/mem.base"
# The kind nodes' own use comes from docker stats (Docker Desktop's VM is outside free's view).
( while true; do
    free -m | awk '/^Mem:/{print $3}' >>"$WORK/mem.log"
    docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' | awk -v c="$CLUSTER" '
      index($1, c) == 1 { v = $2; u = v; gsub(/[0-9.]/, "", u); n = v + 0
        if (u == "GiB") n *= 1024; else if (u == "KiB") n /= 1024; s += n }
      END { printf "%d\n", s }' >>"$WORK/kind-mem.log"
    sleep 5
  done ) &
SAMPLER=$!

mem_check
log "building images ($TAG)"
make runnerd-image >"$WORK/build.log" 2>&1 || { tail -40 "$WORK/build.log"; die "make runnerd-image failed"; }
docker compose -f deploy/compose/docker-compose.yaml --profile build-only build proxy-image >>"$WORK/build.log" 2>&1 \
  || { tail -40 "$WORK/build.log"; die "proxy image build failed"; }
docker tag wardyn/wardyn-runnerd:local "$RUNNERD_IMAGE"
docker tag wardyn/wardyn-proxy:local "$PROXY_IMAGE"
docker build -q -f deploy/compose/Dockerfile.wardynd -t "$WARDYND_IMAGE" . >>"$WORK/build.log" 2>&1 \
  || { tail -40 "$WORK/build.log"; die "wardynd image build failed"; }
# The agent and postgres are rebuilt as single-platform local images: kind load cannot import a
# pulled multi-platform index from Docker Desktop's containerd store (content digest not found).
mkdir -p "$WORK/agent"
CGO_ENABLED=0 go build -o "$WORK/agent/wardyn-rec" ./cmd/wardyn-rec || die "building wardyn-rec failed"
CGO_ENABLED=0 go build -o "$WORK/agent/agentprobe" ./test/k6/agentprobe || die "building agentprobe failed"
# The org execs a run's task as `agent-run TASK`; the harness's agent-run runs TASK as a shell line,
# so a case chooses what its agent does (a long sleep keeps the run live).
# shellcheck disable=SC2016  # $1 is the shim's own argument, expanded when it runs
printf '#!/bin/sh\n[ "$1" = --idle ] && exec sleep 2147483647\nexec sh -c "$1"\n' >"$WORK/agent/agent-run"
chmod 0755 "$WORK/agent/agent-run"
printf 'FROM busybox:1.36\nCOPY wardyn-rec agentprobe agent-run /usr/local/bin/\n' >"$WORK/agent/Dockerfile"
docker build -q -t "$AGENT_IMAGE" "$WORK/agent" >>"$WORK/build.log" 2>&1 \
  || { tail -20 "$WORK/build.log"; die "agent image build failed"; }
printf 'FROM postgres:17\n' | docker build -q -t "$PG_IMAGE" - >>"$WORK/build.log" 2>&1 \
  || { tail -20 "$WORK/build.log"; die "postgres image build failed"; }

docker pull -q "$RUNNER_IMAGE" >/dev/null
CGO_ENABLED=0 go test -c -tags k8s -o "$WORK/k6.test" ./test/k6/ || die "building test/k6 failed"

log "organisation TLS ($ORG_HOST)"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=k6 org CA" \
  -keyout "$WORK/ca.key" -out "$WORK/ca.pem" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj "/CN=$ORG_HOST" -keyout "$WORK/tls.key" -out "$WORK/tls.csr" >/dev/null 2>&1
printf 'subjectAltName=DNS:%s\n' "$ORG_HOST" >"$WORK/san.ext"
openssl x509 -req -in "$WORK/tls.csr" -CA "$WORK/ca.pem" -CAkey "$WORK/ca.key" -CAcreateserial -days 1 \
  -extfile "$WORK/san.ext" -out "$WORK/tls.crt" >/dev/null 2>&1
ADMIN_TOKEN="$(openssl rand -hex 24)"
SEC_TOKEN="wdn_$(openssl rand -hex 32)"
AGE_KEY="$(docker run --rm "$WARDYND_IMAGE" -gen-age-key 2>/dev/null)"
[[ "$AGE_KEY" == AGE-SECRET-KEY-* ]] || die "wardynd -gen-age-key printed no key"

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

# On the control plane: the organisation, postgres and runnerd stay up while a case stops a worker's
# kubelet or restarts a worker node.
CP_PLACE='nodeSelector: {node-role.kubernetes.io/control-plane: ""}
tolerations: [{key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}]'

cat >"$WORK/org-values.yaml" <<EOF
fullnameOverride: $ORG_FULL
serviceAccount: {automount: true}
image: {repository: ${WARDYND_IMAGE%:*}, tag: $TAG, pullPolicy: IfNotPresent}
auth: {adminToken: {value: $ADMIN_TOKEN}}
secrets: {ageKeyFromSecret: true}
service: {type: NodePort}
$CP_PLACE
k8s: {enabled: true, proxyImage: $PROXY_IMAGE, runsNamespace: $ORG_RUNS_NS}
networkPolicy:
  ingress:
    from:
      - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: $OWN_NS}}
      - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: $P_OWN_NS}}
      - ipBlock: {cidr: NODE_CIDR}
env:
  WARDYN_RUNNER_ORG_URL: "$ORG_URL"
  WARDYN_TLS_CERT: /etc/wardyn/org-tls/tls.crt
  WARDYN_TLS_KEY: /etc/wardyn/org-tls/tls.key
  WARDYN_AGENT_IMAGES: '{"claude-code":"$AGENT_IMAGE"}'
extraVolumes: [{name: org-tls, secret: {secretName: org-tls}}]
extraVolumeMounts: [{name: org-tls, mountPath: /etc/wardyn/org-tls, readOnly: true}]
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
  maxRuns: 3
  totalCPU: 6
  totalMemory: 12
  totalDisk: 30
relay:
  servicePort: 8443
# No apiServer.peers: the chart reads the apiserver's address into every proxy's hard deny itself, and
# hard-deny-from-agent proves it.
$CP_PLACE
k8s:
  sandbox:
    nodeSelector: {$SCHED_LABEL: "on"}
egress:
  hardDenyCIDRs: [10.96.0.0/12, 192.168.0.0/16]
  extra:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: $ORG_NS
      ports:
        - protocol: TCP
          port: 8080
EOF

SUMMARY="$WORK/summary.tsv"
: >"$SUMMARY"
rc=0
case_line() { printf '%s\t%s\t%s\t%s\n' "$VERSION" "$1" "$2" "$3" >>"$SUMMARY"; }

# api TOKEN METHOD PATH [BODY]: the org's answer lands in $WORK/resp.json; the status is printed.
# The call runs on the control-plane node: the kind network need not be routable from this host.
api() {
  local args=(-sS --max-time 20 --cacert /k6-org-ca.pem --resolve "$ORG_HOST:$ORG_PORT:$CP_IP"
    -o /k6-resp.json -w '%{http_code}' -X "$2" "https://$ORG_HOST:$ORG_PORT$3" -H "Authorization: Bearer $1")
  [ -n "${4:-}" ] && args+=(-H "Content-Type: application/json" -d "$4")
  docker exec "$CLUSTER-control-plane" curl "${args[@]}" || echo 000
  docker exec "$CLUSTER-control-plane" cat /k6-resp.json >"$WORK/resp.json" 2>/dev/null || : >"$WORK/resp.json"
}

# patch_https_probes DEPLOYMENT NS: the org serves its runner URL over TLS on the probed port.
patch_https_probes() {
  kubectl -n "$2" get deployment "$1" -o json | jq '.spec.template.spec.containers[0] |=
    ((.livenessProbe.httpGet.scheme, .readinessProbe.httpGet.scheme, .startupProbe.httpGet.scheme) = "HTTPS")' |
    kubectl replace -f - >/dev/null
}

# deploy_org stands up postgres and wardynd in $ORG_NS and seeds the security operator's token.
deploy_org() {
  local node_cidr
  node_cidr="$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -v ':' | grep -v '^$' | head -1)"
  kubectl create namespace "$ORG_NS" >/dev/null
  kubectl create namespace "$ORG_RUNS_NS" >/dev/null
  kubectl -n "$ORG_NS" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: postgres}
spec:
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      ${CP_PLACE//$'\n'/$'\n'      }
      containers:
        - name: postgres
          image: $PG_IMAGE
          imagePullPolicy: IfNotPresent
          env:
            - {name: POSTGRES_USER, value: wardyn}
            - {name: POSTGRES_PASSWORD, value: wardyn}
            - {name: POSTGRES_DB, value: wardyn}
          ports: [{containerPort: 5432}]
---
apiVersion: v1
kind: Service
metadata: {name: postgres}
spec:
  selector: {app: postgres}
  ports: [{port: 5432}]
EOF
  kubectl -n "$ORG_NS" create secret generic wardyn-postgres-dsn \
    --from-literal=dsn="postgres://wardyn:wardyn@postgres:5432/wardyn?sslmode=disable" --from-literal=age-key="$AGE_KEY" >/dev/null
  kubectl -n "$ORG_NS" create secret tls org-tls --cert="$WORK/tls.crt" --key="$WORK/tls.key" >/dev/null
  kubectl -n "$ORG_NS" rollout status deployment/postgres --timeout=180s >/dev/null
  sed "s#NODE_CIDR#$node_cidr#" "$WORK/org-values.yaml" >"$WORK/org-values-$VERSION.yaml"
  helm install "$ORG_RELEASE" deploy/helm/wardyn -n "$ORG_NS" -f "$WORK/org-values-$VERSION.yaml" >/dev/null
  patch_https_probes "$ORG_FULL" "$ORG_NS"
  if ! kubectl -n "$ORG_NS" rollout status "deployment/$ORG_FULL" --timeout=300s >/dev/null; then
    kubectl -n "$ORG_NS" describe pods -l app.kubernetes.io/name=wardyn | tail -30 || true
    kubectl -n "$ORG_NS" logs "deployment/$ORG_FULL" --tail=60 || true
    return 1
  fi
  ORG_PORT="$(kubectl -n "$ORG_NS" get svc "$ORG_FULL" -o jsonpath='{.spec.ports[?(@.name=="http")].nodePort}')"
  # The security operator is a person with an api token, which the org's runner doors read as a
  # signed-in human (the same seeding scripts/runner-laptop-walk.sh does for its member).
  kubectl -n "$ORG_NS" exec deploy/postgres -- psql -U wardyn -d wardyn -qtAc "INSERT INTO api_tokens
    (id, principal, email, role, user_type, groups, groups_truncated, name, token_sha256) VALUES (gen_random_uuid(),
    'k6-security', 'k6-security@k6.wardyn.invalid', 'admin', 'standard', '[]', false, 'k6 security operator',
    '$(printf '%s' "$SEC_TOKEN" | sha256sum | cut -d' ' -f1)')" >/dev/null
}

# install_runnerd RELEASE OWN_NS RUNS_NS TOKEN installs the runnerd chart registering with TOKEN, waits
# for its boot checks, and leaves the deployment's name in INSTALLED; it fails when runnerd never boots.
install_runnerd() {
  local release=$1 own=$2 runs=$3 token=$4
  kubectl create namespace "$own" >/dev/null
  kubectl create namespace "$runs" >/dev/null
  kubectl label namespace "$runs" pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/enforce-version=latest >/dev/null
  kubectl -n "$own" create configmap k6-org-ca --from-file=ca.pem="$WORK/ca.pem" >/dev/null
  kubectl -n "$own" create secret generic k6-registration --from-literal=token="$token" >/dev/null
  sed -e "s/arch: ARCH/arch: $ARCH/" -e "s/^runsNamespace: .*/runsNamespace: $runs/" \
    "$WORK/values.yaml" >"$WORK/values-$release-$VERSION.yaml"
  helm install "$release" deploy/helm/wardyn-runnerd -n "$own" -f "$WORK/values-$release-$VERSION.yaml" >/dev/null
  INSTALLED="$(kubectl -n "$own" get deploy -l "app.kubernetes.io/instance=$release" -o jsonpath='{.items[0].metadata.name}')"
  if kubectl -n "$own" rollout status "deployment/$INSTALLED" --timeout=300s >/dev/null &&
     timeout 300 bash -c "until kubectl -n $own logs deployment/$INSTALLED -c runnerd 2>/dev/null | grep 'k8s runner ready to connect' >/dev/null; do sleep 3; done"; then
    return 0
  fi
  kubectl -n "$own" describe pods -l "app.kubernetes.io/instance=$release" | tail -40 || true
  kubectl -n "$own" logs "deployment/$INSTALLED" --all-containers --tail=60 || true
  return 1
}

# fingerprint OWN_NS DEPLOYMENT is the key fingerprint runnerd printed at registration.
fingerprint() {
  kubectl -n "$1" logs "deployment/$2" -c register 2>/dev/null | sed -n 's/.*key fingerprint \([^ ]*\).*/\1/p' | head -1
}

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
  # Cluster DNS on the control plane too: a case that stops a worker's kubelet must not take away the
  # organisation's only way to resolve postgres.
  kubectl -n kube-system patch deployment coredns --type merge -p \
    '{"spec":{"template":{"spec":{"nodeSelector":{"node-role.kubernetes.io/control-plane":""}}}}}' >/dev/null
  kubectl -n kube-system rollout status deployment/coredns --timeout=180s >/dev/null
  for node in $(kind get nodes --name "$CLUSTER"); do
    for img in "$RUNNERD_IMAGE" "$PROXY_IMAGE" "$AGENT_IMAGE"; do
      load_image "$img" "$node"
    done
  done
  for img in "$WARDYND_IMAGE" "$PG_IMAGE"; do
    load_image "$img" "$CLUSTER-control-plane"
  done
  ARCH="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')"
  CP_IP="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "$CLUSTER-control-plane")"
  docker cp "$WORK/ca.pem" "$CLUSTER-control-plane:/k6-org-ca.pem" >/dev/null
  PIN="$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | sort | head -1)"
  kubectl label node "$PIN" "$SCHED_LABEL=on" --overwrite >/dev/null

  log "[$VERSION] the organisation (wardynd and postgres:17)"
  if ! deploy_org; then
    case_line organisation FAIL "the organisation did not come up (see its pod state and log above)"
    rc=1
    if [ "${K6_KEEP:-}" = 1 ]; then break; fi
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
    CREATED=""
    continue
  fi
  code="$(api "$ADMIN_TOKEN" PUT /api/v1/runners/settings '{"enabled":true}')"
  [ "$code" = 200 ] || die "turning runners on answered $code: $(cat "$WORK/resp.json")"
  code="$(api "$SEC_TOKEN" POST /api/v1/runners/org-tokens '{"name":"k6 kind conformance"}')"
  [ "$code" = 201 ] || die "minting the org runner token answered $code: $(cat "$WORK/resp.json")"

  log "[$VERSION] the runnerd chart, registering with the org's token"
  boot=PASS
  install_runnerd "$RELEASE" "$OWN_NS" "$RUNS_NS" "$(jq -r .token "$WORK/resp.json")" || { boot=FAIL; rc=1; }
  DEPLOY="$INSTALLED"
  case_line in-cluster-runnerd-boot "$boot" "chart Role; SSAR, foreign objects, kubelet floor, relay listen and the relay source probe on Calico (log: k8s runner ready to connect)"

  # The claim: the security operator confirms the fingerprint runnerd printed at registration.
  FP="$(fingerprint "$OWN_NS" "$DEPLOY")"
  api "$SEC_TOKEN" GET /api/v1/runners >/dev/null
  RUNNER_ID="$(jq -r '[.[] | select(.owner_kind == "org")][0].id // empty' "$WORK/resp.json")"
  hub=FAIL
  detail="runnerd printed fingerprint '${FP}', the org lists org runner '${RUNNER_ID}'"
  if [ -n "$FP" ] && [ -n "$RUNNER_ID" ]; then
    code="$(api "$SEC_TOKEN" POST "/api/v1/runners/$RUNNER_ID/claim-org" "{\"fingerprint\":\"$FP\"}")"
    detail="claim-org answered $code $(jq -c '{state, reason, error}' "$WORK/resp.json" 2>/dev/null)"
    if [ "$code" = 200 ]; then
      for _ in $(seq 1 60); do
        api "$SEC_TOKEN" GET "/api/v1/runners/$RUNNER_ID" >/dev/null
        if jq -e '.online == true' "$WORK/resp.json" >/dev/null; then hub=PASS; break; fi
        sleep 3
      done
      detail="minted by the org, registered by the chart, claimed by fingerprint; $(jq -c '{state, online, trust_class, owner_kind}' "$WORK/resp.json")"
    fi
  fi
  [ "$hub" = PASS ] || { rc=1; kubectl -n "$OWN_NS" logs "deployment/$DEPLOY" -c runnerd --tail=30 || true; }
  case_line org-hub-session "$hub" "$detail"

  # A person's own runner (trust class personal) for the walk's refusal: the same security operator,
  # signed in as themself, self-mints a token, registers a second chart release with it, claims it by
  # fingerprint and puts it in a pool of their own.
  PERSONAL_POOL=""
  P_DEPLOY=""
  if [ "$hub" = PASS ]; then
    log "[$VERSION] a personal runner (second chart release, self-minted token)"
    api "$ADMIN_TOKEN" POST /api/v1/permissions/grants \
      '{"subject_type":"all","subject":"","capability":"feature","value":"self_host_runner","effect":"allow"}' >/dev/null
    code="$(api "$SEC_TOKEN" POST /api/v1/me/runners/tokens '{}')"
    if [ "$code" = 201 ] && install_runnerd "$P_RELEASE" "$P_OWN_NS" "$P_RUNS_NS" "$(jq -r .token "$WORK/resp.json")"; then
      P_DEPLOY="$INSTALLED"
      PFP="$(fingerprint "$P_OWN_NS" "$P_DEPLOY")"
      api "$SEC_TOKEN" GET /api/v1/me/runners >/dev/null
      PRID="$(jq -r '[.[] | select(.state == "unclaimed")][0].id // empty' "$WORK/resp.json")"
      api "$SEC_TOKEN" POST "/api/v1/me/runners/$PRID/claim" "{\"fingerprint\":\"$PFP\"}" >/dev/null
      api "$SEC_TOKEN" POST /api/v1/me/runner-pools '{"name":"k6-personal"}' >/dev/null
      PERSONAL_POOL="$(jq -r '.id // empty' "$WORK/resp.json")"
      api "$SEC_TOKEN" PUT "/api/v1/me/runner-pools/$PERSONAL_POOL/runners/$PRID" >/dev/null
    else
      echo "personal runner: token mint answered $code" >&2
    fi
  fi

  SA="$(kubectl -n "$OWN_NS" get deployment "$DEPLOY" -o jsonpath='{.spec.template.spec.serviceAccountName}')"
  SELECTOR="$(kubectl -n "$OWN_NS" get deployment "$DEPLOY" -o jsonpath='{.spec.selector.matchLabels}')"
  RELAY_IP="$(kubectl -n "$OWN_NS" get svc -l app.kubernetes.io/instance=$RELEASE -o jsonpath='{.items[0].spec.clusterIP}')"
  TOKEN="$(kubectl -n "$OWN_NS" create token "$SA" --duration=4h)"
  # The canary's apiserver probe dials the kubeconfig's address from a pod, so the suite uses the
  # control plane's address on the kind network and runs in a container attached to it.
  SERVER="https://$CP_IP:6443"
  kind get kubeconfig --name "$CLUSTER" --internal | sed "s#https://$CLUSTER-control-plane:6443#$SERVER#" >"$WORK/admin-internal.kc"
  CADATA="$(kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
  cat >"$WORK/runnerd.kc" <<EOF
apiVersion: v1
kind: Config
clusters: [{name: k6, cluster: {server: "$SERVER", certificate-authority-data: "$CADATA"}}]
users: [{name: runnerd, user: {token: "$TOKEN"}}]
contexts: [{name: runnerd, context: {cluster: k6, user: runnerd}}]
current-context: runnerd
EOF
  cat >"$WORK/env-$VERSION.list" <<EOF
WARDYN_TEST_K6=1
KUBECONFIG=$WORK/runnerd.kc
K6_ADMIN_KUBECONFIG=$WORK/admin-internal.kc
K6_RUNS_NS=$RUNS_NS
K6_OWN_NS=$OWN_NS
K6_SA=$SA
K6_RUNNER_ID=$RUNNER_ID
K6_RELAY_IP=$RELAY_IP
K6_ORG_URL=$ORG_URL
K6_ORG_CA=$WORK/ca.pem
K6_RUNNERD_SELECTOR=$SELECTOR
K6_RUNNERD_DEPLOY=$DEPLOY
K6_ARCH=$ARCH
K6_AGENT_IMAGE=$AGENT_IMAGE
WARDYN_PROXY_IMAGE=$PROXY_IMAGE
K6_ORG_ADDR=$CP_IP:$ORG_PORT
K6_ORG_ADMIN_TOKEN=$ADMIN_TOKEN
K6_ORG_SEC_TOKEN=$SEC_TOKEN
K6_ORG_NS=$ORG_NS
K6_SCHED_NODE=$PIN
K6_APISERVER=$CP_IP:6443
K6_PERSONAL_POOL=$PERSONAL_POOL
K6_AGENTPROBE=$WORK/agent/agentprobe
EOF
  # suite NAME RUN: one test/k6 run in a container on the kind network; its K6-CASE lines join the table.
  suite() {
    set +e
    docker run --rm --network kind -v /var/run/docker.sock:/var/run/docker.sock -v "$WORK:$WORK" \
      -v "$(command -v kubectl):/usr/local/bin/kubectl:ro" --env-file "$WORK/env-$VERSION.list" \
      "$RUNNER_IMAGE" "$WORK/k6.test" -test.count=1 -test.v -test.timeout=90m -test.run "$2" >"$WORK/test-$1-$VERSION.log" 2>&1
    local trc=$?
    set -e
    grep -E '^\s*--- (FAIL|PASS|SKIP)|^(FAIL|ok) |panic:' "$WORK/test-$1-$VERSION.log" | tail -60 || true
    grep '^K6-CASE' "$WORK/test-$1-$VERSION.log" | sed "s/^K6-CASE/$VERSION/" >>"$SUMMARY" || true
    if [ "$trc" != 0 ]; then
      rc=1
      # go test -v prints a failed case's messages above its --- FAIL line, as file:line.
      grep -E '_test\.go:[0-9]+:|panic:' "$WORK/test-$1-$VERSION.log" | tail -120 || true
    fi
    if [ -n "${K6_KEEP_LOGS:-}" ]; then cp "$WORK/test-$1-$VERSION.log" "$K6_KEEP_LOGS/" || true; fi
  }
  if [ "$hub" = PASS ]; then
    log "[$VERSION] org cases (TestK6Org, against the org and the live runnerd)"
    suite org "${K6_ORG_RUN:-^TestK6Org\$}"
  fi

  # runnerd stops here: TestK6 drives the same runner identity in process under its token.
  kubectl -n "$OWN_NS" scale "deployment/$DEPLOY" --replicas=0 >/dev/null
  [ -n "${P_DEPLOY:-}" ] && kubectl -n "$P_OWN_NS" scale "deployment/$P_DEPLOY" --replicas=0 >/dev/null
  kubectl -n "$OWN_NS" wait --for=delete pod -l "app.kubernetes.io/instance=$RELEASE" --timeout=120s >/dev/null 2>&1 || true
  # A 16-address Calico pool for the runs namespace, applied after runnerd's boot checks so the
  # relay probe runs on the default pool. Calico reuses a freed address only after the
  # rest of its block, so pod-ip-reuse-after-stop needs a small block to see a stopped proxy's IP again.
  kubectl apply -f - >/dev/null <<'POOL'
apiVersion: crd.projectcalico.org/v1
kind: IPPool
metadata: {name: k6-runs}
spec: {cidr: 10.250.0.0/28, blockSize: 28, ipipMode: Always, natOutgoing: true, nodeSelector: all()}
POOL
  kubectl annotate namespace "$RUNS_NS" 'cni.projectcalico.org/ipv4pools=["k6-runs"]' >/dev/null
  if [ "${K6_SKIP_INPROC:-}" != 1 ]; then
    log "[$VERSION] in-process cases (TestK6, under runnerd's token)"
    suite inproc '^TestK6$'
  fi

  if [ "${K6_KEEP:-}" = 1 ]; then break; fi
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  CREATED=""
done

kill "$SAMPLER" 2>/dev/null
SAMPLER=""
base="$(cat "$WORK/mem.base")"
peak="$(sort -n "$WORK/mem.log" | tail -1)"
kpeak="$(sort -n "$WORK/kind-mem.log" 2>/dev/null | tail -1)"
echo
printf '%-10s %-40s %-16s %s\n' VERSION CASE STATUS DETAIL
awk -F'\t' '{printf "%-10s %-40s %-16s %s\n", $1, $2, $3, $4}' "$SUMMARY"
echo
echo "commit $(git rev-parse --short HEAD); wall $((SECONDS - START)) s; host memory in use ${base} MiB at start, peak ${peak} MiB; kind node containers peak ${kpeak:-?} MiB"
# IN-PROCESS: a case no live path can reach in this release, pinned by a named in-process test instead.
if [ "$rc" = 0 ] && ! awk -F'\t' '$3!="PASS" && $3!="IN-PROCESS"{f=1} END{exit !f}' "$SUMMARY"; then
  echo "k6-kind-conformance: PASS"
else
  echo "k6-kind-conformance: FAIL"
  rc=1
fi
exit "$rc"
