#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-ops-drive-restore.sh — the Compose restore in docs/OPERATIONS.md makes
# each drive volume from its saved .labels record. This runs that loop, as the
# guide prints it, against a stub docker: a good record must reach
# docker volume create as one --label per recorded line, and a record whose
# drive id is not exactly one canonical id must be held before any database or
# volume step. A copy of the loop with the label forwarding taken out must fail.
#
# Daemon-free, network-free. OPS_DOC overrides the guide read.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
doc="${OPS_DOC:-$ROOT/docs/OPERATIONS.md}"

fail() { echo "FAIL: $*" >&2; exit 1; }

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

# The step-5 loop: from its drives.list check to the end of its loop, in the Restore section.
awk '/^### Restore them/{r=1} r&&/^\[ -f "\$B\/drives.list" \]/{f=1} f{print} f&&/^done 3< "\$B\/drives.list"$/{exit}' \
  "$doc" >"$T/loop.sh"
grep -q '^done 3< "\$B/drives.list"$' "$T/loop.sh" || fail "no step-5 drive loop found in $doc"

ID=0f8c1b2a-3d4e-4f50-8a6b-7c8d9e0f1a2b
cat >"$T/stub.sh" <<'EOF'
docker() {
  case "$*" in
    *psql*) printf '%s\n' "$DB_IDS" ;;
    "volume inspect --format {{.Driver}} {{len .Options}} "*) echo "local 0" ;;
    "volume inspect --format "*) sed -n 's/^--label //p' "$B/create.args" ;;
    "volume inspect "*) return 1 ;;
    "volume create "*) printf '%s\n' "$@" | sed '1,2d;$d' | paste -d' ' - - >"$B/create.args" ;;
    "run "*) return 0 ;;
  esac
}
EOF

# run <name> <loop file> <labels record>: prints the loop's output and returns its status.
run() {
  local B="$T/$1"
  mkdir -p "$B"
  printf '%s\n' "$3" >"$B/wardyn-drive-a.labels"
  : >"$B/wardyn-drive-a.tar.gz"
  echo wardyn-drive-a >"$B/drives.list"
  (cd "$B" && B="$B" DB_IDS="$ID" bash -c 'set -eu; . "$1"; . "$2"; echo REACHED' _ "$T/stub.sh" "$2")
}

good="wardyn.drive=$ID
wardyn.home=bsmith
wardyn.managed=true
wardyn.subject=0123456789abcdef0123"
out="$(run good "$T/loop.sh" "$good")" || fail "a good record did not restore: $out"
[[ "$out" == *REACHED* ]] || fail "a good record did not finish the loop: $out"
for l in "wardyn.drive=$ID" wardyn.home=bsmith wardyn.managed=true wardyn.subject=0123456789abcdef0123; do
  grep -qxF -- "--label $l" "$T/good/create.args" || fail "docker volume create was not given --label $l"
done

# A second, similarly named label is recorded and restored, and is not read as the drive id.
out="$(run similar "$T/loop.sh" "wardyn-drive=sample-text
$good")" || fail "a record with a similarly named label did not restore: $out"
grep -qxF -- "--label wardyn-drive=sample-text" "$T/similar/create.args" || fail "the similarly named label was not restored"

# Two drive ids, an id that is not canonical, or one carrying SQL quoting: held, no volume made.
for bad in "wardyn.drive=$ID
$good" "${good/$ID/0F8C1B2A-3D4E-4F50-8A6B-7C8D9E0F1A2B}" "${good/$ID/$ID\'; SELECT 1; --}"; do
  rm -rf "$T/bad"
  out="$(run bad "$T/loop.sh" "$bad")" || true
  [[ "$out" == *"HOLD:"* ]] || fail "a malformed drive id was not held: $out"
  [ ! -e "$T/bad/create.args" ] || fail "a volume was made for a malformed drive id"
done

# A drive the restored database does not hold stops the loop.
out="$(cd "$T" && mkdir -p nodrive && printf '%s\n' "$good" >nodrive/wardyn-drive-a.labels && : >nodrive/wardyn-drive-a.tar.gz &&
  echo wardyn-drive-a >nodrive/drives.list && B="$T/nodrive" DB_IDS=00000000-0000-4000-8000-000000000000 \
  bash -c 'set -eu; . "$1"; . "$2"; echo REACHED' _ "$T/stub.sh" "$T/loop.sh" 2>&1)" && fail "an unknown drive did not stop the loop: $out"
[[ "$out" == *"has no drive $ID"* ]] || fail "an unknown drive stopped with the wrong line: $out"

# Mutation: the same loop without the label forwarding must not restore the volume.
grep -v -- '--label "\$x"' "$T/loop.sh" >"$T/loop-nolabels.sh"
! cmp -s "$T/loop.sh" "$T/loop-nolabels.sh" || fail "the label forwarding line was not found to take out"
if out="$(run mutant "$T/loop-nolabels.sh" "$good")" && [[ "$out" == *REACHED* ]]; then
  fail "the loop with no label forwarding still restored: this test checks nothing"
fi

echo "test-ops-drive-restore: ok"
