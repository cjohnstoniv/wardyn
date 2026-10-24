#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Checks the runner package's postinstall ledger setup (deploy/runner/macos/pkg-scripts/postinstall) against
# a temp root (WARDYN_PKG_ROOT), without root or macOS. What it pins: the three ledger objects are made from
# nothing; a pre-planted user-owned /Users/Shared/Wardyn is moved aside and replaced; a pre-planted lock is
# replaced; root-owned objects that are not exactly the layout are moved aside or replaced, never chmodded;
# a symlinked Wardyn dir, entries dir or lock is never followed; a login racing the script with links and
# directories never gets a victim's mode or owner changed; a predicted quarantine name is never used; and a
# directory switched for a hardlink to a root-owned file between two lookups never gets that file chmodded.
#
# What it cannot check unprivileged: real root:wheel ownership. Stub stat, chown, mkdir, mktemp and install
# stand in, and keep a shadow table of the inodes the script made or chowned (root-owned; anything else
# reads as uid 501). chown and chmod follow a link unless given -h, as the BSD tools do. The stub reads
# `stat -f '%HT %u:%g %Mp%Lp'` from one GNU lstat, as BSD stat takes one snapshot. That the real BSD
# tools accept the same calls is proven on the hosted `macos` job (ci.yml) and P1 on the rented Mac.
#
# Usage: scripts/test-pkg-postinstall.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
POST="${REPO_ROOT}/deploy/runner/macos/pkg-scripts/postinstall"
fail() { echo "test-pkg-postinstall: FAIL: $*" >&2; exit 1; }
WORK="$(mktemp -d)"
bg=""
trap '[ -z "$bg" ] || { pkill -P "$bg"; kill "$bg"; } 2>/dev/null || true; rm -rf "$WORK"' EXIT

T="${WORK}/tools"; OWN="${WORK}/owners"; mkdir -p "$T" "$OWN"
export OWN
stub() { printf '#!/bin/bash\nmark() { local i; i="$(/usr/bin/stat -c %%i "$1" 2>/dev/null)" && : >"$OWN/$i"; }\nis_root() { local i; i="$(/usr/bin/stat -c %%i "$1" 2>/dev/null)" && [ -e "$OWN/$i" ]; }\n%s\n' "$2" >"$T/$1"; chmod +x "$T/$1"; }
# BSD `stat -f <fmt> <path>` reads the path itself (lstat), from GNU stat and the shadow owner table.
# SWAP_PATH/SWAP_TO: the first stat of SWAP_PATH first swaps it for a hardlink to SWAP_TO, as a login winning
# the gap before that lookup would.
stub stat 'p="$3"; if [ -n "${SWAP_PATH:-}" ] && [ "$p" = "$SWAP_PATH" ] && [ ! -e "$SWAP_PATH.swapped" ]; then
  /bin/mv "$p" "$p.away"; /bin/ln "$SWAP_TO" "$p"; : >"$SWAP_PATH.swapped"; fi
[ -e "$p" ] || [ -L "$p" ] || exit 1; case "$2" in
  "%HT %u:%g %Mp%Lp") IFS="|" read -r t i m < <(/usr/bin/stat -c "%F|%i|%a" "$p")
    case "$t" in directory) t=Directory ;; "regular file"|"regular empty file") t="Regular File" ;; "symbolic link") t="Symbolic Link" ;; esac
    if [ -e "$OWN/$i" ]; then o=0:0; else o=501:20; fi; printf "%s %s %04o\n" "$t" "$o" "0$m" ;;
  %u) if is_root "$p"; then echo 0; else echo 501; fi ;;
  %u:%g) if is_root "$p"; then echo 0:0; else echo 501:20; fi ;;
  %Lp) /usr/bin/stat -c %a "$p" ;;
  %Mp%Lp) printf "%04o\n" "0$(/usr/bin/stat -c %a "$p")" ;;
  *) exit 1 ;;
