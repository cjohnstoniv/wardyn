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
#   3. Registers the periodic converge job with the platform init system,
#      pointed at wardyn-desktop.sh at ITS PRESENT location — i.e. wherever
#      this installer bundle was placed on disk (a pkg/deb payload, or a manual
#      checkout). That location becomes the permanent install path; moving it
#      afterward needs a re-run of this step.
#
#        macOS   com.wardyn.daemon.plist -> launchd (RunAtLoad + StartInterval)
#        Linux   wardyn.service + wardyn.timer -> systemd (OnBootSec +
#                OnUnitActiveSec), a SYSTEM unit running as root, matching the
#                LaunchDaemon. See docs/DESKTOP.md "Which Docker socket" for
#                what that means on a box whose container runtime is rootless.
#
# It does NOT write wardyn.env, secret.env, policy.json or site-config.json —
# those are MDM's files. Until MDM (or an operator, for a pilot) delivers
# them, wardyn-desktop.sh refuses to start with a clear error.
#
# Usage: sudo ./install.sh
#        sudo ./install.sh --uninstall            (keeps age.key + the DB)
#        sudo ./install.sh --uninstall --purge    (destroys BOTH, irreversibly)
set -euo pipefail

OS="$(uname -s)"
case "${OS}" in
  Darwin|Linux) ;;
  *) echo "install.sh: unsupported platform '${OS}' — the desktop tier targets macOS (launchd) and Linux (systemd)." >&2; exit 1 ;;
esac

MODE=install
PURGE=0
for arg in "$@"; do
  case "${arg}" in
    --uninstall) MODE=uninstall ;;
    --purge) PURGE=1 ;;
    *) echo "install.sh: unknown argument '${arg}' (--uninstall, --purge)" >&2; exit 1 ;;
  esac
done

if [ "$(id -u)" != "0" ]; then
  echo "install.sh: must run as root (sudo ./install.sh) — it writes /etc/wardyn and the system init directory" >&2
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WARDYN_DESKTOP_SH="${SCRIPT_DIR}/wardyn-desktop.sh"
PLIST_TEMPLATE="${SCRIPT_DIR}/com.wardyn.daemon.plist"
PLIST_DEST="/Library/LaunchDaemons/com.wardyn.daemon.plist"
UNIT_TEMPLATE="${SCRIPT_DIR}/wardyn.service"
TIMER_TEMPLATE="${SCRIPT_DIR}/wardyn.timer"
UNIT_DEST="/etc/systemd/system/wardyn.service"
TIMER_DEST="/etc/systemd/system/wardyn.timer"
MANAGED_DIR="/etc/wardyn"
AGE_FILE="${MANAGED_DIR}/age.key"

# ── uninstall ──────────────────────────────────────────────────────────────
# Plain uninstall KEEPS /etc/wardyn/age.key and the Postgres volume, matching
# dpkg/rpm convention, so a re-install recovers the device. --purge removes
# them, and age.key deletion is TERMINAL: it is the only identity that can
# decrypt this device's secret store, it is minted per-device, and it never
# rides in an MDM payload, so no copy exists anywhere else.
if [ "${MODE}" = "uninstall" ]; then
  echo "==> Stopping the converge job"
  if [ "${OS}" = "Darwin" ]; then
    launchctl bootout system "${PLIST_DEST}" >/dev/null 2>&1 || true
    rm -f "${PLIST_DEST}"
  else
    systemctl disable --now wardyn.timer >/dev/null 2>&1 || true
    systemctl disable --now wardyn.service >/dev/null 2>&1 || true
    rm -f "${UNIT_DEST}" "${TIMER_DEST}"
    systemctl daemon-reload >/dev/null 2>&1 || true
  fi

  echo "==> Stopping the stack"
  if [ "${PURGE}" -eq 1 ]; then
    "${WARDYN_DESKTOP_SH}" down --purge || true
  else
    "${WARDYN_DESKTOP_SH}" down || true
  fi

  if [ "${PURGE}" -eq 1 ]; then
    echo "==> --purge: removing ${MANAGED_DIR}"
    echo "    ${AGE_FILE} is the ONLY identity that can decrypt this device's"
    echo "    secret store. Removing it makes every secret stored here"
    echo "    UNRECOVERABLE. There is no copy: it is minted per-device and never"
    echo "    rides in an MDM payload."
    rm -rf "${MANAGED_DIR}"
    rm -rf /var/log/wardyn
    echo "==> Purged."
  else
    echo "==> Kept ${MANAGED_DIR} (age.key + the MDM envelope) and the Postgres volume."
    echo "    Re-running install.sh recovers this device. Use --purge to destroy them."
  fi
  exit 0
