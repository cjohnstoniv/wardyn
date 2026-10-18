#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# No-macOS checks for the runner's macOS package (deploy/runner/macos/, scripts/build-runner-pkg.sh).
# The build script runs against stub lipo, pkgbuild, productbuild, productsign, codesign, security and
# xcrun that record their arguments, so this proves the layout, the arguments, and that each signing
# step turns on with its own secrets and is skipped (with a log line) without them. That the real
# tools accept the result is only provable on a macOS runner (the release workflow's runner-pkg job).
#
# Usage: scripts/test-runner-pkg.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="${REPO_ROOT}/deploy/runner/macos"
BUILD="${REPO_ROOT}/scripts/build-runner-pkg.sh"
fail() { echo "test-runner-pkg: FAIL: $*" >&2; exit 1; }
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for f in "$BUILD" "${SRC}/runnerd-managed-start.sh" "${SRC}/pkg-scripts/preinstall" "${SRC}/pkg-scripts/postinstall"; do
  head -1 "$f" | grep -Eq '^#!/(usr/bin/env bash|bin/sh)$' || fail "$f has no shebang"
  [ -x "$f" ] || fail "$f is not executable (pkgbuild would ship a script macOS cannot run)"
  "$(head -1 "$f" | grep -q bash && echo bash || echo sh)" -n "$f" || fail "$f has a syntax error"
  if command -v shellcheck >/dev/null 2>&1; then shellcheck "$f" || fail "shellcheck rejected $f"; fi
done

# ---- the plists parse, and agree with the code and each other ------------------------------------
plist_get() { python3 -I -c 'import plistlib,sys; d=plistlib.load(open(sys.argv[1],"rb")); print(eval(sys.argv[2]))' "$1" "$2"; }
AGENT="${SRC}/com.wardyn.runnerd.managed.plist"
[ "$(plist_get "$AGENT" 'd["Label"]')" = com.wardyn.runnerd.managed ] || fail "the agent label changed"
[ "$(plist_get "$AGENT" 'd["ProgramArguments"][0]')" = /usr/local/wardyn/bin/runnerd-managed-start ] || fail "the agent does not run the launcher"
[ "$(plist_get "$AGENT" 'd["KeepAlive"]["SuccessfulExit"]')" = False ] || fail "the agent must restart only after a failure, or it loops while unregistered"
grep -q 'com.wardyn.runnerd.managed' "${SRC}/pkg-scripts/postinstall" "${SRC}/pkg-scripts/preinstall" || fail "the pkg scripts do not name the agent"
MC="${SRC}/wardyn-runnerd.mobileconfig"
[ "$(plist_get "$MC" 'd["PayloadContent"][0]["PayloadType"]')" = "$(sed -n 's/^const prefsDomain = "\(.*\)"$/\1/p' "${REPO_ROOT}/cmd/wardyn-runnerd/managed_prefs.go")" ] || fail "the profile's payload type is not the domain runnerd reads"
want_keys="$(grep -ho 'managedPref("[A-Za-z]*")\|{"[a-z-]*", "[A-Za-z]*"' "${REPO_ROOT}"/cmd/wardyn-runnerd/serve.go | sed 's/.*"\([A-Za-z]*\)".*/\1/' | sort -u | tr '\n' ' ')"
have_keys="$(plist_get "$MC" '" ".join(sorted(k for k in d["PayloadContent"][0] if not k.startswith("Payload")))')"
[ "$want_keys" = "$have_keys " ] || fail "the profile's keys ($have_keys) are not the preferences serve.go reads ($want_keys)"

# ---- the launcher ---------------------------------------------------------------------------------
H="${WORK}/home"; mkdir -p "$H"
printf '#!/bin/sh\necho "started: $*" >>"$STUB_LOG"\n' >"${WORK}/runnerd"; chmod +x "${WORK}/runnerd"
export STUB_LOG="${WORK}/launcher.log"; : >"$STUB_LOG"
# A stub plutil that finds an OrgURL only in a profile file that exists.
printf '#!/bin/sh\nfor a; do f="$a"; done\n[ -f "$f" ] && echo https://wardyn.example\n' >"${WORK}/plutil"; chmod +x "${WORK}/plutil"
PREFS="${WORK}/prefs"; mkdir -p "$PREFS"
launch() { WARDYN_RUNNERD_BIN="${WORK}/runnerd" WARDYN_RUNNERD_HOME="$H" WARDYN_RUNNERD_PLUTIL="${WORK}/plutil" WARDYN_RUNNERD_PREFS="$PREFS" sh "${SRC}/runnerd-managed-start.sh"; }
launch || fail "the launcher failed while unregistered (a nonzero exit restarts it every 10 seconds)"
[ ! -s "$STUB_LOG" ] || fail "the launcher served an unregistered computer"
grep -q 'not registered' "$H/Library/Logs/wardyn/runnerd.log" || fail "the launcher did not say why it stayed idle"
mkdir -p "$H/Library/Application Support/wardyn/runner"; : >"$H/Library/Application Support/wardyn/runner/runner.json"
mkdir -p "$H/Library/LaunchAgents"; : >"$H/Library/LaunchAgents/com.wardyn.runnerd.plist"
launch || fail "the launcher failed beside a setup agent"
[ ! -s "$STUB_LOG" ] || fail "the launcher started a second runner beside the setup agent"
rm "$H/Library/LaunchAgents/com.wardyn.runnerd.plist"
launch || fail "the launcher failed with no OrgURL (a nonzero exit restarts it every 10 seconds)"
[ ! -s "$STUB_LOG" ] || fail "the launcher served with no OrgURL in the profile"
grep -q 'sets no OrgURL' "$H/Library/Logs/wardyn/runnerd.log" || fail "the launcher did not say the profile has no OrgURL"
: >"$PREFS/com.wardyn.runnerd.plist"
launch || fail "the launcher failed when registered"
[ "$(cat "$STUB_LOG")" = "started: serve" ] || fail "the registered launcher did not exec 'serve': $(cat "$STUB_LOG")"