esac'
stub chown 'h=0; for a; do case "$a" in -h) h=1 ;; root:wheel) ;; *) if [ "$h" = 1 ]; then mark "$a"; else mark "$(readlink -f "$a")"; fi ;; esac; done'
stub chmod 'h=0; args=(); for a; do if [ "$a" = -h ]; then h=1; else args+=("$a"); fi; done
[ "$h" = 1 ] && [ -L "${args[-1]}" ] && exit 0; exec /bin/chmod "${args[@]}"'
stub mkdir '/bin/mkdir "$@" || exit; mark "${@: -1}"'
stub mktemp 'd="$(/usr/bin/mktemp "$@")" || exit; mark "$d"; echo "$d"'
# rm forgets the inodes it frees, so a reused inode never reads as root-owned.
stub rm 'for a; do case "$a" in -*) ;; *) find "$a" -printf "%i\n" 2>/dev/null | while read -r i; do /bin/rm -f "$OWN/$i"; done ;; esac; done; exec /bin/rm "$@"'
stub install 'args=(); dir=0; while [ $# -gt 0 ]; do case "$1" in -o|-g) shift 2 ;; -d) dir=1; shift ;; *) args+=("$1"); shift ;; esac; done
if [ "$dir" = 1 ]; then /usr/bin/install -d "${args[@]}"; else /usr/bin/install "${args[@]}"; fi; mark "$(readlink -f "${args[-1]}")"'
stub launchctl 'exit 0'

# fresh <name>: a new temp root that carries the helper binary, so the package would install the ledger.
fresh() { R="${WORK}/$1"; rm -rf "$R"; mkdir -p "$R/Users/Shared" "$R/usr/local/wardyn/bin"; chmod 1777 "$R/Users/Shared"; : >"$R/usr/local/wardyn/bin/wardyn-macvm"; find "$OWN" -type f -delete; }
post() { env -i PATH="$T:/usr/bin:/bin" OWN="$OWN" WARDYN_PKG_ROOT="$R" SWAP_PATH="${SWAP_PATH:-}" SWAP_TO="${SWAP_TO:-}" sh "$POST" >"${WORK}/out" 2>&1; }
run() { post || { cat "${WORK}/out" >&2; fail "postinstall failed ($1)"; }; }
ino() { /usr/bin/stat -c %i "$1"; }
root_owned() { [ -e "$OWN/$(ino "$1")" ]; }
mark() { : >"$OWN/$(ino "$1")"; }
mode() { /usr/bin/stat -c %a "$1"; }
# user_owns <path>: the shadow table forgets the path, so the stub stat reads it as a login's file again.
user_owns() { rm -f "$OWN/$(ino "$1")"; }
B() { echo "$R/Users/Shared/Wardyn"; }
check_ledger() { # check_ledger <what>
  local b; b="$(B)"
  [ -d "$b" ] && [ ! -L "$b" ] && root_owned "$b" && [ "$(mode "$b")" = 755 ] || fail "$1: Wardyn is not a root-owned 0755 directory"
  [ -d "$b/macos-vm-admission" ] && [ ! -L "$b/macos-vm-admission" ] && root_owned "$b/macos-vm-admission" && [ "$(mode "$b/macos-vm-admission")" = 1777 ] \
    || fail "$1: the entries directory is not a root-owned 1777 directory"
  [ -f "$b/admission.lock" ] && [ ! -L "$b/admission.lock" ] && root_owned "$b/admission.lock" && [ "$(mode "$b/admission.lock")" = 644 ] \
    || fail "$1: admission.lock is not a root-owned regular 0644 file"
  [ ! -e "$b/macos-vm-admission/admission.lock" ] || fail "$1: a second lock sits in the entries directory"
}

# 1. From nothing, and again over a sound ledger: the lock keeps its inode.
fresh nothing; run nothing; check_ledger "from nothing"
echo held >"$(B)/admission.lock"; ino="$(/usr/bin/stat -c %i "$(B)/admission.lock")"
run again; check_ledger "an upgrade"
[ "$(/usr/bin/stat -c %i "$(B)/admission.lock")" = "$ino" ] && [ "$(cat "$(B)/admission.lock")" = held ] || fail "an upgrade replaced a sound lock"

# 2. A pre-planted user-owned Wardyn directory (with a lock of its own and a world-writable mode).
fresh planted-dir; mkdir -p "$(B)/macos-vm-admission"; : >"$(B)/admission.lock"; : >"$(B)/evil"; chmod 777 "$(B)"
run planted-dir; check_ledger "a planted directory"
[ ! -e "$(B)/evil" ] || fail "the planted directory's contents survived in place"
q="$(ls -d "$R"/Users/Shared/.wardyn-quarantine.* 2>/dev/null | head -1 || true)"
[ -n "$q" ] && [ -e "$q/Wardyn/evil" ] || fail "the planted directory was not moved aside"
[ "$(mode "$q")" = 700 ] && root_owned "$q" || fail "the quarantine is not root-only"

