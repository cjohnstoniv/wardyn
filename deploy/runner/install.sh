#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# install.sh — install wardyn-runnerd for ONE person, as that person: place the
# binary, register it with a single-use wdr_ token when one is given, and keep
# `wardyn-runnerd serve` running from login (a systemd user unit on Linux, a
# LaunchAgent on macOS). Refuses to run as root: the runner identity lives in
# the person's own configuration directory, where `wardyn runner claim` looks.
#
# Usage: install.sh --org <url> [--binary <path>] [--token-file <file>]
#                   [--proxy-image <ref>] [--internal-network <name>]
#        install.sh --uninstall
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LABEL=com.wardyn.runnerd
UNIT=wardyn-runnerd.service
# Values that reach a unit file, a plist or an env file are limited to this set,
# so nothing can inject a line, a quote or a sed metacharacter.
SAFE='^[A-Za-z0-9._:/@%+=-]+$'

die() { echo "install.sh: $*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Usage: install.sh --org <url> [--binary <path>] [--token-file <file>]
                  [--proxy-image <ref>] [--internal-network <name>]
       install.sh --uninstall

  --org <url>               organisation public URL (required to install)
  --binary <path>           wardyn-runnerd to install (default: beside this script, else on PATH)
  --token-file <file>       file holding one single-use wdr_ registration token
  --proxy-image <ref>       proxy image (default: the release image matching the binary)
  --internal-network <name> Docker bridge for proxy sidecars (default: wardyn-internal)
  --uninstall               stop and remove the service and the installed binary; keeps the runner identity
USAGE
}

ORG="" BINARY="" TOKEN_FILE="" PROXY_IMAGE="" NETWORK=wardyn-internal UNINSTALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --org) ORG="${2:?--org needs a value}"; shift 2 ;;
    --binary) BINARY="${2:?--binary needs a value}"; shift 2 ;;
    --token-file) TOKEN_FILE="${2:?--token-file needs a value}"; shift 2 ;;
    --proxy-image) PROXY_IMAGE="${2:?--proxy-image needs a value}"; shift 2 ;;
    --internal-network) NETWORK="${2:?--internal-network needs a value}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" != 0 ] || die "run as the person who owns this runner, not as root"

OS="$(uname -s)"
case "$OS" in
  Linux)
    DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/wardyn"
    CONFIG_ROOT="${XDG_CONFIG_HOME:-$HOME/.config}"
    STATE_DIR="$CONFIG_ROOT/wardyn/runner"
    ENV_FILE="$HOME/.config/wardyn/runnerd.env"
    UNIT_FILE="$CONFIG_ROOT/systemd/user/$UNIT"
    ;;
  Darwin)
    DATA_DIR="$HOME/Library/Application Support/wardyn"
    STATE_DIR="$HOME/Library/Application Support/wardyn/runner"
    PLIST_FILE="$HOME/Library/LaunchAgents/$LABEL.plist"
    LOG_FILE="$HOME/Library/Logs/wardyn/runnerd.log"
    ;;
  *) die "unsupported OS: $OS" ;;
esac
INSTALLED="$DATA_DIR/bin/wardyn-runnerd"

# sed_escape / xml_escape make a value safe as a sed replacement / plist string.
sed_escape() { printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'; }
xml_escape() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }
# render FILE SENTINEL=VALUE... — print FILE with each sentinel replaced.
render() {
  local file="$1" args=() pair
  shift
  for pair in "$@"; do args+=(-e "s|${pair%%=*}|$(sed_escape "${pair#*=}")|g"); done
  sed "${args[@]}" "$file"
}

uninstall() {
  case "$OS" in
    Linux)
      systemctl --user disable --now "$UNIT" 2>/dev/null || true
      rm -f "$UNIT_FILE" "$ENV_FILE"
      systemctl --user daemon-reload 2>/dev/null || true
      ;;
    Darwin)
      launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
      rm -f "$PLIST_FILE"
      ;;
  esac
  rm -f "$INSTALLED"
  echo "Removed the wardyn-runnerd service and binary."
  echo "Kept the runner identity in: $STATE_DIR"
  echo "To revoke this runner, use My runner in the Wardyn console."
}

if [ "$UNINSTALL" = 1 ]; then
  uninstall
  exit 0
fi

[ -n "$ORG" ] || { usage >&2; die "--org is required"; }

if [ -z "$BINARY" ]; then
  if [ -f "$SELF_DIR/wardyn-runnerd" ]; then
    BINARY="$SELF_DIR/wardyn-runnerd"
  else
    BINARY="$(command -v wardyn-runnerd || true)"
  fi
