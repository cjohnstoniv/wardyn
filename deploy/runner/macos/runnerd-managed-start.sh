#!/bin/sh
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# runnerd-managed-start — what the pkg's LaunchAgent (com.wardyn.runnerd.managed) runs at login.
# It starts `wardyn-runnerd serve` only when this person is registered and has not run `setup`
# (which installs its own agent for the same runner). Otherwise it says why and exits 0, which the
# agent's KeepAlive does not restart. After `wardyn-runnerd register`, start it with:
#   launchctl kickstart gui/$(id -u)/com.wardyn.runnerd.managed
# The organisation and every other setting come from the com.wardyn.runnerd managed preferences.
# WARDYN_RUNNERD_BIN and WARDYN_RUNNERD_HOME are for tests.
set -eu
bin="${WARDYN_RUNNERD_BIN:-/usr/local/wardyn/bin/wardyn-runnerd}"
home="${WARDYN_RUNNERD_HOME:-$HOME}"
log="$home/Library/Logs/wardyn/runnerd.log"
mkdir -p "$(dirname "$log")"
exec >>"$log" 2>&1
if [ -f "$home/Library/LaunchAgents/com.wardyn.runnerd.plist" ]; then
  echo "wardyn-runnerd: a setup-installed agent runs this runner; the managed agent stays idle"
  exit 0
fi
if [ ! -f "$home/Library/Application Support/wardyn/runner/runner.json" ]; then
  echo "wardyn-runnerd: this computer is not registered; run 'wardyn-runnerd register --org <url> --token-file <file>', then launchctl kickstart gui/$(id -u)/com.wardyn.runnerd.managed"
  exit 0
fi
exec "$bin" serve
