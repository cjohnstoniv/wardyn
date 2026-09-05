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
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# Same "../.." that wardyn-desktop.sh computes, and the same file: the MDM
# payload ships scripts/lib/common.sh at /usr/local/lib/wardyn/scripts/lib/
# precisely so both halves of the desktop tier resolve it with no code change
# (scripts/build-desktop-package.sh "LAYOUT"). We need wardyn_pick_docker_host
# here for the same reason the launcher needs it — see "Which Docker socket"
# below.
[ -f "${REPO_ROOT}/scripts/lib/common.sh" ] || {
  echo "install.sh: ${REPO_ROOT}/scripts/lib/common.sh missing — this bundle is incomplete." >&2
  echo "  wardyn-desktop.sh sources the same file and would die the same way, so a" >&2
  echo "  converge job registered now would never run. Use a full checkout, or the" >&2
  echo "  payload scripts/build-desktop-package.sh produces." >&2
  exit 1
}
# shellcheck source=../../scripts/lib/common.sh
# shellcheck disable=SC1091 # source= above is relative to this file, not resolvable from CWD
. "${REPO_ROOT}/scripts/lib/common.sh"  # wardyn_pick_docker_host, log/warn/die

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
  # This guard used to sit BELOW the uninstall branch, on the install path
  # only — so an uninstall with a missing/unexecutable launcher sailed past it
  # and `--purge` went on to delete age.key with the stack still running.
  [ -x "${WARDYN_DESKTOP_SH}" ] || { echo "install.sh: ${WARDYN_DESKTOP_SH} missing or not executable — it is what stops the stack, so an uninstall cannot proceed without it" >&2; exit 1; }

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
  # The teardown's exit status is load-bearing on the --purge path and only
  # there. wardyn-desktop.sh dies BEFORE it runs any `compose down` when no
  # Docker daemon is reachable (its own socket refusal) — so a swallowed
  # failure here used to mean --purge deleted age.key, deleted the envelope,
  # printed "Purged." and left the stack running against a secret store nothing
  # can decrypt again. A plain uninstall keeps everything, so best-effort is
  # still right there.
  TEARDOWN_OK=1
  if [ "${PURGE}" -eq 1 ]; then
    "${WARDYN_DESKTOP_SH}" down --purge || TEARDOWN_OK=0
  else
    "${WARDYN_DESKTOP_SH}" down || true
  fi

  if [ "${PURGE}" -eq 1 ]; then
    if [ "${TEARDOWN_OK}" -ne 1 ]; then
      echo "install.sh: '${WARDYN_DESKTOP_SH} down --purge' FAILED — refusing to delete ${MANAGED_DIR}." >&2
      echo "  ${AGE_FILE} is the only identity that can decrypt this device's secret" >&2
      echo "  store, and the stack may still be running. Nothing was removed." >&2
      echo "  Fix the teardown (most often: no reachable Docker daemon — see" >&2
      echo "  docs/DESKTOP.md 'Which Docker socket'), then re-run --uninstall --purge." >&2
      echo "  The converge job is already unregistered, so nothing will restart it." >&2
      exit 1
    fi
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

# ── Which Docker socket (enrolment half) ───────────────────────────────────
# This installer runs as ROOT, and Docker Desktop, Colima, rootless Docker and
# Podman all expose a PER-USER socket that root cannot see by default. That is
# the same failure docs/DESKTOP.md "Which Docker socket" calls this tier's
# likeliest install failure — except wardyn-desktop.sh's remedy (read
# WARDYN_DOCKER_SOCK from the MDM envelope) is not available here: enrolment
# runs BEFORE MDM has delivered /etc/wardyn/wardyn.env. So the override comes
# from the environment instead, and the same auto-detection the launcher uses
# fills in the rest:
#
#   sudo WARDYN_DOCKER_SOCK=/Users/<user>/.colima/default/docker.sock ./install.sh
#   sudo DOCKER_HOST=unix:///run/user/1000/docker.sock ./install.sh
#
# An explicit WARDYN_DOCKER_SOCK wins, exactly as the envelope value does at
# wardyn-desktop.sh's equivalent step.
if [ -n "${WARDYN_DOCKER_SOCK:-}" ]; then
  export WARDYN_DOCKER_SOCK
  export DOCKER_HOST="unix://${WARDYN_DOCKER_SOCK}"
fi
wardyn_pick_docker_host  # DOCKER_HOST / WARDYN_DOCKER_SOCK, incl. Colima/Rancher

echo "==> Creating ${MANAGED_DIR}"
install -d -m 0755 "${MANAGED_DIR}"

# com.wardyn.daemon.plist's StandardOutPath/StandardErrorPath — launchd does
# NOT create parent directories for these itself; an absent one is a silently
# swallowed launch failure, not a missing-log inconvenience.
install -d -m 0755 /var/log/wardyn

