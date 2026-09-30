#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-managed-settings-drift.sh — does the Claude Code CLI the agent image ships still
# know every managed-settings key Wardyn writes? (#1279, from #705)
#
# internal/agentpolicy writes frozen managed-settings documents that Claude Code reads from
# /etc/claude-code/managed-settings.json. Those keys are a contract with one vendor's parser:
# a key the CLI stops recognising is ignored silently, and the guard it expressed (no bypass
# mode, no repo hooks) is gone without a single failing test. This probe installs the EXACT
# CLI version deploy/images/claude-code/Dockerfile pins (never a channel) and fails, naming
# each key, when that CLI's bundled code no longer contains it.
#
# DRIFT means: a key in internal/agentpolicy/testdata/*-managed-settings.json, or the
# directory or file name of ClaudeCodeManagedSettingsPath, is absent from the installed
# CLI. The presence check runs on both shipped platforms (amd64, and arm64 from a second
# `npm install --cpu=arm64 --os=linux`; the arm64 binary is only read, never run, and its
# platform package must carry the same version as the amd64 one). A behaviour check then
# starts the amd64 CLI in the agent image's base container with each frozen document
# installed as /etc/claude-code/managed-settings.json, no network and no model, and
# asserts the permissionMode its init event reports is the document's defaultMode, even
# under --permission-mode bypassPermissions when the document disables bypass. Neither
# step needs a credential. Bump the Dockerfile pin and this probe re-runs against the
# new version.
#
# The install runs npm with --ignore-scripts into a temp dir: only the package's files are
# read, and the amd64 CLI is run for `--version` and the behaviour check. Run it where
# installing is harmless (a CI runner, or a throwaway container) and docker is available.
#
# Env (for tests): CLAUDE_CODE_DIR and CLAUDE_CODE_DIR_ARM64 use already-installed prefixes
# instead of installing; AGENTPOLICY_DIR points at a different internal/agentpolicy tree.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="$ROOT/deploy/images/claude-code/Dockerfile"
POLICY_DIR="${AGENTPOLICY_DIR:-$ROOT/internal/agentpolicy}"

die() { echo "FAIL: $*" >&2; exit 1; }

