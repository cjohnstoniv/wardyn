#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/common.sh
source "$ROOT/scripts/lib/common.sh"
for tool in yq jq; do command -v "$tool" >/dev/null || die "$tool is required"; done
umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export FIX="$work/fixture"
mkdir -p "$work/bin" "$work/tmp" "$FIX"
export TMPDIR="$work/tmp"

# Never fall through to a real kubectl, even on an unexpected command.
cat >"$work/bin/kubectl" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
jq -cn --args '$ARGS.positional' -- "$@" >>"$FIX/calls.jsonl"
args=("$@")
if [ "${1:-}" = --context ]; then shift 2; fi
case "${1:-}" in
  get)
    [ "$#" = 3 ] && [ "$2" = --raw ] || exit 90
    [ "$3" != "${FAIL_DISCOVERY:-}" ] || { echo "discovery fixture failure" >&2; exit 42; }
    cat "$FIX/${3//\//_}.json" ;;
  apply)
    file=""; dry=0; strict=0
    shift
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --dry-run=server) dry=$((dry+1)); shift ;;
        --validate=strict) strict=$((strict+1)); shift ;;
        --namespace) shift 2 ;;
        -f) file=$2; shift 2 ;;
        *) exit 91 ;;
      esac
    done
    [ "$dry" = 1 ] && [ "$strict" = 1 ] && [ -r "$file" ] || exit 92
    [ "$(stat -c %a "$file")" = 600 ] && [ "$(stat -c %a "$(dirname "$file")")" = 700 ] || exit 93
    jq -ce --args '{args: $ARGS.positional, body: .}' -- "${args[@]}" <"$file" >>"$FIX/applies.jsonl"
    [ "${FAIL_APPLY:-0}" = 0 ] || { echo "apply fixture failure" >&2; exit 43; } ;;
  *) echo "unexpected kubectl call" >&2; exit 94 ;;
esac
SHIM
chmod +x "$work/bin/kubectl"
export PATH="$work/bin:$PATH"

cat >"$FIX/_api_v1.json" <<'JSON'
{"resources":[{"name":"configmaps","kind":"ConfigMap","namespaced":true},{"name":"configmaps/status","kind":"ConfigMap","namespaced":true},{"name":"secrets","kind":"Secret","namespaced":true},{"name":"namespaces","kind":"Namespace","namespaced":false}]}
JSON
cat >"$FIX/_apis_rbac.authorization.k8s.io_v1.json" <<'JSON'
{"resources":[{"name":"roles","kind":"Role","namespaced":true},{"name":"rolebindings","kind":"RoleBinding","namespaced":true},{"name":"clusterroles","kind":"ClusterRole","namespaced":false}]}
JSON
cat >"$FIX/_apis_example.test_v1.json" <<'JSON'
{"resources":[{"name":"widgets","kind":"Widget","namespaced":true},{"name":"widgets/status","kind":"Widget","namespaced":true},{"name":"clusterwidgets","kind":"ClusterWidget","namespaced":false}]}
JSON
cat >"$work/mixed.yaml" <<'YAML'
---
# An empty separator must not count as an object.
---
apiVersion: v1
kind: ConfigMap
metadata: {name: defaulted}
data: {separator: "---"}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: runner-role, namespace: wardyn-runs}
rules: []
---
apiVersion: v1
kind: Secret
metadata: {name: explicit, namespace: wardyn}
stringData: {token: fixture-only}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: runner-binding, namespace: wardyn-runs}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: runner-role}
subjects: [{kind: ServiceAccount, name: wardyn, namespace: wardyn}]
---
apiVersion: v1
kind: Namespace
metadata: {name: wardyn-runs}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: cluster-reader}
rules: []
---
apiVersion: example.test/v1
kind: Widget
metadata: {name: custom, namespace: team-a}
spec: {value: unchanged}
---
apiVersion: example.test/v1
kind: ClusterWidget
metadata: {name: custom-cluster, namespace: preserve-this}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: null-namespace, namespace: null}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: empty-namespace, namespace: ""}
---
YAML
run_check() {
  : >"$FIX/calls.jsonl"
  : >"$FIX/applies.jsonl"
  rc=0
  "$ROOT/scripts/check-helm-schema.sh" "$@" >"$FIX/out" 2>&1 || rc=$?
  [ -z "$(ls -A "$TMPDIR")" ] || die "helper left temporary files behind"
}
assert() { local label=$1; shift; "$@" >/dev/null || { cat "$FIX/out" >&2; die "$label"; }; echo "ok: $label"; }
reject() {
  local label=$1 pattern=$2
  assert "$label refuses" test "$rc" -ne 0
  assert "$label explains refusal" grep -qE "$pattern" "$FIX/out"
  assert "$label never applies" test ! -s "$FIX/applies.jsonl"
}