# ---- pre/postinstall -------------------------------------------------------------------------------
TB="${WORK}/tbin"; mkdir -p "$TB"
printf '#!/bin/sh\necho 501\n' >"$TB/stat"; printf '#!/bin/sh\necho "launchctl $*" >>"$STUB_LOG"; exit 1\n' >"$TB/launchctl"
printf '#!/bin/sh\necho "$0 $*" >>"$STUB_LOG"\n' >"$TB/ln"; cp "$TB/ln" "$TB/mkdir"; chmod +x "$TB"/*
: >"$STUB_LOG"
PATH="$TB:$PATH" sh "${SRC}/pkg-scripts/preinstall" || fail "preinstall failed (a failing preinstall aborts the install)"
PATH="$TB:$PATH" sh "${SRC}/pkg-scripts/postinstall" || fail "postinstall failed when launchctl refused"
grep -q 'launchctl bootout gui/501/com.wardyn.runnerd.managed' "$STUB_LOG" || fail "preinstall did not stop the agent"
grep -q 'launchctl bootstrap gui/501 /Library/LaunchAgents/com.wardyn.runnerd.managed.plist' "$STUB_LOG" || fail "postinstall did not load the agent"
grep -q 'ln -sf /usr/local/wardyn/bin/wardyn-runnerd /usr/local/bin/wardyn-runnerd' "$STUB_LOG" || fail "postinstall did not link the CLI"

# ---- the build script, against stub macOS tools ----------------------------------------------------
T="${WORK}/tools"; mkdir -p "$T"
stub() { printf '#!/bin/bash\n%s\n' "$2" >"$T/$1"; chmod +x "$T/$1"; }
stub lipo 'echo "lipo $*" >>"$STUB_LOG"; out=""; ins=(); while [ $# -gt 0 ]; do case "$1" in -create) shift;; -output) out="$2"; shift 2;; *) ins+=("$1"); shift;; esac; done; cat "${ins[@]}" >"$out"'
stub pkgbuild 'echo "pkgbuild $*" >>"$STUB_LOG"; root=""; while [ $# -gt 1 ]; do [ "$1" = --root ] && root="$2"; shift; done; (cd "$root" && find . -type f | sort | sed "s/^/payload /") >>"$STUB_LOG"; echo component >"$1"'
stub productbuild 'echo "productbuild $*" >>"$STUB_LOG"; echo product >"${@: -1}"'
stub productsign 'echo "productsign $*" >>"$STUB_LOG"; cp "${@: -2:1}" "${@: -1}"; echo signed >>"${@: -1}"'
stub codesign 'echo "codesign $*" >>"$STUB_LOG"'
stub security 'echo "security $1" >>"$STUB_LOG"'
stub uuidgen 'echo 00000000-0000-0000-0000-000000000000'
stub xcrun 'echo "xcrun $1 $2" >>"$STUB_LOG"'
echo arm >"${WORK}/arm64"; echo amd >"${WORK}/amd64"
CERT="$(printf 'SECRETBLOB' | base64 -w0)"
build() { # build <out> [VAR=value ...]
  local out="$1"; shift
  : >"$STUB_LOG"
  env -i PATH="$T:$PATH" STUB_LOG="$STUB_LOG" HOME="$WORK" "$@" "$BUILD" --version 0.9.3-rc.1 --arm64 "${WORK}/arm64" --amd64 "${WORK}/amd64" --out "$out" >"${WORK}/build.out" 2>&1 || { cat "${WORK}/build.out" >&2; fail "build failed ($*)"; }
}

build "${WORK}/unsigned.pkg"
[ -s "${WORK}/unsigned.pkg" ] || fail "no package written"
for p in ./Library/LaunchAgents/com.wardyn.runnerd.managed.plist ./usr/local/wardyn/bin/runnerd-managed-start ./usr/local/wardyn/bin/wardyn-runnerd ./usr/local/wardyn/share/wardyn-runnerd.mobileconfig; do
  grep -qx "payload $p" "$STUB_LOG" || fail "the payload lacks $p"
done
for p in LICENSE NOTICE THIRD-PARTY-NOTICES.md LICENSING.md; do
  grep -qx "payload ./usr/local/wardyn/share/doc/$p" "$STUB_LOG" || fail "the payload lacks the $p notice"
done
[ "$(grep -c '^payload ./usr/local/wardyn/share/doc/licenses/' "$STUB_LOG")" -eq "$(find "${REPO_ROOT}/licenses/texts" -type f | wc -l)" ] \
  || fail "the payload does not carry every licenses/texts file"
grep -q -- '--version 0.9.3 ' "$STUB_LOG" || fail "pkgbuild was not given the numeric version"
grep -q -- '--identifier com.wardyn.runnerd.pkg' "$STUB_LOG" && grep -q -- "--scripts ${SRC}/pkg-scripts" "$STUB_LOG" || fail "pkgbuild lost its identifier or scripts"
grep -Eq '^(codesign|productsign|xcrun|security)' "$STUB_LOG" && fail "an unsigned build ran a signing tool"
grep -q 'binary signing skipped' "${WORK}/build.out" && grep -q 'package signing skipped' "${WORK}/build.out" && grep -q 'notarization skipped' "${WORK}/build.out" || fail "a skipped step did not log"

# Installer certificate only: the package is signed, the binary is not, so no notarization.
build "${WORK}/inst.pkg" APPLE_DEVELOPER_ID_INSTALLER_P12_BASE64="$CERT" APPLE_DEVELOPER_ID_INSTALLER_P12_PASSWORD=pw "APPLE_DEVELOPER_ID_INSTALLER_IDENTITY=Developer ID Installer: Test (TEAM)" APPLE_NOTARY_APPLE_ID=a@b APPLE_NOTARY_TEAM_ID=T APPLE_NOTARY_PASSWORD=np
grep -q '^productsign --sign Developer ID Installer: Test (TEAM) ' "$STUB_LOG" || fail "productsign did not run with the installer secrets"
grep -q '^codesign' "$STUB_LOG" && fail "codesign ran without the application certificate"
grep -q '^xcrun' "$STUB_LOG" && fail "notarization ran with an unsigned binary"
tail -1 "${WORK}/inst.pkg" | grep -q signed || fail "the output is not the productsign result"

# Everything set: sign the binary, sign the package, notarize, staple, in that order, and never print a secret.
build "${WORK}/full.pkg" APPLE_DEVELOPER_ID_INSTALLER_P12_BASE64="$CERT" APPLE_DEVELOPER_ID_INSTALLER_P12_PASSWORD=pw-secret "APPLE_DEVELOPER_ID_INSTALLER_IDENTITY=Developer ID Installer: Test (TEAM)" \
  APPLE_DEVELOPER_ID_APPLICATION_P12_BASE64="$CERT" APPLE_DEVELOPER_ID_APPLICATION_P12_PASSWORD=pw-secret "APPLE_DEVELOPER_ID_APPLICATION_IDENTITY=Developer ID Application: Test (TEAM)" \
  APPLE_NOTARY_APPLE_ID=a@b APPLE_NOTARY_TEAM_ID=T APPLE_NOTARY_PASSWORD=np-secret
order="$(grep -Eo '^(codesign|pkgbuild|productsign|xcrun [a-z]+)' "$STUB_LOG" | tr '\n' ' ')"
[ "$order" = "codesign pkgbuild productsign xcrun notarytool xcrun stapler " ] || fail "signing steps ran out of order: $order"
grep -q -- '--options runtime --timestamp' "$STUB_LOG" || fail "codesign lost the hardened runtime"
grep -q 'notarized and stapled' "${WORK}/build.out" || fail "no notarization line"
if grep -rq 'pw-secret\|np-secret\|SECRETBLOB' "${WORK}/build.out"; then fail "the build log printed a secret"; fi

# A bad version is refused before any tool runs.
: >"$STUB_LOG"
if env -i PATH="$T:$PATH" STUB_LOG="$STUB_LOG" "$BUILD" --version main --arm64 "${WORK}/arm64" --amd64 "${WORK}/amd64" --out "${WORK}/x.pkg" >/dev/null 2>&1; then fail "a non-release version was accepted"; fi
[ ! -s "$STUB_LOG" ] || fail "a tool ran for a refused version"
echo "test-runner-pkg: ok"
