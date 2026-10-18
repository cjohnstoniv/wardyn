#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-ui-licenses-tsv.sh — scripts/third-party-notices.sh reads one record per
# UI package. A tab is IFS whitespace, so an empty author or homepage collapsed
# two tabs into one separator, shifted every later field left, lost the package
# path and filed the package under "publishes no licence file" (#2465). The
# records now use a non-whitespace separator; this pins that an empty field
# keeps its place.
#
# Daemon-free, network-free: jq and bash only.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "FAIL: $*" >&2; exit 1; }

# The shape of `pnpm licenses list --prod --json`: licence id -> packages.
fixture='{"MIT":[
 {"name":"react","versions":["19.3.0"],"paths":["/x/react"],"license":"MIT","homepage":"https://react.dev/"},
 {"name":"@radix-ui/react-slot","versions":["1.5.0"],"paths":["/x/slot"],"license":"MIT"},
 {"name":"react-remove-scroll-bar","versions":["2.3.8"],"paths":["/x/rrsb"],"license":"MIT","author":"A. Person"}],
 "ISC":[{"name":"lucide-react","versions":["1.57.0","1.52.0"],"paths":["/x/lucide"],"license":"ISC","author":"Eric","homepage":"https://lucide.dev"}]}'

# 1. the same record shape the notices script uses.
tsv="$(printf '%s' "$fixture" | "$ROOT/scripts/ui-licenses-tsv.sh")" || fail "ui-licenses-tsv.sh failed on the fixture"
[ "$(printf '%s\n' "$tsv" | wc -l)" -eq 4 ] || fail "expected 4 records, got: $tsv"

row() { printf '%s\n' "$tsv" | awk -F$'\x1f' -v n="$1" '$1 == n'; }
parse() { # NAME -> "name|vers|lic|author|home|path"
  local name vers lic author home path
  IFS=$'\x1f' read -r name vers lic author home path < <(row "$1")
  printf '%s|%s|%s|%s|%s|%s' "$name" "$vers" "$lic" "$author" "$home" "$path"
}

# 2. an empty author keeps homepage and path in their own fields.
[ "$(parse react)" = "react|19.3.0|MIT||https://react.dev/|/x/react" ] || fail "react: $(parse react)"
# 3. both empty.
[ "$(parse @radix-ui/react-slot)" = "@radix-ui/react-slot|1.5.0|MIT|||/x/slot" ] || fail "radix slot: $(parse @radix-ui/react-slot)"
# 4. an empty homepage.
[ "$(parse react-remove-scroll-bar)" = "react-remove-scroll-bar|2.3.8|MIT|A. Person||/x/rrsb" ] || fail "rrsb: $(parse react-remove-scroll-bar)"
# 5. several versions join with a comma, all fields present.
[ "$(parse lucide-react)" = "lucide-react|1.57.0, 1.52.0|ISC|Eric|https://lucide.dev|/x/lucide" ] || fail "lucide: $(parse lucide-react)"

# 6. the failure being fixed, for contrast: a tab-separated read shifts the fields.
old="$(printf 'react\t19.3.0\tMIT\t\thttps://react.dev/\t/x/react\n' | { IFS=$'\t' read -r _ _ _ _ _ path; printf '%s' "$path"; })"
[ -z "$old" ] || fail "control: the tab read no longer shifts fields, so this test proves nothing"

# 7. no tab-delimited read of these records is left in the notices script.
NOTICES_SCRIPT="${NOTICES_SCRIPT:-$ROOT/scripts/third-party-notices.sh}"
if grep -n "IFS=\$'\\\\t'" "$NOTICES_SCRIPT"; then
  fail "$NOTICES_SCRIPT still reads UI records with a tab IFS"
fi
echo "test-ui-licenses-tsv: PASS"
