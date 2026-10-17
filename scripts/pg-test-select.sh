#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Which tests of a package need Postgres. The PG shards used to run every test
# of their packages, many of them tests the `go` (unit) job already runs under
# -race; this picks the ones that skip without WARDYN_TEST_PG.
#
#   scripts/pg-test-select.sh [--part i/n] [<go package pattern>...]
#       Prints a `go test -run` regexp: ^(TestA|TestB|...)$. A package pattern
#       is anything `go list` accepts; none means the api shard (API_PKGS). --part i/n keeps the
#       i-th of n slices plus every probe: the slow tests listed in test/pg-test-weights.tsv are
#       packed across the slices by their recorded seconds, the rest go by a hash of the name.
#   scripts/pg-test-select.sh --packages [api|store]
#       Prints API_PKGS (the default) or STORE_PKGS: the one place each shard's
#       package list lives (the store shard is store, db, broker, cmd/wardynd, ...).
#   scripts/pg-test-select.sh --guard <go test -json output> [<go package pattern>...]
#       The unit job's JSON (WARDYN_TEST_PG unset) is the independent proof:
#       every top-level test there that skipped naming WARDYN_TEST_PG must be
#       in the selection, else exit 1. A selector blind spot (a gate the
#       analysis cannot see) fails the unit job instead of silently dropping a
#       PG test from CI. With no pattern it guards both shards' packages.
#
# The selection is a taint analysis over every *_test.go file: a func is gated
# if its body names WARDYN_TEST_PG, or names (call, method value or reference) a
# gated func of the same package, or a gating helper from outside any _test.go
# file (subjectkeytest.ThrowawayDB, and what calls it: gating_helpers below); a
# TestXxx is selected when gated. Names are matched without receivers, so it
# over-selects rather than misses. Every `testfloor.Mark(t, "pg")` probe is selected regardless: the
# skip floor in test-report.sh fails the shard when a probe never ran.
set -euo pipefail

API_PKGS="./internal/api/... ./test/apie2e/..."
STORE_PKGS="./internal/store/... ./internal/db/... ./internal/secretstore/... ./internal/broker/... ./internal/recording/... ./cmd/wardynd/... ./internal/notify/... ./internal/livebus/..."

