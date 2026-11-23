#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# codeql-sarif-drop.sh <sarif-dir> — drops one CodeQL rule's results on one workflow file from every *.sarif in the
# directory, before codeql.yml uploads them. Mode 0755: codeql.yml runs it directly.
#
# overlay-proofs.yml's cache-poisoning finding is a known false positive (#3672): both of its jobs run only on
# refs/heads/proofs/* refs, whose cache entries no other branch restores, and no step restores or saves a cache. A
# dismissal is keyed to a line, so every edit that moved the steps reopened it. This drops that one rule on that one
# file by id and path, with no line number in play. A result with a location in any other file is kept, so a finding
# that spans overlay-proofs.yml and another workflow still reports, and the rule still reports every other workflow.
#
# Needs jq. Each file is rewritten in place (written as .new, then moved).
set -euo pipefail
RULE=actions/cache-poisoning/poisonable-step
FILE=.github/workflows/overlay-proofs.yml

dir="${1:?usage: codeql-sarif-drop.sh <sarif-dir>}"
shopt -s nullglob
files=("$dir"/*.sarif)
[ "${#files[@]}" -gt 0 ] || { echo "FAIL: no SARIF in $dir" >&2; exit 1; }
for f in "${files[@]}"; do
  before="$(jq '[.runs[]? | .results // [] | length] | add // 0' "$f")"
  jq --arg rule "$RULE" --arg file "$FILE" '
    .runs |= map(if .results then .results |= map(select(
      ((.ruleId // .rule.id) == $rule
       and ((.locations // []) | length) > 0
       and all(.locations[]; (.physicalLocation.artifactLocation.uri // "" | ltrimstr("/")) == $file))
      | not)) else . end)' "$f" > "$f.new"
  mv "$f.new" "$f"
  after="$(jq '[.runs[]? | .results // [] | length] | add // 0' "$f")"
  echo "codeql-sarif-drop: dropped $((before - after)) $RULE result(s) on $FILE, kept $after"
done
