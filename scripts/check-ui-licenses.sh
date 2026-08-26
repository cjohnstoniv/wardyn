#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Licence gate for SHIPPED (prod) UI dependencies — an ALLOWLIST, not a denylist.
# A denylist fails OPEN on everything it does not name: SSPL, BUSL, Elastic-2.0,
# CC-BY-NC and any "Unknown" — the source-available terms legal fears most — sail
# straight through. Here every prod licence token must appear in
# licenses/ALLOWED-LICENSES.txt, the SAME file the Go gate reads; anything else
# fails the build. A dual-licensed expression passes only when EVERY token in it
# is allowlisted ("X OR AGPL-3.0" is rejected — we do not assume the OR resolves
# in our favour without review).
#
# Two assertions keep this from passing vacuously:
#   1. The allowlist must be non-empty.
#   2. At least one package must actually have been examined. `pnpm licenses list`
#      on a tree with no node_modules returns {}, which yields an empty key set,
#      an empty `bad`, and a cheerful green gate that checked nothing. A gate must
#      not depend on job ordering.
#
# --self-test exercises the residue logic against a fixed table. That logic is the
# only non-trivial part of this script, so it is the one thing that gets a check.
set -euo pipefail

cd "$(dirname "$0")/.."
ALLOWED_LICENSES_FILE="${ALLOWED_LICENSES_FILE:-licenses/ALLOWED-LICENSES.txt}"

# Build the alternation from the shared allowlist, regex-escaping dots. Ids are
# enumerated upstream, never wildcarded (a bare `BSD` token is an unversioned,
# ambiguous declaration; `BSD-*` admits BSD-4-Clause and BSD-Protection).
allow=$(grep -vE '^[[:space:]]*(#|$)' "$ALLOWED_LICENSES_FILE" | sed 's/\./\\./g' | paste -sd'|')
[ -n "$allow" ] || { echo "ERROR: allowlist $ALLOWED_LICENSES_FILE is empty or unreadable — failing closed"; exit 1; }

# residue <expression> -> whatever is left after stripping allowlisted tokens and
# SPDX glue. Empty residue == every token vouched for -- EXCEPT for an empty or
# whitespace-only expression, which strips to nothing for the opposite reason: no
# licence was declared at all. That is the least vouched-for case there is, so it
# is rejected explicitly rather than falling through the empty-residue test.
residue() {
  case "$1" in
    *[![:space:]]*) : ;;
    *) printf 'NO-LICENCE-DECLARED'; return ;;
  esac
  printf '%s' "$1" \
    | sed -E "s/\b($allow)\b//g; s/\b(OR|AND|WITH)\b//g; s/[()]//g; s/[[:space:]]+//g"
}

if [ "${1:-}" = "--self-test" ]; then
  fail=0
  check() { # check <expr> <allow|reject>
    r=$(residue "$1")
    if [ "$2" = allow ]; then [ -z "$r" ] || { echo "SELF-TEST FAIL: '$1' should be ALLOWED (residue '$r')"; fail=1; }
    else [ -n "$r" ] || { echo "SELF-TEST FAIL: '$1' should be REJECTED"; fail=1; }
    fi
  }
  for e in "MIT" "Apache-2.0" "BSD-3-Clause" "ISC" "0BSD" "OFL-1.1" "(MIT OR CC0-1.0)"; do check "$e" allow; done
  for e in "Unknown" "UNLICENSED" "SEE LICENSE IN LICENSE.md" "SSPL-1.0" "BUSL-1.1" \
           "Elastic-2.0" "CC-BY-NC-4.0" "MIT OR AGPL-3.0" "BSD-4-Clause" "BSD-Protection" \
           "GPL-3.0" "MPL-2.0" ""; do check "$e" reject; done
  [ "$fail" -eq 0 ] && echo "check-ui-licenses --self-test: PASS (19 expressions)" || exit 1
  exit 0
fi

cd ui
pnpm licenses list --prod --json > /tmp/ui-prod-licenses.json

n=$(jq '[.[][]] | length' /tmp/ui-prod-licenses.json)
[ "$n" -ge 1 ] || { echo "ERROR: examined 0 production packages — is ui/node_modules installed? Failing closed."; exit 1; }

bad=$(jq -r 'keys[]' /tmp/ui-prod-licenses.json | while IFS= read -r lic; do
  [ -z "$(residue "$lic")" ] || printf '%s\n' "$lic"
done || true)

if [ -n "$bad" ]; then
  echo "Non-allowlisted licence(s) found in PRODUCTION dependencies:"
  echo "$bad"
  jq . /tmp/ui-prod-licenses.json
  exit 1
fi
echo "All $n production dependency licences are on the allowlist."
