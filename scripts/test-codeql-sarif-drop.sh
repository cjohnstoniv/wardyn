#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-codeql-sarif-drop.sh — codeql-sarif-drop.sh must drop the one rule's results on the one file and nothing else:
# the rule on another workflow, another rule on that file, and a result that also has a location elsewhere all stay.
# It runs the script directly, never through bash, so a file that is not executable fails here as it would in
# codeql.yml. Daemon-free, network-free; needs jq.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DROP="$ROOT/scripts/codeql-sarif-drop.sh"
fail() { echo "FAIL: $*" >&2; exit 1; }
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

RULE=actions/cache-poisoning/poisonable-step
OVERLAY=.github/workflows/overlay-proofs.yml

# fixture <uri> <rule-key>: four results in the first run, a run with no results key in the second.
# $1 is the uri of result 1; $2 is "ruleId" or "rule.id" for result 1.
fixture() {
  local uri="$1" key="$2" rid
  if [ "$key" = ruleId ]; then rid="\"ruleId\":\"$RULE\""; else rid="\"rule\":{\"id\":\"$RULE\"}"; fi
  cat <<JSON
{"runs":[{"tool":{"driver":{"name":"CodeQL"}},"results":[
 {$rid,"locations":[{"physicalLocation":{"artifactLocation":{"uri":"$uri"}}}]},
 {"ruleId":"$RULE","locations":[{"physicalLocation":{"artifactLocation":{"uri":".github/workflows/ci.yml"}}}]},
 {"ruleId":"actions/untrusted-checkout/high","locations":[{"physicalLocation":{"artifactLocation":{"uri":"$OVERLAY"}}}]},
 {"ruleId":"$RULE","locations":[{"physicalLocation":{"artifactLocation":{"uri":"$OVERLAY"}}},{"physicalLocation":{"artifactLocation":{"uri":".github/workflows/nightly.yml"}}}]}
]},{"tool":{}}]}
JSON
}
# remaining <file>: "<ruleId>@<uri>" of every result left, sorted, one line.
remaining() { jq -r '[.runs[].results[]? | ((.ruleId // .rule.id) + "@" + (.locations | map(.physicalLocation.artifactLocation.uri) | join("+")))] | sort | join(" ")' "$1"; }
want="actions/cache-poisoning/poisonable-step@.github/workflows/ci.yml actions/cache-poisoning/poisonable-step@$OVERLAY+.github/workflows/nightly.yml actions/untrusted-checkout/high@$OVERLAY"

d="$tmp/a"; mkdir "$d"; fixture "$OVERLAY" ruleId > "$d/actions.sarif"
out="$("$DROP" "$d")"
[ "$(remaining "$d/actions.sarif")" = "$want" ] || fail "(a) left [$(remaining "$d/actions.sarif")], want [$want]"
case "$out" in *"dropped 1 "*) ;; *) fail "(a) printed [$out], want 'dropped 1'" ;; esac
jq -e '(.runs | length) == 2' "$d/actions.sarif" >/dev/null || fail "(a) the run count changed"
echo "ok  the rule on overlay-proofs.yml is dropped; the other three results stay"

d="$tmp/b"; mkdir "$d"; fixture "/$OVERLAY" ruleId > "$d/actions.sarif"
"$DROP" "$d" >/dev/null
[ "$(remaining "$d/actions.sarif")" = "$want" ] || fail "(b) left [$(remaining "$d/actions.sarif")], want [$want]"
echo "ok  a uri written with a leading slash is dropped too"

d="$tmp/c"; mkdir "$d"; fixture "$OVERLAY" rule.id > "$d/actions.sarif"
"$DROP" "$d" >/dev/null
[ "$(remaining "$d/actions.sarif")" = "$want" ] || fail "(c) left [$(remaining "$d/actions.sarif")], want [$want]"
echo "ok  a result that names its rule as rule.id is dropped"

d="$tmp/d"; mkdir "$d"
rc=0; err="$("$DROP" "$d" 2>&1)" || rc=$?
{ [ "$rc" = 1 ] && case "$err" in *"no SARIF"*) true ;; *) false ;; esac; } || fail "(d) an empty directory: rc=$rc, got [$err], want exit 1 and 'no SARIF'"
echo "ok  an empty directory exits 1 with no SARIF"

d="$tmp/e"; mkdir "$d"; echo '{"runs":[{"tool":{}}]}' > "$d/actions.sarif"
"$DROP" "$d" >/dev/null || fail "(e) a run with no results key must exit 0"
jq -e .runs "$d/actions.sarif" >/dev/null || fail "(e) the file is no longer valid SARIF"
echo "ok  a run with no results key is left as it is"

echo "--- test-codeql-sarif-drop: PASS ---"
