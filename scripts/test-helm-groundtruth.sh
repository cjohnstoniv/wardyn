#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-helm-groundtruth.sh — the Kubernetes ground-truth sensor's switch (groundtruth.k8s.enabled) must be OFF by
# default and cost nothing off. Asserts on rendered output, not on the templates' text:
#   - off (enabled=false, the key null, the 0.8.5 reuse-values file) renders the same bytes as one another, with no
#     DaemonSet, no groundtruth Secret or Role, no WARDYN_GROUNDTRUTH_TOKEN_SECRET; when BASE is a git rev, the
#     off render is byte-identical to that commit's chart under the same values;
#   - on renders exactly: one DaemonSet that mounts the Tetragon export DIRECTORY read-only (a file mount would keep
#     the old inode across a rename-and-recreate rotation), one Secret with no data, kept on uninstall, one Role
#     granting `patch` on that Secret by name and nothing else, no new ClusterRole rule, wardynd's env naming the
#     same Secret, and the internal-port NetworkPolicy peer for a sensor namespace other than the release's;
#   - on without k8s.enabled, with no runs namespace, with WARDYN_GROUNDTRUTH_TOKEN_FILE in env, or with a path in
#     exportFilename is refused.
# `make helm-lint` runs it. Needs helm and yq (mikefarah v4).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

CHART=deploy/helm/wardyn
ALLON="$CHART/ci/all-on-values.yaml"
REUSE="$CHART/ci/reuse-values/v0.8.5.yaml"
BASAL=(--set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true)
command -v helm >/dev/null || die "helm not installed: this test cannot run, and a skip proves nothing"
command -v yq >/dev/null || die "yq not installed: this test cannot run, and a skip proves nothing"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
ok() { echo "ok  $1"; }
render() { helm template r "$CHART" "$@"; }
# yq prints a blank line (or an empty array) for each document a select drops; keep only real output.
y() { yq -N "$@" | sed -e '/^$/d' -e '/^\[\]$/d'; }
want() { # <label> <got> <want>
  [[ "$2" == "$3" ]] || { echo "got:  $2" >&2; echo "want: $3" >&2; die "$1"; }
}
refused() { # <label> <message pattern> <helm args...>
  local label="$1" pat="$2"
  shift 2
  if render "$@" >"$TMP/o" 2>"$TMP/e"; then die "$label: rendered, want a refusal"; fi
  grep -q -- "$pat" "$TMP/e" || { cat "$TMP/e" >&2; die "$label: refused with the wrong message (want: $pat)"; }
  ok "$label"
}
# Every marker the sensor may add: none may appear in an off render.
absent() { # <label> <render file>
  if grep -qiE 'groundtruth|tetragon' "$2"; then
    grep -niE 'groundtruth|tetragon' "$2" >&2
    die "$1: an off render carries a ground-truth marker"
  fi
}

# ---- off
render -f "$ALLON" --set groundtruth.k8s.enabled=false >"$TMP/false.out"
render -f "$ALLON" --set groundtruth=null >"$TMP/null.out"
cmp -s "$TMP/false.out" "$TMP/null.out" || die "an explicit null renders differently from enabled=false (--reuse-values from a release without the key)"
absent "all-on values, sensor off" "$TMP/false.out"
render -f "$REUSE" "${BASAL[@]}" >"$TMP/reuse.out"
absent "0.8.5 reuse-values render" "$TMP/reuse.out"
render "${BASAL[@]}" >"$TMP/default.out"
absent "default render" "$TMP/default.out"
ok "off (false, null, reuse-values, defaults): no DaemonSet, Secret, Role, env or NetworkPolicy peer"

if [[ -n "${BASE:-}" ]]; then
  mkdir -p "$TMP/base"
  git archive "$BASE" "$CHART" | tar -x -C "$TMP/base"
  for args in "-f $ALLON --set groundtruth.k8s.enabled=false" "-f $REUSE ${BASAL[*]}" "${BASAL[*]}"; do
    # shellcheck disable=SC2086
    helm template r "$TMP/base/$CHART" $args >"$TMP/base.out"
    # shellcheck disable=SC2086
    helm template r "$CHART" $args >"$TMP/head.out"
    cmp -s "$TMP/base.out" "$TMP/head.out" || { diff "$TMP/base.out" "$TMP/head.out" | head -20 >&2; die "off render differs from $BASE under: $args"; }
  done
  ok "off render is byte-identical to $BASE (all-on values, 0.8.5 reuse-values, defaults)"