# 3. A pre-planted lock: user-owned, and root-owned but writable.
fresh planted-lock; run first; echo planted >"$(B)/admission.lock.new"; rm "$(B)/admission.lock"; mv "$(B)/admission.lock.new" "$(B)/admission.lock"; user_owns "$(B)/admission.lock"
run planted-lock; check_ledger "a planted lock"
[ ! -s "$(B)/admission.lock" ] || fail "the planted lock's content survived"
# The inode may be reused once freed, so the planted content, not the inode number, shows a replacement.
chmod 666 "$(B)/admission.lock"; echo planted >"$(B)/admission.lock"
run writable-lock; check_ledger "a writable lock"
[ ! -s "$(B)/admission.lock" ] || fail "a group/other-writable lock was kept"

# 4. Symlinks are never followed.
fresh link-dir; mkdir "$R/elsewhere"; echo keep >"$R/elsewhere/f"; chmod 700 "$R/elsewhere"; ln -s "$R/elsewhere" "$(B)"
mark "$(B)" # even a link that reads as root-owned is replaced, not followed
run link-dir; check_ledger "a symlinked Wardyn directory"
[ "$(mode "$R/elsewhere")" = 700 ] && ! root_owned "$R/elsewhere" && [ "$(cat "$R/elsewhere/f")" = keep ] && [ -z "$(ls "$R/elsewhere" | grep -v '^f$')" ] \
  || fail "the symlink's target was changed"
fresh link-lock; run first; rm "$(B)/admission.lock"; echo keep >"$R/target"; chmod 666 "$R/target"; ln -s "$R/target" "$(B)/admission.lock"; mark "$(B)/admission.lock"; mark "$R/target"
run link-lock; check_ledger "a symlinked lock"
[ "$(cat "$R/target")" = keep ] && [ "$(mode "$R/target")" = 666 ] || fail "the lock symlink's target was changed"
fresh link-ents; run first; rm -rf "$(B)/macos-vm-admission"; mkdir "$R/ents-target"; ln -s "$R/ents-target" "$(B)/macos-vm-admission"
run link-ents; check_ledger "a symlinked entries directory"
[ -z "$(ls -A "$R/ents-target")" ] || fail "the entries symlink's target was written to"

# 5. Nothing found existing is chmodded. A root-owned base that is not exact is moved aside and made again;
# inside a sound base, children that are not exact are replaced.
fresh not-exact-base; run first; chmod 700 "$(B)"; : >"$(B)/marker"
run not-exact-base; check_ledger "a root-owned 0700 base"
[ ! -e "$(B)/marker" ] && [ -n "$(ls -d "$R"/Users/Shared/.wardyn-quarantine.*/Wardyn/marker 2>/dev/null || true)" ] \
  || fail "a root-owned base that was not exact was kept"
fresh not-exact-children; run first; chmod 755 "$(B)/macos-vm-admission"; : >"$(B)/macos-vm-admission/e-1.json"
chmod 600 "$(B)/admission.lock"; echo held >"$(B)/admission.lock"
run not-exact-children; check_ledger "children that are not exact"
[ ! -e "$(B)/macos-vm-admission/e-1.json" ] && [ ! -s "$(B)/admission.lock" ] || fail "a child that was not exact was kept"

# 6. A package without the helper makes no ledger.
fresh nohelper; rm "$R/usr/local/wardyn/bin/wardyn-macvm"; run nohelper
[ ! -e "$(B)" ] || fail "a package without the helper made the ledger"

# victim <name>: a login's directory and file the script must never change; prints their state.
victim_state() { printf '%s %s %s %s\n' "$(mode "$R/victim")" "$(root_owned "$R/victim" && echo root || echo user)" \
  "$(mode "$R/victimf")" "$(root_owned "$R/victimf" && echo root || echo user)"; }

