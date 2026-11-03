#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-helm-agent-sandbox.sh — the experimental agent-sandbox backend's switch (k8s.agentSandbox.enabled, #3863)
# must be OFF by default and cost nothing off. Asserts on rendered output, not on the templates' text:
#   - off (key absent, null, false, the 0.8.5 reuse-values file) renders the same bytes as one another, with no
#     agents.x-k8s.io rule, no pods patch, no admissionregistration object, no CRD and no WARDYN_K8S_AGENT_SANDBOX;
#   - when BASE is a git rev (the lane check sets it to the commit before the key existed), the off render is
#     byte-identical to that commit's chart under the same values;
#   - on renders exactly: pods patch, a sandboxes create/get/list/delete rule, the env, and a
#     ValidatingAdmissionPolicy + binding requiring the wardyn.dev/verify gate; still no CRD, no other new rule;
#   - on without k8s.enabled, or with the unenforced-NetworkPolicy override (value or env or extraEnv), is refused.
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
  if grep -qE 'agents\.x-k8s\.io|WARDYN_K8S_AGENT_SANDBOX|admissionregistration|ValidatingAdmissionPolic|CustomResourceDefinition|sandboxes' "$f"; then
    grep -nE 'agents\.x-k8s\.io|WARDYN_K8S_AGENT_SANDBOX|admissionregistration|ValidatingAdmissionPolic|CustomResourceDefinition|sandboxes' "$f" >&2
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

render -f "$REUSE" "${BASAL[@]}" >"$TMP/reuse.out"
absent "0.8.5 reuse-values render" "$TMP/reuse.out"
ok "off under the 0.8.5 reuse-values file: no marker"

# Byte-identity to the commit before the key existed, when the caller names one.
if [[ -n "${BASE:-}" ]]; then
  mkdir -p "$TMP/base"
  git archive "$BASE" "$CHART" | tar -x -C "$TMP/base"
  for args in "-f $ALLON" "-f $REUSE ${BASAL[*]}" "${BASAL[*]}"; do
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

# On: exactly the backend's additions.
render -f "$ALLON" --set k8s.agentSandbox.enabled=true >"$TMP/on.out"
role='select(.kind == "Role" and (.metadata.name | test("k8s-runner$")))'
[[ "$(y "$role | .rules[] | select(.resources[0] == \"pods\" and (.apiGroups[0] == \"\")) | .verbs | join(\",\")" "$TMP/on.out" | head -1)" == "create,get,list,delete,deletecollection,patch" ]] \
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

# Nothing else changed: the added lines are the rule, the verb, the env and the two policy objects.
diff "$TMP/key-absent.out" "$TMP/on.out" | grep '^<' | grep -v 'verbs: \["create", "get", "list", "delete", "deletecollection"\]' \
  | grep -v -e '^< *$' -e '^< # Source:' >"$TMP/removed" || true
[[ ! -s "$TMP/removed" ]] || { cat "$TMP/removed" >&2; die "on: the backend removed or changed lines other than the pods verbs"; }
ok "on: the diff to off only adds lines (apart from the pods verb list)"

# The policy: present only when on, with the expressions lane 0 passed on kind v1.30.
[[ "$(y 'select(.kind == "ValidatingAdmissionPolicy") | .metadata.name' "$TMP/on.out")" == "r-wardyn-sandbox-pod-gate" ]] \
  || die "on: no ValidatingAdmissionPolicy r-wardyn-sandbox-pod-gate"
vap='select(.kind == "ValidatingAdmissionPolicy")'
[[ "$(y "$vap | .spec.failurePolicy" "$TMP/on.out")" == "Fail" ]] || die "on: the policy does not fail closed"
[[ "$(y "$vap | .spec.matchConstraints.resourceRules[0] | [.apiGroups[0], .apiVersions[0], .operations[0], .resources[0]] | join(\",\")" "$TMP/on.out")" == ",v1,CREATE,pods" ]] \
  || die "on: the policy does not match pod CREATE only"
[[ "$(y "$vap | .spec.matchConditions[0].expression" "$TMP/on.out")" == "has(object.metadata.ownerReferences) && object.metadata.ownerReferences.exists(o, o.kind == 'Sandbox' && o.apiVersion.startsWith('agents.x-k8s.io/'))" ]] \
  || die "on: the match condition is not the one lane 0 passed on kind"
[[ "$(y "$vap | .spec.validations[0].expression" "$TMP/on.out")" == "has(object.spec.schedulingGates) && object.spec.schedulingGates.exists(g, g.name == 'wardyn.dev/verify')" ]] \
  || die "on: the validation is not the one lane 0 passed on kind"
bind='select(.kind == "ValidatingAdmissionPolicyBinding")'
[[ "$(y "$bind | .spec.policyName + \" \" + .spec.validationActions[0] + \" \" + .spec.matchResources.namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"]" "$TMP/on.out")" == "r-wardyn-sandbox-pod-gate Deny wardyn-runs" ]] \
  || die "on: the binding is not Deny on the runs namespace for the policy"
ok "on: ValidatingAdmissionPolicy + binding, fail closed, pod CREATE, the lane 0 expressions, bound to the runs namespace"

# A second render in the release namespace (no runsNamespace) binds that namespace.
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.runsNamespace= --set k8s.allowRunsInReleaseNamespace=true -n relns >"$TMP/rel.out"
[[ "$(y "$bind | .spec.matchResources.namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"]" "$TMP/rel.out")" == "relns" ]] \
  || die "on: with no runsNamespace the binding is not the release namespace"
ok "on: no runsNamespace binds the release namespace"

# Refusals, each by its message.
refused "on without k8s.enabled" "k8s.agentSandbox.enabled needs k8s.enabled=true" "${BASAL[@]}" --set k8s.agentSandbox.enabled=true
refused "on with k8s.allowUnenforcedNetPol" "refused with the unenforced-NetworkPolicy override" -f "$ALLON" --set k8s.agentSandbox.enabled=true --set k8s.allowUnenforcedNetPol=true
refused "on with env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL" "refused with the unenforced-NetworkPolicy override" -f "$ALLON" --set k8s.agentSandbox.enabled=true --set-string env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1
refused "on with extraEnv WARDYN_K8S_ALLOW_UNENFORCED_NETPOL" "refused with the unenforced-NetworkPolicy override" -f "$ALLON" --set k8s.agentSandbox.enabled=true --set 'extraEnv[0].name=WARDYN_K8S_ALLOW_UNENFORCED_NETPOL' --set-string 'extraEnv[0].value=true'
render -f "$ALLON" --set k8s.agentSandbox.enabled=true --set-string env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=0 >/dev/null \
  || die "an explicit false override refused an otherwise valid on render"
ok "an explicit false override is not refused"
helm lint "$CHART" -f "$ALLON" --set k8s.agentSandbox.enabled=true >/dev/null
ok "helm lint with the backend on"
