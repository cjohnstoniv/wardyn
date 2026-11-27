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
#     fingerprint, then runnerd dialling the org's hub outbound; it serves two runs namespaces;
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
# up and prints how to remove them; K6_SKIP_INPROC=1 skips TestK6. K6_SUITES (default "org inproc")
# picks which suites run: the nightly runs each kubelet as two jobs, one per suite, so a job fits in 30
# minutes. The cluster and organisation are stood up the same way whichever suites run. K6_SUITES=workload
# runs TestK6Workload (#3804) against the same organisation, with a throwaway in-cluster Dex for its
# people's sign-ins: only that suite deploys Dex and turns OIDC on, so the org and inproc legs are unchanged.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/common.sh disable=SC1091  # followed under shellcheck -x
source "$ROOT/scripts/lib/common.sh"
# shellcheck source=scripts/lib/image-build.sh disable=SC1091
source "$ROOT/scripts/lib/image-build.sh"
# shellcheck source=scripts/lib/kind-org.sh disable=SC1091
source "$ROOT/scripts/lib/kind-org.sh"
cd "$ROOT"

[ "${WARDYN_TEST_K8S:-}" = "1" ] || skip_lane "k6-kind-conformance: WARDYN_TEST_K8S=1 not set; skipping"
for tool in docker kind kubectl helm go openssl jq curl; do
  command -v "$tool" >/dev/null || die "k6-kind-conformance needs $tool"
done

NODE_IMAGES="${K6_NODE_IMAGES:-kindest/node:v1.31.4 kindest/node:v1.35.0}"
SUITES="${K6_SUITES:-org inproc}"
for s in $SUITES; do
  case "$s" in org | inproc | workload) ;; *) die "K6_SUITES names '$s', want org, inproc and/or workload" ;; esac
done
want() { case " $SUITES " in *" $1 "*) return 0 ;; esac; return 1; }
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
EXTRA_RUNS_NS=wardyn-k6-runs-b
RELEASE=k6
P_RELEASE=k6p
P_OWN_NS=wardyn-k6-prunner
P_RUNS_NS=wardyn-k6-pruns
ORG_RELEASE=k6org
ORG_FULL=wardyn-org
ORG_HOST="$ORG_FULL.$ORG_NS.svc.cluster.local"
ORG_URL="https://$ORG_HOST:8080"
SCHED_LABEL="wardyn-k6/sched"
# The workload suite: the namespace it binds, one nothing binds, its Dex and the two people who sign in there.
WL_NS=wardyn-k6-wl
WL_FREE_NS=wardyn-k6-wl-free
DEX_HOST="k6-dex.$ORG_NS.svc.cluster.local"
WL_SECOP_EMAIL=k6-secop@k6.wardyn.invalid
WL_SPONSOR_EMAIL=k6-sponsor@k6.wardyn.invalid
DEX_PORT=""
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
build_org_images

docker pull -q "$RUNNER_IMAGE" >/dev/null
CGO_ENABLED=0 go test -c -tags k8s -o "$WORK/k6.test" ./test/k6/ || die "building test/k6 failed"

mint_org_tls
if want workload; then
  # Dex serves its issuer over TLS from the same CA: wardynd refuses a plain http:// issuer while it
  # serves TLS itself. The OIDC client secret is minted per run and reaches Dex and wardynd only through
  # a Secret; it is never printed.
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$DEX_HOST" -keyout "$WORK/dex.key" -out "$WORK/dex.csr" >/dev/null 2>&1
  printf 'subjectAltName=DNS:%s\n' "$DEX_HOST" >"$WORK/dex-san.ext"
  openssl x509 -req -in "$WORK/dex.csr" -CA "$WORK/ca.pem" -CAkey "$WORK/ca.key" -CAcreateserial -days 1 \
    -extfile "$WORK/dex-san.ext" -out "$WORK/dex.crt" >/dev/null 2>&1
  OIDC_SECRET="$(openssl rand -hex 24)"
fi
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