else
  echo "note: BASE not set, byte-identity to a base commit not checked (BASE=<rev> bash $0)"
fi

# ---- on (all-on values put the sensor in namespace tetragon, the runs in wardyn-runs)
render -f "$ALLON" >"$TMP/on.out"
want "on: not exactly one DaemonSet" "$(y 'select(.kind == "DaemonSet") | .metadata.name' "$TMP/on.out")" "r-wardyn-default-groundtruth"
want "on: the DaemonSet is not in the sensor namespace" "$(y 'select(.kind == "DaemonSet") | .metadata.namespace' "$TMP/on.out")" "tetragon"
ds='select(.kind == "DaemonSet") | .spec.template.spec'
want "on: the sensor pod has an API token" "$(y "$ds | .automountServiceAccountToken" "$TMP/on.out")" "false"
want "on: wrong command" "$(y -o=json -I=0 "$ds | .containers[0].command" "$TMP/on.out")" '["/wardyn-tetragon-ingest"]'
env() { y "$ds | .containers[0].env[] | select(.name == \"$1\") | (.value // .valueFrom.fieldRef.fieldPath)" "$TMP/on.out"; }
want "on: WARDYN_GROUNDTRUTH_SOURCE" "$(env WARDYN_GROUNDTRUTH_SOURCE)" "k8s"
want "on: WARDYN_GROUNDTRUTH_K8S_NAMESPACES is not the runs namespace" "$(env WARDYN_GROUNDTRUTH_K8S_NAMESPACES)" "wardyn-runs"
want "on: WARDYN_NODE_NAME is not the downward-API node" "$(env WARDYN_NODE_NAME)" "spec.nodeName"
want "on: WARDYN_GROUNDTRUTH_TOKEN_FILE" "$(env WARDYN_GROUNDTRUTH_TOKEN_FILE)" "/var/run/wardyn-groundtruth/token"
want "on: WARDYN_CONTROL_PLANE_CA_FILE" "$(env WARDYN_CONTROL_PLANE_CA_FILE)" "/var/run/wardyn-groundtruth/control-plane-ca.pem"
want "on: WARDYN_CONTROL_PLANE_URL is not the internal service port" "$(env WARDYN_CONTROL_PLANE_URL)" "https://r-wardyn.default.svc.cluster.local:8443"
ok "on: DaemonSet command, source, namespaces, node, token and CA paths, control-plane URL, no API token"

# Rotation through the chart: the export DIRECTORY is the hostPath, read-only, mounted with no subPath, and the
# file the ingest tails is inside that mount. A hostPath of the file itself would bind one inode.
want "on: the hostPath is not the export directory" "$(y -o=json -I=0 "$ds | .volumes[] | select(.name == \"tetragon-export\") | .hostPath" "$TMP/on.out")" '{"path":"/var/log/tetragon","type":"Directory"}'
want "on: the export mount is not a read-only directory mount" "$(y -o=json -I=0 "$ds | .containers[0].volumeMounts[] | select(.name == \"tetragon-export\")" "$TMP/on.out")" '{"name":"tetragon-export","mountPath":"/tetragon-export","readOnly":true}'
want "on: the export file is outside the mounted directory" "$(env WARDYN_TETRAGON_EXPORT)" "/tetragon-export/tetragon.log"
want "on: the token mount is not a read-only directory mount" "$(y -o=json -I=0 "$ds | .containers[0].volumeMounts[] | select(.name == \"token\")" "$TMP/on.out")" '{"name":"token","mountPath":"/var/run/wardyn-groundtruth","readOnly":true}'
ok "on: export directory mounted read-only with no subPath, the tailed file inside it; token Secret mounted as a directory"

want "on: container hardening" "$(y -o=json -I=0 "$ds | .containers[0].securityContext" "$TMP/on.out")" \
  '{"runAsUser":0,"runAsNonRoot":false,"allowPrivilegeEscalation":false,"readOnlyRootFilesystem":true,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}'
want "on: nodeSelector and tolerations did not reach the pod" "$(y -o=json -I=0 "$ds | [.nodeSelector, .tolerations]" "$TMP/on.out")" '[{"kubernetes.io/os":"linux"},[{"operator":"Exists"}]]'
ok "on: runs as root with every capability dropped, read-only root, seccomp RuntimeDefault; placement passes through"

