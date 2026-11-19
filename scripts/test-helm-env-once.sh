#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-helm-env-once.sh — wardynd's container env must carry each name once. `helm upgrade` patches a Deployment's env
# by name, and an upgrade that takes one of two same-named entries away deletes the name outright; with the Recreate
# strategy the new pod then starts without it. Asserts:
#   - every literal chart entry between `env:` and extraEnv tests $eff right above it (the one check on template text:
#     it keeps an entry added later from rendering beside an env or extraEnv entry of its name);
#   - one entry, with the value of the entry the chart rendered last when it rendered both, for: a chart setting plus
#     extraEnv, a key-service setting plus extraEnv, env plus extraEnv, extraEnv twice, k8s runner names in env and
#     extraEnv, and the metrics listener in extraEnv;
#   - the all-on render repeats no name, and an unrelated extraEnv entry adds exactly itself to the default env;
#   - when BASE is a git rev, the default and all-on renders are byte-identical to that commit's chart.
# `make helm-lint` runs it. Needs helm and yq (mikefarah v4).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"
cd "$ROOT"

CHART=deploy/helm/wardyn
ALLON="$CHART/ci/all-on-values.yaml"
BASAL=(--set auth.adminToken.secretRef.name=wardyn-auth --set secrets.ageKeyFromSecret=true)