fi

[ -x "${WARDYN_DESKTOP_SH}" ] || { echo "install.sh: ${WARDYN_DESKTOP_SH} missing or not executable" >&2; exit 1; }
if [ "${OS}" = "Darwin" ]; then
  [ -f "${PLIST_TEMPLATE}" ] || { echo "install.sh: ${PLIST_TEMPLATE} missing" >&2; exit 1; }
else
  [ -f "${UNIT_TEMPLATE}" ] || { echo "install.sh: ${UNIT_TEMPLATE} missing" >&2; exit 1; }
  [ -f "${TIMER_TEMPLATE}" ] || { echo "install.sh: ${TIMER_TEMPLATE} missing" >&2; exit 1; }
  command -v systemctl >/dev/null 2>&1 || { echo "install.sh: systemctl not found — this Linux path targets systemd. docs/DESKTOP.md covers running wardyn-desktop.sh from another supervisor." >&2; exit 1; }
fi
command -v docker >/dev/null 2>&1 || { echo "install.sh: docker not found on PATH — install Docker Desktop, Colima or Docker Engine first" >&2; exit 1; }

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

if [ "${OS}" = "Darwin" ]; then
  echo "==> Registering ${PLIST_DEST}"
  sed "s#__WARDYN_DESKTOP_SH__#${WARDYN_DESKTOP_SH}#" "${PLIST_TEMPLATE}" > "${PLIST_DEST}"
  chmod 0644 "${PLIST_DEST}"
  # root:wheel is macOS-only — Debian and Ubuntu have no `wheel` group, so this
  # is a HARD FAILURE under `set -euo pipefail` rather than a cosmetic
  # difference. It is why this whole block is platform-branched.
  chown root:wheel "${PLIST_DEST}"

  # Idempotent (re)load: bootout is a no-op (ignore its failure) if not
  # currently loaded — same "safe to run twice" posture as the rest of this
  # script.
  launchctl bootout system "${PLIST_DEST}" >/dev/null 2>&1 || true
  launchctl bootstrap system "${PLIST_DEST}"
  echo "==> com.wardyn.daemon loaded"
else
  echo "==> Registering ${UNIT_DEST} + ${TIMER_DEST}"
  sed "s#__WARDYN_DESKTOP_SH__#${WARDYN_DESKTOP_SH}#" "${UNIT_TEMPLATE}" > "${UNIT_DEST}"
  cp "${TIMER_TEMPLATE}" "${TIMER_DEST}"
  chmod 0644 "${UNIT_DEST}" "${TIMER_DEST}"
  chown root:root "${UNIT_DEST}" "${TIMER_DEST}"

  systemctl daemon-reload
  # Enable the TIMER, not the service: the service is a oneshot the timer
  # drives. Enabling both would run a converge at boot outside the timer's
  # schedule and confuse `systemctl list-timers`.
  systemctl enable --now wardyn.timer
  echo "==> wardyn.timer enabled (next converge: systemctl list-timers wardyn.timer)"
fi

echo
echo "install.sh: done. Still needed before the daemon can boot:"
echo "  MDM must deliver ${MANAGED_DIR}/wardyn.env, secret.env, policy.json"
echo "  (docs/DESKTOP.md 'The MDM file table'). Until then:"
if [ "${OS}" = "Darwin" ]; then
  echo "    sudo launchctl print system/com.wardyn.daemon | head -20"
else
  echo "    systemctl status wardyn.service; journalctl -u wardyn -n 30"
fi
echo "  will show the job retrying and failing closed, not silently succeeding."
echo
echo "  Uninstall:  sudo ${BASH_SOURCE[0]} --uninstall            (keeps age.key + the DB)"
echo "              sudo ${BASH_SOURCE[0]} --uninstall --purge    (destroys both, no undo)"