# The workload suite's people sign in through Dex (a binding is made by a signed-in person, never an API
# token), so the org gets OIDC on top of its admin token, trusting the CA above for Dex's TLS. Dex reuses
# deploy/kind/sso/dex.yaml's image and public demo password hash, read from it rather than retyped.
ORG_OIDC=()
if want workload; then
  DEX_IMAGE="$(sed -n 's/^ *image: \(ghcr\.io\/dexidp\/dex:[^ ]*\)$/\1/p' deploy/kind/sso/dex.yaml | head -1)"
  DEX_HASH="$(sed -n 's/^ *hash: "\(.*\)"$/\1/p' deploy/kind/sso/dex.yaml | head -1)"
  [ -n "$DEX_IMAGE" ] && [ -n "$DEX_HASH" ] || die "could not read Dex's image and demo hash from deploy/kind/sso/dex.yaml"
  cat >"$WORK/org-oidc.yaml" <<EOF
env:
  WARDYN_OIDC_ISSUER: "https://$DEX_HOST:5554"
  WARDYN_OIDC_CLIENT_ID: "wardyn-k6"
  WARDYN_OIDC_REDIRECT_URL: "$ORG_URL/auth/callback"
  WARDYN_OIDC_EMAIL_DOMAINS: "k6.wardyn.invalid"
  WARDYN_OIDC_ROLE_MAP: "$WL_SECOP_EMAIL=security_admin,$WL_SPONSOR_EMAIL=user"
  WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS: "true"
extraEnv:
  - name: WARDYN_OIDC_CLIENT_SECRET
    valueFrom: {secretKeyRef: {name: k6-oidc, key: client-secret}}
networkPolicy:
  egress:
    extra:
      - to: [{podSelector: {matchLabels: {app: k6-dex}}}]
        ports: [{protocol: TCP, port: 5554}]
EOF
  ORG_OIDC=(-f "$WORK/org-oidc.yaml" --set-file "trustedCA=$WORK/ca.pem")
  cat >"$WORK/dex.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {name: k6-dex}
data:
  config.yaml: |
    issuer: https://$DEX_HOST:5554
    storage: {type: memory}
    web:
      https: 0.0.0.0:5554
      tlsCert: /etc/dex/tls/tls.crt
      tlsKey: /etc/dex/tls/tls.key
    oauth2: {skipApprovalScreen: true}
    enablePasswordDB: true
    staticClients:
      - id: wardyn-k6
        name: k6
        secretEnv: K6_OIDC_CLIENT_SECRET
        redirectURIs: ["$ORG_URL/auth/callback"]
    staticPasswords:
      - {email: "$WL_SECOP_EMAIL", hash: "$DEX_HASH", username: k6-secop, userID: k6-secop-0001}
      - {email: "$WL_SPONSOR_EMAIL", hash: "$DEX_HASH", username: k6-sponsor, userID: k6-sponsor-0001}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: k6-dex, labels: {app: k6-dex}}