# Log rotation. The plist appends stdout AND stderr to one file every 300s
# forever and compose sets no max-size, so nothing bounded this before.
if [ "${OS}" = "Darwin" ]; then
  [ -f "${SCRIPT_DIR}/wardyn.newsyslog.conf" ] && install -m 0644 "${SCRIPT_DIR}/wardyn.newsyslog.conf" /etc/newsyslog.d/wardyn.conf
else
  # Linux logs to journald (which rotates itself); this only covers an operator
  # who has redirected the converge job's output to a file.
  [ -d /etc/logrotate.d ] && [ -f "${SCRIPT_DIR}/wardyn.logrotate.conf" ] && install -m 0644 "${SCRIPT_DIR}/wardyn.logrotate.conf" /etc/logrotate.d/wardyn
fi

if [ -s "${AGE_FILE}" ]; then
  echo "==> ${AGE_FILE} already exists — leaving it alone (a re-mint orphans every secret encrypted under the old key)"
else
  echo "==> Minting this device's secret-store age key"
  # Same mechanism scripts/up.sh and scripts/setup.sh use: `wardynd -gen-age-key`
  # needs no DSN and exits immediately, so no separate build is needed here.
  #
  # This is NOT the image the fleet then runs: wardyn.env pins
  # WARDYN_WARDYND_IMAGE by DIGEST (wardyn.env.example), while the default here
  # is the CONTINUOUS main-tip tag :latest that
  # .github/workflows/publish-image.yml pushes on every merge and does not
  # cosign-sign. Enrolment therefore pulls and runs unsigned main-tip code as
  # root, once, on this device. A fleet that will not accept that sets
  # WARDYN_INSTALL_IMAGE to the release digest it already pins in the envelope,
  # or to its corporate mirror of it — see docs/DESKTOP.md "The install lane".
  IMG="${WARDYN_INSTALL_IMAGE:-ghcr.io/cjohnstoniv/wardynd:latest}"
  # Say it at the console, not only in this comment: the comment is read by
  # whoever edits the installer, and the person who needs this is whoever RUNS
  # it. Nothing in the repo gated this call site — scripts/check-image-pins.sh
  # covers Dockerfile FROMs and deploy/compose/*.yaml only, so a `docker run` in
  # a shell script is outside every pin gate by design.
  case "${IMG}" in
    *@sha256:*) ;;
    *)
      echo "install.sh: WARNING — ${IMG} is a MUTABLE tag, not a digest." >&2
      echo "  This step runs that image AS ROOT to mint ${AGE_FILE}, the only" >&2
      echo "  identity that can decrypt this device's secret store. The default is" >&2
      echo "  the CONTINUOUS main-tip tag publish-image.yml pushes on every merge;" >&2
      echo "  it is not cosign-signed, so nothing verifies what gets pulled." >&2
      echo "  Pin it to the digest wardyn.env already pins for WARDYN_WARDYND_IMAGE:" >&2
      echo "    sudo WARDYN_INSTALL_IMAGE=ghcr.io/cjohnstoniv/wardynd@sha256:<digest> ./install.sh" >&2
      echo "  See docs/DESKTOP.md 'The install lane'." >&2
      ;;
  esac
  # Refuse LOUDLY on an unreachable daemon rather than let `docker run` fail
  # into the generic "produced no key" below — on Colima and rootless Docker
  # that is the ACTUAL cause, and it is the one message an enroller can act on.
  # Same diagnostic wardyn-desktop.sh prints, pointed at this script's override
  # instead of the envelope's, because there is no envelope yet.
  if ! docker info >/dev/null 2>&1; then
    echo "install.sh: no reachable Docker daemon." >&2
    echo "  Tried: DOCKER_HOST='${DOCKER_HOST:-<unset>}', WARDYN_DOCKER_SOCK='${WARDYN_DOCKER_SOCK:-<unset>}'." >&2
    echo "  This installer runs as root; Docker Desktop, Colima, rootless Docker and" >&2
    echo "  Podman all expose a PER-USER socket that root cannot see by default. Re-run" >&2
    echo "  with that socket's absolute path, e.g." >&2
    echo "    sudo WARDYN_DOCKER_SOCK=/Users/<user>/.colima/default/docker.sock ./install.sh" >&2
    echo "    sudo WARDYN_DOCKER_SOCK=/run/user/\$(id -u <user>)/docker.sock ./install.sh   # rootless Linux" >&2
    echo "  and set the same value in ${MANAGED_DIR}/wardyn.env so every converge tick" >&2
    echo "  finds it too. See docs/DESKTOP.md 'Which Docker socket'." >&2
    exit 1
  fi
  KEY="$(docker run --rm "${IMG}" -gen-age-key 2>/dev/null | grep -E '^AGE-SECRET-KEY-' | head -1 || true)"
  [ -n "${KEY}" ] || { echo "install.sh: '${IMG} -gen-age-key' produced no key — the daemon is reachable, so check the image ref (WARDYN_INSTALL_IMAGE overrides it) and that this host can pull it" >&2; exit 1; }
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
