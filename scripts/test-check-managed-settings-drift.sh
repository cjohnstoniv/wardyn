#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-check-managed-settings-drift.sh — the drift probe fails on the drift it exists for,
# and passes when there is none. A probe that cannot go red is worse than none.
#
# Daemon-free, network-free: CLAUDE_CODE_DIR and CLAUDE_CODE_DIR_ARM64 point the probe at fake
# installed CLIs (a shell script whose text carries the strings a real binary would), and a
# fake `docker` on PATH stands in for the behaviour check's container, inside a throwaway tree.
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

# fake_cli <version> <string...>: an amd64 prefix whose "claude" reports <version> and contains <string...>,
# and an arm64 prefix (same version, same strings) that is only ever read.
fake_cli() {
    local v="$1" d arch; shift
    for arch in x64 arm64; do
        d="$TMP/cli-$arch/node_modules/@anthropic-ai/claude-code-linux-$arch"
        rm -rf "$TMP/cli-$arch"; mkdir -p "$d"
        { echo '#!/bin/sh'; echo "echo '$v (Claude Code)'"; echo 'exit 0'; printf '# %s\n' "$@"; } > "$d/claude"
        chmod +x "$d/claude"
        printf '{\n  "name": "@anthropic-ai/claude-code-linux-%s",\n  "version": "%s"\n}\n' "$arch" "$v" > "$d/package.json"
    done
}
# A docker that reports what a CLI honouring the mounted managed-settings.json would. Mode runs report the
# document's defaultMode, or $FAKE_MODE (a CLI that ignores the file); a --permission-mode bypassPermissions run
# reports the bypass mode when $FAKE_BYPASS is set (a CLI ignoring disableBypassPermissionsMode); a
# --permission-mode manual run reports $FAKE_MANUAL_MODE (default: "default"). A hook run touches the marker
# unless the file sets allowManagedHooksOnly, or always when $FAKE_HOOK_FIRES is set.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/docker" <<'FAKE'
#!/bin/sh
for a in "$@"; do case "$a" in *:/etc/claude-code:ro) d="${a%%:*}" ;; *:/marker) m="${a%%:*}" ;; bypassPermissions) by=1 ;; manual) man=1 ;; esac; done
if [ -n "${m:-}" ]; then
    if [ -z "${d:-}" ] || [ -n "${FAKE_HOOK_FIRES:-}" ] || ! grep -q '"allowManagedHooksOnly": true' "$d/managed-settings.json"; then touch "$m/fired"; fi
    exit 1
