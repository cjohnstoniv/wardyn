# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# scripts/lib/kind-org.sh — stand up an organisation and enrol an organisation runner on a kind cluster of your
# own, the production way: wardynd and postgres from deploy/helm/wardyn serving their runner URL over TLS from a CA
# minted here, an org token minted by the org, the deploy/helm/wardyn-runnerd chart registering with it, and a
# security operator claiming the runner by the fingerprint it printed. Sourced by scripts/k6-kind-conformance.sh
# (where every function here came from, unchanged) and scripts/kind-outage-recover-walk.sh (its runner leg).
# Needs scripts/lib/common.sh (die, log) and scripts/lib/image-build.sh (wardyn_docker_build) sourced first.
#
# The caller sets these before calling (k6-kind-conformance.sh's header shows each one's shape):
#   WORK, CLUSTER, TAG, CALICO_VERSION, VERSION (a label for per-cluster file names)
#   RUNNERD_IMAGE, PROXY_IMAGE, WARDYND_IMAGE, PG_IMAGE, AGENT_IMAGE
#   ORG_NS, ORG_RUNS_NS, ORG_RELEASE, ORG_FULL, ORG_HOST, CP_PLACE, ORG_OIDC (an array; empty for none)
#   RELEASE, OWN_NS, DEPLOY, SCHED_LABEL, ADMIN_TOKEN, SEC_TOKEN, AGE_KEY
#   $WORK/org-values.yaml and $WORK/values.yaml (the two charts' values; NODE_CIDR and ARCH are filled in here)
#   want SUITE, a function: whether the caller runs that suite (return 1 for every suite when it has none)
#   deploy_dex, a function, only when want workload answers 0
# and reads these after: ARCH, CP_IP, PIN and ORG_PORT (set by prepare_org_cluster and deploy_org), INSTALLED
# (install_runnerd), FP, RUNNER_ID, hub (PASS or FAIL) and detail (claim_org_runner), code and $WORK/resp.json
# (the last api call).

# load_image IMAGE NODE imports a local image into a kind node's containerd directly. kind load
# cannot detect the snapshotter of newer node images (containerd 2) on older kind releases.
load_image() {
  docker save "$1" | docker exec -i "$2" ctr --namespace=k8s.io images import --digests - >/dev/null
}

# build_org_images builds the runnerd, proxy and wardynd images from this tree, the agent image (busybox with
# wardyn-rec, agentprobe and an agent-run shim) and a single-platform postgres, all tagged $TAG.
build_org_images() {
  log "building images ($TAG)"
  make runnerd-image >"$WORK/build.log" 2>&1 || { tail -40 "$WORK/build.log"; die "make runnerd-image failed"; }
  docker compose -f deploy/compose/docker-compose.yaml --profile build-only build proxy-image >>"$WORK/build.log" 2>&1 \
    || { tail -40 "$WORK/build.log"; die "proxy image build failed"; }
  docker tag wardyn/wardyn-runnerd:local "$RUNNERD_IMAGE"
  docker tag wardyn/wardyn-proxy:local "$PROXY_IMAGE"
  wardyn_docker_build -f deploy/compose/Dockerfile.wardynd -t "$WARDYND_IMAGE" . || die "wardynd image build failed"
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
  wardyn_docker_build -t "$AGENT_IMAGE" "$WORK/agent" || die "agent image build failed"
  printf 'FROM postgres:17\n' | wardyn_docker_build -t "$PG_IMAGE" - || die "postgres image build failed"
}

# mint_org_tls mints a one-day CA and the organisation's serving certificate for ORG_HOST into $WORK.
mint_org_tls() {
  log "organisation TLS ($ORG_HOST)"
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=k6 org CA" \
    -keyout "$WORK/ca.key" -out "$WORK/ca.pem" >/dev/null 2>&1
  openssl req -newkey rsa:2048 -nodes -subj "/CN=$ORG_HOST" -keyout "$WORK/tls.key" -out "$WORK/tls.csr" >/dev/null 2>&1
  printf 'subjectAltName=DNS:%s\n' "$ORG_HOST" >"$WORK/san.ext"
  openssl x509 -req -in "$WORK/tls.csr" -CA "$WORK/ca.pem" -CAkey "$WORK/ca.key" -CAcreateserial -days 1 \
    -extfile "$WORK/san.ext" -out "$WORK/tls.crt" >/dev/null 2>&1
}

