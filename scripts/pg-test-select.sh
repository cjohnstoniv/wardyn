#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Which tests of a package need Postgres. The PG shards used to run every test
# of internal/api, ~46% of them tests the `go` (unit) job already runs under
# -race; this picks the ones that skip without WARDYN_TEST_PG.
#
#   scripts/pg-test-select.sh [--part i/n] [<go package pattern>...]
#       Prints a `go test -run` regexp: ^(TestA|TestB|...)$. A package pattern
#       is anything `go list` accepts; none means the api shard (API_PKGS). --part i/n keeps the
#       i-th of n slices (by a hash of the test name) plus every probe.
#   scripts/pg-test-select.sh --packages
#       Prints API_PKGS, the one place the api shard's package list lives.
#   scripts/pg-test-select.sh --guard <go test -json output> [<go package pattern>...]
#       The unit job's JSON (WARDYN_TEST_PG unset) is the independent proof:
#       every top-level test there that skipped naming WARDYN_TEST_PG must be
#       in the selection, else exit 1. A selector blind spot (a gate the
#       analysis cannot see) fails the unit job instead of silently dropping a
#       PG test from CI.
#
# The selection is a package-local taint analysis over every *_test.go file: a
# func is gated if its body names WARDYN_TEST_PG, or names (call, method value
# or reference) a gated func of the same package; a TestXxx is selected when
# gated. Names are matched without receivers, so it over-selects rather than
# misses. Every `testfloor.Mark(t, "pg")` probe is selected regardless: the
# skip floor in test-report.sh fails the shard when a probe never ran.
set -euo pipefail

API_PKGS="./internal/api/... ./test/apie2e/..."

select_pkg() { # <dir>: prints the gated top-level Test funcs of one package dir
  local f=("$1"/*_test.go)
  [ -e "${f[0]}" ] || return 0
  awk '
    function flush() {
      if (name == "") return
      body[name] = body[name] " " cur;
      if (name ~ /^Test[A-Z0-9_]/) istest[name] = 1
      if (cur ~ /WARDYN_TEST_PG/) gated[name] = 1
      if (cur ~ /testfloor\.Mark\(t, "pg"\)/) probe[name] = 1
      name = ""; cur = ""
    }
    FNR == 1 { flush() }
    /^func / {
      flush()
      s = $0
      if (s ~ /^func \(/) sub(/^func \([^)]*\) */, "", s); else sub(/^func */, "", s)
      sub(/[^A-Za-z0-9_].*/, "", s)
      name = s
    }
    name != "" { line = $0; if (line !~ /^[ \t]*\/\//) cur = cur " " line }
    END {
      flush()
      for (n in body) {
        t = body[n]
        while (match(t, /[A-Za-z_][A-Za-z0-9_]*/)) {
          id = substr(t, RSTART, RLENGTH); t = substr(t, RSTART + RLENGTH)
          if ((id in body) && id != n && !((n SUBSEP id) in seen)) { seen[n, id] = 1; refs[n] = refs[n] " " id }
        }
      }
      do {
        changed = 0
        for (n in refs) {
          if (n in gated) continue
          k = split(refs[n], r, " ")
          for (i = 1; i <= k; i++) if (r[i] in gated) { gated[n] = 1; changed = 1; break }
        }
      } while (changed)
      for (n in istest) if ((n in gated) || (n in probe)) if (n != "TestMain") print n ((n in probe) ? " probe" : "")
    }' "${f[@]}"
}

selection() { # <patterns...>: sorted unique "<import path> <Test> [probe]" lines
  local path dir
  while read -r path dir; do
    select_pkg "$dir" | sed "s|^|${path} |"
  done < <(go list -f '{{.ImportPath}} {{.Dir}}' "$@" 2>/dev/null | sort -u)
}

if [ "${1:-}" = "--packages" ]; then echo "$API_PKGS"; exit 0; fi

if [ "${1:-}" = "--guard" ]; then
  json="${2:?--guard needs the unit go test -json file}"
  shift 2
  [ $# -gt 0 ] || set -- $API_PKGS
  sel="$(selection "$@" | awk '{print $1 " " $2}')"
  [ -n "$sel" ] || { echo "pg-test-select: empty selection for: $*" >&2; exit 1; }
  scope="$(go list "$@" 2>/dev/null)"
  # Top-level tests (a subtest counts for its parent) that skipped AND said they need WARDYN_TEST_PG.
  top='s/.*"Package":"\([^"]*\)","Test":"\([^"/]*\)[^"]*".*/\1 \2/p'
  said="$(grep -F 'WARDYN_TEST_PG' "$json" | grep -F '"Action":"output"' | sed -n "$top" | sort -u)"
  skips="$(grep -F '"Action":"skip"' "$json" | sed -n "$top" | sort -u)"
  skipped="$(comm -12 <(echo "$said") <(echo "$skips"))"
  missing=""
  while IFS= read -r pt; do
    [ -n "$pt" ] || continue
    pkg="${pt% *}"; t="${pt#* }"
    # Only packages this selection covers; the other shards run whole packages.
    grep -qxF "$pkg" <<<"$scope" || continue
    grep -qxF "${pkg} ${t}" <<<"$sel" || missing="${missing}${pkg} ${t}"$'\n'
  done <<<"$skipped"
  if [ -n "$missing" ]; then
    echo "pg-test-select: these tests skip without WARDYN_TEST_PG but the PG shard would not run them:" >&2
    printf '%s' "$missing" >&2
    echo "pg-test-select: scripts/pg-test-select.sh cannot see how they reach the gate; teach it, or call the gate helper directly." >&2
    exit 1
  fi
  echo "pg-test-select: guard ok ($(wc -l <<<"$skipped" | tr -d ' ') skipped-without-PG tests checked)"
  exit 0
fi

# --part i/n: the i-th of n slices by a hash of the test name, so a new test never reshuffles the
# others. Every probe rides in every slice: each shard's skip floor must see all of them pass.
part=1 parts=1
if [ "${1:-}" = "--part" ]; then part="${2%/*}" parts="${2#*/}"; shift 2; fi
[ $# -gt 0 ] || set -- $API_PKGS
names="$(selection "$@" | awk -v part="$part" -v parts="$parts" '
  BEGIN { chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz" }
  { h = 7; for (i = 1; i <= length($2); i++) h = (h * 31 + index(chars, substr($2, i, 1))) % 1000003
    if ($3 == "probe" || h % parts == part - 1) print $2 }' | sort -u | paste -sd'|')"
[ -n "$names" ] || { echo "pg-test-select: no PG tests found in: $*" >&2; exit 1; }
echo "^(${names})\$"