# The sensor pod must not match the Service, PDB or Deployment selector (name + instance), or it would take console traffic.
want "on: sensor pod labels" "$(y -o=json -I=0 'select(.kind == "DaemonSet") | .spec.template.metadata.labels' "$TMP/on.out")" \
  '{"app.kubernetes.io/component":"groundtruth","app.kubernetes.io/instance":"r","app.kubernetes.io/name":"wardyn-groundtruth","wardyn.dev/control-plane-namespace":"default","app.kubernetes.io/part-of":"wardyn"}'
ok "on: sensor pod labels do not match the control-plane selector"

# The Secret: named, kept, no data.
sec='select(.kind == "Secret" and .metadata.name == "r-wardyn-default-groundtruth")'
want "on: the groundtruth Secret is missing or not in the sensor namespace" "$(y "$sec | .metadata.namespace" "$TMP/on.out")" "tetragon"
want "on: the Secret is not kept on uninstall" "$(y "$sec | .metadata.annotations.\"helm.sh/resource-policy\"" "$TMP/on.out")" "keep"
want "on: the Secret carries data the chart would overwrite" "$(y "$sec | select(.data == null and .stringData == null) | .metadata.name" "$TMP/on.out")" "r-wardyn-default-groundtruth"
ok "on: one Secret, in the sensor namespace, kept, no data"

# RBAC: patch on the one named Secret, and nothing else added anywhere.
role='select(.kind == "Role" and .metadata.name == "r-wardyn-default-groundtruth-token")'
want "on: the token Role is not patch on the one Secret" "$(y -o=json -I=0 "$role | .rules" "$TMP/on.out")" '[{"apiGroups":[""],"resources":["secrets"],"resourceNames":["r-wardyn-default-groundtruth"],"verbs":["patch"]}]'
want "on: the token Role is not in the sensor namespace" "$(y "$role | .metadata.namespace" "$TMP/on.out")" "tetragon"
want "on: the token RoleBinding" "$(y -o=json -I=0 'select(.kind == "RoleBinding" and .metadata.name == "r-wardyn-default-groundtruth-token") | [.metadata.namespace, .roleRef.name, .subjects[0].kind, .subjects[0].namespace]' "$TMP/on.out")" '["tetragon","r-wardyn-default-groundtruth-token","ServiceAccount","default"]'
want "on: the sensor added a ClusterRole or changed one" "$(y 'select(.kind == "ClusterRole")' "$TMP/on.out" | sha256sum)" "$(y 'select(.kind == "ClusterRole")' "$TMP/false.out" | sha256sum)"
want "on: the sensor added a ClusterRoleBinding" "$(y 'select(.kind == "ClusterRoleBinding")' "$TMP/on.out" | sha256sum)" "$(y 'select(.kind == "ClusterRoleBinding")' "$TMP/false.out" | sha256sum)"
want "on: a groundtruth Role or RoleBinding other than the two" "$(y 'select(.kind == "Role" or .kind == "RoleBinding") | .metadata.name' "$TMP/on.out" | grep -c groundtruth)" "2"
ok "on: Role is patch on one named Secret in the sensor namespace; no ClusterRole or ClusterRoleBinding change"

# wardynd names the same Secret and does not also set the file form.
dep='select(.kind == "Deployment") | .spec.template.spec.containers[0].env[]'
want "on: WARDYN_GROUNDTRUTH_TOKEN_SECRET" "$(y "$dep | select(.name == \"WARDYN_GROUNDTRUTH_TOKEN_SECRET\") | .value" "$TMP/on.out")" "tetragon/r-wardyn-default-groundtruth"
want "on: wardynd also sets WARDYN_GROUNDTRUTH_TOKEN_FILE" "$(y "$dep | select(.name == \"WARDYN_GROUNDTRUTH_TOKEN_FILE\") | .name" "$TMP/on.out")" ""
ok "on: wardynd's WARDYN_GROUNDTRUTH_TOKEN_SECRET is <sensor namespace>/<the Secret>, and not the file form"

# NetworkPolicy: the sensor namespace is a peer of the internal port rule only.
np='select(.kind == "NetworkPolicy") | .spec.ingress[]'
want "on: the sensor peer is not exactly in the internal-port rule" \
  "$(y "$np | select(.from[].podSelector.matchLabels.\"app.kubernetes.io/component\" == \"groundtruth\") | [.ports[].port]" "$TMP/on.out" | tr -d ' \n')" "-internal"
