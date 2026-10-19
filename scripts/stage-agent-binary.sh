#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Stage a native agent CLI binary on the HOST for an offline / strict-allowlist
# corp image build (CLAUDE_INSTALL=native / CODEX_INSTALL=native). Downloads and
# CHECKSUM-VERIFIES the binary, then writes it where the agent Dockerfile's
# gitignored glob picks it up. Run this on a host that CAN reach the download
# source, then `docker build --build-arg CLAUDE_INSTALL=native ...` (or
# `make agent-images-core CLAUDE_INSTALL=native`) in the sealed environment.
#
# Usage:
#   scripts/stage-agent-binary.sh claude-code [version] [--arch amd64|arm64]   # version: stable|latest|X.Y.Z (default stable)
#   scripts/stage-agent-binary.sh codex-cli [--arch amd64|arm64]               # needs WARDYN_CODEX_BIN_URL (+ WARDYN_CODEX_BIN_SHA256)
#
# --arch (or WARDYN_AGENT_ARCH; x86_64 / aarch64 spellings work too) stages the binary for a target
# architecture other than this host's, e.g. the arm64 blob from an amd64 build host. The flag wins over
# the env var, which wins over `uname -m`. claude-code picks that platform's manifest entry and checks
# its sha256 as before; codex-cli only logs it, because its URL is yours and must already match.
#
# claude-code uses the official, documented download surface
# (https://downloads.claude.ai/claude-code-releases) — the only host it contacts,
# with per-release SHA256 checksums from the release manifest. codex has no
# Wardyn-verified public contract, so you supply the URL + checksum yourself.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
. "${REPO_ROOT}/scripts/lib/common.sh"

arch_flag=""; pos=()
while [ $# -gt 0 ]; do
  case "$1" in
    --arch) [ $# -ge 2 ] || die "--arch needs a value (amd64 or arm64)"; arch_flag="$2"; shift 2 ;;
    --arch=*) arch_flag="${1#--arch=}"; shift ;;
    *) pos+=("$1"); shift ;;
  esac
done
set -- ${pos[@]+"${pos[@]}"}

AGENT="${1:-}"
[ -n "${AGENT}" ] || die "usage: scripts/stage-agent-binary.sh <claude-code|codex-cli> [version] [--arch amd64|arm64]"

# Resolve the target arch (flag, then WARDYN_AGENT_ARCH, then this host) to the platform tokens each source uses.
want="${arch_flag:-${WARDYN_AGENT_ARCH:-$(uname -m)}}"
case "${want}" in
  x86_64|amd64)  arch="amd64"; cc_plat="linux-x64" ;;
  aarch64|arm64) arch="arm64"; cc_plat="linux-arm64" ;;
  *) die "unsupported arch ${want} — use amd64 (x86_64) or arm64 (aarch64) via --arch or WARDYN_AGENT_ARCH, matching your image's target arch" ;;
esac

case "${AGENT}" in
  claude-code)
    ver="${2:-stable}"
    base="https://downloads.claude.ai/claude-code-releases"
    case "${ver}" in stable|latest) log "Resolving ${ver} channel"; ver="$(curl -fsSL "${base}/${ver}")" ;; esac
    [ -n "${ver}" ] || die "could not resolve claude-code version"
    out="deploy/images/claude-code/claude-bin"
    log "Fetching claude ${ver} (${cc_plat}) manifest for the checksum"
    man="$(curl -fsSL "${base}/${ver}/manifest.json")"
    sum="$(printf '%s' "${man}" | grep -A3 "\"${cc_plat}\"" | grep -oiE '[a-f0-9]{64}' | head -1)" || true  # pipefail: a grep miss must reach the die below, not abort here
    [ -n "${sum}" ] || die "no sha256 for ${cc_plat} in the ${ver} manifest"
    log "Downloading the native claude binary (~250MB)"
    curl -fsSL "${base}/${ver}/${cc_plat}/claude" -o "${out}.tmp"
    echo "${sum}  ${out}.tmp" | sha256sum -c - || { rm -f "${out}.tmp"; die "checksum mismatch — refusing to stage"; }
    chmod 0755 "${out}.tmp"; mv "${out}.tmp" "${out}"
    log "Staged ${out} (claude ${ver}, ${arch}/${cc_plat}, sha256 ${sum}). Build with: make agent-images-core CLAUDE_INSTALL=native"
    ;;
  codex-cli)
    url="${WARDYN_CODEX_BIN_URL:-}"
    [ -n "${url}" ] || die "codex has no Wardyn-verified public contract — set WARDYN_CODEX_BIN_URL (and WARDYN_CODEX_BIN_SHA256 to verify) to a native codex binary you trust"
    out="deploy/images/codex-cli/codex-bin"
    log "Downloading codex from ${url} (target ${arch}; the binary at the URL must already match it)"
    curl -fsSL "${url}" -o "${out}.tmp"
    if [ -n "${WARDYN_CODEX_BIN_SHA256:-}" ]; then
      echo "${WARDYN_CODEX_BIN_SHA256}  ${out}.tmp" | sha256sum -c - || { rm -f "${out}.tmp"; die "checksum mismatch — refusing to stage"; }
    else
      log "WARN: no WARDYN_CODEX_BIN_SHA256 set — staging WITHOUT checksum verification (set it to verify)"
    fi
    chmod 0755 "${out}.tmp"; mv "${out}.tmp" "${out}"
    log "Staged ${out} (${arch}). Build with: make agent-images-core CODEX_INSTALL=native"
    ;;
  *) die "unknown agent '${AGENT}' — supported: claude-code, codex-cli" ;;
esac
