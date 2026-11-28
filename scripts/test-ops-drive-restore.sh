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
    "volume inspect --format "*) sed -n 's/^--label //p' "$B/create.args"; echo ;;
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
  printf '%s\n\n' "$3" >"$B/wardyn-drive-a.labels"
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

# Two drive ids, an id that is not canonical, one carrying SQL quoting, a valid id
# then an empty one, or only similarly named keys: held, no volume made.
lookalike="wardyn-drive=$ID
wardyn-home=legacy
wardyn.managed=true"
for bad in "wardyn.drive=$ID
$good" "${good/$ID/0F8C1B2A-3D4E-4F50-8A6B-7C8D9E0F1A2B}" "${good/$ID/$ID\'; SELECT 1; --}" "$good
wardyn.drive=" "$lookalike"; do
  rm -rf "$T/bad"
  out="$(run bad "$T/loop.sh" "$bad")" || true
  [[ "$out" == *"HOLD:"* ]] || fail "a malformed drive id was not held: $out"
  [ ! -e "$T/bad/create.args" ] || fail "a volume was made for a malformed drive id"
done

# A drive the restored database does not hold stops the loop.
out="$(cd "$T" && mkdir -p nodrive && printf '%s\n\n' "$good" >nodrive/wardyn-drive-a.labels && : >nodrive/wardyn-drive-a.tar.gz &&
  echo wardyn-drive-a >nodrive/drives.list && B="$T/nodrive" DB_IDS=00000000-0000-4000-8000-000000000000 \
  bash -c 'set -eu; . "$1"; . "$2"; echo REACHED' _ "$T/stub.sh" "$T/loop.sh" 2>&1)" && fail "an unknown drive did not stop the loop: $out"
[[ "$out" == *"has no drive $ID"* ]] || fail "an unknown drive stopped with the wrong line: $out"

# The backup marks a record unresolved with the same check: both copies of usable
# must match, and the backup's must refuse what the restore holds.
awk '/^usable\(\) \{/{f=1} f{print} f&&/^\}$/{exit}' "$doc" >"$T/usable-backup.sh"
awk '/^### Restore them/{r=1} r&&/^usable\(\) \{/{f=1} f{print} f&&/^\}$/{exit}' "$doc" >"$T/usable-restore.sh"
[ -s "$T/usable-backup.sh" ] || fail "no usable check found in the backup"
cmp -s "$T/usable-backup.sh" "$T/usable-restore.sh" || fail "the backup and restore usable checks differ"
printf '%s\n' "$good" >"$T/rec"
(. "$T/usable-backup.sh"; [ "$(usable "$T/rec")" = "$ID" ]) || fail "the backup refused a good record"
for bad in "$lookalike" "${good/$ID/${ID%?}}" "$good
wardyn.drive=" "$good
wardyn.home=other" "${good/wardyn.managed=true/wardyn.managed=yes}"; do
  printf '%s\n' "$bad" >"$T/rec"
  ! (. "$T/usable-backup.sh"; usable "$T/rec" >/dev/null) || fail "the backup took a record it should mark unresolved: $bad"
done

# The backup's record loop, as the guide prints it, against a stub docker that
# ends each label record with the empty line the real CLI adds: a good volume of
# this installation gets a .labels record, and lookalike keys or a drive this
# database does not hold get .unresolved. The same loop counting n lines, not
# n+1, must fail the good volume.
awk '/^mv "\$B\/drives.list.tmp" "\$B\/drives.list"$/{f=1} f{print} f&&/^done 3< "\$B\/drives.list"$/{exit}' \
  "$doc" >"$T/backup.sh"
grep -q '^done 3< "\$B/drives.list"$' "$T/backup.sh" || fail "no step-4 record loop found in $doc"
cat >"$T/bstub.sh" <<'EOF2'
docker() {
  case "$*" in
    "run "*) return 0 ;;
    "volume inspect --format {{len .Labels}} "*) grep -c . "$B/src/${*: -1}" ;;
    "volume inspect --format "*) cat "$B/src/${*: -1}"; echo ;;
  esac
}
EOF2
# backup <name> <loop file> <labels>: runs the loop for one volume, wardyn-drive-a.
backup() {
  local B="$T/b-$1"
  mkdir -p "$B/src"
  printf '%s\n' "$3" >"$B/src/wardyn-drive-a"
  echo wardyn-drive-a >"$B/drives.list.tmp"
  B="$B" ids="${4:-$ID}" bash -c 'set -eu; . "$1"; . "$2"' _ "$T/bstub.sh" "$2" >/dev/null
}
backup good "$T/backup.sh" "$good" || fail "the backup loop failed on a good volume"
[ -f "$T/b-good/wardyn-drive-a.labels" ] && [ ! -e "$T/b-good/wardyn-drive-a.unresolved" ] ||
  fail "the backup marked a good volume unresolved"