fi
if ! { [ -n "$BINARY" ] && [ -e "$BINARY" ]; }; then die "wardyn-runnerd not found: pass --binary <path>"; fi
if ! { [ -f "$BINARY" ] && [ -r "$BINARY" ]; }; then die "wardyn-runnerd is not a readable file: $BINARY"; fi
# A release download has no exec bit. Work from a private 0755 copy, so the
# version check can run it and the install copies a runnable file.
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
cp "$BINARY" "$STAGE/wardyn-runnerd"
chmod 0755 "$STAGE/wardyn-runnerd"
BINARY="$STAGE/wardyn-runnerd"

# Validate everything that can refuse before the first side effect.
HAVE_IDENTITY=0
[ ! -e "$STATE_DIR/runner.json" ] || HAVE_IDENTITY=1
if [ "$HAVE_IDENTITY" = 0 ] && [ -z "$TOKEN_FILE" ]; then
  die "no runner identity yet: register first by passing --token-file <file> holding the wdr_ token an administrator minted for you"
fi
[ -z "$TOKEN_FILE" ] || [ -f "$TOKEN_FILE" ] || die "token file not found: $TOKEN_FILE"

if [ -z "$PROXY_IMAGE" ]; then
  # cobra prints "wardyn-runnerd version <v>"; a dev build's <v> carries +<commit>; a release candidate is X.Y.Z-rc.N.
  version="$("$BINARY" --version | awk '{print $NF}')"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]] \
    || die "this wardyn-runnerd is a development build ($version): pass --proxy-image <ref>"
  PROXY_IMAGE="ghcr.io/cjohnstoniv/wardyn-proxy:$version"
fi
for value in "$ORG" "$PROXY_IMAGE" "$NETWORK" "${DOCKER_HOST:-x}"; do
  [[ "$value" =~ $SAFE ]] || die "unsupported characters in a setting: $value"
done

docker info >/dev/null 2>&1 \
  || die "docker is not usable as this user: on Linux add yourself to the docker group, on macOS start Docker Desktop"

if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
  docker network create --driver bridge "$NETWORK" >/dev/null
  echo "Created Docker network $NETWORK."
fi

# Stop a running copy first, then replace the binary atomically.
case "$OS" in
  Linux) systemctl --user stop "$UNIT" 2>/dev/null || true ;;
esac
mkdir -p "$(dirname "$INSTALLED")"
cp "$BINARY" "$INSTALLED.new"
chmod 0755 "$INSTALLED.new"
mv -f "$INSTALLED.new" "$INSTALLED"
echo "Installed $INSTALLED"

if [ "$HAVE_IDENTITY" = 0 ]; then
  "$INSTALLED" register --org "$ORG" --token-file "$TOKEN_FILE"
fi

case "$OS" in
  Linux)
    mkdir -p "$(dirname "$ENV_FILE")" "$(dirname "$UNIT_FILE")"
    umask 077
    {
      echo "WARDYN_RUNNER_ORG_URL=$ORG"
      echo "WARDYN_PROXY_IMAGE=$PROXY_IMAGE"
      echo "WARDYN_INTERNAL_NETWORK=$NETWORK"
      [ -z "${DOCKER_HOST:-}" ] || echo "DOCKER_HOST=$DOCKER_HOST"
    } >"$ENV_FILE"
    chmod 0600 "$ENV_FILE"
    render "$SELF_DIR/$UNIT" "__WARDYN_RUNNERD__=$INSTALLED" >"$UNIT_FILE"
    systemctl --user daemon-reload
    systemctl --user enable --now "$UNIT"
    echo "Started the wardyn-runnerd user service. Logs: journalctl --user -u wardyn-runnerd"
    echo "To keep it running when you are logged out: loginctl enable-linger $USER"
    ;;
  Darwin)
    mkdir -p "$(dirname "$PLIST_FILE")" "$(dirname "$LOG_FILE")"
    docker_host_xml="$(xml_escape "${DOCKER_HOST:-}")"
    render "$SELF_DIR/$LABEL.plist" \
      "__WARDYN_RUNNERD__=$(xml_escape "$INSTALLED")" \
      "__WARDYN_ORG_URL__=$(xml_escape "$ORG")" \
      "__WARDYN_PROXY_IMAGE__=$(xml_escape "$PROXY_IMAGE")" \
      "__WARDYN_INTERNAL_NETWORK__=$(xml_escape "$NETWORK")" \
      "__DOCKER_HOST__=$docker_host_xml" \
      "__WARDYN_LOG__=$(xml_escape "$LOG_FILE")" >"$PLIST_FILE"
    # No DOCKER_HOST in the caller's environment: omit the key rather than set it empty.
    if [ -z "${DOCKER_HOST:-}" ]; then
      sed -e '/<key>DOCKER_HOST<\/key>/{N;d;}' "$PLIST_FILE" >"$PLIST_FILE.new"
      mv -f "$PLIST_FILE.new" "$PLIST_FILE"
    fi
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST_FILE"
    echo "Started the wardyn-runnerd LaunchAgent. Log: $LOG_FILE"
    ;;
esac