# Names of funcs in non-test files that name WARDYN_TEST_PG, or call one that does, anywhere in the
# module: the DSN gates that live in a helper package (a test in another package reaches them by
# name, which the package-local analysis below cannot see). Found by growing the set until stable.
gating_helpers() {
  local names="" new files root all="--include=*.go --exclude=*_test.go --exclude-dir=node_modules --exclude-dir=.git"
  root="$(go list -m -f '{{.Dir}}')"
  files="$(grep -rl $all WARDYN_TEST_PG "$root" || true)"
  while [ -n "$files" ]; do
    # shellcheck disable=SC2086 # one word per file
    new="$(awk -v ext="$names" '
      function flush(   t, id) {
        if (name == "") return
        if (cur ~ /WARDYN_TEST_PG/) out[name] = 1
        t = cur
        while (match(t, /[A-Za-z_][A-Za-z0-9_]*/)) { id = substr(t, RSTART, RLENGTH); t = substr(t, RSTART + RLENGTH); if (id in known) out[name] = 1 }
        name = ""; cur = ""
      }
      BEGIN { k = split(ext, e, " "); for (i = 1; i <= k; i++) known[e[i]] = 1 }
      FNR == 1 { flush() }
      /^func / { flush(); s = $0; if (s ~ /^func \(/) sub(/^func \([^)]*\) */, "", s); else sub(/^func */, "", s); sub(/[^A-Za-z0-9_].*/, "", s); name = s }
      name != "" { if ($0 !~ /^[ \t]*\/\//) cur = cur " " $0 }
      END { flush(); for (n in out) print n }' $files | sort -u | paste -sd' ')"
    [ "$new" != "$names" ] || break
    names="$new"
    files="$(grep -rlwE $all "$(tr ' ' '|' <<<"$names")" "$root" || true)"
  done
  echo "$names"
}

select_pkg() { # <dir>: prints the gated top-level Test funcs of one package dir
  local f=("$1"/*_test.go)
  [ -e "${f[0]}" ] || return 0
  awk -v ext="$GATING" '
    BEGIN { k = split(ext, e, " "); for (i = 1; i <= k; i++) external[e[i]] = 1 }
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
    # An export_test.go alias (var Name = unexported) is a name a test reaches the gate through.
    /^var [A-Za-z_]/ { flush(); s = $0; sub(/^var +/, "", s); sub(/[^A-Za-z0-9_].*/, "", s); name = s }
    # The same inside a grouped `var ( ... )` block (gofmt indents it with one tab).
    /^var \(/ { flush(); invar = 1; next }
    invar && /^\)/ { flush(); invar = 0; next }
    invar && /^\t[A-Za-z_][A-Za-z0-9_]*([ \t]+[^ \t=]+)?[ \t]*=/ { flush(); s = $0; sub(/^\t/, "", s); sub(/[^A-Za-z0-9_].*/, "", s); name = s }
    name != "" { line = $0; if (line !~ /^[ \t]*\/\//) cur = cur " " line }
    END {
      flush()
      for (n in body) {
        t = body[n]
        while (match(t, /[A-Za-z_][A-Za-z0-9_]*/)) {
          id = substr(t, RSTART, RLENGTH); t = substr(t, RSTART + RLENGTH)
          if (id in external) gated[n] = 1
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
  GATING="${GATING-$(gating_helpers)}"
  while read -r path dir; do
    select_pkg "$dir" | sed "s|^|${path} |"
  done < <(go list -f '{{.ImportPath}} {{.Dir}}' "$@" 2>/dev/null | sort -u)
}

if [ "${1:-}" = "--packages" ]; then
  case "${2:-api}" in
    api) echo "$API_PKGS" ;;
    store) echo "$STORE_PKGS" ;;
    *) echo "pg-test-select: --packages takes api or store" >&2; exit 2 ;;
  esac
  exit 0
fi

if [ "${1:-}" = "--guard" ]; then
  json="${2:?--guard needs the unit go test -json file}"
  shift 2
  [ $# -gt 0 ] || set -- $API_PKGS $STORE_PKGS
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
    # Only packages this selection covers.
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

# --part i/n: the i-th of n slices. The tests in $weights ("<seconds><TAB><test>", the slow ones from
# a green hosted run) are dealt out heaviest first, each to the lightest slice so far, so the slices
# finish together; the file alone decides that, so a new test never moves them. Every other test goes
# by a hash of its name, so it reshuffles nothing either. Every probe rides in every slice: each
# shard's skip floor must see all of them pass.
weights="${PG_TEST_WEIGHTS:-$(dirname "$0")/../test/pg-test-weights.tsv}"
part=1 parts=1
if [ "${1:-}" = "--part" ]; then part="${2%/*}" parts="${2#*/}"; shift 2; fi
[ $# -gt 0 ] || set -- $API_PKGS
names="$({ LC_ALL=C sort -t$'\t' -k1,1nr -k2,2 "$weights" 2>/dev/null || true; echo "-"; selection "$@"; } | awk -v part="$part" -v parts="$parts" '
  BEGIN { chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ_abcdefghijklmnopqrstuvwxyz"; FS = "[ \t]" }
  $0 == "-" { sel = 1; next }
  !sel { w = 0; for (b = 1; b <= parts; b++) if (b == 1 || load[b] < load[w]) w = b
         load[w] += $1; slice[$2] = w; next }
  { h = 7; for (i = 1; i <= length($2); i++) h = (h * 31 + index(chars, substr($2, i, 1))) % 1000003
    mine = ($2 in slice) ? slice[$2] == part : h % parts == part - 1
    if ($3 == "probe" || mine) print $2 }' | sort -u | paste -sd'|')"
[ -n "$names" ] || { echo "pg-test-select: no PG tests found in: $*" >&2; exit 1; }
echo "^(${names})\$"