backup lookalike "$T/backup.sh" "$lookalike" || fail "the backup loop failed on a lookalike volume"
[ -f "$T/b-lookalike/wardyn-drive-a.unresolved" ] || fail "the backup recorded a volume with only lookalike keys"
backup foreign "$T/backup.sh" "$good" 00000000-0000-4000-8000-000000000000 || fail "the backup loop failed on a foreign drive"
[ -f "$T/b-foreign/wardyn-drive-a.unresolved" ] || fail "the backup recorded a drive this database does not hold"
sed 's/-eq "$((n + 1))"/-eq "$n"/' "$T/backup.sh" >"$T/backup-n.sh"
! cmp -s "$T/backup.sh" "$T/backup-n.sh" || fail "the n+1 count was not found to change"
backup mutant "$T/backup-n.sh" "$good" || true
[ ! -f "$T/b-mutant/wardyn-drive-a.labels" ] || fail "the backup counting n lines still recorded the volume: this test checks nothing"

# A volume another process made between the existence check and create: the
# create succeeds on the existing volume, its labels differ, and the STOP must
# leave it in place, with no removal by the loop or in its words.
sed 's#^    "volume inspect --format "\*).*#    "volume inspect --format "*) printf "owner=other\\n\\n" ;;#' "$T/stub.sh" >"$T/stub-raced.sh"
! cmp -s "$T/stub.sh" "$T/stub-raced.sh" || fail "the label stub was not found to change"
rm -rf "$T/raced"
out="$(cd "$T" && mkdir -p raced && printf '%s\n\n' "$good" >raced/wardyn-drive-a.labels && : >raced/wardyn-drive-a.tar.gz &&
  echo wardyn-drive-a >raced/drives.list && B="$T/raced" DB_IDS="$ID" \
  bash -c 'set -eu; . "$1"; . "$2"; echo REACHED' _ "$T/stub-raced.sh" "$T/loop.sh" 2>&1)" && fail "a raced volume did not stop the loop: $out"
[[ "$out" == *"is not restored: leave it in place"* ]] || fail "a raced volume stopped with the wrong line: $out"
[[ "$out" != *"volume rm"* ]] || fail "the STOP for a raced volume says to remove it: $out"

# Step 2's reset runs down -v in the script's own environment, so on the
# installation step 0 named, and only when answered reset.
awk '/^### Restore them/{r=1} r&&/^printf .Does %s already hold/{f=1} f{print} f&&/^esac$/{exit}' "$doc" >"$T/reset.sh"
grep -q 'down -v' "$T/reset.sh" || fail "no step-2 reset found in $doc"
cat >"$T/rstub.sh" <<'EOF2'
docker() { echo "${COMPOSE_PROJECT_NAME:-} $*" >>"$T/reset.log"; }
ask() { :; }
EOF2
reset() { : >"$T/reset.log"; printf '%s\n' "$1" | T="$T" COMPOSE_PROJECT_NAME=named id="project=named ns=x" \
  bash -c 'set -eu; . "$1"; . "$2"' _ "$T/rstub.sh" "$T/reset.sh" >/dev/null; }
reset reset || fail "the reset answer did not run"
grep -qxF 'named compose -f deploy/compose/docker-compose.yaml down -v' "$T/reset.log" || fail "down -v did not run for the named installation: $(cat "$T/reset.log")"
reset no || fail "the no answer did not continue"
[ ! -s "$T/reset.log" ] || fail "down -v ran on a no answer"
! reset "" || fail "an empty answer did not stop"

# Mutation: the same loop without the label forwarding must not restore the volume.
grep -v -- '--label "\$x"' "$T/loop.sh" >"$T/loop-nolabels.sh"
! cmp -s "$T/loop.sh" "$T/loop-nolabels.sh" || fail "the label forwarding line was not found to take out"
if out="$(run mutant "$T/loop-nolabels.sh" "$good")" && [[ "$out" == *REACHED* ]]; then
  fail "the loop with no label forwarding still restored: this test checks nothing"
fi

echo "test-ops-drive-restore: ok"
