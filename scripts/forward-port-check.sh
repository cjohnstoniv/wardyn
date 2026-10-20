#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# forward-port-check.sh [--version V] [--allow-dir DIR] PREV BASE HEAD
#   Fails a forward-port (release/X.Y -> main) that throws away content which
#   exists only on main. BASE is the merge-base of the port and main, PREV the
#   release the branch was cut from (a tag, or the branch-point sha that
#   --prev-tag prints). For every file HEAD changes against BASE, each line HEAD
#   removes must also be in that file at PREV: a removed line that is not there
#   is main-only content, and the port is deleting it. A binary file whose
#   content at HEAD is PREV's while BASE's differs is main's change reverted.
#   Prints each as `file: line` (`file: <binary reverted to PREV>`), exits 1.
#   A removal the port makes on purpose (a release-side rewrite of a main line, a
#   budget sum) is listed in DIR/V.allow, one `path<TAB>exact line<TAB>reason`
#   per line (`#` comments and blank lines ignored; `<binary>` stands for a
#   binary file). Only V's own file is read. An entry with no real reason, or
#   one the port does not need, fails. DIR defaults to scripts/forward-port-check.d.
#   A port that edits this script or any other file in that directory is refused.
#   Also fails when CHANGELOG.md at HEAD carries the same `## [..]` heading twice.
#
# forward-port-check.sh --prev-tag X.Y.Z[-rc.N] [MAIN_REF]
#   Prints PREV. X.Y.0 and X.Y.0-rc.1: the branch point, a commit sha, the
#   merge-base of MAIN_REF (default origin/main) and the tag. X.Y.0-rc.N, N>1:
#   vX.Y.0-rc.(N-1). Otherwise the newest release tag below the version
#   (candidates count only when the version is itself a candidate).
set -euo pipefail
export GIT_LITERAL_PATHSPECS=1 LC_ALL=C

die() { echo "forward-port-check: $*" >&2; exit 2; }

has_tag() { git rev-parse -q --verify "refs/tags/$1^{commit}" >/dev/null; }

prev_tag() {  # prev_tag VERSION MAIN_REF
  local v="$1" main="$2" patch rc
  [[ $v =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)(-rc\.([0-9]+))?$ ]] || die "not a version: $v"
  patch="${BASH_REMATCH[3]}"; rc="${BASH_REMATCH[5]}"
  if [ "$patch" = 0 ] && { [ -z "$rc" ] || [ "$rc" = 1 ]; }; then
    has_tag "v$v" || die "no tag v$v to find the branch point from"
    git merge-base "$main" "v$v" || die "no merge-base of $main and v$v"
    return
  fi
  if [ "$patch" = 0 ] && [ -n "$rc" ]; then
    has_tag "v${v%-rc.*}-rc.$((rc - 1))" || die "no tag v${v%-rc.*}-rc.$((rc - 1))"
    echo "v${v%-rc.*}-rc.$((rc - 1))"
    return
  fi
  git tag -l 'v[0-9]*' | awk -v v="$v" '
    function key(s,   a, n, rc) {
      if (s !~ /^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$/) return ""
      n = split(s, a, /[.-]/); rc = (s ~ /-rc\./) ? a[5] : 999999999
      return sprintf("%09d.%09d.%09d.%09d", a[1], a[2], a[3], rc)
    }
    BEGIN { vk = key(v); isrc = (v ~ /-rc\./) }
    { t = substr($0, 2); k = key(t) }
    k != "" && k < vk && (isrc || t !~ /-rc\./) && k > best { best = k; tag = $0 }
    END { if (tag != "") print tag }'
}

if [ "${1:-}" = --prev-tag ]; then
  [ $# -ge 2 ] && [ $# -le 3 ] || { echo "usage: $0 --prev-tag X.Y.Z[-rc.N] [MAIN_REF]  (prints a tag, or a sha for X.Y.0 and X.Y.0-rc.1)" >&2; exit 2; }
  prev_tag "$2" "${3:-origin/main}"
  exit 0
fi

VERSION=""; ALLOW_DIR=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
    --allow-dir) [ $# -ge 2 ] || die "--allow-dir needs a value"; ALLOW_DIR="$2"; shift 2 ;;
    *) break ;;
  esac
