#!/usr/bin/env bash
# Stage a native agent CLI binary on the HOST for an offline / strict-allowlist
# corp image build (CLAUDE_INSTALL=native / CODEX_INSTALL=native). Downloads and
# CHECKSUM-VERIFIES the binary, then writes it where the agent Dockerfile's
# gitignored glob picks it up. Run this on a host that CAN reach the download
# source, then `docker build --build-arg CLAUDE_INSTALL=native ...` (or
# `make agent-images-core CLAUDE_INSTALL=native`) in the sealed environment.
#
# Usage:
#   scripts/stage-agent-binary.sh claude-code [version]   # version: stable|latest|X.Y.Z (default stable)
#   scripts/stage-agent-binary.sh codex-cli               # needs WARDYN_CODEX_BIN_URL (+ WARDYN_CODEX_BIN_SHA256)
#
# claude-code uses the official, documented download surface
# (https://downloads.claude.ai/claude-code-releases) — the only host it contacts,
# with per-release SHA256 checksums from the release manifest. codex has no
# Wardyn-verified public contract, so you supply the URL + checksum yourself.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

AGENT="${1:-}"
[ -n "${AGENT}" ] || die "usage: scripts/stage-agent-binary.sh <claude-code|codex-cli> [version]"

# Resolve host arch to the platform tokens each source uses.
case "$(uname -m)" in
  x86_64|amd64)  cc_plat="linux-x64" ;;
  aarch64|arm64) cc_plat="linux-arm64" ;;
  *) die "unsupported host arch $(uname -m) — stage on x86_64 or arm64, matching your image's target arch" ;;
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
    sum="$(printf '%s' "${man}" | grep -A3 "\"${cc_plat}\"" | grep -oiE '[a-f0-9]{64}' | head -1)"
    [ -n "${sum}" ] || die "no sha256 for ${cc_plat} in the ${ver} manifest"
    log "Downloading the native claude binary (~250MB)"
    curl -fsSL "${base}/${ver}/${cc_plat}/claude" -o "${out}.tmp"
    echo "${sum}  ${out}.tmp" | sha256sum -c - || { rm -f "${out}.tmp"; die "checksum mismatch — refusing to stage"; }
    chmod 0755 "${out}.tmp"; mv "${out}.tmp" "${out}"
    log "Staged ${out} (claude ${ver}, ${cc_plat}, sha256 ${sum}). Build with: make agent-images-core CLAUDE_INSTALL=native"
    ;;
  codex-cli)
    url="${WARDYN_CODEX_BIN_URL:-}"
    [ -n "${url}" ] || die "codex has no Wardyn-verified public contract — set WARDYN_CODEX_BIN_URL (and WARDYN_CODEX_BIN_SHA256 to verify) to a native codex binary you trust"
    out="deploy/images/codex-cli/codex-bin"
    log "Downloading codex from ${url}"
    curl -fsSL "${url}" -o "${out}.tmp"
    if [ -n "${WARDYN_CODEX_BIN_SHA256:-}" ]; then
      echo "${WARDYN_CODEX_BIN_SHA256}  ${out}.tmp" | sha256sum -c - || { rm -f "${out}.tmp"; die "checksum mismatch — refusing to stage"; }
    else
      log "WARN: no WARDYN_CODEX_BIN_SHA256 set — staging WITHOUT checksum verification (set it to verify)"
    fi
    chmod 0755 "${out}.tmp"; mv "${out}.tmp" "${out}"
    log "Staged ${out}. Build with: make agent-images-core CODEX_INSTALL=native"
    ;;
  *) die "unknown agent '${AGENT}' — supported: claude-code, codex-cli" ;;
esac