# 1. The pin, read from the image's Dockerfile: exactly one, exact semver.
pins="$(grep -E '^ARG CLAUDE_CODE_VERSION=' "$DOCKERFILE" || true)"
[ "$(grep -c . <<<"$pins")" = 1 ] || die "$DOCKERFILE must carry exactly one 'ARG CLAUDE_CODE_VERSION=' line, found: ${pins:-none}"
pin="${pins#ARG CLAUDE_CODE_VERSION=}"
[[ "$pin" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "CLAUDE_CODE_VERSION='$pin' is not an exact x.y.z version — the probe never tests a channel"

# 2. What Wardyn writes: every JSON key in the frozen documents, and the file path.
keys="$(cat "$POLICY_DIR"/testdata/*-managed-settings.json | grep -oE '"[^"]+"[[:space:]]*:' | sed -E 's/^"([^"]*)".*/\1/' | sort -u)"
[ -n "$keys" ] || die "no managed-settings keys found under $POLICY_DIR/testdata — the probe would pass vacuously"
path="$(sed -nE 's/^const ClaudeCodeManagedSettingsPath = "([^"]+)"$/\1/p' "$POLICY_DIR/agentpolicy.go")"
[ -n "$path" ] || die "ClaudeCodeManagedSettingsPath not found in $POLICY_DIR/agentpolicy.go"

# 3. The CLI at that pin, amd64 and arm64.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
install_cli() { # <prefix> [npm --cpu value]
    npm install --prefix "$1" ${2:+--cpu="$2" --os=linux} --ignore-scripts --no-audit --no-fund "@anthropic-ai/claude-code@$pin" >/dev/null
}
prefix="${CLAUDE_CODE_DIR:-$tmp/amd64}"
arm_prefix="${CLAUDE_CODE_DIR_ARM64:-$tmp/arm64}"
[ -n "${CLAUDE_CODE_DIR:-}" ] || install_cli "$prefix"
[ -n "${CLAUDE_CODE_DIR_ARM64:-}" ] || install_cli "$arm_prefix" arm64
# The CLI is a native binary in a per-platform optional package (older releases: cli.js).
# .d.ts/.md files are excluded on purpose: documenting a key is not honouring it.
find_bundles() { # <prefix> -> $bundles, $exe
    mapfile -t bundles < <(find "$1/node_modules/@anthropic-ai" -type f \( -name claude -o -name claude.exe -o -name cli.js \) 2>/dev/null | sort)
    [ "${#bundles[@]}" -gt 0 ] || die "no Claude Code executable or cli.js under $1/node_modules/@anthropic-ai"
    exe=""
    for b in "${bundles[@]}"; do [ -x "$b" ] && [ "$(wc -c <"$b")" -gt 1000000 ] && exe="$b" && break; done
    [ -n "$exe" ] || exe="${bundles[0]}"
}
pkg_version() { sed -nE 's/^[[:space:]]*"version":[[:space:]]*"([^"]+)".*/\1/p' "$(dirname "$1")/package.json" | head -1; }

find_bundles "$prefix"
amd_bundles=("${bundles[@]}"); amd_exe="$exe"
version="$(CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 "$exe" --version 2>&1 | head -1 || true)"
case "$version" in
    "$pin "*|"$pin") ;;
    *) die "installed CLI reports '$version', not the pinned $pin" ;;
esac
amd_version="$(pkg_version "$amd_exe")"

# arm64 cannot run on this runner; its package.json must still be the same release.
find_bundles "$arm_prefix"
arm_bundles=("${bundles[@]}"); arm_exe="$exe"
arm_version="$(pkg_version "$arm_exe")"
[ -n "$arm_version" ] && [ "$arm_version" = "$amd_version" ] || die "arm64 package version '${arm_version:-none}' (${arm_exe#"$arm_prefix"/}) is not the amd64 one '${amd_version:-none}' — the two platforms must ship the same Claude Code release"
echo "Claude Code $pin amd64 ($amd_exe, ${#amd_bundles[@]} bundle(s)) and arm64 ($arm_exe, ${#arm_bundles[@]} bundle(s)); probing $(wc -w <<<"$keys") keys + $path"

# 4. Presence. A key or path part counts as present when ANY bundle of that platform contains
# it. The path is checked as directory + file name: the CLI assembles it from two literals.
missing=0
check_presence() { # <arch> <bundle>...
    local arch="$1" k b found; shift
    for k in $keys; do
        found=""
        for b in "$@"; do grep -aqF -- "$k" "$b" && { found=1; break; }; done
        [ -n "$found" ] || { echo "DRIFT: managed-settings key '$k' is not in Claude Code $pin ($arch) — Wardyn writes it (internal/agentpolicy) and the CLI no longer knows it" >&2; missing=1; }
    done
    for k in "$(dirname "$path")" "$(basename "$path")"; do
        found=""
        for b in "$@"; do grep -aqF -- "$k" "$b" && { found=1; break; }; done
        [ -n "$found" ] || { echo "DRIFT: managed-settings path component '$k' (of $path) is not in Claude Code $pin ($arch)" >&2; missing=1; }
    done
}
check_presence amd64 "${amd_bundles[@]}"
check_presence arm64 "${arm_bundles[@]}"
[ "$missing" = 0 ] || die "managed-settings drift against Claude Code $pin"
echo "ok: every managed-settings key and the path are present in Claude Code $pin (amd64 and arm64)"

# 5. Behaviour. Presence says the parser knows a word; this says the CLI acts on the file. The
# init event is printed before any model call, so no credential or network is needed.
image="$(sed -nE 's/^FROM (node:[^ ]+)$/\1/p' "$DOCKERFILE")"
[ "$(grep -c . <<<"$image")" = 1 ] || die "$DOCKERFILE must carry exactly one 'FROM node:' line for the behaviour check, found: ${image:-none}"
command -v docker >/dev/null || die "docker is required for the behaviour check"
mode_of() { # <settings dir> [claude args...] -> the init event's permissionMode
    local dir="$1"; shift
    # As the image's non-root user (the agent image never runs as root; the CLI refuses bypass as
    # root, which would make the bypass assertion pass for the wrong reason). stderr is kept for the
    # failure message.
    docker run --rm --network none --user node --cap-drop ALL --security-opt no-new-privileges \
        --read-only --tmpfs /tmp -v "$amd_exe:/usr/local/bin/claude:ro" -v "$dir:/etc/claude-code:ro" \
        -e HOME=/tmp -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 "$image" \
        claude --print hi --output-format stream-json --verbose "$@" 2>"$tmp/stderr" \
        | sed -nE 's/.*"subtype":"init".*"permissionMode":"([^"]*)".*/\1/p' | head -1 || true
}
behaved=0
for doc in "$POLICY_DIR"/testdata/*-managed-settings.json; do
    want="$(sed -nE 's/^[[:space:]]*"defaultMode":[[:space:]]*"([^"]*)".*/\1/p' "$doc" | head -1)"
    [ -n "$want" ] || die "$doc has no permissions.defaultMode — the behaviour check would pass vacuously"
    dir="$tmp/settings-$(basename "$doc" -managed-settings.json)"
    mkdir -p "$dir"; cp "$doc" "$dir/$(basename "$path")"
    got="$(mode_of "$dir")"
    [ "$got" = "$want" ] || die "$(basename "$doc"): Claude Code $pin reports permissionMode '${got:-none}', the managed defaultMode is '$want' — the CLI does not act on the file it reads; stderr: $(head -c 400 "$tmp/stderr")"
    if grep -q '"disableBypassPermissionsMode"' "$doc"; then
        got="$(mode_of "$dir" --permission-mode bypassPermissions)"
        [ "$got" = "$want" ] || die "$(basename "$doc"): --permission-mode bypassPermissions yields '${got:-none}' although the file disables bypass (want '$want') — the bypass lock is not honoured; stderr: $(head -c 400 "$tmp/stderr")"
    fi
    behaved=$((behaved + 1))
done
[ "$behaved" -gt 0 ] || die "no managed-settings documents under $POLICY_DIR/testdata for the behaviour check"
echo "ok: Claude Code $pin acts on $behaved managed-settings document(s) (reported mode matches; bypass stays locked where disabled)"
