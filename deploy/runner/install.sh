#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# install.sh — a thin wrapper over `wardyn-runnerd setup`, which does the work: it registers this
# computer (once), makes the proxy network, writes the service files and starts the service from
# login (a systemd user unit on Linux, a LaunchAgent on macOS). This script only finds a wardyn-runnerd
# and runs it. Every other argument goes to `setup` unchanged; `wardyn-runnerd setup --help` lists them.
# Run it as the person who owns the runner, not as root.
#
# Usage: install.sh --org <url> [--binary <path>] [--token-file <file>] [setup flags...]
#        install.sh --uninstall
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LABEL=com.wardyn.runnerd
UNIT=wardyn-runnerd.service

die() { echo "install.sh: $*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Usage: install.sh --org <url> [--binary <path>] [--token-file <file>] [setup flags...]
       install.sh --uninstall

  --binary <path>   wardyn-runnerd to install (default: beside this script, else on PATH)
  --uninstall       stop and remove the service, the installed wardyn-runnerd and, on a Mac, the wardyn-macvm and wardyn-guest-agent beside it, with their .previous rollback copies; keeps the runner identity, and the wardyn CLI and its cli.json (they are yours)

Everything else is passed to `wardyn-runnerd setup` (--org, --token-file, --proxy-image, --internal-network, --trusted-ca-file,
--relay-listen, --relay-host-target, --claim, --pool, --default and the limit flags); run `wardyn-runnerd setup --help`.
setup finds the relay address on Docker Desktop, Rancher Desktop and other VM-backed engines; --relay-listen and
--relay-host-target override it.
USAGE
}

BINARY="" UNINSTALL=0 PASS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BINARY="${2:?--binary needs a value}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) PASS+=("$1"); shift ;;
  esac
done

[ "$(id -u)" != 0 ] || die "run as the person who owns this runner, not as root"

uninstall() {
  case "$(uname -s)" in
    Linux)
      data_dir="${XDG_DATA_HOME:-$HOME/.local/share}/wardyn"
      identity_dir="$HOME/.config/wardyn/runner"
      # Setup names the identity directory in runnerd.json (an earlier setup, in the env file), which can be under a custom XDG_CONFIG_HOME; read it before the file goes.
      config="${XDG_CONFIG_HOME:-$HOME/.config}/wardyn/runnerd.json"
      kept="$(sed -n 's/^ *"state_dir": *"\(.*[^,]\),\{0,1\}$/\1/p' "$config" 2>/dev/null | tail -n 1)" || true
      kept="${kept%\"}"
      [ -n "$kept" ] || kept="$(sed -n 's/^WARDYN_RUNNER_STATE_DIR=//p' "$HOME/.config/wardyn/runnerd.env" 2>/dev/null | tail -n 1)" || true
      kept="${kept#\'}"; kept="${kept%\'}"
      [ -z "$kept" ] || identity_dir="$kept"
      # A service that is already gone is success; one that cannot be confirmed stopped leaves every file in place.
      state="$(systemctl --user show -p LoadState -p ActiveState "$UNIT" 2>/dev/null)" \
        || die "cannot reach the systemd user manager from this shell, so the service may still be running. Nothing was removed. Run this from a login session of the person who owns the runner, or stop it yourself (systemctl --user disable --now $UNIT), then run --uninstall again."
      if ! { grep -qx 'LoadState=not-found' <<<"$state" && grep -qx 'ActiveState=inactive' <<<"$state"; }; then
        systemctl --user disable --now "$UNIT" 2>/dev/null \
          || die "systemctl --user disable --now $UNIT failed, so the service may still be running. Nothing was removed. Check: systemctl --user status $UNIT"
      fi
      # Setup puts the unit where the user manager looks, which can be a custom XDG_CONFIG_HOME.
      rm -f "$HOME/.config/systemd/user/$UNIT" "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/$UNIT" "$HOME/.config/wardyn/runnerd.env" "$config"
      systemctl --user daemon-reload 2>/dev/null || true
      ;;
    Darwin)
      data_dir="$HOME/Library/Application Support/wardyn"
      identity_dir="$data_dir/runner"
      # A service that is already gone is success; one that cannot be confirmed stopped leaves every file in place.
      domain="gui/$(id -u)"
      launchctl print "$domain" >/dev/null 2>&1 \
        || die "cannot reach this login's launchd domain ($domain), so the agent may still be running. Nothing was removed. Run this from the Mac's own login session, then run --uninstall again."
      launchctl bootout "$domain/$LABEL" >/dev/null 2>&1 || true
      # bootout returns before launchd has finished unloading the job. Wait, bounded (about 10 seconds; the tick count
      # is WARDYN_UNINSTALL_UNLOAD_TICKS, for tests), until launchd answers 113, its answer for a label it does not
      # know. Any other failure of the lookup is not evidence that the job is gone.
      ticks="${WARDYN_UNINSTALL_UNLOAD_TICKS:-50}"
      while :; do
        rc=0
        launchctl print "$domain/$LABEL" >/dev/null 2>&1 || rc=$?
        [ "$rc" = 113 ] && break
        [ "$rc" = 0 ] || die "launchctl could not say whether $LABEL is still loaded (print exited $rc), so it may still be running. Nothing was removed. Check: launchctl print $domain/$LABEL"
        ticks=$((ticks - 1))
        [ "$ticks" -gt 0 ] || die "$LABEL is still loaded after launchctl bootout, so it may still be running. Nothing was removed. Check: launchctl print $domain/$LABEL"
        sleep 0.2
      done
      rm -f "$HOME/Library/LaunchAgents/$LABEL.plist" "$data_dir/runnerd.json"
      ;;
    *) die "unsupported OS: $(uname -s)" ;;
  esac
  # setup on a Mac also installs the macOS VM helper and its guest agent beside the runner.
  for f in wardyn-runnerd wardyn-macvm wardyn-guest-agent; do
    rm -f "$data_dir/bin/$f" "$data_dir/bin/$f.previous"
  done
  echo "Removed the wardyn-runnerd service and binary (on a Mac, the VM helper and guest agent too). The wardyn CLI and its cli.json are yours and stay."
  echo "Kept the runner identity in: $identity_dir"
  echo "To revoke this runner, run: wardyn runner revoke <runner id or name>   (or use My runner in the Wardyn console)."
}

if [ "$UNINSTALL" = 1 ]; then
  uninstall
  exit 0
fi

if [ -z "$BINARY" ]; then
  if [ -f "$SELF_DIR/wardyn-runnerd" ]; then
    BINARY="$SELF_DIR/wardyn-runnerd"
  else
    BINARY="$(command -v wardyn-runnerd || true)"
  fi
fi
if ! { [ -n "$BINARY" ] && [ -e "$BINARY" ]; }; then die "wardyn-runnerd not found: pass --binary <path>"; fi
if ! { [ -f "$BINARY" ] && [ -r "$BINARY" ]; }; then die "wardyn-runnerd is not a readable file: $BINARY"; fi
# A release download has no exec bit. Run a private 0755 copy; setup installs it to its permanent path.
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
cp "$BINARY" "$STAGE/wardyn-runnerd"
chmod 0755 "$STAGE/wardyn-runnerd"

"$STAGE/wardyn-runnerd" setup ${PASS[@]+"${PASS[@]}"}