spec:
  selector: {matchLabels: {app: k6-dex}}
  template:
    metadata: {labels: {app: k6-dex}}
    spec:
      ${CP_PLACE//$'\n'/$'\n'      }
      containers:
        - name: dex
          image: $DEX_IMAGE
          imagePullPolicy: IfNotPresent
          command: ["dex", "serve", "/etc/dex/cfg/config.yaml"]
          ports: [{containerPort: 5554}]
          env:
            - name: K6_OIDC_CLIENT_SECRET
              valueFrom: {secretKeyRef: {name: k6-oidc, key: client-secret}}
          readinessProbe: {httpGet: {path: /healthz, port: 5554, scheme: HTTPS}, periodSeconds: 2}
          volumeMounts:
            - {name: cfg, mountPath: /etc/dex/cfg, readOnly: true}
            - {name: tls, mountPath: /etc/dex/tls, readOnly: true}
      volumes:
        - {name: cfg, configMap: {name: k6-dex}}
        - {name: tls, secret: {secretName: k6-dex-tls}}
---
apiVersion: v1
kind: Service
metadata: {name: k6-dex}
spec:
  type: NodePort
  selector: {app: k6-dex}
  ports: [{port: 5554, targetPort: 5554}]
EOF
fi

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


# deploy_dex stands up the workload suite's Dex in $ORG_NS and waits until it serves, so wardynd's OIDC
# discovery at boot meets a ready issuer; it leaves Dex's node port in DEX_PORT. The control plane pulls
# the image (pinned by digest in deploy/kind/sso/dex.yaml) from ghcr, as the kind-sso overlay's Dex does.
deploy_dex() {
  kubectl -n "$ORG_NS" create secret tls k6-dex-tls --cert="$WORK/dex.crt" --key="$WORK/dex.key" >/dev/null
  kubectl -n "$ORG_NS" create secret generic k6-oidc --from-file=client-secret=<(printf %s "$OIDC_SECRET") >/dev/null
  kubectl -n "$ORG_NS" apply -f "$WORK/dex.yaml" >/dev/null
  if ! kubectl -n "$ORG_NS" rollout status deployment/k6-dex --timeout=180s >/dev/null; then
    kubectl -n "$ORG_NS" describe pods -l app=k6-dex | tail -30 || true
    kubectl -n "$ORG_NS" logs deployment/k6-dex --tail=40 || true
    return 1
  fi
  DEX_PORT="$(kubectl -n "$ORG_NS" get svc k6-dex -o jsonpath='{.spec.ports[0].nodePort}')"
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
  prepare_org_cluster

  log "[$VERSION] the organisation (wardynd and postgres:17)"
  if ! deploy_org; then
    case_line organisation FAIL "the organisation did not come up (see its pod state and log above)"
    rc=1
    if [ "${K6_KEEP:-}" = 1 ]; then break; fi
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
    CREATED=""
    continue
  fi
  org_runner_token

  log "[$VERSION] the runnerd chart, registering with the org's token"
  boot=PASS
  install_runnerd "$RELEASE" "$OWN_NS" "$RUNS_NS" "$(jq -r .token "$WORK/resp.json")" "$EXTRA_RUNS_NS" || { boot=FAIL; rc=1; }
  DEPLOY="$INSTALLED"
  case_line in-cluster-runnerd-boot "$boot" "chart Role in two runs namespaces; SSAR, foreign objects, the canary in each, kubelet floor, relay listen and the relay source probe on Calico (log: k8s runner ready to connect)"

  claim_org_runner
  [ "$hub" = PASS ] || { rc=1; kubectl -n "$OWN_NS" logs "deployment/$DEPLOY" -c runnerd --tail=30 || true; }
  case_line org-hub-session "$hub" "$detail"

  # #4065: under the charts' own Roles, both binaries' pod waits ring on a pod watch. The Info line is
  # logged at boot for each runs namespace; the Forbidden fallback's Warn line must never appear.
  if [ "$hub" = PASS ]; then
    pw=PASS
    org_log="$(kubectl -n "$ORG_NS" logs "deployment/$ORG_FULL" --all-containers 2>/dev/null || true)"
    rd_log="$(kubectl -n "$OWN_NS" logs "deployment/$DEPLOY" -c runnerd 2>/dev/null || true)"
    for l in "$org_log" "$rd_log"; do
      grep -q 'pod waits ring on a pod watch' <<<"$l" || pw=FAIL
      if grep -q 'grants no watch on pods' <<<"$l"; then pw=FAIL; fi
    done
    [ "$pw" = PASS ] || rc=1
    case_line pod-watch-under-chart-rbac "$pw" "wardynd and runnerd log 'pod waits ring on a pod watch' and never 'grants no watch on pods' (#4065)"
  fi

  # A person's own runner (trust class personal) for the walk's refusal: the same security operator,
  # signed in as themself, self-mints a token, registers a second chart release with it, claims it by
  # fingerprint and puts it in a pool of their own.
  PERSONAL_POOL=""
  P_DEPLOY=""
  if [ "$hub" = PASS ] && want org; then
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
K6_EXTRA_RUNS_NS=$EXTRA_RUNS_NS
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
K6_DEX_HOST=$DEX_HOST
K6_DEX_ADDR=$CP_IP:$DEX_PORT
K6_WL_NS=$WL_NS
K6_WL_FREE_NS=$WL_FREE_NS
K6_WL_SECOP_EMAIL=$WL_SECOP_EMAIL
K6_WL_SPONSOR_EMAIL=$WL_SPONSOR_EMAIL
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
    if [ "$trc" != 0 ]; then rc=1; fi
    # A case that ends INCONCLUSIVE skips, so its suite still exits 0: its messages print as a failed
    # suite's do, whenever a case's row is one the verdict below refuses.
    if [ "$trc" != 0 ] || awk -F'\t' '/^K6-CASE/ && $3 != "PASS" && $3 != "IN-PROCESS" { f = 1 } END { exit !f }' "$WORK/test-$1-$VERSION.log"; then
      # go test -v prints a failed case's messages above its --- FAIL line, each as file:line, and a
      # message's later lines (every failure dump section's body) indented 8 spaces under it, with no
      # file:line: keep them with their message, or each dump section prints as its header alone. No
      # tail: the dump bounds each of its sources, and a tail would cut a case's failure line, which
      # comes before its dump.
      awk '/_test\.go:[0-9]+:|panic:/ { keep = 1; print; next } keep && /^        / { print; next } { keep = 0 }' \
        "$WORK/test-$1-$VERSION.log" || true
    fi
    if [ -n "${K6_KEEP_LOGS:-}" ]; then cp "$WORK/test-$1-$VERSION.log" "$K6_KEEP_LOGS/" || true; fi
  }
  if want org && [ "$hub" = PASS ]; then
    log "[$VERSION] org cases (TestK6Org, against the org and the live runnerd)"
    suite org "${K6_ORG_RUN:-^TestK6Org\$}"
  fi
  if want workload && [ "$hub" = PASS ]; then
    log "[$VERSION] workload case (TestK6Workload, Dex sign-ins against the org and the live runnerd)"
    suite workload '^TestK6Workload$'
    # Run pods go to the runs namespaces; these two hold only the case's resources.
    kubectl delete namespace "$WL_NS" "$WL_FREE_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi

  # runnerd stops here: TestK6 drives the same runner identity in process under its token.
  kubectl -n "$OWN_NS" scale "deployment/$DEPLOY" --replicas=0 >/dev/null
  [ -n "${P_DEPLOY:-}" ] && kubectl -n "$P_OWN_NS" scale "deployment/$P_DEPLOY" --replicas=0 >/dev/null
  kubectl -n "$OWN_NS" wait --for=delete pod -l "app.kubernetes.io/instance=$RELEASE" --timeout=120s >/dev/null 2>&1 || true
  # Both suites on one cluster: the org suite's last runs outlive the runner it revoked (a revoke stops no
  # run), held by runnerd's finalizer on the worker node-name-reuse-tick needs; TestK6 starts without them.
  if want org && want inproc; then
    kubectl -n "$RUNS_NS" get pods -o name | xargs -r -n1 kubectl -n "$RUNS_NS" patch --type merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1 || true
    kubectl -n "$RUNS_NS" delete pods --all --grace-period=0 --force --wait=true >/dev/null 2>&1 || true
  fi
  # A killed inproc run skips the Go cleanups: drop pod-ip-reuse-after-stop's IP reservation so a kept
  # cluster never starts starved (Calico before v3.22 has no such resource; the case names that).
  kubectl delete ipreservations.crd.projectcalico.org k6-only-ip --ignore-not-found >/dev/null 2>&1 || true
  # A 16-address Calico pool for the runs namespace, applied after runnerd's boot checks so the
  # relay probe runs on the default pool. pod-ip-reuse-after-stop reserves every address of it but the
  # one the next proxy must receive (an IPReservation; test/k6 onlyIP), so the reuse is deterministic.
  kubectl apply -f - >/dev/null <<'POOL'
apiVersion: crd.projectcalico.org/v1
kind: IPPool
metadata: {name: k6-runs}
spec: {cidr: 10.250.0.0/28, blockSize: 28, ipipMode: Always, natOutgoing: true, nodeSelector: all()}
POOL
  kubectl annotate namespace "$RUNS_NS" 'cni.projectcalico.org/ipv4pools=["k6-runs"]' >/dev/null
  if want inproc && [ "${K6_SKIP_INPROC:-}" != 1 ]; then
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
