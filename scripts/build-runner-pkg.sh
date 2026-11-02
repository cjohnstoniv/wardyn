#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# build-runner-pkg.sh — build the macOS installer package for the laptop runner (wardyn-runnerd).
#
#   scripts/build-runner-pkg.sh --version X.Y.Z[-rc.N] --arm64 <runnerd> --amd64 <runnerd> \
#                               --cli-arm64 <wardyn> --cli-amd64 <wardyn> [--macvm <wardyn-macvm> --guest-agent <wardyn-guest-agent>] --out <file.pkg>
#
# Needs macOS (lipo, pkgbuild, productbuild; security, codesign, productsign and xcrun when signing),
# so the release workflow runs it on a macOS runner. The package installs
#   /usr/local/wardyn/bin/wardyn-runnerd           universal binary (+ /usr/local/bin symlink, by postinstall)
#   /usr/local/wardyn/bin/wardyn                   the CLI, universal, signed like the runner (+ /usr/local/bin symlink)
#   /usr/local/wardyn/bin/wardyn-macvm             the macOS VM helper (--macvm; arm64 only), carrying the one
#                                                  com.apple.security.virtualization entitlement
#   /usr/local/wardyn/bin/wardyn-guest-agent       the in-guest agent the helper installs into a VM (--guest-agent,
#                                                  given with --macvm; arm64, no entitlement); the helper checks its version
#   /usr/local/wardyn/bin/runnerd-managed-start    what the LaunchAgent runs
#   /Library/LaunchAgents/com.wardyn.runnerd.managed.plist
#   /usr/local/wardyn/share/wardyn-runnerd.mobileconfig   the managed-preferences profile template
# from deploy/runner/macos/. It cannot register the runner: that needs a person's single-use token.
# With --macvm, postinstall also makes the host-wide macOS VM admission ledger.
#
# SIGNING IS OPT-IN BY SECRET, each step on its own, and a missing secret logs a "skipped" line and goes on:
#   binary   APPLE_DEVELOPER_ID_APPLICATION_P12_BASE64 + APPLE_DEVELOPER_ID_APPLICATION_P12_PASSWORD
#            + APPLE_DEVELOPER_ID_APPLICATION_IDENTITY   (codesign, hardened runtime; notarization needs it)
#   package  APPLE_DEVELOPER_ID_INSTALLER_P12_BASE64 + APPLE_DEVELOPER_ID_INSTALLER_P12_PASSWORD
#            + APPLE_DEVELOPER_ID_INSTALLER_IDENTITY     (productsign)
#   notarize APPLE_NOTARY_APPLE_ID + APPLE_NOTARY_TEAM_ID + APPLE_NOTARY_PASSWORD (an app-specific password);
#            runs only when both certificates signed, then staples the ticket. DRY_RUN=true skips it (the
#            package goes to Apple), and the release workflow sets that outside a vX.Y.Z tag run.
# The helper is signed with its entitlement (deploy/runner/macos/wardyn-macvm.entitlements) either way: with the
# Developer ID when the binary secrets are set, otherwise ad hoc (`codesign --sign -`), which the Virtualization
# framework accepts for a local run. The guest agent is signed the same two ways but with no entitlement: it
# runs inside the guest. No IPSW or disk image is ever packaged.
# Without them the package is unsigned: a person allows it in System Settings, Privacy & Security, Open
# Anyway (macOS 15 and later; right-click, Open no longer works), and MDM can push it only through an
# agent or script install (a Jamf or Kandji policy, `installer` run as root), not Apple's native app install.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="${REPO_ROOT}/deploy/runner/macos"
VERSION="" ARM64="" AMD64="" CLI_ARM64="" CLI_AMD64="" MACVM="" GUEST_AGENT="" OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?}"; shift 2 ;;
    --arm64) ARM64="${2:?}"; shift 2 ;;
    --amd64) AMD64="${2:?}"; shift 2 ;;
    --cli-arm64) CLI_ARM64="${2:?}"; shift 2 ;;
    --cli-amd64) CLI_AMD64="${2:?}"; shift 2 ;;
    --macvm) MACVM="${2:?}"; shift 2 ;;
    --guest-agent) GUEST_AGENT="${2:?}"; shift 2 ;;
    --out) OUT="${2:?}"; shift 2 ;;
    *) echo "usage: $0 --version X.Y.Z[-rc.N] --arm64 <runnerd> --amd64 <runnerd> --cli-arm64 <wardyn> --cli-amd64 <wardyn> [--macvm <wardyn-macvm> --guest-agent <wardyn-guest-agent>] --out <file.pkg>" >&2; exit 2 ;;
  esac
