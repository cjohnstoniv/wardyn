#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-helm-agent-sandbox.sh — the experimental agent-sandbox backend's switch (k8s.agentSandbox.enabled, #3863)
# must be OFF by default and cost nothing off. Asserts on rendered output, not on the templates' text:
#   - off (key absent, null, false, the 0.8.5 reuse-values file) renders the same bytes as one another, with no
#     agents.x-k8s.io rule, no pods patch, no CRD, no WARDYN_K8S_AGENT_SANDBOX and no admissionregistration object but
#     the two keep policies and their bindings, which render with k8s.enabled;
#   - when BASE is a git rev (the commit before the key existed), the k8s-off renders (defaults and the 0.8.5
#     reuse-values file) are byte-identical to that commit's chart under the same values. A k8s.enabled render is not:
#     it now carries the per-run Service rules and the keep policies;
#   - on renders exactly: pods patch, a sandboxes create/get/list/delete rule, the env, and a
#     ValidatingAdmissionPolicy + binding that selects the controller identity and requires the gate, a Sandbox owner and an empty nodeName;
#   - on with k8s.agentSandbox.template adds only get on that one sandboxtemplate and WARDYN_K8S_AGENT_SANDBOX_TEMPLATE;
#     the template without the backend is refused;
#   - every k8s.enabled render grants the per-run Service rules (services create/get/delete, endpointslices
#     create/get/update/deletecollection) whatever the backend;
#   - keep off (k8s.agentSandbox.keep absent, null, false) renders the same bytes as on without it; keep on adds only
#     WARDYN_K8S_AGENT_SANDBOX_KEEP, and the rest of the render is on's, document for document; keep on without the backend is refused;
#   - keep's two ValidatingAdmissionPolicies and two bindings, <fullname>-keep-service (Services only) and
#     <fullname>-keep-endpoints (EndpointSlices only), selected on wardynd's identity and bounding create, update and
#     delete, render with k8s.enabled whatever the backend; a k8s.enabled=false render has none;
#   - a k8s.enabled render for Kubernetes below 1.30 is refused; a k8s-off render for it is not;
#   - on without k8s.enabled, or with an unenforced-NetworkPolicy or ambient-default-deny override (value, env, extraEnv or valueFrom), is refused.
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
# yq prints a blank line for each document a select drops; keep only real output.
y() { yq -N "$@" | sed '/^$/d'; }
refused() { # <label> <message pattern> <helm args...>
  local label="$1" pat="$2"
  shift 2
  if render "$@" >"$TMP/o" 2>"$TMP/e"; then die "$label: rendered, want a refusal"; fi
  grep -q -- "$pat" "$TMP/e" || { cat "$TMP/e" >&2; die "$label: refused with the wrong message (want: $pat)"; }
  ok "$label"
}
# Every marker the backend may add: none may appear in an off render.
absent() { # <label> <render file>
  local f="$2"
  y 'select(((.kind == "ValidatingAdmissionPolicy" or .kind == "ValidatingAdmissionPolicyBinding") and (.metadata.name | test("-keep-(service|endpoints)$"))) | not)' "$f" >"$TMP/absent-f"
  if grep -qE 'agents\.x-k8s\.io|WARDYN_K8S_AGENT_SANDBOX|admissionregistration|ValidatingAdmissionPolic|CustomResourceDefinition|sandboxes' "$TMP/absent-f"; then
    grep -nE 'agents\.x-k8s\.io|WARDYN_K8S_AGENT_SANDBOX|admissionregistration|ValidatingAdmissionPolic|CustomResourceDefinition|sandboxes' "$TMP/absent-f" >&2
    die "$1: an off render carries an agent-sandbox marker"
  fi
  [[ "$(y 'select(.kind == "Role") | .rules[] | select(.resources[0] == "pods") | .verbs | contains(["patch"])' "$f" | sort -u | tr '\n' ' ')" != *true* ]] \
    || die "$1: an off render grants patch on pods"
}

# The chart gates on k8s.enabled, so the all-on values file is the render that has the Role and Deployment env.
render -f "$ALLON" >"$TMP/key-absent.out"
render -f "$ALLON" --set k8s.agentSandbox.enabled=false >"$TMP/false.out"
render -f "$ALLON" --set k8s.agentSandbox=null >"$TMP/null.out"
cmp -s "$TMP/key-absent.out" "$TMP/false.out" || die "enabled=false renders differently from the default"
cmp -s "$TMP/key-absent.out" "$TMP/null.out" || die "an explicit null renders differently from the default (--reuse-values from a release without the key)"
grep -qx 'kind: Role' "$TMP/key-absent.out" && grep -q 'WARDYN_K8S_READ_NODES\|name: WARDYN_RUNNER' "$TMP/key-absent.out" \
  || die "the all-on render has no k8s Role or env: the absence checks below would pass on nothing"