done
[ $# -eq 3 ] || { echo "usage: $0 [--version V] [--allow-dir DIR] PREV BASE HEAD | --prev-tag X.Y.Z[-rc.N] [MAIN_REF]" >&2; exit 2; }
PREV="$1"; BASE="$2"; HEAD_REF="$3"
for r in "$PREV" "$BASE" "$HEAD_REF"; do
  git rev-parse --verify -q "$r^{commit}" >/dev/null || die "no such commit: $r"
done
ALLOW_DIR="${ALLOW_DIR:-$(git rev-parse --show-toplevel)/scripts/forward-port-check.d}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
git diff --no-renames --name-only -z "$BASE" "$HEAD_REF" >"$TMP/names" || die "git diff $BASE $HEAD_REF failed"

# A port may not edit the check or widen its own exceptions: only V's allow file. Nor may it
# carry a .gitattributes, which git reads from the checkout and which can mark files -diff.
refused=0
while IFS= read -r -d '' f; do
  case "$f" in
    scripts/forward-port-check.sh | scripts/forward-port-check.d | .gitattributes | */.gitattributes) ;;
    scripts/forward-port-check.d/*) [ -n "$VERSION" ] && [ "$f" = "scripts/forward-port-check.d/$VERSION.allow" ] && continue ;;
    *) continue ;;
  esac
  echo "forward-port-check: a forward-port may not change $f" >&2
  refused=1
done <"$TMP/names"
[ "$refused" = 0 ] || exit 1

bad=0; files=0; lines=0
: >"$TMP/flagged"
while IFS= read -r -d '' f; do
  files=$((files + 1))
  git diff --no-renames --numstat "$BASE" "$HEAD_REF" -- "$f" >"$TMP/stat" || die "git diff of $f failed"
  if grep -q $'^-\t-\t' "$TMP/stat"; then
    h="$(git rev-parse -q --verify "$HEAD_REF:$f" 2>/dev/null || true)"
    p="$(git rev-parse -q --verify "$PREV:$f" 2>/dev/null || true)"
    b="$(git rev-parse -q --verify "$BASE:$f" 2>/dev/null || true)"
    if [ -n "$h" ] && [ "$h" = "$p" ] && [ "$h" != "$b" ]; then
      lines=$((lines + 1))
      printf '%s\t%s\n' "$f" '<binary>' >>"$TMP/flagged"
    fi
    continue
  fi
  # Removed lines only: skip everything before the first hunk (the `--- a/f`
  # header), so a removed line that itself starts with `-` (a bullet) is kept.
  git diff --no-renames -U0 "$BASE" "$HEAD_REF" -- "$f" >"$TMP/diff" || die "git diff of $f failed"
  awk '/^@@/ { h = 1; next } h && /^-/ { print substr($0, 2) }' "$TMP/diff" >"$TMP/removed.all"
  grep -v '^[[:space:]]*$' "$TMP/removed.all" >"$TMP/removed" || [ $? = 1 ]
  [ -s "$TMP/removed" ] || continue
  lines=$((lines + $(wc -l <"$TMP/removed")))
  if git cat-file -e "$PREV:$f" 2>/dev/null; then git show "$PREV:$f" >"$TMP/old"; else : >"$TMP/old"; fi
  grep -vxFf "$TMP/old" "$TMP/removed" >"$TMP/missing" || [ $? = 1 ]
  while IFS= read -r l; do printf '%s\t%s\n' "$f" "$l" >>"$TMP/flagged"; done <"$TMP/missing"
done <"$TMP/names"

: >"$TMP/allowed"
allow_file=""
[ -z "$VERSION" ] || allow_file="$ALLOW_DIR/$VERSION.allow"
if [ -n "$allow_file" ] && [ -f "$allow_file" ]; then
  n=0
  while IFS= read -r e || [ -n "$e" ]; do
    n=$((n + 1))
    [[ $e =~ ^[[:space:]]*(#|$) ]] && continue
    if [[ $e != *$'\t'*$'\t'* ]]; then
      echo "forward-port-check: $allow_file:$n: want path<TAB>line<TAB>reason" >&2; bad=1; continue
    fi
    path="${e%%$'\t'*}"; rest="${e#*$'\t'}"; reason="${rest##*$'\t'}"; line="${rest%$'\t'*}"
    reason="${reason#"${reason%%[![:space:]]*}"}"; reason="${reason%"${reason##*[![:space:]]}"}"
    if [ -z "$path" ] || [ -z "$line" ]; then
      echo "forward-port-check: $allow_file:$n: empty path or line" >&2; bad=1; continue
    fi
    case "${reason,,}" in
      ''|-|n/a) echo "forward-port-check: $allow_file:$n: give a real reason for $path" >&2; bad=1; continue ;;
    esac
    printf '%s\t%s\n' "$path" "$line" >>"$TMP/allowed"
  done <"$allow_file"
fi
sort -u "$TMP/flagged" >"$TMP/flagged.s"
sort -u "$TMP/allowed" >"$TMP/allowed.s"
comm -23 "$TMP/flagged.s" "$TMP/allowed.s" >"$TMP/unlisted"
comm -13 "$TMP/flagged.s" "$TMP/allowed.s" >"$TMP/stale"

while IFS= read -r row; do
  f="${row%%$'\t'*}"; l="${row#*$'\t'}"
  if [ "$l" = '<binary>' ]; then l='<binary reverted to PREV>'; fi
  printf '%s: %s\n' "$f" "$l"
  bad=1
done <"$TMP/unlisted"
while IFS= read -r row; do
  f="${row%%$'\t'*}"; l="${row#*$'\t'}"
  echo "forward-port-check: stale allow entry, the port does not remove it: $f: $l" >&2
  bad=1
done <"$TMP/stale"

if git cat-file -e "$HEAD_REF:CHANGELOG.md" 2>/dev/null; then
  git show "$HEAD_REF:CHANGELOG.md" | { grep -E '^## \[[^]]+\]' || [ $? = 1 ]; } | sed -E 's/^(## \[[^]]+\]).*/\1/' >"$TMP/headings"
  if [ ! -s "$TMP/headings" ]; then
    echo "forward-port-check: note: CHANGELOG.md at $HEAD_REF has no '## [' heading; duplicate-heading check skipped" >&2
  else
    dup="$(sort "$TMP/headings" | uniq -d)"
    if [ -n "$dup" ]; then
      echo "CHANGELOG.md: more than one heading for: $(echo "$dup" | tr '\n' ' ')" >&2
      bad=1
    fi
  fi
fi

if [ "$bad" != 0 ]; then
  echo "forward-port-check: FAIL: lines above exist on $BASE but not at $PREV and $HEAD_REF removes them; list an intended one in ${allow_file:-scripts/forward-port-check.d/<version>.allow} (path<TAB>line<TAB>reason)" >&2
  exit 1
fi
echo "forward-port-check: PASS ($files files, $lines removed lines, $(wc -l <"$TMP/allowed.s") allow-listed, the rest present at $PREV)"