done
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] || { echo "build-runner-pkg: --version must be X.Y.Z or X.Y.Z-rc.N" >&2; exit 2; }
if [ ! -f "$ARM64" ] || [ ! -f "$AMD64" ] || [ ! -f "$CLI_ARM64" ] || [ ! -f "$CLI_AMD64" ] || [ -z "$OUT" ]; then
  echo "build-runner-pkg: --arm64, --amd64, --cli-arm64, --cli-amd64 (existing files) and --out are required" >&2
  exit 2
fi
if [ -n "$MACVM" ] && [ ! -f "$MACVM" ]; then
  echo "build-runner-pkg: --macvm must be an existing file" >&2
  exit 2
fi
if [ -n "$MACVM$GUEST_AGENT" ] && { [ -z "$MACVM" ] || [ -z "$GUEST_AGENT" ]; }; then
  echo "build-runner-pkg: --macvm and --guest-agent go together: the helper needs the agent to prepare a VM" >&2
  exit 2
fi
if [ -n "$GUEST_AGENT" ] && [ ! -f "$GUEST_AGENT" ]; then
  echo "build-runner-pkg: --guest-agent must be an existing file" >&2
  exit 2
fi
PKG_VERSION="${VERSION%%-*}"   # pkgbuild versions are dotted numbers