run_check "$work/mixed.yaml" wardyn fixture-context
assert "mixed manifest exits zero" test "$rc" = 0
assert "each API version discovered once using its core/group path" jq -es '
  [.[] | select(index("get")) | .[-1]] | sort ==
  ["/api/v1", "/apis/example.test/v1", "/apis/rbac.authorization.k8s.io/v1"]
' "$FIX/calls.jsonl"
assert "context is carried on every discovery and apply" jq -es 'all(.[]; .[0:2] == ["--context", "fixture-context"])' "$FIX/calls.jsonl"
assert "all applies use strict server dry-run and JSON Lists" jq -es '
  length == 4 and all(.[];
    (.args | index("--dry-run=server") != null and index("--validate=strict") != null) and
    .body.apiVersion == "v1" and .body.kind == "List" and (.body.items | length > 0))
' "$FIX/applies.jsonl"
assert "cluster objects are grouped without namespace flags" jq -es '
  [.[] | select(.args | index("--namespace") == null) | .body.items[].metadata.name] | sort ==
  ["cluster-reader", "custom-cluster", "wardyn-runs"]
' "$FIX/applies.jsonl"
for pair in 'wardyn defaulted,empty-namespace,explicit,null-namespace' 'wardyn-runs runner-binding,runner-role' 'team-a custom'; do
  namespace=${pair%% *}; names=${pair#* }
  assert "$namespace has exactly the expected objects" jq -es --arg namespace "$namespace" --arg names "$names" '
    [.[] | select(.args | index("--namespace") as $i | $i != null and .[$i+1] == $namespace)] |
    length == 1 and ([.[0].body.items[].metadata.name] | sort | join(",")) == $names
  ' "$FIX/applies.jsonl"
done
yq -o=json '.' "$work/mixed.yaml" | jq -s 'map(select(. != null)) | sort_by(.metadata.name)' >"$work/expected.json"
assert "all ten objects assigned once with content unchanged" jq -es --slurpfile expected "$work/expected.json" '
  ([.[].body.items[]] | sort_by(.metadata.name)) == $expected[0] and ([.[].body.items[]] | length) == 10
' "$FIX/applies.jsonl"
run_check "$work/mixed.yaml" wardyn
assert "omitted context succeeds" test "$rc" = 0
assert "omitted context uses kubectl default throughout" jq -es 'all(.[]; index("--context") == null)' "$FIX/calls.jsonl"

valid='{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"valid"},"data":{"route":"first"}}'
printf '%s\n%s\n' "$valid" "$valid" >"$work/valid.json"
run_check "$work/valid.json" wardyn
assert "JSON document stream succeeds" test "$rc" = 0
assert "JSON documents retain their complete content" jq -es --argjson expected "$valid" '
  length == 1 and .[0].body.items == [$expected, $expected]
' "$FIX/applies.jsonl"

case_number=0
for input in \
  $'apiVersion: v1\nkind: Invalid\nkind: ConfigMap\nmetadata: {name: duplicate}' \
  $'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: duplicate, namespace: original, namespace: replacement}' \
  $'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: duplicate}\ndata: {route: first, "route": second}' \
  $'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: duplicate}\nspec: {list: [{nested: {route: first, route: second}}]}' \
  '{"apiVersion":"v1","kind":"Invalid","kind":"ConfigMap","metadata":{"name":"duplicate"}}' \
  '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"duplicate","namespace":"original","namespace":"replacement"}}' \
  '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"duplicate"},"data":{"route":"first","\u0072oute":"second"}}' \
  '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"duplicate"},"spec":{"list":[{"nested":{"route":"first","route":"second"}}]}}'; do
  case_number=$((case_number+1))
  format=yaml
  separator=$'---\n'
  if [ "${input:0:1}" = '{' ]; then format=json; separator=''; fi
  for position in standalone before-valid after-valid; do
    case "$position" in
      standalone) printf '%s\n' "$input" ;;
      before-valid) printf '%s\n%s%s\n' "$input" "$separator" "$valid" ;;
      after-valid) printf '%s\n%s%s\n' "$valid" "$separator" "$input" ;;
    esac >"$work/duplicate.$format"
    run_check "$work/duplicate.$format" wardyn
    reject "$format duplicate mapping keys case $case_number ($position)" 'duplicate mapping keys'
    assert "duplicate case $case_number ($position) makes no discovery calls" test ! -s "$FIX/calls.jsonl"
  done