fi
mode="$(sed -nE 's/^[[:space:]]*"defaultMode":[[:space:]]*"([^"]*)".*/\1/p' "$d/managed-settings.json" | head -1)"
mode="${FAKE_MODE:-$mode}"
[ -z "${by:-}" ] || [ -z "${FAKE_BYPASS:-}" ] || mode=bypassPermissions
[ -z "${man:-}" ] || mode="${FAKE_MANUAL_MODE:-default}"
echo "{\"type\":\"system\",\"subtype\":\"init\",\"permissionMode\":\"$mode\"}"
exit 1
FAKE
chmod +x "$TMP/bin/docker"
probe() { PATH="$TMP/bin:$PATH" CLAUDE_CODE_DIR="$TMP/cli-x64" CLAUDE_CODE_DIR_ARM64="$TMP/cli-arm64" "$TMP/scripts/check-managed-settings-drift.sh" 2>&1; }
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

# 5. arm64 gone a key the amd64 binary still has => fail, naming the key and the arch.
fake_cli "$pin" "${all[@]}"
printf '# %s\n' permissions defaultMode disableBypassPermissionsMode disableAutoMode allowManagedHooksOnly /etc/claude-code managed-settings.json > "$TMP/cli-arm64/node_modules/@anthropic-ai/claude-code-linux-arm64/claude"
if out="$(probe)"; then fail "an arm64 CLI missing allowManagedPermissionRulesOnly must FAIL"; fi
grep -q "key 'allowManagedPermissionRulesOnly' is not in Claude Code $pin (arm64)" <<<"$out" || fail "arm64 drift must name the key and the arch; got: $out"
grep -q "(amd64)" <<<"$out" && fail "amd64 still has the key and must not be blamed; got: $out"
echo "ok  an arm64-only missing key fails and is named"

# 6. arm64 package at another version than amd64 => fail.
fake_cli "$pin" "${all[@]}"
sed -i 's/"version": ".*"/"version": "0.0.1"/' "$TMP/cli-arm64/node_modules/@anthropic-ai/claude-code-linux-arm64/package.json"
if out="$(probe)"; then fail "an arm64 package at another version than amd64 must FAIL"; fi
grep -q "arm64 package version '0.0.1'.*not the amd64 one '$pin'" <<<"$out" || fail "arm64 version skew must be named; got: $out"
echo "ok  an arm64 version that differs from amd64 fails"

# 7. A CLI that reads the file but does not act on it => fail naming the document.
fake_cli "$pin" "${all[@]}"
if out="$(FAKE_MODE=bypassPermissions probe)"; then fail "a CLI reporting a mode other than the managed defaultMode must FAIL"; fi
grep -q "permissionMode 'bypassPermissions', the managed defaultMode is" <<<"$out" || fail "behaviour drift must name the mode; got: $out"
echo "ok  a reported mode that differs from the file fails"

# 8. Bypass not locked under --permission-mode bypassPermissions => fail. The fake answers the plain
# run correctly and the bypass run with the bypass mode, as a CLI ignoring disableBypassPermissionsMode would.
fake_cli "$pin" "${all[@]}"
if out="$(FAKE_BYPASS=1 probe)"; then fail "a CLI that lets --permission-mode bypassPermissions through must FAIL"; fi
grep -q "bypass lock is not honoured" <<<"$out" || fail "an unlocked bypass must be named; got: $out"
echo "ok  an unlocked bypass fails"

# 8b. A launch flag that does not beat the managed defaultMode => fail naming the document. The hold lane's
# --permission-mode manual would otherwise run under acceptEdits.
if out="$(FAKE_MANUAL_MODE=acceptEdits probe)"; then fail "a CLI where the managed defaultMode beats --permission-mode manual must FAIL"; fi
grep -q "managed-settings.json: --permission-mode manual yields 'acceptEdits'" <<<"$out" || fail "a manual-mode override must be named; got: $out"
echo "ok  a managed mode that beats --permission-mode manual fails"

# 8c. A hook that still fires under allowManagedHooksOnly => fail naming the document.
if out="$(FAKE_HOOK_FIRES=1 probe)"; then fail "a CLI that runs a repository hook despite allowManagedHooksOnly must FAIL"; fi
grep -q "the hook check would pass vacuously" <<<"$out" && fail "the control run (no managed file) must fire the hook; got: $out"
grep -q "L2-locked-managed-settings.json: a repository hook still fired" <<<"$out" || fail "a fired hook must be named; got: $out"
echo "ok  a repository hook that fires under allowManagedHooksOnly fails"

# 8d. No bypass-launchable document sets allowManagedHooksOnly => fail: the hook check would prove nothing.
cp -r "$TMP/internal/agentpolicy" "$TMP/internal/agentpolicy.bak"
rm "$TMP/internal/agentpolicy/testdata/L2-locked-managed-settings.json"
if out="$(probe)"; then fail "no bypass-launchable document with allowManagedHooksOnly must FAIL"; fi
grep -q "the hook check has nothing to prove" <<<"$out" || fail "an empty hook check must be named; got: $out"
rm -r "$TMP/internal/agentpolicy" && mv "$TMP/internal/agentpolicy.bak" "$TMP/internal/agentpolicy"
echo "ok  a hook check with no document to prove fails"

# 9. An unpinned Dockerfile (a channel) => fail before anything is installed. Last: it leaves the copy unpinned.
fake_cli "$pin" "${all[@]}"
sed -i -E 's/^ARG CLAUDE_CODE_VERSION=.*/ARG CLAUDE_CODE_VERSION=latest/' "$TMP/deploy/images/claude-code/Dockerfile"
if out="$(probe)"; then fail "CLAUDE_CODE_VERSION=latest must FAIL"; fi
grep -q "not an exact x.y.z" <<<"$out" || fail "a channel pin must be named; got: $out"
echo "ok  a channel pin fails"
