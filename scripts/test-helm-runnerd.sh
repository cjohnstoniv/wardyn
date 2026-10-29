#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-helm-runnerd.sh — lint and render deploy/helm/wardyn-runnerd, assert each refusal by its message, and
# hold its RBAC to the control-plane chart's: the runs-namespace Role is the server's k8s-runner Role plus
# exactly one `pods: patch` rule, and the ClusterRole is the server's with k8s.readNodes on. Also asserts
# that Helm orders every RBAC object before the Deployment, which is what keeps the image from starting
# ahead of a verb it needs. `make helm-lint` runs it. Needs helm and yq (mikefarah v4).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

CHART=deploy/helm/wardyn-runnerd
NS=wardyn-runner
VALUES="$CHART/ci/test-values.yaml"
command -v yq >/dev/null || die "yq not installed: the RBAC parity test cannot run, and a skip proves nothing"

ok() { echo "ok  $1"; }
render() { helm template r "$CHART" -n "$NS" -f "$VALUES" "$@"; }
docs() { yq -o=json -I=0 "$2" <<<"$1"; }
val() { yq -r "$2" <<<"$1" | grep -vx -- "---" || true; }

./scripts/check-helm-templates.sh "$CHART"
helm lint "$CHART" -f "$VALUES" >/dev/null
ok "helm lint"

out="$(render)"
for kind in NetworkPolicy ServiceAccount PersistentVolumeClaim ClusterRole ClusterRoleBinding Role RoleBinding Service Deployment; do
  grep -qx "kind: $kind" <<<"$out" || die "default render has no $kind"
done
[ "$(val "$out" 'select(.kind == "Deployment") | (.spec.replicas | tostring) + " " + .spec.strategy.type')" = "1 Recreate" ] \
  || die "the Deployment is not 1 replica with Recreate: two runnerd pods would contend for daemon.lock"
[ "$(val "$out" 'select(.kind == "PersistentVolumeClaim") | .spec.accessModes | join(",")')" = "ReadWriteOnce" ] \
  || die "the state volume is not ReadWriteOnce only"
[ "$(val "$out" 'select(.kind == "ServiceAccount") | .automountServiceAccountToken')" = "false" ] \
  || die "the ServiceAccount object does not turn token automount off"
[ "$(val "$out" 'select(.kind == "Deployment") | .spec.template.spec.automountServiceAccountToken')" = "true" ] \
  || die "runnerd's pod does not opt in to its token"
[ "$(yq 'select(.kind == "Deployment")' <<<"$out" | yq -r '[.spec.template.spec.initContainers[], .spec.template.spec.containers[]] | map(.securityContext.readOnlyRootFilesystem and (.securityContext.allowPrivilegeEscalation | not)) | all')" = "true" ] \
  || die "a container lost readOnlyRootFilesystem or allowPrivilegeEscalation=false"
ok "default render: objects, one writer, RWO, token scope, container hardening"

# Ingress: only proxy pods of the runs namespaces, only the relay and sink ports. No other rule may exist,
# because NetworkPolicy allows are additive: one more rule would let agent pods reach runnerd.
ingress="$(docs "$out" 'select(.kind == "NetworkPolicy") | .spec.ingress')"
want='[{"from":[{"namespaceSelector":{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["wardyn-runs-ci"]}]},"podSelector":{"matchLabels":{"wardyn.managed":"true","wardyn.component":"proxy"}}}],"ports":[{"protocol":"TCP","port":"relay"},{"protocol":"TCP","port":"sink"}]}]'
[ "$ingress" = "$want" ] || die "runnerd ingress is not exactly proxy pods of the runs namespace on relay and sink: $ingress"
[ "$(grep -c 'namespaceSelector: {}' <<<"$out")" = 1 ] || die "a namespaceSelector: {} peer other than DNS rendered"
val "$out" 'select(.kind == "NetworkPolicy") | .spec.egress[] | select(.ports[0].port == 443 and .to[0].ipBlock) | .to[0].ipBlock.except | join(",")' \
  | grep -x '10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,169.254.0.0/16' >/dev/null \
  || die "the default organisation egress no longer excludes the private ranges pods use"
grep -q 'cidr: 203.0.113.0/24' <<<"$(render --set 'egress.orgCIDRs[0]=203.0.113.0/24')" || die "egress.orgCIDRs did not render"
ok "NetworkPolicy: proxy-only ingress, DNS the only cluster-wide peer, private ranges excluded"

