#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-check-managed-settings-drift.sh — the drift probe fails on the drift it exists for,
# and passes when there is none. A probe that cannot go red is worse than none.
#
# Daemon-free, network-free: CLAUDE_CODE_DIR points the probe at a fake installed CLI (a
# shell script whose text carries the strings a real binary would), inside a throwaway tree.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p "$TMP/scripts" "$TMP/deploy/images/claude-code" "$TMP/internal"
cp "$ROOT/scripts/check-managed-settings-drift.sh" "$TMP/scripts/"
cp "$ROOT/deploy/images/claude-code/Dockerfile" "$TMP/deploy/images/claude-code/"
cp -r "$ROOT/internal/agentpolicy" "$TMP/internal/"
pin="$(sed -nE 's/^ARG CLAUDE_CODE_VERSION=(.*)$/\1/p' "$TMP/deploy/images/claude-code/Dockerfile")"
[ -n "$pin" ] || fail "no CLAUDE_CODE_VERSION pin in the Dockerfile"

# fake_cli <version> <string...>: an installed prefix whose "claude" reports <version> and contains <string...>.
fake_cli() {
    local dir="$TMP/cli" v="$1"; shift
    rm -rf "$dir"; mkdir -p "$dir/node_modules/@anthropic-ai/claude-code-linux-x64"
    { echo '#!/bin/sh'; echo "echo '$v (Claude Code)'"; echo 'exit 0'; printf '# %s\n' "$@"; } > "$dir/node_modules/@anthropic-ai/claude-code-linux-x64/claude"
    chmod +x "$dir/node_modules/@anthropic-ai/claude-code-linux-x64/claude"
}
probe() { CLAUDE_CODE_DIR="$TMP/cli" "$TMP/scripts/check-managed-settings-drift.sh" 2>&1; }
all=(permissions defaultMode disableBypassPermissionsMode disableAutoMode allowManagedHooksOnly allowManagedPermissionRulesOnly /etc/claude-code managed-settings.json)

# 1. Nothing drifted => pass.
fake_cli "$pin" "${all[@]}"
probe >/dev/null || fail "a CLI carrying every key and the path must PASS"
echo "ok  no drift passes"

# 2. One key gone => fail, and the message names that key (not a bare non-zero).
fake_cli "$pin" permissions defaultMode disableBypassPermissionsMode disableAutoMode allowManagedHooksOnly /etc/claude-code managed-settings.json
if out="$(probe)"; then fail "a CLI missing allowManagedPermissionRulesOnly must FAIL"; fi
grep -q "DRIFT: managed-settings key 'allowManagedPermissionRulesOnly'" <<<"$out" || fail "drift must name the missing key; got: $out"
echo "ok  a missing key fails and is named"

# 3. The file name gone => fail naming it.
fake_cli "$pin" "${all[@]:0:7}"
if out="$(probe)"; then fail "a CLI missing the managed-settings.json file name must FAIL"; fi
grep -q "path component 'managed-settings.json'" <<<"$out" || fail "drift must name the missing path component; got: $out"
echo "ok  a missing path component fails and is named"

# 4. A CLI that is not the pinned version => fail (the probe must test what the image ships).
fake_cli "0.0.1" "${all[@]}"
if out="$(probe)"; then fail "a CLI at a version other than the pin must FAIL"; fi
grep -q "not the pinned $pin" <<<"$out" || fail "version mismatch must be named; got: $out"
echo "ok  a version other than the pin fails"

# 5. An unpinned Dockerfile (a channel) => fail before anything is installed.
fake_cli "$pin" "${all[@]}"
sed -i -E 's/^ARG CLAUDE_CODE_VERSION=.*/ARG CLAUDE_CODE_VERSION=latest/' "$TMP/deploy/images/claude-code/Dockerfile"
if out="$(probe)"; then fail "CLAUDE_CODE_VERSION=latest must FAIL"; fi
grep -q "not an exact x.y.z" <<<"$out" || fail "a channel pin must be named; got: $out"
echo "ok  a channel pin fails"