done

# Aliases and custom tags must not bypass the check before JSON erases them.
for data in \
  '!custom {route: first, route: second}' \
  '{base: &base {route: first, route: second}, copy: *base}' \
  '{&key route: first, *key: second}' \
  '{1: first, "1": second}' \
  '{? [one, two]: first, ? [three, four]: second}'; do
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: duplicate}\ndata: %s\n' "$data" >"$work/duplicate.yaml"
  run_check "$work/duplicate.yaml" wardyn
  reject "ambiguous YAML mapping [$data]" 'mapping keys'
  assert "ambiguous YAML mapping makes no discovery calls" test ! -s "$FIX/calls.jsonl"
done
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: &key valid}\ndata: {*key: value}\n' >"$work/alias.yaml"
run_check "$work/alias.yaml" wardyn
assert "unique aliased mapping key succeeds" test "$rc" = 0
assert "unique aliased mapping key retains its resolved value" jq -es '.[0].body.items[0].data == {"valid":"value"}' "$FIX/applies.jsonl"

for input in '' '# comment only' $'---\n---\n' 'null' '~' '!!null' '!!null ""' '[]' 'false' 'plain text' 'apiVersion: v1' \
  $'apiVersion: v1\nkind: ConfigMap\nmetadata: {namespace: 42}' \
  $'apiVersion: ../../v1\nkind: ConfigMap\nmetadata: {}' 'metadata: ['; do
  printf '%s\n' "$input" >"$work/invalid.yaml"
  run_check "$work/invalid.yaml" wardyn
  reject "empty/malformed document [$input]" 'manifest|YAML'
  assert "invalid manifest makes no discovery calls" test ! -s "$FIX/calls.jsonl"
done
# A valid prefix must not hide a malformed or explicit-null later document.
for suffix in 'null' '!!null' '!!null ""' 'metadata: ['; do
  cat "$work/mixed.yaml" >"$work/invalid.yaml"
  printf '%s\n' "$suffix" >>"$work/invalid.yaml"
  run_check "$work/invalid.yaml" wardyn
  reject "invalid document after valid objects" 'manifest|YAML'
done
run_check "$work/missing.yaml" wardyn
reject "unreadable manifest" 'not readable'
run_check "$work/mixed.yaml" ''
reject "empty release namespace" 'RELEASE_NAMESPACE'
run_check "$work/mixed.yaml" wardyn ''
reject "empty explicit context" 'KUBE_CONTEXT'

