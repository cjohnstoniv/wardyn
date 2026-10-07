#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"
[ "$#" -ge 2 ] && [ "$#" -le 3 ] || die "usage: $0 MANIFEST_FILE RELEASE_NAMESPACE [KUBE_CONTEXT]"
manifest=$1
release_namespace=$2
[ -r "$manifest" ] || die "manifest is not readable: $manifest"
[ -n "$release_namespace" ] || die "RELEASE_NAMESPACE must not be empty"
for tool in yq jq kubectl; do
  command -v "$tool" >/dev/null || die "$tool is required (yq must be Mike Farah v4)"
done
kube=(kubectl)
if [ "$#" = 3 ]; then
  [ -n "$3" ] || die "KUBE_CONTEXT must not be empty when supplied"
  kube+=(--context "$3")
fi

# Rendered Secrets must stay private, including intermediate discovery/group files.
umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
yq -o=json -I=0 'select(tag != "!!null" or (. | to_string) != "" or style != "")' -- "$manifest" >"$work/documents.json" \
  || die "malformed YAML manifest: $manifest"
jq -se '
  if length == 0 then error("manifest has no objects") else . end |
  if all(.[];
    type == "object" and
    (.apiVersion | type == "string" and test("^[a-z0-9][a-z0-9.-]*(/[a-z0-9][a-z0-9]*)?$")) and
    (.kind | type == "string" and length > 0) and
    (.metadata | type == "object") and
    (.metadata.namespace | . == null or type == "string"))
  then . else error("each document must be a Kubernetes object with apiVersion, kind and metadata") end
' "$work/documents.json" >"$work/objects.json" || die "invalid manifest: $manifest"

jq -r 'map(.apiVersion) | unique[]' "$work/objects.json" >"$work/versions"
: >"$work/discovery.jsonl"
while IFS= read -r version; do
  case "$version" in
    */*) path="/apis/$version" ;;
    *) path="/api/$version" ;;
  esac
  "${kube[@]}" get --raw "$path" >"$work/discovery.json" || die "discovery failed for $version"
  jq -ce --arg version "$version" '
    if (.resources | type) != "array" then error("discovery resources must be an array")
    else {apiVersion: $version, resources} end
  ' "$work/discovery.json" >>"$work/discovery.jsonl" || die "invalid discovery for $version"
done <"$work/versions"

# Resolve every document before any apply: an unknown kind cannot silently disappear.
jq -s --slurpfile objects "$work/objects.json" --arg fallback "$release_namespace" '
  . as $discovery |
  $objects[0] | to_entries | map(
    .key as $index | .value as $object |
    [$discovery[] | select(.apiVersion == $object.apiVersion) | .resources[] |
      select(.kind == $object.kind) |
      select(.name | type == "string" and length > 0 and (contains("/") | not))] as $matches |
    "\($object.apiVersion) \($object.kind)" as $label |
    if ($matches | length) != 1 then error("discovery for \($label): expected one resource, found \($matches | length)")
    elif ($matches[0].namespaced | type) != "boolean" then error("discovery for \($label): namespaced must be boolean")
    else {index: $index, object: $object, namespace:
      (if $matches[0].namespaced then
        ($object.metadata.namespace | if . == null or . == "" then $fallback else . end)
      else null end)} end
  ) | group_by(.namespace) |
  if ([.[][] | .index] | sort) != [range(0; $objects[0] | length)]
  then error("manifest accounting failed: each object must be assigned exactly once")
  else map({namespace: .[0].namespace, manifest: {apiVersion: "v1", kind: "List", items: map(.object)}}) end
' "$work/discovery.jsonl" >"$work/groups.json" || die "cannot group manifest by namespace and scope"

jq -c '.[]' "$work/groups.json" >"$work/groups.jsonl"
while IFS= read -r group; do
  namespace=$(jq -r '.namespace // empty' <<<"$group")
  args=()
  [ -z "$namespace" ] || args+=(--namespace "$namespace")
  jq '.manifest' <<<"$group" >"$work/group.json"
  "${kube[@]}" apply --dry-run=server --validate=strict "${args[@]}" -f "$work/group.json" \
    || die "schema dry-run failed for ${namespace:-cluster scope}"
done <"$work/groups.jsonl"
log "schema dry-run passed: $(jq 'length' "$work/objects.json") objects in $(jq 'length' "$work/groups.json") groups"