want "on: the sensor peer" "$(y -o=json -I=0 "$np | .from[] | select(.podSelector.matchLabels.\"app.kubernetes.io/component\" == \"groundtruth\")" "$TMP/on.out")" \
  '{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"tetragon"}},"podSelector":{"matchLabels":{"app.kubernetes.io/name":"wardyn-groundtruth","app.kubernetes.io/instance":"r","app.kubernetes.io/component":"groundtruth","wardyn.dev/control-plane-namespace":"default"}}}'
want "on: the sensor namespace appears in more than one rule" "$(grep -c 'kubernetes.io/metadata.name: tetragon' "$TMP/on.out")" "1"
ok "on: the sensor namespace and pod label are a peer of the internal port only"

# The sensor in the release namespace is already admitted by the same-namespace peer: nothing added.
render -f "$ALLON" --set groundtruth.k8s.namespace= >"$TMP/same.out"
want "same namespace: a NetworkPolicy peer was added" "$(y 'select(.kind == "NetworkPolicy")' "$TMP/same.out" | grep -c groundtruth || true)" "0"
want "same namespace: the DaemonSet is not in the release namespace" "$(y 'select(.kind == "DaemonSet") | .metadata.namespace' "$TMP/same.out")" "default"
want "same namespace: wardynd env" "$(y "$dep | select(.name == \"WARDYN_GROUNDTRUTH_TOKEN_SECRET\") | .value" "$TMP/same.out")" "default/r-wardyn-default-groundtruth"
ok "sensor in the release namespace: no extra NetworkPolicy peer"

# A non-default export location reaches the pod.
render -f "$ALLON" --set groundtruth.k8s.exportDirectory=/var/run/cilium/tetragon --set groundtruth.k8s.exportFilename=export.log >"$TMP/path.out"
want "custom path: hostPath" "$(y "$ds | .volumes[] | select(.name == \"tetragon-export\") | .hostPath.path" "$TMP/path.out")" "/var/run/cilium/tetragon"
want "custom path: file" "$(y "$ds | .containers[0].env[] | select(.name == \"WARDYN_TETRAGON_EXPORT\") | .value" "$TMP/path.out")" "/tetragon-export/export.log"
ok "a custom exportDirectory and exportFilename reach the hostPath and the tailed file"

# The runs namespace can be the release namespace when the operator accepts that.
render -f "$ALLON" --set k8s.runsNamespace= --set k8s.allowRunsInReleaseNamespace=true >"$TMP/rel.out"
want "release-namespace runs: WARDYN_GROUNDTRUTH_K8S_NAMESPACES" "$(y "$ds | .containers[0].env[] | select(.name == \"WARDYN_GROUNDTRUTH_K8S_NAMESPACES\") | .value" "$TMP/rel.out")" "default"
ok "runs in the release namespace (accepted): the sensor correlates that namespace"

# Two releases with the same name in different namespaces, one shared sensor namespace: every object the
# sensor puts there has a name of its own, and the selectors do not overlap.
twin() { helm template wardyn "$CHART" -n "$1" -f "$ALLON" "${@:2}"; }
twin prod >"$TMP/prod.out"
twin staging >"$TMP/staging.out"
inns() { y 'select(.metadata.namespace == "tetragon") | .kind + "/" + .metadata.name' "$1" | sort; }
inns "$TMP/prod.out" >"$TMP/prod.names"
inns "$TMP/staging.out" >"$TMP/staging.names"
want "twin releases: the sensor namespace holds no Secret, DaemonSet, Role and RoleBinding for prod" "$(wc -l <"$TMP/prod.names" | tr -d ' ')" "4"
want "twin releases: staging" "$(wc -l <"$TMP/staging.names" | tr -d ' ')" "4"
want "twin releases: an object name is shared" "$(comm -12 "$TMP/prod.names" "$TMP/staging.names" | wc -l | tr -d ' ')" "0"
ok "twin releases (wardyn in prod and in staging, sensor in tetragon): disjoint object names"
sel() { y -o=json -I=0 'select(.kind == "DaemonSet") | .spec.selector.matchLabels' "$1"; }
[[ "$(sel "$TMP/prod.out")" != "$(sel "$TMP/staging.out")" ]] || die "twin releases: both DaemonSets select the same pods"
want "twin releases: prod's DaemonSet selects staging's pods" "$(y -o=json -I=0 'select(.kind == "DaemonSet") | .spec.selector.matchLabels."wardyn.dev/control-plane-namespace"' "$TMP/prod.out" | tr -d '"')" "prod"
want "twin releases: the NetworkPolicy peer is not prod's own sensor" \
  "$(y 'select(.kind == "NetworkPolicy") | .spec.ingress[].from[].podSelector.matchLabels."wardyn.dev/control-plane-namespace"' "$TMP/prod.out" | grep -v '^null$')" "prod"
