#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-helm-spire.sh — the experimental SPIRE identity provider's switch (spire.enabled) must be OFF by default and
# cost nothing off. Asserts on rendered output, not on the templates' text:
#   - off (the default, and the block null) renders the same bytes, with no WARDYN_IDENTITY, WARDYN_SPIRE_* or
#     WARDYN_K8S_SPIFFE_* variable, no CSI volume and no proxy ServiceAccount;
#   - on renders WARDYN_IDENTITY=spire (over an env entry), the four WARDYN_SPIRE_* variables with the selectors
#     naming the same runs namespace as WARDYN_K8S_NAMESPACE and the proxy account, WARDYN_POD_SERVICE_ACCOUNT from
#     the pod's own spec, the CSI volume read-only in wardynd, the proxies' ServiceAccount in the runs namespace with
#     no token and no binding, and one egress rule to the SPIRE server's namespace and port (no new cluster-wide peer);
#   - on without k8s.enabled, with the proxy account empty, "default" or equal to wardynd's, with wardynd on
#     "default", or with no server address, server namespace or parent ID is refused.
# `make helm-lint` runs it. Needs helm and yq (mikefarah v4).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

CHART=deploy/helm/wardyn
ALLON="$CHART/ci/all-on-values.yaml"
BASAL=(--set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true)
ON=(--set spire.enabled=true --set spire.server.address=spire-server.spire-server.svc --set spire.server.namespace=spire-server
  --set spire.parentId=spiffe://example.org/spire/agent/k8s_psat/ci)
command -v helm >/dev/null || die "helm not installed: this test cannot run, and a skip proves nothing"
command -v yq >/dev/null || die "yq not installed: this test cannot run, and a skip proves nothing"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
ok() { echo "ok  $1"; }
render() { helm template r "$CHART" "$@"; }
# yq prints a blank line for each document a select drops; keep only real output.
y() { yq -N "$@" | sed '/^$/d'; }
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
dep='select(.kind == "Deployment") | .spec.template.spec'
envval() { # <name> <render file>
  y "$dep | .containers[0].env[] | select(.name == \"$1\") | .value" "$2"
}

# ---- off
render -f "$ALLON" >"$TMP/absent.out"
render -f "$ALLON" --set spire=null >"$TMP/null.out"
cmp -s "$TMP/absent.out" "$TMP/null.out" || die "an explicit null renders differently from the default (--reuse-values from a release without the block)"
grep -q 'name: WARDYN_RUNNER' "$TMP/absent.out" || die "the all-on render has no k8s env: the absence check below would pass on nothing"
if grep -qE 'WARDYN_IDENTITY|WARDYN_SPIRE_|WARDYN_K8S_SPIFFE_|WARDYN_POD_SERVICE_ACCOUNT|spiffe' "$TMP/absent.out"; then
  grep -nE 'WARDYN_IDENTITY|WARDYN_SPIRE_|WARDYN_K8S_SPIFFE_|WARDYN_POD_SERVICE_ACCOUNT|spiffe' "$TMP/absent.out" >&2
  die "off: a SPIRE marker rendered"
fi
want "off: proxy ServiceAccount" "$(y 'select(.kind == "ServiceAccount") | .metadata.name' "$TMP/absent.out" | grep -cx wardyn-proxy || true)" "0"
ok "off (default, null): identical renders, no SPIRE env, volume or ServiceAccount"

# ---- on
render -f "$ALLON" "${ON[@]}" --set-string env.WARDYN_IDENTITY=embedded >"$TMP/on.out"
want "on: WARDYN_IDENTITY, rendered once over env" "$(y "$dep | .containers[0].env[] | select(.name == \"WARDYN_IDENTITY\") | .value" "$TMP/on.out")" "spire"
want "on: WARDYN_SPIRE_AGENT_SOCKET" "$(envval WARDYN_SPIRE_AGENT_SOCKET "$TMP/on.out")" "unix:///spiffe-workload-api/spire-agent.sock"
want "on: WARDYN_SPIRE_SERVER_ADDRESS" "$(envval WARDYN_SPIRE_SERVER_ADDRESS "$TMP/on.out")" "spire-server.spire-server.svc:443"
want "on: WARDYN_SPIRE_PARENT_ID" "$(envval WARDYN_SPIRE_PARENT_ID "$TMP/on.out")" "spiffe://example.org/spire/agent/k8s_psat/ci"
want "on: WARDYN_K8S_NAMESPACE" "$(envval WARDYN_K8S_NAMESPACE "$TMP/on.out")" "wardyn-runs"
want "on: WARDYN_SPIRE_SELECTORS" "$(envval WARDYN_SPIRE_SELECTORS "$TMP/on.out")" \
  "k8s:ns:wardyn-runs,k8s:sa:wardyn-proxy,k8s:pod-label:wardyn.component:proxy,k8s:pod-label:wardyn.run-id:{run_id}"
want "on: WARDYN_POD_SERVICE_ACCOUNT from the pod spec" \
  "$(y "$dep | .containers[0].env[] | select(.name == \"WARDYN_POD_SERVICE_ACCOUNT\") | .valueFrom.fieldRef.fieldPath" "$TMP/on.out")" "spec.serviceAccountName"
want "on: WARDYN_K8S_SPIFFE_CSI_DRIVER" "$(envval WARDYN_K8S_SPIFFE_CSI_DRIVER "$TMP/on.out")" "csi.spiffe.io"
want "on: WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT" "$(envval WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT "$TMP/on.out")" "wardyn-proxy"
ok "on: WARDYN_IDENTITY=spire over env, the SPIRE env, and selectors naming the runs namespace and proxy account"

want "on: wardynd's CSI volume" "$(y "$dep | .volumes[] | select(.name == \"spiffe-workload-api\") | .csi.driver + \" \" + (.csi.readOnly | tostring)" "$TMP/on.out")" "csi.spiffe.io true"
want "on: wardynd's CSI mount" "$(y "$dep | .containers[0].volumeMounts[] | select(.name == \"spiffe-workload-api\") | .mountPath + \" \" + (.readOnly | tostring)" "$TMP/on.out")" "/spiffe-workload-api true"
ok "on: wardynd mounts the CSI volume read-only at /spiffe-workload-api"

sa='select(.kind == "ServiceAccount" and .metadata.name == "wardyn-proxy")'
want "on: proxy ServiceAccount namespace" "$(y "$sa | .metadata.namespace" "$TMP/on.out")" "wardyn-runs"
want "on: proxy ServiceAccount token" "$(y "$sa | .automountServiceAccountToken" "$TMP/on.out")" "false"
want "on: bindings naming the proxy account" "$(y 'select(.kind == "RoleBinding" or .kind == "ClusterRoleBinding") | (.subjects // [])[] | select(.name == "wardyn-proxy") | .name' "$TMP/on.out" | wc -l | tr -d ' ')" "0"
ok "on: the proxies' ServiceAccount in the runs namespace, no token, no binding"

np='select(.kind == "NetworkPolicy" and .metadata.name == "r-wardyn") | .spec.egress[]'
want "on: SPIRE server egress rule" \
  "$(y "$np | select(.to[0].namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"] == \"spire-server\") | .ports[0].port" "$TMP/on.out")" "8081"
want "on: cluster-wide peers" "$(grep -c 'namespaceSelector: {}' "$TMP/on.out")" "$(grep -c 'namespaceSelector: {}' "$TMP/absent.out")"
ok "on: one egress rule to the SPIRE server's namespace and port, no new cluster-wide peer"
render -f "$ALLON" "${ON[@]}" --set spire.server.port=8443 --set spire.server.targetPort=9443 >"$TMP/target.out"
want "targetPort: address" "$(envval WARDYN_SPIRE_SERVER_ADDRESS "$TMP/target.out")" "spire-server.spire-server.svc:8443"
want "targetPort: egress port" "$(y "$np | select(.to[0].namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"] == \"spire-server\") | .ports[0].port" "$TMP/target.out")" "9443"
ok "targetPort: the address keeps the Service port, the egress rule names the pod's"

render "${BASAL[@]}" -n wardyn-sys --set k8s.enabled=true --set k8s.proxyImage=example.test/proxy:1 \
  --set serviceAccount.automount=true --set k8s.allowRunsInReleaseNamespace=true "${ON[@]}" >"$TMP/release-ns.out"
want "release namespace: selectors" "$(envval WARDYN_SPIRE_SELECTORS "$TMP/release-ns.out" | cut -d, -f1-2)" "k8s:ns:wardyn-sys,k8s:sa:wardyn-proxy"
want "release namespace: proxy ServiceAccount" "$(y "$sa | .metadata.namespace" "$TMP/release-ns.out")" "wardyn-sys"
ok "no runs namespace: the selectors and the proxy account follow the release namespace"
helm lint "$CHART" -f "$ALLON" "${ON[@]}" >/dev/null
ok "helm lint with spire on"

# ---- refusals
refused "on without k8s.enabled" "spire.enabled needs k8s.enabled=true" "${BASAL[@]}" "${ON[@]}"
refused "proxy account equal to wardynd's" "spire.proxyServiceAccount equals wardynd's ServiceAccount" -f "$ALLON" "${ON[@]}" --set spire.proxyServiceAccount=wardyn-byo
# Named default outright: an empty name is refused earlier, by the k8s runner's own ServiceAccount refusal in rbac.yaml.
refused "wardynd on default" "needs wardynd on its own ServiceAccount" -f "$ALLON" "${ON[@]}" --set serviceAccount.name=default
refused "proxy account default" "spire.proxyServiceAccount must name a dedicated account" -f "$ALLON" "${ON[@]}" --set spire.proxyServiceAccount=default
refused "proxy account empty" "spire.proxyServiceAccount must name a dedicated account" -f "$ALLON" "${ON[@]}" --set spire.proxyServiceAccount=
refused "no server address" "spire.server.address is required" -f "$ALLON" "${ON[@]}" --set spire.server.address=
refused "no server namespace" "spire.server.namespace is required" -f "$ALLON" "${ON[@]}" --set spire.server.namespace=
refused "no parent ID" "spire.parentId is required" -f "$ALLON" "${ON[@]}" --set spire.parentId=
refused "named target port" "spire.server.targetPort must be a port number" -f "$ALLON" "${ON[@]}" --set spire.server.targetPort=grpc
refused "target port 0" "spire.server.targetPort must be a port number" -f "$ALLON" "${ON[@]}" --set spire.server.targetPort=0
refused "target port 65536" "spire.server.targetPort must be a port number" -f "$ALLON" "${ON[@]}" --set spire.server.targetPort=65536

log "helm spire: PASS"