cp "$FIX/_apis_example.test_v1.json" "$work/discovery-good.json"
for discovery in \
  '{"resources":[]}' \
  '{"resources":[{"kind":"Widget","name":"widgets/status","namespaced":true}]}' \
  '{"resources":[{"kind":"Widget","name":"widgets","namespaced":true},{"kind":"Widget","name":"others","namespaced":false}]}' \
  '{"resources":[{"kind":"Widget","name":"widgets","namespaced":"true"}]}' \
  '{"resources":[{"kind":"Widget","name":"widgets"}]}' \
  '{"resources":null}' 'invalid JSON'; do
  printf '%s\n' "$discovery" >"$FIX/_apis_example.test_v1.json"
  run_check "$work/mixed.yaml" wardyn fixture-context
  reject "missing/ambiguous/malformed discovery [$discovery]" 'discovery'
done
cp "$work/discovery-good.json" "$FIX/_apis_example.test_v1.json"
export FAIL_DISCOVERY=/apis/example.test/v1
run_check "$work/mixed.yaml" wardyn fixture-context
reject "discovery command failure" 'discovery fixture failure'
unset FAIL_DISCOVERY
export FAIL_APPLY=1
run_check "$work/mixed.yaml" wardyn fixture-context
assert "apply failure propagated" test "$rc" -ne 0
assert "apply failure explained" grep -q 'apply fixture failure' "$FIX/out"
assert "apply failure stops remaining groups" jq -es 'length == 1' "$FIX/applies.jsonl"
unset FAIL_APPLY

# Execute only the optional-object recipe in a scratch tree, never the install target.
mkdir -p "$work/recipe/scripts/lib"
cp "$ROOT/scripts/check-helm-schema.sh" "$work/recipe/scripts/"
cp "$ROOT/scripts/lib/common.sh" "$work/recipe/scripts/lib/"
{
  printf 'HELM_TEST_RELEASE := fixture-release\nHELM_TEST_NAMESPACE := wardyn\nHELM_TEST_SET := --set k8s.enabled=true\ncheck:\n'
  awk '
    /server-side dry-run of the optional objects/ { found=1; next }
    found && /@echo "==> teardown"/ { exit }
    found { print }
    END { if (!found) exit 1 }
  ' "$ROOT/Makefile"
} >"$work/recipe/Makefile"
cp "$work/mixed.yaml" "$FIX/render.yaml"
cat >"$work/bin/helm" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = template ] || exit 95
jq -cn --args '$ARGS.positional' -- "$@" >>"$FIX/helm.jsonl"
[ "${RENDER_RESULT:-}" = empty ] || cat "$FIX/render.yaml"
[ -z "${RENDER_RESULT:-}" ] || exit 41
SHIM
chmod +x "$work/bin/helm"
for result in empty partial ''; do
  : >"$FIX/calls.jsonl"
  : >"$FIX/applies.jsonl"
  rc=0
  RENDER_RESULT=$result make --no-print-directory -C "$work/recipe" check >"$FIX/out" 2>&1 || rc=$?
  if [ -n "$result" ]; then
    assert "Makefile rejects failed $result render" test "$rc" -ne 0
    assert "Makefile never invokes kubectl after failed $result render" test ! -s "$FIX/calls.jsonl"
  else
    assert "Makefile recipe succeeds with grouped schema helper" test "$rc" = 0
    assert "Makefile passes all four namespace/scope groups" jq -es 'length == 4' "$FIX/applies.jsonl"
  fi
  assert "Makefile cleans protected render and helper files ($result)" test -z "$(ls -A "$TMPDIR")"
done
assert "Makefile retains release, namespace and extra Helm values" jq -es '
  length == 3 and all(.[];
    .[0:3] == ["template", "fixture-release", "./deploy/helm/wardyn"] and
    (index("--namespace") as $i | .[$i+1] == "wardyn") and index("k8s.enabled=true") != null)
' "$FIX/helm.jsonl"

log "helm schema offline tests passed"
