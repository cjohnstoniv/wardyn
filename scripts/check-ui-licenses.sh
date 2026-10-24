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
ALLOWED_LICENSES_FILE="$(realpath "${ALLOWED_LICENSES_FILE:-licenses/ALLOWED-LICENSES.txt}")"

# Build the alternation from the shared allowlist, regex-escaping dots. Ids are
# enumerated upstream, never wildcarded (a bare `BSD` token is an unversioned,
# ambiguous declaration; `BSD-*` admits BSD-4-Clause and BSD-Protection).
# Only one-token lines: a scoped "<id> <package>..." line allows its id for the named packages
# alone (scoped_ok below), so it never joins the alternation.
allow=$(grep -vE '^[[:space:]]*(#|$)' "$ALLOWED_LICENSES_FILE" | awk 'NF == 1' | sed 's/\./\\./g' | paste -sd'|')
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

# scoped_ok <licence> <package> -> true when a scoped allowlist line names exactly this licence id and
# this package. Exact id only: "MPL-2.0 OR GPL-3.0" on the named package is still rejected.
scoped_ok() {
  grep -vE '^[[:space:]]*(#|$)' "$ALLOWED_LICENSES_FILE" | awk -v id="$1" -v pkg="$2" \
    'NF > 1 && $1 == id { for (i = 2; i <= NF; i++) if ($i == pkg) found = 1 } END { exit !found }'
}

# no_vendored_novnc -> fails when noVNC is copied or patched in-tree: the MPL-2.0 exception holds
# only while Wardyn ships the package unmodified (owner ruling 2026-10-23).
no_vendored_novnc() {
  local bad=""
  if jq -e '(.pnpm.patchedDependencies // {}) | keys[] | select(startswith("@novnc/novnc"))' ui/package.json >/dev/null 2>&1; then
    bad+=" ui/package.json pnpm.patchedDependencies names @novnc/novnc;"
  fi
  if jq -e '((.dependencies // {}) + (.devDependencies // {})) | has("patch-package")' ui/package.json >/dev/null 2>&1; then
    bad+=" ui/package.json depends on patch-package;"
  fi
  if grep -qE '^patchedDependencies:' ui/pnpm-lock.yaml \
    && grep -q '@novnc/novnc' <<<"$(grep -A20 -E '^patchedDependencies:' ui/pnpm-lock.yaml)"; then
    bad+=" ui/pnpm-lock.yaml patches @novnc/novnc;"
  fi
  local copied
  copied=$(git ls-files -- ui ':!:ui/node_modules' | grep -vE '^ui/(node_modules|dist)/' \
    | xargs -r grep -lE 'noVNC: HTML5 VNC client|The noVNC authors' 2>/dev/null || true)
  [ -z "$copied" ] || bad+=" noVNC source copied into the tree: $(echo $copied);"
  [ -z "$bad" ] || { echo "ERROR: @novnc/novnc must ship unmodified:$bad"; return 1; }
}

# expr_ok <expression> <package> -> true when every id in a non-empty expression is on the bare allowlist or
# scoped to that package. Whole ids, never substrings: "MIT-open-group" is not MIT.
expr_ok() {
  local id seen=""
  for id in ${1//[()]/ }; do
    case "$id" in AND|OR|WITH) continue ;; esac
    seen=1
    grep -vE '^[[:space:]]*(#|$)' "$ALLOWED_LICENSES_FILE" | awk 'NF == 1' | grep -qxF -- "$id" || scoped_ok "$id" "$2" || return 1
  done
  [ -n "$seen" ]
}

# bundled_texts_present -> fails unless every component a UI package bundles (licenses/bundled-in-ui-packages.tsv)
# is under an allowlisted licence (or one scoped to that package) and has its text, the same bytes as the package's
# (a whole licence file, a header's line range, a checked-in repo: notice, or for "-" the licence's canonical
# text), and its notices row.
bundled_texts_present() {
  local bad="" pkg path comp lic txt src lines
  while IFS=$'\t' read -r pkg path comp _ lic; do
    case "$pkg" in ''|'#'*) continue ;; esac
    expr_ok "$lic" "$pkg" || bad+=" $comp ($pkg) is $lic, not on the allowlist;"
    src="ui/node_modules/$pkg/${path%:*}"
    case "$path" in
      repo:*) txt="licenses/texts/npm/$pkg/$comp-NOTICE"
         cmp -s "${path#repo:}" "$txt" || bad+=" $comp ($pkg) has no $txt matching ${path#repo:};" ;;
      -) for txt in $(sed -E 's/ (AND|OR) / /g' <<<"$lic"); do
           [ -f "licenses/texts/common/$txt.txt" ] || bad+=" $comp ($pkg) has no licenses/texts/common/$txt.txt;"
         done ;;
      *:*-*) txt="licenses/texts/npm/$pkg/$comp-NOTICE"; lines=${path##*:}
         cmp -s <(sed -n "${lines%-*},${lines#*-}p" "$src") "$txt" || bad+=" $comp ($pkg) has no $txt matching $src lines $lines;" ;;
      *) txt="licenses/texts/npm/$pkg/$comp-LICENSE"
         cmp -s "$src" "$txt" || bad+=" $comp ($pkg) has no $txt matching $src;" ;;
    esac
    grep -qF "| \`$pkg\` | $comp |" THIRD-PARTY-NOTICES.md || bad+=" $comp ($pkg) has no THIRD-PARTY-NOTICES.md row;"
  done < licenses/bundled-in-ui-packages.tsv
  [ -z "$bad" ] || { echo "ERROR: a bundled component's licence is not shipped:$bad"; return 1; }
}