# Helm sorts kinds into its install order, the same order install and upgrade apply them in.
first_deploy="$(grep -n '^kind: Deployment$' <<<"$out" | head -1 | cut -d: -f1)"
last_rbac="$(grep -nE '^kind: (ClusterRole|ClusterRoleBinding|Role|RoleBinding)$' <<<"$out" | tail -1 | cut -d: -f1)"
[[ -n "$first_deploy" && -n "$last_rbac" && "$last_rbac" -lt "$first_deploy" ]] \
  || die "an RBAC object renders after the Deployment: the image could start before its verbs exist"
ok "RBAC renders before the Deployment"

norbac="$(render --set rbac.create=false)"
grep -qE '^kind: (ClusterRole|ClusterRoleBinding|Role|RoleBinding)$' <<<"$norbac" && die "rbac.create=false still rendered RBAC"
# helm template does not render NOTES and a client dry run still dials a cluster, so read the source.
grep -A2 'if not .Values.rbac.create' "$CHART/templates/NOTES.txt" | grep 'every Role and RoleBinding BEFORE the' >/dev/null \
  || die "rbac.create=false NOTES no longer tell the operator to apply RBAC before the image"
ok "rbac.create=false: no RBAC, NOTES say apply it first"

tagged="$(render --set image.tag=1.2.3)"
grep -q '"--proxy-image=ghcr.io/cjohnstoniv/wardyn-proxy:1.2.3"' <<<"$tagged" || die "the proxy image does not follow runnerd's tag"
ok "runnerd and proxy share one tag"

arm="$(render --set sandbox.arch=arm64 | yq 'select(.kind == "Deployment") | .spec.template.spec')"
[ "$(val "$arm" '.initContainers[0].args[] | select(. == "--arch=*")')" = "--arch=arm64" ] \
  || die "register is not passed --arch=arm64: the runner would report no arch"
[ "$(val "$arm" '.containers[0].env[] | select(.name == "WARDYN_RUNNERD_ARCH") | .value')" = "arm64" ] \
  || die "serve is not given WARDYN_RUNNERD_ARCH=arm64: run pods would not be pinned to arm64 nodes"
ok "sandbox.arch reaches register --arch and serve WARDYN_RUNNERD_ARCH"

vm="$(render --set k8s.linuxVM=true | yq 'select(.kind == "Deployment") | .spec.template.spec')"
[ "$(val "$vm" '.containers[0].env[] | select(.name == "WARDYN_RUNNER_LINUX_VM") | .value')" = "1" ] \
  || die "k8s.linuxVM=true does not give serve WARDYN_RUNNER_LINUX_VM=1"
grep -q WARDYN_RUNNER_LINUX_VM <<<"$out" && die "Linux VM sandboxes are on by default"
ok "k8s.linuxVM reaches serve WARDYN_RUNNER_LINUX_VM; off by default"

refused() { # <label> <message pattern> <helm args...>
  local label="$1" pattern="$2" err
  shift 2
  if err="$(helm template r "$CHART" -n "$NS" "$@" 2>&1)"; then die "$label: rendered"; fi
  grep -q "$pattern" <<<"$err" || die "$label: refused for another reason: $err"
  ok "refused: $label"
}
refused "org empty" "org is required" -f "$VALUES" --set org=
refused "runsNamespace empty" "runsNamespace is required" -f "$VALUES" --set runsNamespace=
refused "runsNamespace is the release namespace" "must not be the release namespace" -f "$VALUES" --set runsNamespace="$NS"
refused "sandbox.arch empty" "sandbox.arch must be amd64 or arm64" -f "$VALUES" --set sandbox.arch=
refused "sandbox.arch unknown" "sandbox.arch must be amd64 or arm64" -f "$VALUES" --set sandbox.arch=x86
refused "registration.existingSecret empty" "registration.existingSecret is required" -f "$VALUES" --set registration.existingSecret=
refused "k8s.allowUnenforcedNetPol set" "k8s.allowUnenforcedNetPol is refused" -f "$VALUES" --set k8s.allowUnenforcedNetPol=false
refused "k8s.ackAmbientDefaultDeny set" "k8s.ackAmbientDefaultDeny is refused" -f "$VALUES" --set k8s.ackAmbientDefaultDeny=true
refused "extraRunsNamespaces holds the release namespace" "must not be the release namespace" -f "$VALUES" --set "extraRunsNamespaces[0]=$NS"
refused "extraRunsNamespaces repeats runsNamespace" "must be unique" -f "$VALUES" --set extraRunsNamespaces[0]=wardyn-runs-ci
refused "extraRunsNamespaces repeats itself" "must be unique" -f "$VALUES" --set extraRunsNamespaces[0]=a --set extraRunsNamespaces[1]=a
refused "extraRunsNamespaces holds an empty name" "is not a namespace name" -f "$VALUES" --set extraRunsNamespaces[0]=
refused "workloads.namespaces repeats itself" "workloads.namespaces must be unique" -f "$VALUES" --set workloads.namespaces[0]=t --set workloads.namespaces[1]=t
refused "workloads.namespaces holds an empty name" "is not a namespace name" -f "$VALUES" --set workloads.namespaces[0]=