WORK="$(mktemp -d)"
KEYCHAIN=""
cleanup() {
  [ -z "$KEYCHAIN" ] || security delete-keychain "$KEYCHAIN" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# secret_set is true when every named variable is non-empty.
secret_set() { local v; for v in "$@"; do [ -n "${!v:-}" ] || return 1; done; }
APP_SECRETS=(APPLE_DEVELOPER_ID_APPLICATION_P12_BASE64 APPLE_DEVELOPER_ID_APPLICATION_P12_PASSWORD APPLE_DEVELOPER_ID_APPLICATION_IDENTITY)
INST_SECRETS=(APPLE_DEVELOPER_ID_INSTALLER_P12_BASE64 APPLE_DEVELOPER_ID_INSTALLER_P12_PASSWORD APPLE_DEVELOPER_ID_INSTALLER_IDENTITY)
NOTARY_SECRETS=(APPLE_NOTARY_APPLE_ID APPLE_NOTARY_TEAM_ID APPLE_NOTARY_PASSWORD)

# import_p12 <base64-var> <password-var>: add a certificate to the throwaway keychain, made on first use.
import_p12() {
  if [ -z "$KEYCHAIN" ]; then
    KEYCHAIN="${WORK}/build.keychain-db"
    local pw; pw="$(uuidgen)"
    security create-keychain -p "$pw" "$KEYCHAIN"
    security set-keychain-settings -lut 3600 "$KEYCHAIN"
    security unlock-keychain -p "$pw" "$KEYCHAIN"
    security list-keychains -d user -s "$KEYCHAIN" login.keychain-db
    KEYCHAIN_PW="$pw"
  fi
  printf '%s' "${!1}" | base64 --decode >"${WORK}/cert.p12"
  security import "${WORK}/cert.p12" -k "$KEYCHAIN" -P "${!2}" -T /usr/bin/codesign -T /usr/bin/productsign
  security set-key-partition-list -S apple-tool:,apple: -s -k "$KEYCHAIN_PW" "$KEYCHAIN" >/dev/null
  rm -f "${WORK}/cert.p12"
}

ROOT="${WORK}/root"
BIN="${ROOT}/usr/local/wardyn/bin"
mkdir -p "$BIN" "${ROOT}/usr/local/wardyn/share" "${ROOT}/Library/LaunchAgents"
lipo -create -output "${BIN}/wardyn-runnerd" "$ARM64" "$AMD64"
lipo -create -output "${BIN}/wardyn" "$CLI_ARM64" "$CLI_AMD64"
install -m 0755 "${SRC}/runnerd-managed-start.sh" "${BIN}/runnerd-managed-start"
install -m 0644 "${SRC}/com.wardyn.runnerd.managed.plist" "${ROOT}/Library/LaunchAgents/"
install -m 0644 "${SRC}/wardyn-runnerd.mobileconfig" "${ROOT}/usr/local/wardyn/share/"
chmod 0755 "${BIN}/wardyn-runnerd" "${BIN}/wardyn"
[ -z "$MACVM" ] || install -m 0755 "$MACVM" "${BIN}/wardyn-macvm"
[ -z "$GUEST_AGENT" ] || install -m 0755 "$GUEST_AGENT" "${BIN}/wardyn-guest-agent"
# The licence and notices travel with the binary, in the images' /usr/share/doc/wardyn/ layout.
DOC="${ROOT}/usr/local/wardyn/share/doc"
mkdir -p "${DOC}/licenses"
install -m 0644 "${REPO_ROOT}/LICENSE" "${REPO_ROOT}/NOTICE" "${REPO_ROOT}/THIRD-PARTY-NOTICES.md" "${REPO_ROOT}/LICENSING.md" "$DOC/"
cp -R "${REPO_ROOT}/licenses/texts/." "${DOC}/licenses/"

BIN_SIGNED=0 PKG_SIGNED=0
if secret_set "${APP_SECRETS[@]}"; then
  import_p12 APPLE_DEVELOPER_ID_APPLICATION_P12_BASE64 APPLE_DEVELOPER_ID_APPLICATION_P12_PASSWORD
  for b in wardyn-runnerd wardyn; do
    codesign --force --options runtime --timestamp --sign "$APPLE_DEVELOPER_ID_APPLICATION_IDENTITY" "${BIN}/${b}"
  done
  if [ -n "$MACVM" ]; then
    codesign --force --options runtime --timestamp --entitlements "${SRC}/wardyn-macvm.entitlements" \
      --sign "$APPLE_DEVELOPER_ID_APPLICATION_IDENTITY" "${BIN}/wardyn-macvm"
  fi
  if [ -n "$GUEST_AGENT" ]; then
    codesign --force --options runtime --timestamp --sign "$APPLE_DEVELOPER_ID_APPLICATION_IDENTITY" "${BIN}/wardyn-guest-agent"
  fi
  BIN_SIGNED=1
  echo "build-runner-pkg: signed the wardyn-runnerd and wardyn binaries${MACVM:+, the wardyn-macvm helper}${GUEST_AGENT:+ and the wardyn-guest-agent}"
else
  echo "build-runner-pkg: binary signing skipped: APPLE_DEVELOPER_ID_APPLICATION_* secrets are not all set"
  if [ -n "$MACVM" ]; then
    codesign --force --entitlements "${SRC}/wardyn-macvm.entitlements" --sign - "${BIN}/wardyn-macvm"
    echo "build-runner-pkg: signed the wardyn-macvm helper ad hoc, with its virtualization entitlement"
  fi
  if [ -n "$GUEST_AGENT" ]; then
    codesign --force --sign - "${BIN}/wardyn-guest-agent"
    echo "build-runner-pkg: signed the wardyn-guest-agent ad hoc, with no entitlement"
  fi
fi

pkgbuild --root "$ROOT" --identifier com.wardyn.runnerd.pkg --version "$PKG_VERSION" \
  --scripts "${SRC}/pkg-scripts" --install-location / "${WORK}/component.pkg"
productbuild --package "${WORK}/component.pkg" --version "$PKG_VERSION" "${WORK}/unsigned.pkg"

mkdir -p "$(dirname "$OUT")"
if secret_set "${INST_SECRETS[@]}"; then
  import_p12 APPLE_DEVELOPER_ID_INSTALLER_P12_BASE64 APPLE_DEVELOPER_ID_INSTALLER_P12_PASSWORD
  productsign --sign "$APPLE_DEVELOPER_ID_INSTALLER_IDENTITY" "${WORK}/unsigned.pkg" "$OUT"
  PKG_SIGNED=1
  echo "build-runner-pkg: signed the package"
else
  cp "${WORK}/unsigned.pkg" "$OUT"
  echo "build-runner-pkg: package signing skipped: APPLE_DEVELOPER_ID_INSTALLER_* secrets are not all set; the package is unsigned (Privacy & Security, Open Anyway)"
fi

if [ "${DRY_RUN:-}" = true ]; then
  echo "build-runner-pkg: notarization skipped: DRY_RUN=true, nothing is submitted to Apple"
elif [ "$PKG_SIGNED" = 1 ] && [ "$BIN_SIGNED" = 1 ] && secret_set "${NOTARY_SECRETS[@]}"; then
  xcrun notarytool submit "$OUT" --apple-id "$APPLE_NOTARY_APPLE_ID" --team-id "$APPLE_NOTARY_TEAM_ID" \
    --password "$APPLE_NOTARY_PASSWORD" --wait
  xcrun stapler staple "$OUT"
  echo "build-runner-pkg: notarized and stapled"
else
  echo "build-runner-pkg: notarization skipped: it needs the binary and package signed and the APPLE_NOTARY_* secrets set"
fi
echo "build-runner-pkg: wrote $OUT"