# prepare_org_cluster readies a cluster kind just created with $WORK/admin.kc: Calico, nodes Ready, cluster DNS on
# the control plane, the images loaded, then ARCH, CP_IP, the CA on the control-plane node, and PIN, the first
# worker, labelled SCHED_LABEL=on for the runs.
prepare_org_cluster() {
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
}

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
# The deployment controller can update the object between the get and the replace (a Conflict),
# so the read-modify-replace is retried on a fresh read.
patch_https_probes() {
  local i
  for i in 1 2 3 4 5; do
    kubectl -n "$2" get deployment "$1" -o json | jq '.spec.template.spec.containers[0] |=
      ((.livenessProbe.httpGet.scheme, .readinessProbe.httpGet.scheme, .startupProbe.httpGet.scheme) = "HTTPS")' |
      kubectl replace -f - >/dev/null && return 0
    sleep "$i"
  done
  return 1
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
  if want workload; then deploy_dex || return 1; fi
  sed "s#NODE_CIDR#$node_cidr#" "$WORK/org-values.yaml" >"$WORK/org-values-$VERSION.yaml"
  helm install "$ORG_RELEASE" deploy/helm/wardyn -n "$ORG_NS" -f "$WORK/org-values-$VERSION.yaml" "${ORG_OIDC[@]}" >/dev/null
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

# install_runnerd RELEASE OWN_NS RUNS_NS TOKEN [EXTRA_RUNS_NS] installs the runnerd chart registering with
# TOKEN (placing runs in EXTRA_RUNS_NS too when given), waits for its boot checks, and leaves the
# deployment's name in INSTALLED; it fails when runnerd never boots.
install_runnerd() {
  local release=$1 own=$2 runs=$3 token=$4 extra=${5:-} ns
  local extra_set=()
  kubectl create namespace "$own" >/dev/null
  for ns in "$runs" ${extra:+"$extra"}; do
    kubectl create namespace "$ns" >/dev/null
    kubectl label namespace "$ns" pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/enforce-version=latest >/dev/null
  done
  [ -z "$extra" ] || extra_set=(--set "extraRunsNamespaces[0]=$extra")
  # The workload suite's two namespaces, watched by the org runner only: one the case binds, one never bound.
  if [ "$release" = "$RELEASE" ] && want workload; then
    kubectl create namespace "$WL_NS" >/dev/null
    kubectl create namespace "$WL_FREE_NS" >/dev/null
    extra_set+=(--set "workloads.namespaces={$WL_NS,$WL_FREE_NS}")
  fi
  kubectl -n "$own" create configmap k6-org-ca --from-file=ca.pem="$WORK/ca.pem" >/dev/null
  kubectl -n "$own" create secret generic k6-registration --from-literal=token="$token" >/dev/null
  sed -e "s/arch: ARCH/arch: $ARCH/" -e "s/^runsNamespace: .*/runsNamespace: $runs/" \
    "$WORK/values.yaml" >"$WORK/values-$release-$VERSION.yaml"
  helm install "$release" deploy/helm/wardyn-runnerd -n "$own" -f "$WORK/values-$release-$VERSION.yaml" "${extra_set[@]}" >/dev/null
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

# org_runner_token turns runners on and mints an org runner token; the token is .token in $WORK/resp.json.
org_runner_token() {
  code="$(api "$ADMIN_TOKEN" PUT /api/v1/runners/settings '{"enabled":true}')"
  [ "$code" = 200 ] || die "turning runners on answered $code: $(cat "$WORK/resp.json")"
  code="$(api "$SEC_TOKEN" POST /api/v1/runners/org-tokens '{"name":"k6 kind conformance"}')"
  [ "$code" = 201 ] || die "minting the org runner token answered $code: $(cat "$WORK/resp.json")"
}

# claim_org_runner claims the org runner DEPLOY in OWN_NS registered, by its fingerprint, and waits for it to come
# online: hub is PASS when it did, and detail says what happened either way.
claim_org_runner() {
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
}
