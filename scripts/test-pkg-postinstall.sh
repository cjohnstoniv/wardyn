#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Checks the runner package's postinstall ledger setup (deploy/runner/macos/pkg-scripts/postinstall) against
# a temp root (WARDYN_PKG_ROOT), without root or macOS. Four things it pins: the three ledger objects are
# made from nothing; a pre-planted user-owned /Users/Shared/Wardyn is moved aside and replaced; a
# pre-planted lock is replaced; a symlinked Wardyn dir or lock is never followed.
#
# What it cannot check unprivileged: real root:wheel ownership. Stub stat, chown and install stand in, and keep
# a shadow table of which paths the script made root-owned (anything else reads as uid 501). That the real
# BSD tools accept the same calls is proven only on a macOS runner (P1 on the rented Mac).
#
# Usage: scripts/test-pkg-postinstall.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
POST="${REPO_ROOT}/deploy/runner/macos/pkg-scripts/postinstall"
fail() { echo "test-pkg-postinstall: FAIL: $*" >&2; exit 1; }
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

T="${WORK}/tools"; OWN="${WORK}/owners"; mkdir -p "$T" "$OWN"
export OWN
stub() { printf '#!/bin/bash\n%s\n' "$2" >"$T/$1"; chmod +x "$T/$1"; }
key() { printf '%s' "$1" | tr / _; }
# BSD `stat -f %u|%Lp <path>`, from GNU stat and the shadow owner table.
stub stat 'p="$3"; case "$2" in
  %u) if [ -e "$OWN/$(printf %s "$p" | tr / _)" ]; then echo 0; else echo 501; fi ;;
  %Lp) /usr/bin/stat -L -c %a "$p" ;;
  *) exit 1 ;;
esac'
stub chown 'for a; do case "$a" in -h|root:wheel) ;; *) : >"$OWN/$(printf %s "$a" | tr / _)" ;; esac; done'
stub chmod 'args=(); for a; do [ "$a" = -h ] || args+=("$a"); done; exec /bin/chmod "${args[@]}"'
stub install 'args=(); dir=0; while [ $# -gt 0 ]; do case "$1" in -o|-g) shift 2 ;; -d) dir=1; shift ;; *) args+=("$1"); shift ;; esac; done
if [ "$dir" = 1 ]; then /usr/bin/install -d "${args[@]}"; target="${args[-1]}"; else /usr/bin/install "${args[@]}"; target="${args[-1]}"; fi
: >"$OWN/$(printf %s "$target" | tr / _)"'
stub launchctl 'exit 0'

# fresh <name>: a new temp root that carries the helper binary, so the package would install the ledger.
fresh() { R="${WORK}/$1"; rm -rf "$R"; mkdir -p "$R/Users/Shared" "$R/usr/local/wardyn/bin"; : >"$R/usr/local/wardyn/bin/wardyn-macvm"; : >"$OWN/x"; rm -f "$OWN"/*; }
run() { env -i PATH="$T:/usr/bin:/bin" OWN="$OWN" WARDYN_PKG_ROOT="$R" sh "$POST" >"${WORK}/out" 2>&1 || { cat "${WORK}/out" >&2; fail "postinstall failed ($1)"; }; }
root_owned() { [ -e "$OWN/$(key "$1")" ]; }
mode() { /usr/bin/stat -c %a "$1"; }
# user_owns <path>: the shadow table forgets the path, so the stub stat reads it as a login's file again.
user_owns() { rm -f "$OWN/$(key "$1")"; }
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
ino="$(/usr/bin/stat -c %i "$(B)/admission.lock")"
run again; check_ledger "an upgrade"
[ "$(/usr/bin/stat -c %i "$(B)/admission.lock")" = "$ino" ] || fail "an upgrade replaced a sound lock"

# 2. A pre-planted user-owned Wardyn directory (with a lock of its own and a world-writable mode).
fresh planted-dir; mkdir -p "$(B)/macos-vm-admission"; : >"$(B)/admission.lock"; : >"$(B)/evil"; chmod 777 "$(B)"
run planted-dir; check_ledger "a planted directory"
[ ! -e "$(B)/evil" ] || fail "the planted directory's contents survived in place"
q="$(ls -d "$R"/Users/Shared/.wardyn-quarantine-* 2>/dev/null | head -1)"
[ -n "$q" ] && [ -e "$q/evil" ] || fail "the planted directory was not moved aside"
[ "$(mode "$q")" = 700 ] && root_owned "$q" || fail "the quarantine is not root-only"

# 3. A pre-planted lock: user-owned, and root-owned but writable.
fresh planted-lock; run first; echo planted >"$(B)/admission.lock.new"; rm "$(B)/admission.lock"; mv "$(B)/admission.lock.new" "$(B)/admission.lock"; user_owns "$(B)/admission.lock"
run planted-lock; check_ledger "a planted lock"
[ ! -s "$(B)/admission.lock" ] || fail "the planted lock's content survived"
chmod 666 "$(B)/admission.lock"; ino="$(/usr/bin/stat -c %i "$(B)/admission.lock")"
run writable-lock; check_ledger "a writable lock"
[ "$(/usr/bin/stat -c %i "$(B)/admission.lock")" != "$ino" ] || fail "a group/other-writable lock was kept"

# 4. Symlinks are never followed.
fresh link-dir; mkdir "$R/elsewhere"; echo keep >"$R/elsewhere/f"; chmod 700 "$R/elsewhere"; ln -s "$R/elsewhere" "$(B)"
: >"$OWN/$(key "$(B)")" # even a link that reads as root-owned is replaced, not followed
run link-dir; check_ledger "a symlinked Wardyn directory"
[ "$(mode "$R/elsewhere")" = 700 ] && ! root_owned "$R/elsewhere" && [ "$(cat "$R/elsewhere/f")" = keep ] && [ -z "$(ls "$R/elsewhere" | grep -v '^f$')" ] \
  || fail "the symlink's target was changed"
fresh link-lock; run first; rm "$(B)/admission.lock"; echo keep >"$R/target"; chmod 666 "$R/target"; ln -s "$R/target" "$(B)/admission.lock"; : >"$OWN/$(key "$(B)/admission.lock")"; : >"$OWN/$(key "$R/target")"
run link-lock; check_ledger "a symlinked lock"
[ "$(cat "$R/target")" = keep ] && [ "$(mode "$R/target")" = 666 ] || fail "the lock symlink's target was changed"
fresh link-ents; run first; rm -rf "$(B)/macos-vm-admission"; mkdir "$R/ents-target"; ln -s "$R/ents-target" "$(B)/macos-vm-admission"
run link-ents; check_ledger "a symlinked entries directory"
[ -z "$(ls -A "$R/ents-target")" ] || fail "the entries symlink's target was written to"

# 5. A package without the helper makes no ledger.
fresh nohelper; rm "$R/usr/local/wardyn/bin/wardyn-macvm"; run nohelper
[ ! -e "$(B)" ] || fail "a package without the helper made the ledger"
echo "test-pkg-postinstall: ok"
