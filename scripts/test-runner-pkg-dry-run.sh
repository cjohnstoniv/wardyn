#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Pins build-runner-pkg.sh's dry-run path: with DRY_RUN=true and every signing and notary secret set,
# the package is still built and signed locally, but nothing is submitted to Apple (no notarytool, no
# stapler), and the log says so. The same build without DRY_RUN does notarize, so the stub is proven
# to see the call. Stub macOS tools record their arguments; no macOS needed.
#
# Usage: scripts/test-runner-pkg-dry-run.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD="${REPO_ROOT}/scripts/build-runner-pkg.sh"
fail() { echo "test-runner-pkg-dry-run: FAIL: $*" >&2; exit 1; }
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

T="${WORK}/tools"; mkdir -p "$T"
stub() { printf '#!/bin/bash\n%s\n' "$2" >"$T/$1"; chmod +x "$T/$1"; }
stub lipo 'out=""; ins=(); while [ $# -gt 0 ]; do case "$1" in -create) shift;; -output) out="$2"; shift 2;; *) ins+=("$1"); shift;; esac; done; cat "${ins[@]}" >"$out"'
stub pkgbuild 'while [ $# -gt 1 ]; do shift; done; echo component >"$1"'
stub productbuild 'echo product >"${@: -1}"'
stub productsign 'cp "${@: -2:1}" "${@: -1}"'
stub codesign ':'
stub security ':'
stub uuidgen 'echo 00000000-0000-0000-0000-000000000000'
stub xcrun 'echo "xcrun $1 $2" >>"$STUB_LOG"'
export STUB_LOG="${WORK}/calls.log"
for n in arm64 amd64 cli-arm64 cli-amd64; do echo "$n" >"${WORK}/$n"; done
CERT="$(printf 'BLOB' | base64 -w0)"
build() { # build [VAR=value ...]; the log of tool calls is $STUB_LOG, the script's output ${WORK}/build.out
  : >"$STUB_LOG"
  env -i PATH="$T:$PATH" STUB_LOG="$STUB_LOG" HOME="$WORK" \
    APPLE_DEVELOPER_ID_INSTALLER_P12_BASE64="$CERT" APPLE_DEVELOPER_ID_INSTALLER_P12_PASSWORD=pw "APPLE_DEVELOPER_ID_INSTALLER_IDENTITY=Developer ID Installer: Test (TEAM)" \
    APPLE_DEVELOPER_ID_APPLICATION_P12_BASE64="$CERT" APPLE_DEVELOPER_ID_APPLICATION_P12_PASSWORD=pw "APPLE_DEVELOPER_ID_APPLICATION_IDENTITY=Developer ID Application: Test (TEAM)" \
    APPLE_NOTARY_APPLE_ID=a@b APPLE_NOTARY_TEAM_ID=T APPLE_NOTARY_PASSWORD=np "$@" \
    "$BUILD" --version 0.9.3 --arm64 "${WORK}/arm64" --amd64 "${WORK}/amd64" --cli-arm64 "${WORK}/cli-arm64" --cli-amd64 "${WORK}/cli-amd64" --out "${WORK}/out.pkg" >"${WORK}/build.out" 2>&1 \
    || { cat "${WORK}/build.out" >&2; fail "build failed ($*)"; }
}

build DRY_RUN=true
[ -s "${WORK}/out.pkg" ] || fail "a dry run wrote no package"
if grep -q 'notarytool\|stapler' "$STUB_LOG"; then fail "a dry run called Apple: $(cat "$STUB_LOG")"; fi
grep -q 'notarization skipped: DRY_RUN=true' "${WORK}/build.out" || fail "a dry run did not say it skipped notarization"
if grep -q 'notarized and stapled' "${WORK}/build.out"; then fail "a dry run reported notarizing"; fi

build
grep -q 'xcrun notarytool' "$STUB_LOG" || fail "without DRY_RUN a fully signed build did not notarize, so this test cannot see the call"

build DRY_RUN=false
grep -q 'xcrun notarytool' "$STUB_LOG" || fail "DRY_RUN=false did not notarize"
echo "test-runner-pkg-dry-run: ok"