if [ "${1:-}" = "--self-test" ]; then
  fail=0
  check() { # check <expr> <allow|reject>
    r=$(residue "$1")
    if [ "$2" = allow ]; then [ -z "$r" ] || { echo "SELF-TEST FAIL: '$1' should be ALLOWED (residue '$r')"; fail=1; }
    else [ -n "$r" ] || { echo "SELF-TEST FAIL: '$1' should be REJECTED"; fail=1; }
    fi
  }
  for e in "MIT" "Apache-2.0" "BSD-3-Clause" "ISC" "0BSD" "OFL-1.1" "(MIT OR Apache-2.0)"; do check "$e" allow; done
  for e in "Unknown" "UNLICENSED" "SEE LICENSE IN LICENSE.md" "SSPL-1.0" "BUSL-1.1" \
           "Elastic-2.0" "CC-BY-NC-4.0" "MIT OR AGPL-3.0" "BSD-4-Clause" "BSD-Protection" \
           "GPL-3.0" "MPL-2.0" ""; do check "$e" reject; done
  # The scoped exception: MPL-2.0 for @novnc/novnc alone.
  scope() { # scope <licence> <package> <allow|reject>
    if scoped_ok "$1" "$2"; then got=allow; else got=reject; fi
    [ "$got" = "$3" ] || { echo "SELF-TEST FAIL: '$1' for '$2' should be ${3^^}ED"; fail=1; }
  }
  scope "MPL-2.0" "@novnc/novnc" allow
  scope "MPL-2.0" "some-other-package" reject
  scope "MPL-2.0" "@novnc/novnc-fork" reject
  scope "MPL-2.0 OR GPL-3.0" "@novnc/novnc" reject
  scope "GPL-3.0" "@novnc/novnc" reject
  # des.js and keysymdef.js inside @novnc/novnc (owner ruling 2026-10-24), and only there.
  expr() { # expr <expression> <package> <allow|reject>
    if expr_ok "$1" "$2"; then got=allow; else got=reject; fi
    [ "$got" = "$3" ] || { echo "SELF-TEST FAIL: '$1' for '$2' should be ${3^^}ED"; fail=1; }
  }
  expr "BSD-2-Clause AND LicenseRef-noVNC-des" "@novnc/novnc" allow
  expr "LicenseRef-noVNC-des" "some-other-package" reject
  expr "MIT-open-group AND HPND" "@novnc/novnc" allow
  expr "MIT-open-group AND HPND" "some-other-package" reject
  expr "HPND OR GPL-3.0" "@novnc/novnc" reject
  expr "" "@novnc/novnc" reject
  [ "$fail" -eq 0 ] && echo "check-ui-licenses --self-test: PASS (31 expressions)" || exit 1
  exit 0
fi

cd ui
LICENSES_JSON="$(mktemp)"
trap 'rm -f "$LICENSES_JSON"' EXIT
pnpm licenses list --prod --json > "$LICENSES_JSON"

n=$(jq '[.[][]] | length' "$LICENSES_JSON")
[ "$n" -ge 1 ] || { echo "ERROR: examined 0 production packages — is ui/node_modules installed? Failing closed."; exit 1; }

# A licence off the bare allowlist passes only when a scoped line names it for EVERY package under it.
bad=$(jq -r 'keys[]' "$LICENSES_JSON" | while IFS= read -r lic; do
  [ -z "$(residue "$lic")" ] && continue
  while IFS= read -r pkg; do
    scoped_ok "$lic" "$pkg" || printf '%s (%s)\n' "$lic" "$pkg"
  done < <(jq -r --arg l "$lic" '.[$l][].name' "$LICENSES_JSON")
done || true)

if [ -n "$bad" ]; then
  echo "Non-allowlisted licence(s) found in PRODUCTION dependencies:"
  echo "$bad"
  jq . "$LICENSES_JSON"
  exit 1
fi
(cd .. && no_vendored_novnc && bundled_texts_present)
echo "All $n production dependency licences are on the allowlist."
