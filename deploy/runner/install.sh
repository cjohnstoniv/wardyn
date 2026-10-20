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
  --uninstall       stop and remove the service, the installed wardyn-runnerd and its .previous rollback copy; keeps the runner identity, and the wardyn CLI and its cli.json (they are yours)

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
      systemctl --user disable --now "$UNIT" 2>/dev/null || true
      # Setup puts the unit where the user manager looks, which can be a custom XDG_CONFIG_HOME.
      rm -f "$HOME/.config/systemd/user/$UNIT" "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/$UNIT" "$HOME/.config/wardyn/runnerd.env" "$config"
      systemctl --user daemon-reload 2>/dev/null || true
      ;;
    Darwin)
      data_dir="$HOME/Library/Application Support/wardyn"
      identity_dir="$data_dir/runner"
      launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
      rm -f "$HOME/Library/LaunchAgents/$LABEL.plist" "$data_dir/runnerd.json"
      ;;
    *) die "unsupported OS: $(uname -s)" ;;
  esac
  rm -f "$data_dir/bin/wardyn-runnerd" "$data_dir/bin/wardyn-runnerd.previous"
  echo "Removed the wardyn-runnerd service and binary. The wardyn CLI and its cli.json are yours and stay."
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