absent "default k8s-on render" "$TMP/key-absent.out"
ok "off (absent, false, null): identical renders, no marker, no pods patch"

# k8s on: the per-run Service rules, on every k8s.enabled render whatever the backend.
k8srole='select(.kind == "Role" and (.metadata.name | test("k8s-runner$")))'
[[ "$(y -o=json -I=0 "$k8srole | .rules[] | select(.resources[0] == \"services\")" "$TMP/key-absent.out")" == '{"apiGroups":[""],"resources":["services"],"verbs":["create","get","delete"]}' ]] \
  || die "k8s on: the runner Role has no, or a wider, services rule"
[[ "$(y -o=json -I=0 "$k8srole | .rules[] | select(.apiGroups[0] == \"discovery.k8s.io\")" "$TMP/key-absent.out")" == '{"apiGroups":["discovery.k8s.io"],"resources":["endpointslices"],"verbs":["create","get","update","deletecollection"]}' ]] \
  || die "k8s on: the runner Role has no, or a wider, endpointslices rule"
# Every rule that names any of these resources or groups, in any position or with a wildcard: exactly the two above.
[[ "$(y "$k8srole | [.rules[] | select((.resources | any_c(. == \"services\" or . == \"endpointslices\" or . == \"endpoints\" or . == \"*\")) or (.apiGroups | any_c(. == \"discovery.k8s.io\" or . == \"*\")))] | length" "$TMP/key-absent.out" | grep -vx 0 || true)" == 2 ]] \
  || die "k8s on: a rule other than those two names services, endpointslices, endpoints, discovery.k8s.io or a wildcard"
ok "k8s on: the per-run Service rules, exactly"

render -f "$REUSE" "${BASAL[@]}" >"$TMP/reuse.out"
absent "0.8.5 reuse-values render" "$TMP/reuse.out"
ok "off under the 0.8.5 reuse-values file: no marker"

# Byte-identity to the commit before the key existed, when the caller names one: k8s-off renders only, since every
# k8s.enabled render now adds the per-run Service rules and the keep policies.
if [[ -n "${BASE:-}" ]]; then
  mkdir -p "$TMP/base"
  git archive "$BASE" "$CHART" | tar -x -C "$TMP/base"
  for args in "-f $REUSE ${BASAL[*]}" "${BASAL[*]}"; do
    # shellcheck disable=SC2086
    helm template r "$TMP/base/$CHART" $args >"$TMP/base.out"
    # shellcheck disable=SC2086
    helm template r "$CHART" $args >"$TMP/head.out"
    cmp -s "$TMP/base.out" "$TMP/head.out" || { diff "$TMP/base.out" "$TMP/head.out" | head -20 >&2; die "off render differs from $BASE under: $args"; }
  done
  ok "k8s-off render is byte-identical to $BASE (0.8.5 reuse-values, defaults)"
else
  echo "note: BASE not set, byte-identity to a base commit not checked (BASE=<rev> bash $0)"
fi

# On: exactly the backend's additions.
render -f "$ALLON" --set k8s.agentSandbox.enabled=true >"$TMP/on.out"
role='select(.kind == "Role" and (.metadata.name | test("k8s-runner$")))'
[[ "$(y "$role | .rules[] | select(.resources[0] == \"pods\" and (.apiGroups[0] == \"\")) | .verbs | join(\",\")" "$TMP/on.out" | head -1)" == "create,get,list,watch,delete,deletecollection,patch" ]] \
  || die "on: the runner Role's pods rule is not the base verbs plus patch"
[[ "$(y -o=json -I=0 "$role | .rules[] | select(.apiGroups[0] == \"agents.x-k8s.io\")" "$TMP/on.out")" == '{"apiGroups":["agents.x-k8s.io"],"resources":["sandboxes"],"verbs":["create","get","list","delete"]}' ]] \
  || die "on: the runner Role has no, or a wider, agents.x-k8s.io sandboxes rule"
[[ "$(y "$role | [.rules[] | select(.apiGroups[0] == \"agents.x-k8s.io\")] | length" "$TMP/on.out" | grep -vx 0 || true)" == 1 ]] || die "on: not exactly one agents.x-k8s.io rule"
[[ "$(y 'select(.kind == "ClusterRole") | .rules[] | select(.apiGroups[0] == "agents.x-k8s.io") | .resources[0]' "$TMP/on.out")" == "" ]] \
  || die "on: the ClusterRole grants agents.x-k8s.io"
[[ "$(y 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] | select(.name == "WARDYN_K8S_AGENT_SANDBOX") | .value' "$TMP/on.out")" == "true" ]] \
  || die "on: the Deployment lacks WARDYN_K8S_AGENT_SANDBOX=true"
[[ "$(grep -c 'kind: CustomResourceDefinition' "$TMP/on.out")" == 0 ]] || die "on: the chart installs a CRD"
ok "on: pods patch, one sandboxes rule (create,get,list,delete), WARDYN_K8S_AGENT_SANDBOX=true, no CRD"

# A template adds get on that one SandboxTemplate and its env; no other extensions rule.
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.agentSandbox.template=platform >"$TMP/tmpl.out"
[[ "$(y -o=json -I=0 "$role | [.rules[] | select(.apiGroups[0] == \"extensions.agents.x-k8s.io\")]" "$TMP/tmpl.out" | grep -vx '\[\]' || true)" == '[{"apiGroups":["extensions.agents.x-k8s.io"],"resources":["sandboxtemplates"],"resourceNames":["platform"],"verbs":["get"]}]' ]] \
  || die "template: the runner Role has no, or a wider, sandboxtemplates rule"
[[ "$(y 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] | select(.name == "WARDYN_K8S_AGENT_SANDBOX_TEMPLATE") | .value' "$TMP/tmpl.out")" == "platform" ]] \
  || die "template: the Deployment lacks WARDYN_K8S_AGENT_SANDBOX_TEMPLATE=platform"
grep -q sandboxtemplates "$TMP/on.out" && die "on without a template: a sandboxtemplates rule rendered"
ok "template: get on the one sandboxtemplate, WARDYN_K8S_AGENT_SANDBOX_TEMPLATE set"

# Keep: off renders the same bytes as on without the key; on adds only its two rules and its env.
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.agentSandbox.keep.enabled=false >"$TMP/keep-false.out"
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.agentSandbox.keep=null >"$TMP/keep-null.out"
cmp -s "$TMP/on.out" "$TMP/keep-false.out" || die "keep.enabled=false renders differently from keep absent"
cmp -s "$TMP/on.out" "$TMP/keep-null.out" || die "an explicit keep null renders differently from keep absent"
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.agentSandbox.keep.enabled=true >"$TMP/keep.out"
svcrules="$role | [.rules[] | select(.resources[0] == \"services\" or .apiGroups[0] == \"discovery.k8s.io\")]"
[[ "$(y -o=json -I=0 "$svcrules" "$TMP/keep.out")" == "$(y -o=json -I=0 "$svcrules" "$TMP/key-absent.out")" ]] \
  || die "keep: the per-run Service rules differ from the k8s-on render's"
[[ "$(y 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] | select(.name == "WARDYN_K8S_AGENT_SANDBOX_KEEP") | .value' "$TMP/keep.out")" == "true" ]] \
  || die "keep: the Deployment lacks WARDYN_K8S_AGENT_SANDBOX_KEEP=true"
grep -q 'WARDYN_K8S_AGENT_SANDBOX_KEEP' "$TMP/on.out" && die "on without keep: a keep marker rendered"
# Whole documents: keep.out less its env is on.out, so nothing else changed.
y 'del(.spec.template.spec.containers[0].env[] | select(.name == "WARDYN_K8S_AGENT_SANDBOX_KEEP"))' "$TMP/keep.out" >"$TMP/keep-less.out"
y '.' "$TMP/on.out" >"$TMP/on-whole.out"
diff "$TMP/on-whole.out" "$TMP/keep-less.out" >"$TMP/keep-diff" || { cat "$TMP/keep-diff" >&2; die "keep: the render differs from on in more than its env"; }
ok "keep: off (absent, false, null) renders the same bytes; on adds only WARDYN_K8S_AGENT_SANDBOX_KEEP"

# Keep's two admission policies render with k8s.enabled: one per kind, each selected on wardynd's identity, Deny on the runs namespace.
[[ "$(y 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("keep-"))) | .metadata.name' "$TMP/key-absent.out" | sort | tr '\n' ' ')" == "r-wardyn-keep-endpoints r-wardyn-keep-service " ]] \
  || die "keep: not exactly the policies r-wardyn-keep-service and r-wardyn-keep-endpoints"
[[ "$(y 'select(.kind == "ValidatingAdmissionPolicyBinding" and (.metadata.name | test("keep-"))) | .metadata.name' "$TMP/key-absent.out" | sort | tr '\n' ' ')" == "r-wardyn-keep-endpoints r-wardyn-keep-service " ]] \
  || die "keep: not exactly the bindings r-wardyn-keep-service and r-wardyn-keep-endpoints"
for n in keep-service keep-endpoints; do
  [[ "$(y "select(.kind == \"ValidatingAdmissionPolicy\" and .metadata.name == \"r-wardyn-$n\") | .spec.matchConditions[0].expression" "$TMP/key-absent.out")" == "request.userInfo.username == 'system:serviceaccount:default:wardyn-byo'" ]] \
    || die "keep: the $n match condition is not wardynd's authenticated identity"
  [[ "$(y "select(.kind == \"ValidatingAdmissionPolicyBinding\" and .metadata.name == \"r-wardyn-$n\") | .spec.policyName + \" \" + (.spec.validationActions | join(\",\"))" "$TMP/key-absent.out")" == "r-wardyn-$n Deny" ]] \
    || die "keep: the $n binding is not Deny for its policy"
done
[[ "$(y 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "r-wardyn-keep-service") | .spec.matchConstraints.resourceRules[] | .resources | join(",")' "$TMP/key-absent.out")" == "services" ]] \
  || die "keep: the keep-service policy does not match services only"
[[ "$(y 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "r-wardyn-keep-endpoints") | .spec.matchConstraints.resourceRules[] | .resources | join(",")' "$TMP/key-absent.out")" == "endpointslices" ]] \
  || die "keep: the keep-endpoints policy does not match endpointslices only"
# The whole-document compare above leaves these four documents out, so each is read field by field here: its
# apiVersion, the number of keys at each level, and every value but the common labels.
for n in keep-service keep-endpoints; do
  case "$n" in
    keep-service)
      rule='{"apiGroups":[""],"apiVersions":["v1"],"operations":["CREATE","UPDATE","DELETE"],"resources":["services"]}'
      expr="request.operation == 'DELETE' ? oldObject.metadata.name.startsWith('wardyn-proxy-svc-') : (object.metadata.name.startsWith('wardyn-proxy-svc-') && object.spec.type == 'ClusterIP' && (!has(object.spec.selector) || size(object.spec.selector) == 0) && (!has(object.spec.externalIPs) || size(object.spec.externalIPs) == 0))"
      msg="wardynd may make, change or delete only a run's own ClusterIP Service, with no selector and no external IPs" ;;
    keep-endpoints)
      rule='{"apiGroups":["discovery.k8s.io"],"apiVersions":["v1"],"operations":["CREATE","UPDATE","DELETE"],"resources":["endpointslices"]}'
      expr="(request.operation == 'CREATE' || (has(oldObject.metadata.labels) && 'kubernetes.io/service-name' in oldObject.metadata.labels && oldObject.metadata.labels['kubernetes.io/service-name'].startsWith('wardyn-proxy-svc-'))) && (request.operation == 'DELETE' || (has(object.metadata.labels) && 'kubernetes.io/service-name' in object.metadata.labels && object.metadata.labels['kubernetes.io/service-name'].startsWith('wardyn-proxy-svc-')))"
      msg="wardynd may write or delete EndpointSlices only for a run's own Service" ;;
  esac
  kp="select(.kind == \"ValidatingAdmissionPolicy\" and .metadata.name == \"r-wardyn-$n\")"
  kb="select(.kind == \"ValidatingAdmissionPolicyBinding\" and .metadata.name == \"r-wardyn-$n\")"
  [[ "$(y "$kp | [.apiVersion, (. | length), (.metadata | length), .spec.failurePolicy, (.spec | length), (.spec.matchConstraints | length), (.spec.matchConstraints.resourceRules | length), (.spec.matchConditions | length), (.spec.matchConditions[0] | length), .spec.matchConditions[0].name, (.spec.validations | length), (.spec.validations[0] | length)] | join(\",\")" "$TMP/key-absent.out")" == "admissionregistration.k8s.io/v1,4,2,Fail,4,1,1,1,2,requested-by-wardynd,1,2" ]] \
    || die "keep: the $n policy is not v1, fail-closed, with exactly one resource rule, one match condition and one validation"
  [[ "$(y -o=json -I=0 "$kp | .spec.matchConstraints.resourceRules[0]" "$TMP/key-absent.out")" == "$rule" ]] \
    || die "keep: the $n policy's resource rule is not exactly v1 CREATE, UPDATE and DELETE on its one resource"
  [[ "$(y "$kp | .spec.validations[0].expression" "$TMP/key-absent.out")" == "$expr" ]] || die "keep: the $n validation is not the expected expression"
  [[ "$(y "$kp | .spec.validations[0].message" "$TMP/key-absent.out")" == "$msg" ]] || die "keep: the $n validation message changed"
  [[ "$(y "$kb | [.apiVersion, (. | length), (.metadata | length)] | join(\",\")" "$TMP/key-absent.out")" == "admissionregistration.k8s.io/v1,4,2" ]] \
    || die "keep: the $n binding is not v1 with only a name and labels"
  [[ "$(y -o=json -I=0 "$kb | .spec" "$TMP/key-absent.out")" == "{\"policyName\":\"r-wardyn-$n\",\"validationActions\":[\"Deny\"],\"matchResources\":{\"namespaceSelector\":{\"matchLabels\":{\"kubernetes.io/metadata.name\":\"wardyn-runs\"}}}}" ]] \
    || die "keep: the $n binding is not exactly Deny for its policy on the runs namespace"
done
keepdocs='select((.kind == "ValidatingAdmissionPolicy" or .kind == "ValidatingAdmissionPolicyBinding") and (.metadata.name | test("-keep-(service|endpoints)$")))'
y "$keepdocs" "$TMP/key-absent.out" >"$TMP/keepdocs-k8s"
for f in on keep-false keep-null keep; do
  y "$keepdocs" "$TMP/$f.out" | diff "$TMP/keepdocs-k8s" - >/dev/null || die "keep policies differ between the k8s-on render and $f"
done
render "${BASAL[@]}" >"$TMP/k8s-off.out"
if grep -q 'keep-service\|keep-endpoints' "$TMP/k8s-off.out"; then die "keep policy rendered with k8s.enabled=false"; fi
ok "keep: two policies and two bindings, one kind each, on wardynd's identity, bounding deletes; the same whatever the backend; none with k8s off"

# The render-time refusal: the keep policies need Kubernetes 1.30 (a k8s-off render is never refused).
refused "k8s on for Kubernetes 1.29" "needs Kubernetes 1.30 or later" -f "$ALLON" --kube-version 1.29.0
render -f "$ALLON" --kube-version 1.30.0 >"$TMP/k130.out" || die "k8s on for Kubernetes 1.30: refused, want a render"
[[ "$(y 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("-keep-(service|endpoints)$"))) | .metadata.name' "$TMP/k130.out" | sort | tr '\n' ' ')" == "r-wardyn-keep-endpoints r-wardyn-keep-service " ]] \
  || die "k8s on for Kubernetes 1.30: not both keep policies"
render "${BASAL[@]}" --kube-version 1.29.0 >/dev/null || die "k8s off for Kubernetes 1.29: refused, want a render"
ok "k8s on: refused below Kubernetes 1.30, renders both policies at 1.30; k8s off renders at 1.29"

# Nothing else changed: the added lines are the rule, the verb, the env and the two policy objects.
diff "$TMP/key-absent.out" "$TMP/on.out" | grep '^<' | grep -v 'verbs: \["create", "get", "list", "watch", "delete", "deletecollection"\]' \
  | grep -v -e '^< *$' -e '^< # Source:' >"$TMP/removed" || true
[[ ! -s "$TMP/removed" ]] || { cat "$TMP/removed" >&2; die "on: the backend removed or changed lines other than the pods verbs"; }
ok "on: the diff to off only adds lines (apart from the pods verb list)"

# The policy: present only when on. This test reads the rendered CEL as text; it cannot evaluate it. The
# decisions it must reach (kind job cases) are listed in the comment block below.
#   admitted: controller identity, gate + Sandbox owner + empty nodeName
#   denied:   controller identity, gate and owner reference both removed by a hook
#   denied:   controller identity, gate kept, spec.nodeName set
#   denied:   controller identity, owner kept, gate removed
#   not selected: any other identity (wardynd's proxy and canary pods, kubectl), whatever the pod holds
[[ "$(y 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("-keep-(service|endpoints)$") | not)) | .metadata.name' "$TMP/on.out" | sort | tr '\n' ' ')" == "r-wardyn-sandbox-pod-gate r-wardyn-sandbox-pod-ungate " ]] \
  || die "on: no ValidatingAdmissionPolicy r-wardyn-sandbox-pod-gate"
vap='select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "r-wardyn-sandbox-pod-gate")'
[[ "$(y "$vap | .spec.failurePolicy" "$TMP/on.out")" == "Fail" ]] || die "on: the policy does not fail closed"
[[ "$(y "$vap | .spec.matchConstraints.resourceRules[0] | [.apiGroups[0], .apiVersions[0], .operations[0], .resources[0]] | join(\",\")" "$TMP/on.out")" == ",v1,CREATE,pods" ]] \
  || die "on: the policy does not match pod CREATE only"
# Selection: the authenticated requester alone. A pod field in a match condition is one a hook can remove.
[[ "$(y "$vap | .spec.matchConditions | length" "$TMP/on.out")" == 1 ]] || die "on: not exactly one match condition"
[[ "$(y "$vap | .spec.matchConditions[0].expression" "$TMP/on.out")" == "request.userInfo.username == 'system:serviceaccount:agent-sandbox-system:agent-sandbox-controller'" ]] \
  || die "on: the match condition is not the controller's authenticated identity alone"
conds="$(y "$vap | .spec.matchConditions[].expression" "$TMP/on.out")"
[[ "$conds" != *object.* ]] || die "on: a match condition reads the pod, which an admission hook can rewrite"
# Validation: each field a hook could remove, checked after mutation.
[[ "$(y "$vap | .spec.validations | length" "$TMP/on.out")" == 3 ]] || die "on: not exactly three validations"
[[ "$(y "$vap | .spec.validations[0].expression" "$TMP/on.out")" == "has(object.spec.schedulingGates) && object.spec.schedulingGates.exists(g, g.name == 'wardyn.dev/verify')" ]] \
  || die "on: validation 0 is not the wardyn.dev/verify gate"
[[ "$(y "$vap | .spec.validations[1].expression" "$TMP/on.out")" == "has(object.metadata.ownerReferences) && object.metadata.ownerReferences.exists(o, o.kind == 'Sandbox' && o.apiVersion.startsWith('agents.x-k8s.io/'))" ]] \
  || die "on: validation 1 is not the Sandbox owner reference"
[[ "$(y "$vap | .spec.validations[2].expression" "$TMP/on.out")" == "!has(object.spec.nodeName) || object.spec.nodeName == ''" ]] \
  || die "on: validation 2 is not an empty nodeName"
bind='select(.kind == "ValidatingAdmissionPolicyBinding" and .spec.policyName == "r-wardyn-sandbox-pod-gate")'
[[ "$(y "$bind | .spec.policyName + \" \" + .spec.validationActions[0] + \" \" + .spec.matchResources.namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"]" "$TMP/on.out")" == "r-wardyn-sandbox-pod-gate Deny wardyn-runs" ]] \
  || die "on: the binding is not Deny on the runs namespace for the policy"
ok "on: policy selects on the controller identity alone; gate, Sandbox owner and empty nodeName are validations; Deny, runs namespace"

# The ungate policy (UPDATE). Rendered CEL text only. The decisions the kind job must reach:
#   admitted: wardynd's identity removes the gate, nothing else in the write changed
#   denied:   wardynd's identity removes the gate and the same write changes a label (wardyn.component)
#   denied:   wardynd's identity removes the gate and the same write changes a container or init container image
#   denied:   wardynd's identity removes the gate and the same write changes spec.nodeSelector, spec.affinity or an annotation
#   denied:   wardynd's identity removes the gate and the same write sets spec.nodeName, changes tolerations,
#             activeDeadlineSeconds or owner references, or adds an ephemeral container
#   denied:   any other identity (the controller's included) removes the gate
#   denied:   the gate removed from a pod with no Sandbox owner
#   not selected: an UPDATE of a pod whose stored object holds no wardyn.dev/verify gate
un='select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "r-wardyn-sandbox-pod-ungate")'
[[ "$(y "$un | .spec.failurePolicy" "$TMP/on.out")" == "Fail" ]] || die "on: the ungate policy does not fail closed"
[[ "$(y "$un | .spec.matchConstraints.resourceRules[0] | [.operations[0], .resources[0], (.operations | length), (.resources | length)] | join(\",\")" "$TMP/on.out")" == "UPDATE,pods,1,1" ]] \
  || die "on: the ungate policy does not match pod UPDATE (main resource) only"
[[ "$(y "$un | .spec.matchConditions | length" "$TMP/on.out")" == 1 ]] || die "on: not exactly one ungate match condition"
[[ "$(y "$un | .spec.matchConditions[0].expression" "$TMP/on.out")" == "has(oldObject.spec.schedulingGates) && oldObject.spec.schedulingGates.exists(g, g.name == 'wardyn.dev/verify')" ]] \
  || die "on: the ungate policy does not select on the STORED pod holding the gate (oldObject alone)"
[[ "$(y "$un | .spec.validations | length" "$TMP/on.out")" == 3 ]] || die "on: not exactly three ungate validations"
[[ "$(y "$un | .spec.validations[0].expression" "$TMP/on.out")" == "variables.gateHeld || request.userInfo.username == 'system:serviceaccount:default:wardyn-byo'" ]] \
  || die "on: a gate removal is not restricted to wardynd's own identity"
[[ "$(y "$un | .spec.validations[1].expression" "$TMP/on.out")" == "variables.gateHeld || (has(oldObject.metadata.ownerReferences) && oldObject.metadata.ownerReferences.exists(o, o.kind == 'Sandbox' && o.apiVersion.startsWith('agents.x-k8s.io/')))" ]] \
  || die "on: a gate removal from a pod with no Sandbox owner is not denied"
[[ "$(y "$un | .spec.validations[2].expression" "$TMP/on.out")" == "variables.gateHeld || variables.unchanged" ]] \
  || die "on: a gate removal is not held to variables.unchanged"
[[ "$(y "$un | .spec.variables[0].expression" "$TMP/on.out")" == "has(object.spec.schedulingGates) && object.spec.schedulingGates.exists(g, g.name == 'wardyn.dev/verify')" ]] \
  || die "on: variables.gateHeld is not the gate on the new object"
unchanged="$(y "$un | .spec.variables[1].expression" "$TMP/on.out")"
for tok in 'oldObject.metadata.labels == object.metadata.labels' 'oldObject.metadata.ownerReferences == object.metadata.ownerReferences' \
  'oldObject.spec.containers.map(c, c.image) == object.spec.containers.map(c, c.image)' \
  'oldObject.spec.initContainers.map(c, c.image) == object.spec.initContainers.map(c, c.image)' \
  'oldObject.spec.tolerations == object.spec.tolerations' 'oldObject.spec.nodeSelector == object.spec.nodeSelector' \
  'oldObject.spec.affinity == object.spec.affinity' 'oldObject.metadata.annotations == object.metadata.annotations' 'oldObject.spec.activeDeadlineSeconds == object.spec.activeDeadlineSeconds' \
  "object.spec.nodeName == ''" 'size(object.spec.ephemeralContainers) == 0'; do
  [[ "$unchanged" == *"$tok"* ]] || die "on: variables.unchanged does not compare: $tok"
done
[[ "$(y "$un | .spec.variables | length" "$TMP/on.out")" == 2 ]] || die "on: the ungate policy has other variables than gateHeld and unchanged"
[[ "$(y 'select(.kind == "ValidatingAdmissionPolicyBinding" and .spec.policyName == "r-wardyn-sandbox-pod-ungate") | .spec.validationActions[0] + " " + .spec.matchResources.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]' "$TMP/on.out")" == "Deny wardyn-runs" ]] \
  || die "on: the ungate binding is not Deny on the runs namespace"
[[ "$(grep -c '^kind: ValidatingAdmissionPolic' "$TMP/on.out")" == 8 ]] || die "on: not exactly the backend's two policies and two bindings plus keep's"
ok "on: ungate policy selects on the stored gate; only wardynd removes it; labels, annotations, owners, images, tolerations, nodeSelector, affinity, deadline, node, ephemeral containers unchanged"

# The controller identity is a value, and may not be wardynd's own ServiceAccount.
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.agentSandbox.controller.namespace=as-sys --set k8s.agentSandbox.controller.serviceAccount=ctl >"$TMP/ctl.out"
[[ "$(y "$vap | .spec.matchConditions[0].expression" "$TMP/ctl.out")" == "request.userInfo.username == 'system:serviceaccount:as-sys:ctl'" ]] \
  || die "on: controller.namespace and .serviceAccount do not reach the match condition"
ok "on: the controller identity values reach the match condition"
refused "controller identity is wardynd's own ServiceAccount" "is wardynd's own ServiceAccount" -f "$ALLON" --set k8s.agentSandbox.enabled=true \
  --set k8s.agentSandbox.controller.namespace=default --set k8s.agentSandbox.controller.serviceAccount=wardyn-byo -n default
refused "controller identity with a quote in it" "must be Kubernetes names" -f "$ALLON" --set k8s.agentSandbox.enabled=true --set "k8s.agentSandbox.controller.serviceAccount=x' || true || '"
refused "keep without the backend" "k8s.agentSandbox.keep.enabled needs k8s.agentSandbox.enabled=true" -f "$ALLON" --set k8s.agentSandbox.keep.enabled=true
refused "template without the backend" "k8s.agentSandbox.template needs k8s.agentSandbox.enabled=true" -f "$ALLON" --set k8s.agentSandbox.template=platform

# A second render in the release namespace (no runsNamespace) binds that namespace.
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.runsNamespace= --set k8s.allowRunsInReleaseNamespace=true -n relns >"$TMP/rel.out"
[[ "$(y "$bind | .spec.matchResources.namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"]" "$TMP/rel.out")" == "relns" ]] \
  || die "on: with no runsNamespace the binding is not the release namespace"
ok "on: no runsNamespace binds the release namespace"

# Refusals, each by its message.
refused "on without k8s.enabled" "k8s.agentSandbox.enabled needs k8s.enabled=true" "${BASAL[@]}" --set k8s.agentSandbox.enabled=true
refused "on with k8s.allowUnenforcedNetPol" "refused with the NetworkPolicy override" -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.allowUnenforcedNetPol=true
refused "on with env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL" "refused with the NetworkPolicy override" -f "$ALLON" --set k8s.agentSandbox.enabled=true --set-string env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1
refused "on with extraEnv WARDYN_K8S_ALLOW_UNENFORCED_NETPOL" "refused with the NetworkPolicy override" -f "$ALLON" --set k8s.agentSandbox.enabled=true --set 'extraEnv[0].name=WARDYN_K8S_ALLOW_UNENFORCED_NETPOL' --set-string 'extraEnv[0].value=true'
refused "on with the override from a valueFrom configMapKeyRef" "set through extraEnv valueFrom" -f "$ALLON" --set k8s.agentSandbox.enabled=true \
  --set 'extraEnv[0].name=WARDYN_K8S_ALLOW_UNENFORCED_NETPOL' --set 'extraEnv[0].valueFrom.configMapKeyRef.name=cm' --set 'extraEnv[0].valueFrom.configMapKeyRef.key=k'
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set 'extraEnv[0].name=SOMETHING_ELSE' --set 'extraEnv[0].valueFrom.configMapKeyRef.name=cm' --set 'extraEnv[0].valueFrom.configMapKeyRef.key=k' >/dev/null \
  || die "a valueFrom on an unrelated variable refused an otherwise valid on render"
refused "on with k8s.ackAmbientDefaultDeny" "refused with the NetworkPolicy override k8s.ackAmbientDefaultDeny" -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.ackAmbientDefaultDeny=true
refused "on with env.WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY" "refused with the NetworkPolicy override k8s.ackAmbientDefaultDeny" -f "$ALLON" --set k8s.agentSandbox.enabled=true --set-string env.WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1
refused "on with the ack override from a valueFrom configMapKeyRef" "WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY is set through extraEnv valueFrom" -f "$ALLON" --set k8s.agentSandbox.enabled=true \
  --set 'extraEnv[0].name=WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY' --set 'extraEnv[0].valueFrom.configMapKeyRef.name=cm' --set 'extraEnv[0].valueFrom.configMapKeyRef.key=k'
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set-string env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=0 >/dev/null \
  || die "an explicit false override refused an otherwise valid on render"
ok "an explicit false override is not refused"
helm lint "$CHART" -f "$ALLON" --set k8s.agentSandbox.enabled=true >/dev/null
ok "helm lint with the backend on"