# RBAC parity with the control-plane chart.
server="$(helm template w deploy/helm/wardyn -n wardyn \
  --set auth.adminToken.secretRef.name=a --set secrets.ageKeyFromSecret=true --set serviceAccount.automount=true \
  --set k8s.enabled=true --set k8s.runsNamespace=wardyn-runs --set k8s.proxyImage=p:1 --set k8s.readNodes=true)"
server_role="$(docs "$server" 'select(.kind == "Role" and .metadata.name == "w-wardyn-k8s-runner") | .rules')"
server_cluster="$(docs "$server" 'select(.kind == "ClusterRole") | .rules')"
runs_role="$(docs "$out" 'select(.kind == "Role" and .metadata.name == "r-wardyn-runnerd-runs") | .rules')"
cluster="$(docs "$out" 'select(.kind == "ClusterRole") | .rules')"
[[ -n "$server_role" && -n "$server_cluster" ]] || die "the control-plane chart rendered no k8s-runner Role or ClusterRole to compare with"
[ "$(yq -o=json -I=0 '.[:-1]' <<<"$runs_role")" = "$server_role" ] \
  || die "the runs-namespace Role is not the control-plane Role rule for rule: runnerd $runs_role vs server $server_role"
[ "$(yq -o=json -I=0 '.[-1]' <<<"$runs_role")" = '{"apiGroups":[""],"resources":["pods"],"verbs":["patch"]}' ] \
  || die "the runs-namespace Role's one addition is not exactly pods: patch: $runs_role"
[ "$cluster" = "$server_cluster" ] \
  || die "the ClusterRole is not the control-plane ClusterRole with k8s.readNodes: runnerd $cluster vs server $server_cluster"
[ "$(docs "$out" 'select(.kind == "Role" and .metadata.namespace == null) | .rules')" = '[{"apiGroups":[""],"resources":["secrets"],"verbs":["create","get","delete","deletecollection"]}]' ] \
  || die "the release-namespace Role is not exactly secrets create/get/delete/deletecollection"
ok "RBAC parity: server Role + pods: patch; server ClusterRole with readNodes; own-namespace secrets only"

# Several runs namespaces: every one gets the same Role, a RoleBinding to runnerd's ServiceAccount and a
# --runs-namespace flag (the default first), and the ingress rule names all of them in one In list.
multi="$(render --set extraRunsNamespaces[0]=wardyn-runs-b --set extraRunsNamespaces[1]=wardyn-runs-c)"
[ "$(val "$multi" 'select(.kind == "Role" and .metadata.name == "r-wardyn-runnerd-runs") | .metadata.namespace')" = "$(printf 'wardyn-runs-ci\nwardyn-runs-b\nwardyn-runs-c')" ] \
  || die "extraRunsNamespaces did not render one runs Role per namespace"
[ "$(docs "$multi" 'select(.kind == "Role" and .metadata.name == "r-wardyn-runnerd-runs") | .rules' | sort -u | wc -l | tr -d ' ')" = 1 ] \
  || die "the runs Roles of the namespaces differ"
[ "$(val "$multi" 'select(.kind == "RoleBinding" and .metadata.name == "r-wardyn-runnerd-runs") | .metadata.namespace + ">" + .roleRef.name + ">" + .subjects[0].name + "." + .subjects[0].namespace')" \
  = "$(printf 'wardyn-runs-ci>r-wardyn-runnerd-runs>r-wardyn-runnerd.wardyn-runner\nwardyn-runs-b>r-wardyn-runnerd-runs>r-wardyn-runnerd.wardyn-runner\nwardyn-runs-c>r-wardyn-runnerd-runs>r-wardyn-runnerd.wardyn-runner')" ] \
  || die "a runs RoleBinding does not bind the Role to runnerd's ServiceAccount"
[ "$(val "$multi" 'select(.kind == "Deployment") | .spec.template.spec.containers[0].args[] | select(. == "--runs-namespace=*")' | tr '\n' ' ')" \
  = "--runs-namespace=wardyn-runs-ci --runs-namespace=wardyn-runs-b --runs-namespace=wardyn-runs-c " ] \
  || die "serve is not given one --runs-namespace per namespace, the default first"
