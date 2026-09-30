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
# CLI. It is a presence check on the shipped binary; it does not launch a model and needs no
# credential. Bump the Dockerfile pin and this probe re-runs against the new version.
#
# The install runs npm with --ignore-scripts into a temp dir: only the package's files are
# read, and `claude --version` is run once. Run it where installing is harmless (a CI
# runner, or a throwaway container).
#
# Env (for tests): CLAUDE_CODE_DIR uses an already-installed prefix instead of installing;
# AGENTPOLICY_DIR points at a different internal/agentpolicy tree.
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

# 3. The CLI at that pin.
if [ -n "${CLAUDE_CODE_DIR:-}" ]; then
    prefix="$CLAUDE_CODE_DIR"
else
    prefix="$(mktemp -d)"
    trap 'rm -rf "$prefix"' EXIT
    npm install --prefix "$prefix" --ignore-scripts --no-audit --no-fund "@anthropic-ai/claude-code@$pin" >/dev/null
fi
# The CLI is a native binary in a per-platform optional package (older releases: cli.js).
# .d.ts/.md files are excluded on purpose: documenting a key is not honouring it.
mapfile -t bundles < <(find "$prefix/node_modules/@anthropic-ai" -type f \( -name claude -o -name claude.exe -o -name cli.js \) 2>/dev/null | sort)
[ "${#bundles[@]}" -gt 0 ] || die "no Claude Code executable or cli.js under $prefix/node_modules/@anthropic-ai"
exe=""
for b in "${bundles[@]}"; do [ -x "$b" ] && [ "$(wc -c <"$b")" -gt 1000000 ] && exe="$b" && break; done
[ -n "$exe" ] || exe="${bundles[0]}"
version="$(CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 "$exe" --version 2>&1 | head -1 || true)"
case "$version" in
    "$pin "*|"$pin") ;;
    *) die "installed CLI reports '$version', not the pinned $pin" ;;
esac
echo "Claude Code $pin ($exe), ${#bundles[@]} bundle(s); probing $(wc -w <<<"$keys") keys + $path"

# 4. Presence. A key or path part counts as present when ANY bundle contains it. The path is
# checked as directory + file name: the CLI assembles it from two literals.
present() { local b; for b in "${bundles[@]}"; do grep -aqF -- "$1" "$b" && return 0; done; return 1; }
missing=0
for k in $keys; do
    present "$k" || { echo "DRIFT: managed-settings key '$k' is not in Claude Code $pin — Wardyn writes it (internal/agentpolicy) and the CLI no longer knows it" >&2; missing=1; }
done
for part in "$(dirname "$path")" "$(basename "$path")"; do
    present "$part" || { echo "DRIFT: managed-settings path component '$part' (of $path) is not in Claude Code $pin" >&2; missing=1; }
done
[ "$missing" = 0 ] || die "managed-settings drift against Claude Code $pin"
echo "ok: every managed-settings key and the path are present in Claude Code $pin"