want "twin releases: the NetworkPolicy peer is not staging's own sensor" \
  "$(y 'select(.kind == "NetworkPolicy") | .spec.ingress[].from[].podSelector.matchLabels."wardyn.dev/control-plane-namespace"' "$TMP/staging.out" | grep -v '^null$')" "staging"
tokenenv() { y 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] | select(.name == "WARDYN_GROUNDTRUTH_TOKEN_SECRET") | .value' "$1"; }
[[ "$(tokenenv "$TMP/prod.out")" != "$(tokenenv "$TMP/staging.out")" ]] || die "twin releases: both wardynds write the same Secret"
want "twin releases: prod's wardynd writes a Secret its DaemonSet does not mount" "$(tokenenv "$TMP/prod.out")" "tetragon/$(y 'select(.kind == "DaemonSet") | .spec.template.spec.volumes[] | select(.name == "token") | .secret.secretName' "$TMP/prod.out")"
want "twin releases: staging's wardynd Secret differs from its DaemonSet's mount" "$(tokenenv "$TMP/staging.out")" "tetragon/$(y 'select(.kind == "DaemonSet") | .spec.template.spec.volumes[] | select(.name == "token") | .secret.secretName' "$TMP/staging.out")"
want "twin releases: Role resourceNames differ from the Secret" "$(y 'select(.kind == "Role" and (.metadata.name | test("groundtruth-token$"))) | .rules[0].resourceNames[0]' "$TMP/prod.out")" "$(y 'select(.kind == "Secret" and .metadata.namespace == "tetragon") | .metadata.name' "$TMP/prod.out")"
ok "twin releases: selectors, NetworkPolicy peers, wardynd's Secret and each Role's resourceNames are per release"

# Long names stay valid and stay apart: cut to 63 characters with a hash of the full name.
long="$(printf 'w%.0s' $(seq 1 50))"
helm template wardyn "$CHART" -n long-namespace-one -f "$ALLON" --set fullnameOverride="$long" >"$TMP/long1.out"
helm template wardyn "$CHART" -n long-namespace-two -f "$ALLON" --set fullnameOverride="$long" >"$TMP/long2.out"
inns "$TMP/long1.out" >"$TMP/long1.names"
inns "$TMP/long2.out" >"$TMP/long2.names"
[[ "$(sed 's#^[^/]*/##' "$TMP/long1.names" | awk '{ if (length($0) > 63) print }' | wc -l | tr -d ' ')" == 0 ]] || die "a sensor object name is over 63 characters"
want "long names: a name is shared across namespaces" "$(comm -12 "$TMP/long1.names" "$TMP/long2.names" | wc -l | tr -d ' ')" "0"
ok "long names: every sensor object name is at most 63 characters and distinct per namespace"

# ---- refusals
refused "on without k8s.enabled" "groundtruth.k8s.enabled needs k8s.enabled=true" -f "$ALLON" --set k8s.enabled=false
refused "on with no runs namespace" "groundtruth.k8s.enabled needs k8s.runsNamespace" -f "$ALLON" --set k8s.runsNamespace= --set serviceAccount.automount=false
refused "on with WARDYN_GROUNDTRUTH_TOKEN_FILE in env" "must not be set in env or extraEnv" -f "$ALLON" --set-string env.WARDYN_GROUNDTRUTH_TOKEN_FILE=/x
refused "on with a path in exportFilename" "exportFilename must be a file name" -f "$ALLON" --set groundtruth.k8s.exportFilename=a/b
refused "on with a relative exportDirectory" "exportDirectory must be an absolute path" -f "$ALLON" --set groundtruth.k8s.exportDirectory=var/log

log "helm ground-truth sensor: PASS"
