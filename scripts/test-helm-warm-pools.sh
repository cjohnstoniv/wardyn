#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-helm-warm-pools.sh — the experimental warm pools (k8s.warmPools) must be OFF by default and cost nothing off.
# Asserts on rendered output, not on the templates' text:
#   - no warmPools (key absent, null, empty list) renders the same bytes as one another, with no PriorityClass and
#     no WARDYN_K8S_WARM_POOLS or WARDYN_K8S_WARM_PRIORITY_CLASS;
#   - one pool renders the env as JSON that round-trips to the pool, and a PriorityClass with value -10 and
#     preemptionPolicy Never, named in the env;
#   - create=false with a name renders no PriorityClass, and the env names that class;
#   - warmPools without k8s.enabled is refused.
# `make helm-lint` runs it. Needs helm and yq (mikefarah v4).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

CHART=deploy/helm/wardyn
ALLON="$CHART/ci/all-on-values.yaml"
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
envval() { # <name> <render file>
  y "select(.kind == \"Deployment\") | .spec.template.spec.containers[0].env[] | select(.name == \"$1\") | .value" "$2"
}

# Off: absent, null and an empty list are the same bytes, and carry no marker.
render -f "$ALLON" >"$TMP/absent.out"
render -f "$ALLON" --set k8s.warmPools=null >"$TMP/null.out"
render -f "$ALLON" --set 'k8s.warmPools=[]' >"$TMP/empty.out"
cmp -s "$TMP/absent.out" "$TMP/null.out" || die "an explicit null renders differently from the default"
cmp -s "$TMP/absent.out" "$TMP/empty.out" || die "an empty list renders differently from the default"
grep -qx 'kind: Role' "$TMP/absent.out" && grep -q 'WARDYN_K8S_READ_NODES\|name: WARDYN_RUNNER' "$TMP/absent.out" \
  || die "the all-on render has no k8s Role or env: the absence checks below would pass on nothing"
if grep -qE 'PriorityClass|WARDYN_K8S_WARM_' "$TMP/absent.out"; then die "off: a warm pool marker rendered"; fi
ok "off (absent, null, empty): identical renders, no PriorityClass, no WARDYN_K8S_WARM_* env"

# One pool: the env round-trips, and the PriorityClass is below every run and never preempts.
POOLS='[{"name":"small","image":"example.test/agent:1","class":"CC1","cpu":"500m","memory":"1Gi","size":2}]'
render -f "$ALLON" --set-json "k8s.warmPools=$POOLS" >"$TMP/one.out"
[[ "$(envval WARDYN_K8S_WARM_POOLS "$TMP/one.out" | yq -o=json -I=0 '.')" == "$POOLS" ]] || die "one pool: WARDYN_K8S_WARM_POOLS does not round-trip to the pool"
pc='select(.kind == "PriorityClass")'
[[ "$(y "$pc | .value" "$TMP/one.out")" == "-10" ]] || die "one pool: the PriorityClass value is not -10"
[[ "$(y "$pc | .preemptionPolicy" "$TMP/one.out")" == "Never" ]] || die "one pool: the PriorityClass preemptionPolicy is not Never"
[[ "$(y "$pc | .globalDefault" "$TMP/one.out")" == "false" ]] || die "one pool: the PriorityClass is a global default"
[[ "$(y "$pc | .metadata.name" "$TMP/one.out")" == "r-wardyn-warm-placeholder" ]] || die "one pool: the PriorityClass has the wrong default name"
[[ "$(envval WARDYN_K8S_WARM_PRIORITY_CLASS "$TMP/one.out")" == "r-wardyn-warm-placeholder" ]] || die "one pool: the env does not name the rendered PriorityClass"
[[ "$(grep -c '^kind: PriorityClass' "$TMP/one.out")" == 1 ]] || die "one pool: not exactly one PriorityClass"
ok "one pool: env round-trips; PriorityClass value -10, preemptionPolicy Never, named in the env"

# An existing class: nothing rendered, and the env names it.
render -f "$ALLON" --set-json "k8s.warmPools=$POOLS" --set k8s.warmPoolPriorityClass.create=false --set k8s.warmPoolPriorityClass.name=platform-low >"$TMP/named.out"
if grep -q '^kind: PriorityClass' "$TMP/named.out"; then die "create=false: a PriorityClass rendered"; fi
[[ "$(envval WARDYN_K8S_WARM_PRIORITY_CLASS "$TMP/named.out")" == "platform-low" ]] || die "create=false: the env does not name the existing class"
ok "create=false with a name: no PriorityClass, env names it"

refused "warmPools without k8s.enabled" "k8s.warmPools needs k8s.enabled=true" "${BASAL[@]}" --set-json "k8s.warmPools=$POOLS"
helm lint "$CHART" -f "$ALLON" --set-json "k8s.warmPools=$POOLS" >/dev/null
ok "helm lint with a pool"
