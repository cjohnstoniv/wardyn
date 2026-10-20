#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# forward-port-check.sh PREV_TAG BASE HEAD
#   Fails a forward-port (release/X.Y -> main) that throws away content which
#   exists only on main. For every file HEAD changes against BASE, each line HEAD
#   removes must also be in that file at PREV_TAG, the last release the branch
#   was cut from: a removed line that is not there is main-only content, and the
#   forward-port is deleting it. Prints each as `file: line`, exits 1.
#   Exempt: scripts/doc-form.d/*.budget (budget arithmetic changes on purpose).
#   Also fails when CHANGELOG.md at HEAD carries the same `## [..]` heading twice.
#
# forward-port-check.sh --prev-tag X.Y.Z[-rc.N]
#   Prints the newest release tag below that version (release candidates count
#   only when the version is itself a candidate).
set -euo pipefail

if [ "${1:-}" = --prev-tag ]; then
  [ $# -eq 2 ] || { echo "usage: $0 --prev-tag X.Y.Z[-rc.N]" >&2; exit 2; }
  git tag -l 'v[0-9]*' | awk -v v="$2" '
    function key(s,   a, n, rc) {
      if (s !~ /^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$/) return ""
      n = split(s, a, /[.-]/); rc = (s ~ /-rc\./) ? a[5] : 999999999
      return sprintf("%09d.%09d.%09d.%09d", a[1], a[2], a[3], rc)
    }
    BEGIN { vk = key(v); isrc = (v ~ /-rc\./) }
    { t = substr($0, 2); k = key(t) }
    k != "" && k < vk && (isrc || t !~ /-rc\./) && k > best { best = k; tag = $0 }
    END { if (tag != "") print tag }'
  exit 0
fi

[ $# -eq 3 ] || { echo "usage: $0 PREV_TAG BASE HEAD" >&2; exit 2; }
PREV="$1"; BASE="$2"; HEAD_REF="$3"
for r in "$PREV" "$BASE" "$HEAD_REF"; do
  git rev-parse --verify -q "$r^{commit}" >/dev/null || { echo "forward-port-check: no such commit: $r" >&2; exit 2; }
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
bad=0; files=0; lines=0

while IFS= read -r -d '' f; do
  case "$f" in scripts/doc-form.d/*.budget) continue ;; esac
  files=$((files + 1))
  # Removed lines only: skip the diff header (everything before the first hunk),
  # so a removed line that itself starts with `-` (a bullet) is kept.
  git diff --no-renames -U0 "$BASE" "$HEAD_REF" -- "$f" \
    | awk '/^@@/ { h = 1; next } h && /^-/ { print substr($0, 2) }' \
    | grep -v '^[[:space:]]*$' >"$TMP/removed" || true
  [ -s "$TMP/removed" ] || continue
  lines=$((lines + $(wc -l <"$TMP/removed")))
  git show "$PREV:$f" >"$TMP/old" 2>/dev/null || : >"$TMP/old"
  if grep -vxFf "$TMP/old" "$TMP/removed" >"$TMP/missing"; then
    while IFS= read -r l; do printf '%s: %s\n' "$f" "$l"; done <"$TMP/missing"
    bad=1
  fi
done < <(git diff --no-renames --name-only -z "$BASE" "$HEAD_REF")

if git cat-file -e "$HEAD_REF:CHANGELOG.md" 2>/dev/null; then
  dup="$(git show "$HEAD_REF:CHANGELOG.md" | grep -E '^## \[[^]]+\]' | sed -E 's/^(## \[[^]]+\]).*/\1/' | sort | uniq -d)"
  if [ -n "$dup" ]; then
    echo "CHANGELOG.md: more than one heading for: $(echo "$dup" | tr '\n' ' ')" >&2
    bad=1
  fi
fi

if [ "$bad" != 0 ]; then
  echo "forward-port-check: FAIL: the lines above exist on $BASE but not at $PREV and $HEAD_REF removes them" >&2
  exit 1
fi
echo "forward-port-check: PASS ($files files, $lines removed lines, all present at $PREV)"
