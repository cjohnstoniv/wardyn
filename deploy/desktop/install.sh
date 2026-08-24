#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# install.sh — the desktop-tier installer. Run ONCE per device, as root
# (an MDM package's postinstall script, or by hand for a pilot). It does
# exactly three things, matching docs/DESKTOP.md's "MDM file table":
#
#   1. Creates the managed directory, /etc/wardyn, that MDM will go on to
#      render wardyn.env / secret.env / policy.json / site-config.json into.
#   2. Mints this device's per-device secret-store key — NEVER via MDM (see
#      "Why age.key never rides in an MDM payload"). Idempotent: an existing
#      key is left alone, because re-minting orphans every secret encrypted
#      under the old one.
#   3. Registers com.wardyn.daemon.plist with launchd, pointed at
#      wardyn-desktop.sh at ITS PRESENT location — i.e. wherever this
#      installer bundle was placed on disk (a pkg payload, or a manual
#      checkout). That location becomes the permanent install path; moving it
#      afterward needs a re-run of this step.
#
# It does NOT write wardyn.env, secret.env, policy.json or site-config.json —
# those are MDM's files. Until MDM (or an operator, for a pilot) delivers
# them, wardyn-desktop.sh refuses to start with a clear error.
#
# Usage: sudo ./install.sh
set -euo pipefail

if [ "$(uname -s)" != "Darwin" ]; then
  echo "install.sh: this installer targets macOS (launchd). docs/DESKTOP.md's Linux/systemd path is not built yet." >&2
  exit 1
fi
if [ "$(id -u)" != "0" ]; then
  echo "install.sh: must run as root (sudo ./install.sh) — it writes /etc/wardyn and /Library/LaunchDaemons" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WARDYN_DESKTOP_SH="${SCRIPT_DIR}/wardyn-desktop.sh"
PLIST_TEMPLATE="${SCRIPT_DIR}/com.wardyn.daemon.plist"
PLIST_DEST="/Library/LaunchDaemons/com.wardyn.daemon.plist"
MANAGED_DIR="/etc/wardyn"
AGE_FILE="${MANAGED_DIR}/age.key"

[ -x "${WARDYN_DESKTOP_SH}" ] || { echo "install.sh: ${WARDYN_DESKTOP_SH} missing or not executable" >&2; exit 1; }
[ -f "${PLIST_TEMPLATE}" ] || { echo "install.sh: ${PLIST_TEMPLATE} missing" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "install.sh: docker not found on PATH — install Docker Desktop or Colima first" >&2; exit 1; }

echo "==> Creating ${MANAGED_DIR}"
install -d -m 0755 "${MANAGED_DIR}"

# com.wardyn.daemon.plist's StandardOutPath/StandardErrorPath — launchd does
# NOT create parent directories for these itself; an absent one is a silently
# swallowed launch failure, not a missing-log inconvenience.
install -d -m 0755 /var/log/wardyn

if [ -s "${AGE_FILE}" ]; then
  echo "==> ${AGE_FILE} already exists — leaving it alone (a re-mint orphans every secret encrypted under the old key)"
else
  echo "==> Minting this device's secret-store age key"
  # Same mechanism scripts/up.sh and scripts/setup.sh use: `wardynd -gen-age-key`
  # needs no DSN and exits immediately. Runs against the same image
  # wardyn-desktop.sh will run wardynd from, so no separate build is needed
  # here — an operator wanting a pinned/mirrored image sets WARDYN_INSTALL_IMAGE.
  IMG="${WARDYN_INSTALL_IMAGE:-ghcr.io/cjohnstoniv/wardynd:latest}"
  KEY="$(docker run --rm "${IMG}" -gen-age-key 2>/dev/null | grep -E '^AGE-SECRET-KEY-' | head -1 || true)"
  [ -n "${KEY}" ] || { echo "install.sh: '${IMG} -gen-age-key' produced no key — check docker and the image ref" >&2; exit 1; }
  umask 077
  printf '%s\n' "${KEY}" > "${AGE_FILE}"
  chmod 0600 "${AGE_FILE}"
  echo "==> Wrote ${AGE_FILE} (0600)"
fi

echo "==> Registering ${PLIST_DEST}"
sed "s#__WARDYN_DESKTOP_SH__#${WARDYN_DESKTOP_SH}#" "${PLIST_TEMPLATE}" > "${PLIST_DEST}"
chmod 0644 "${PLIST_DEST}"
chown root:wheel "${PLIST_DEST}"

# Idempotent (re)load: bootout is a no-op (ignore its failure) if not
# currently loaded — same "safe to run twice" posture as the rest of this
# script.
launchctl bootout system "${PLIST_DEST}" >/dev/null 2>&1 || true
launchctl bootstrap system "${PLIST_DEST}"
echo "==> com.wardyn.daemon loaded"

echo
echo "install.sh: done. Still needed before the daemon can boot:"
echo "  MDM must deliver ${MANAGED_DIR}/wardyn.env, secret.env, policy.json"
echo "  (docs/DESKTOP.md 'The MDM file table'). Until then:"
echo "    sudo launchctl print system/com.wardyn.daemon | head -20"
echo "  will show the job retrying and failing closed, not silently succeeding."