[ "$(docs "$multi" 'select(.kind == "NetworkPolicy") | .spec.ingress[0].from[0].namespaceSelector')" \
  = '{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["wardyn-runs-ci","wardyn-runs-b","wardyn-runs-c"]}]}' ] \
  || die "the ingress rule does not select exactly the runs namespaces"
ok "extraRunsNamespaces: a Role, binding and flag per namespace; ingress In list"

# Declarative workloads: nothing by default; per listed namespace one get/list/watch Role with status
# update/patch, and never a secret or a cluster-wide verb.
[ "$(val "$out" 'select(.kind == "Role" and .metadata.name == "r-wardyn-runnerd-workloads") | .metadata.name')" = "" ] \
  || die "a workloads Role renders by default"
grep -q WARDYN_RUNNERD_WORKLOAD_NAMESPACES <<<"$out" && die "WARDYN_RUNNERD_WORKLOAD_NAMESPACES is set by default"
wl="$(render --set workloads.namespaces[0]=team-a --set workloads.namespaces[1]=team-b)"
[ "$(val "$wl" 'select(.kind == "Role" and .metadata.name == "r-wardyn-runnerd-workloads") | .metadata.namespace')" = "$(printf 'team-a\nteam-b')" ] \
  || die "workloads.namespaces did not render one Role per namespace"
[ "$(docs "$wl" 'select(.kind == "Role" and .metadata.name == "r-wardyn-runnerd-workloads") | .rules' | sort -u)" \
  = '[{"apiGroups":["wardyn.dev"],"resources":["wardynworkloads"],"verbs":["get","list","watch"]},{"apiGroups":["wardyn.dev"],"resources":["wardynworkloads/status"],"verbs":["update","patch"]}]' ] \
  || die "the workloads Role is not exactly wardynworkloads get/list/watch and wardynworkloads/status update/patch"
[ "$(val "$wl" 'select(.kind == "RoleBinding" and .metadata.name == "r-wardyn-runnerd-workloads") | .metadata.namespace + ">" + .roleRef.name + ">" + .subjects[0].name + "." + .subjects[0].namespace')" \
  = "$(printf 'team-a>r-wardyn-runnerd-workloads>r-wardyn-runnerd.wardyn-runner\nteam-b>r-wardyn-runnerd-workloads>r-wardyn-runnerd.wardyn-runner')" ] \
  || die "a workloads RoleBinding does not bind the Role to runnerd's ServiceAccount"
[ "$(val "$wl" 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] | select(.name == "WARDYN_RUNNERD_WORKLOAD_NAMESPACES") | .value')" = "team-a,team-b" ] \
  || die "workloads.namespaces did not reach serve as WARDYN_RUNNERD_WORKLOAD_NAMESPACES"
[ "$(val "$wl" 'select(.kind == "Deployment") | .spec.template.spec.initContainers[0].env[]? | select(.name == "WARDYN_RUNNERD_WORKLOAD_NAMESPACES") | .name')" = "" ] \
  || die "register is given WARDYN_RUNNERD_WORKLOAD_NAMESPACES"
[ -z "$(val "$(render --set rbac.create=false --set workloads.namespaces[0]=team-a)" 'select(.kind == "Role") | .metadata.name')" ] \
  || die "rbac.create=false still rendered a workloads Role"
ok "workloads.namespaces: Role and binding per namespace, no secrets, env set; off by default"

# The CRD ships in crds/, which Helm installs once and never upgrades (so two arch releases do not fight over it).
crd="$(helm template r "$CHART" -n "$NS" -f "$VALUES" --include-crds | yq 'select(.kind == "CustomResourceDefinition")')"
[ "$(val "$crd" '.metadata.name + " " + .spec.scope + " " + (.spec.versions | map(.name + ":" + (.subresources.status | tag)) | join(","))')" \
  = "wardynworkloads.wardyn.dev Namespaced v1alpha1:!!map" ] \
  || die "the WardynWorkload CRD is not a namespaced v1alpha1 with a status subresource"
[ "$(val "$crd" '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.intent["x-kubernetes-preserve-unknown-fields"]')" = "true" ] \
  || die "the CRD's intent is not preserve-unknown: the server validates it"
[ -z "$(find "$CHART/crds" -type f ! -name '*.yaml')" ] || die "a non-yaml file under crds/"
ok "WardynWorkload CRD: v1alpha1, status subresource, intent preserve-unknown"