# 7. The reviewer's race: while the script runs, a login keeps planting a link at Wardyn to a directory it
# owns, whose entries and lock are links to its victims, and keeps re-planting them after anything replaces
# them; or plants an empty directory of its own there. It only creates in /Users/Shared and only changes
# what it owns, as a real login can. Over 20 runs the victims never change; a run that makes the ledger
# makes it exactly, and one that cannot fails closed.
made=0
for i in $(seq 1 20); do
  fresh "race-$i"; mkdir "$R/victim" "$R/farm"; chmod 700 "$R/victim"; echo keep >"$R/victimf"; chmod 600 "$R/victimf"
  before="$(victim_state)"
  ( set +e; f="$R/farm"; b="$(B)"
    while :; do ln -s "$f" "$b"; /bin/mkdir "$b"; done 2>/dev/null &
    while :; do
      [ -L "$f/macos-vm-admission" ] || { "$T/rm" -rf "$f/macos-vm-admission"; ln -s "$R/victim" "$f/macos-vm-admission"; }
      [ -L "$f/admission.lock" ] || { "$T/rm" -rf "$f/admission.lock"; ln -s "$R/victimf" "$f/admission.lock"; }
    done 2>/dev/null &
    wait ) &
  bg=$!
  rc=0; post || rc=$?
  pkill -P "$bg" || true; kill "$bg" 2>/dev/null || true; wait "$bg" 2>/dev/null || true; bg=""
  [ "$(victim_state)" = "$before" ] || fail "race run $i changed a victim: $before -> $(victim_state)"
  [ "$(cat "$R/victimf")" = keep ] && [ -z "$(ls -A "$R/victim")" ] || fail "race run $i wrote to a victim"
  if [ "$rc" = 0 ]; then check_ledger "race run $i"; made=$((made + 1))
  else grep -q 'macOS VMs stay off' "${WORK}/out" || fail "race run $i failed without saying so: $(cat "${WORK}/out")"; fi
done
echo "test-pkg-postinstall: race: victims untouched in 20 runs; ledger made in $made, failed closed in $((20 - made))"

# 8. A predicted quarantine name: the login plants a link to its victim at the name the script would pick
# from the time and its pid (known here: the wrapper's pid survives exec), so a moved-aside directory would
# land in the victim. The quarantine name must be random and the planted link never used.
fresh predict; mkdir "$R/victim" "$(B)"; chmod 700 "$R/victim"; : >"$(B)/evil"
stub date 'echo 1700000000'
env -i PATH="$T:/usr/bin:/bin" OWN="$OWN" WARDYN_PKG_ROOT="$R" sh -c 'echo $$ >"$1"; while [ ! -e "$2" ]; do sleep 0.01; done; exec sh "$3"' _ \
  "$WORK/pid" "$WORK/go" "$POST" >"${WORK}/out" 2>&1 &
p=$!; until [ -s "$WORK/pid" ]; do sleep 0.01; done
for t in 1700000000 1700000001; do ln -s "$R/victim" "$R/Users/Shared/.wardyn-quarantine-$t-$(cat "$WORK/pid")"; done
: >"$WORK/go"; wait "$p" || { cat "${WORK}/out" >&2; fail "postinstall failed (predict)"; }
/bin/rm "$T/date"
check_ledger "a predicted quarantine name"
[ -z "$(ls -A "$R/victim")" ] && [ "$(mode "$R/victim")" = 700 ] && ! root_owned "$R/victim" || fail "the moved-aside directory landed in the predicted name's target"
q="$(ls -d "$R"/Users/Shared/.wardyn-quarantine.?????? 2>/dev/null | head -1 || true)"
[ -n "$q" ] && [ -e "$q/Wardyn/evil" ] && [ "$(mode "$q")" = 700 ] && root_owned "$q" || fail "the directory was not moved into a random root-only quarantine"

# 9. A type switch between lookups: a login's Wardyn directory is renamed away and a hardlink to a root-owned
# 0600 file planted at the name just before the script's first stat of it. That file must keep its mode and
# owner, and the hardlink must be moved aside, not made the base.
fresh type-switch; mkdir "$(B)"; echo secret >"$R/secret"; chmod 600 "$R/secret"; mark "$R/secret"
rc=0; SWAP_PATH="$(B)" SWAP_TO="$R/secret" post || rc=$?
[ -e "$(B).swapped" ] || fail "the type switch never happened"
[ "$(mode "$R/secret")" = 600 ] && root_owned "$R/secret" && [ "$(cat "$R/secret")" = secret ] \
  || fail "the type switch changed the root-owned file: mode 0600 -> $(mode "$R/secret") (postinstall exit $rc)"
[ "$rc" = 0 ] || { cat "${WORK}/out" >&2; fail "postinstall failed (type-switch)"; }
check_ledger "a type switch"; [ "$(ino "$(B)")" != "$(ino "$R/secret")" ] || fail "the hardlink became the base"

echo "test-pkg-postinstall: ok"