# The entries after extraEnv live in wardyn.envAfterExtra, below `ports:`, so they are outside this range.
# shellcheck disable=SC2016
awk '
/^          env:$/ { on = 1; next }
/^          ports:$/ { on = 0 }
!on { next }
/^            - name: [A-Z0-9_]+$/ {
  n = $3; ok = 0
  for (i = NR - 1; i in L && (L[i] ~ /^ *\{\{/ || L[i] ~ /^ *#/); i--) if (index(L[i], "hasKey $eff \"" n "\"")) ok = 1
  if (!ok) { print FILENAME ":" NR ": " n; bad = 1 }
}
{ L[NR] = $0 }
END { exit bad }' "$CHART/templates/deployment.yaml" >&2 \
  || die "a chart env entry above does not test \$eff: an env or extraEnv entry of its name would render beside it"
echo "ok  every chart env entry before extraEnv tests \$eff"

command -v helm >/dev/null || die "helm not installed: this test cannot run, and a skip proves nothing"
command -v yq >/dev/null || die "yq not installed: this test cannot run, and a skip proves nothing"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
ok() { echo "ok  $1"; }
render() { helm template r "$CHART" "$@"; }
# yq prints a blank line for each document a select drops; keep only real output.
y() { yq -N "$@" | sed '/^$/d'; }
names() { y 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[].name' "$1"; }
value() { y "select(.kind == \"Deployment\") | .spec.template.spec.containers[0].env[] | select(.name == \"$2\") | .value" "$1"; }
once() { # <label> <render file> <name> <wanted value>
  local n
  n="$(names "$2" | grep -cx "$3" || true)"
  [[ "$n" == 1 ]] || die "$1: $3 renders $n times, want once"
  [[ "$(value "$2" "$3")" == "$4" ]] || die "$1: $3 is $(value "$2" "$3"), want $4"
  ok "$1"
}

render "${BASAL[@]}" --set 'extraEnv[0].name=WARDYN_RECORDING_STORE' --set 'extraEnv[0].value=pg' >"$TMP/setting.out"
once "persistence's WARDYN_RECORDING_STORE plus an extraEnv one: the extraEnv value" "$TMP/setting.out" WARDYN_RECORDING_STORE pg

render "${BASAL[@]}" --set kek.required=true --set 'extraEnv[0].name=WARDYN_KEK_REQUIRED' --set-string 'extraEnv[0].value=false' >"$TMP/kek.out"
once "kek.required plus an extraEnv WARDYN_KEK_REQUIRED: the extraEnv value" "$TMP/kek.out" WARDYN_KEK_REQUIRED false

render "${BASAL[@]}" --set-string env.WARDYN_HOST_MAX_LOAD1=50 --set 'extraEnv[0].name=WARDYN_HOST_MAX_LOAD1' --set-string 'extraEnv[0].value=100' >"$TMP/env.out"
once "env plus extraEnv of one name: the extraEnv value" "$TMP/env.out" WARDYN_HOST_MAX_LOAD1 100

render "${BASAL[@]}" --set 'extraEnv[0].name=WARDYN_ENV_ONCE' --set 'extraEnv[0].value=a' --set 'extraEnv[1].name=WARDYN_ENV_ONCE' --set 'extraEnv[1].value=b' >"$TMP/twice.out"
once "extraEnv naming one variable twice: the last entry" "$TMP/twice.out" WARDYN_ENV_ONCE b

render "${BASAL[@]}" --set metrics.listener.enabled=true --set 'extraEnv[0].name=WARDYN_METRICS_LISTEN' --set-string 'extraEnv[0].value=:1' >"$TMP/metrics.out"
once "metrics.listener plus an extraEnv WARDYN_METRICS_LISTEN: the chart value" "$TMP/metrics.out" WARDYN_METRICS_LISTEN :9464

# A values file, not --set: --set on extraEnv would replace the all-on file's own extraEnv list.
render -f "$ALLON" >"$TMP/allon.out"
[[ -z "$(names "$TMP/allon.out" | sort | uniq -d)" ]] || die "the all-on render repeats an env name: $(names "$TMP/allon.out" | sort | uniq -d | tr '\n' ' ')"
ok "all-on render: no repeated env name"
cat >"$TMP/k8s.yaml" <<'EOF'
env:
  WARDYN_K8S_NAMESPACE: from-env
extraEnv:
  - name: WARDYN_OIDC_CLIENT_SECRET
    valueFrom:
      secretKeyRef:
        name: wardyn-oidc
        key: client-secret
  - name: WARDYN_RUNNER
    value: docker
EOF
render -f "$ALLON" -f "$TMP/k8s.yaml" >"$TMP/k8s.out"
once "k8s.enabled plus an extraEnv WARDYN_RUNNER: the chart value" "$TMP/k8s.out" WARDYN_RUNNER k8s
once "k8s.enabled plus an env WARDYN_K8S_NAMESPACE: the chart value" "$TMP/k8s.out" WARDYN_K8S_NAMESPACE "$(value "$TMP/allon.out" WARDYN_K8S_NAMESPACE)"

render "${BASAL[@]}" >"$TMP/default.out"
render "${BASAL[@]}" --set 'extraEnv[0].name=WARDYN_ENV_ONCE_PROBE' --set 'extraEnv[0].value=x' >"$TMP/probe.out"
[[ "$(names "$TMP/probe.out")" == "$(names "$TMP/default.out")"$'\n'WARDYN_ENV_ONCE_PROBE ]] \
  || die "an unrelated extraEnv entry changed the rest of the default env, or did not render last"
ok "an unrelated extraEnv entry adds only itself"

# Byte-identity to the commit before this change, when the caller names one.
if [[ -n "${BASE:-}" ]]; then
  mkdir -p "$TMP/base"
  git archive "$BASE" "$CHART" | tar -x -C "$TMP/base"
  for args in "${BASAL[*]}" "-f $ALLON"; do
    # shellcheck disable=SC2086
    helm template r "$TMP/base/$CHART" $args >"$TMP/base.out"
    # shellcheck disable=SC2086
    helm template r "$CHART" $args >"$TMP/head.out"
    cmp -s "$TMP/base.out" "$TMP/head.out" || { diff "$TMP/base.out" "$TMP/head.out" | head -20 >&2; die "render differs from $BASE under: $args"; }
  done
  ok "default and all-on renders are byte-identical to $BASE"
else
  echo "note: BASE not set, byte-identity to a base commit not checked (BASE=<rev> bash $0)"
fi
