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
# DRIFT means: a key in internal/agentpolicy/testdata/*-managed-settings.json (the frozen documents) or
# testdata/attended-rules/*.json (the frozen documents with an interactive run's tool rules written in), or the
# directory or file name of ClaudeCodeManagedSettingsPath, is absent from the installed
# CLI. The presence check runs on both shipped platforms (amd64, and arm64 from a second
# `npm install --cpu=arm64 --os=linux`; the arm64 binary is only read, never run, and its
# platform package must carry the same version as the amd64 one). A behaviour check then
# starts the amd64 CLI in the agent image's base container with each frozen document
# installed as /etc/claude-code/managed-settings.json, no network and no model, and
# asserts the permissionMode its init event reports is the document's defaultMode, even
# under --permission-mode bypassPermissions when the document disables bypass, and that the
# hold lane's --permission-mode manual still reports `default` under every document (a launch
# flag beats a managed defaultMode: acceptEdits, so a hold run is never auto-accepting). A last
# check proves allowManagedHooksOnly on the bypass-launched document; another that every built-in tool an
# attended-rules document denies is missing from the tools its init event lists, and present without a file: a repository SessionStart
# hook fires without the managed file and does not with it. A final pair drives an offline Messages API stand-in
# to prove a permissions.ask entry still holds WebFetch under --dangerously-skip-permissions and that a repository's
# own permissions.allow does not lift it. No step needs a credential. Bump the Dockerfile pin and this probe re-runs against the
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
docs=("$POLICY_DIR"/testdata/*-managed-settings.json "$POLICY_DIR"/testdata/attended-rules/*.json)
keys="$(cat "${docs[@]}" | grep -oE '"[^"]+"[[:space:]]*:' | sed -E 's/^"([^"]*)".*/\1/' | sort -u)"
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
for doc in "${docs[@]}"; do
    want="$(sed -nE 's/^[[:space:]]*"defaultMode":[[:space:]]*"([^"]*)".*/\1/p' "$doc" | head -1)"
    [ -n "$want" ] || die "$doc has no permissions.defaultMode — the behaviour check would pass vacuously"
    dir="$tmp/settings-$(basename "$(dirname "$doc")")-$(basename "$doc" .json)"
    mkdir -p "$dir"; cp "$doc" "$dir/$(basename "$path")"
    got="$(mode_of "$dir")"
    [ "$got" = "$want" ] || die "$(basename "$doc"): Claude Code $pin reports permissionMode '${got:-none}', the managed defaultMode is '$want' — the CLI does not act on the file it reads; stderr: $(head -c 400 "$tmp/stderr")"
    got="$(mode_of "$dir" --permission-mode manual)"
    [ "$got" = default ] || die "$(basename "$doc"): --permission-mode manual yields '${got:-none}', not 'default' — the managed defaultMode beats the hold lane's launch flag; stderr: $(head -c 400 "$tmp/stderr")"
    if grep -q '"disableBypassPermissionsMode"' "$doc"; then
        got="$(mode_of "$dir" --permission-mode bypassPermissions)"
        [ "$got" = "$want" ] || die "$(basename "$doc"): --permission-mode bypassPermissions yields '${got:-none}' although the file disables bypass (want '$want') — the bypass lock is not honoured; stderr: $(head -c 400 "$tmp/stderr")"
    fi
    behaved=$((behaved + 1))
done
[ "$behaved" -gt 0 ] || die "no managed-settings documents under $POLICY_DIR/testdata for the behaviour check"
echo "ok: Claude Code $pin acts on $behaved managed-settings document(s) (reported mode matches; --permission-mode manual stays default; bypass stays locked where disabled)"

# 6. Hooks. A document that carries allowManagedHooksOnly and leaves bypass launchable (the locked L2
# one) has to keep a repository's own hook from running under --dangerously-skip-permissions. SessionStart
# fires before any model call, so the check is offline; the run without the managed file must fire the hook
# or the assertion is vacuous.
mkdir -p "$tmp/ws/.claude" "$tmp/marker"
chmod 777 "$tmp/marker"
cat > "$tmp/ws/.claude/settings.json" <<'JSON'
{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch /marker/fired"}]}]}}
JSON
hook_fired() { # [managed settings dir] -> "fired" when the repository hook ran
    local mount=()
    [ -z "${1:-}" ] || mount=(-v "$1:/etc/claude-code:ro")
    rm -f "$tmp/marker/fired"
    docker run --rm --network none --user node --cap-drop ALL --security-opt no-new-privileges \
        --read-only --tmpfs /tmp -v "$amd_exe:/usr/local/bin/claude:ro" ${mount[@]+"${mount[@]}"} \
        -v "$tmp/ws:/ws:ro" -v "$tmp/marker:/marker" -w /ws \
        -e HOME=/tmp -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 "$image" \
        timeout 60 claude --print hi --dangerously-skip-permissions >/dev/null 2>"$tmp/stderr" || true
    [ ! -e "$tmp/marker/fired" ] || echo fired
}
[ "$(hook_fired)" = fired ] || die "a repository SessionStart hook did not fire without a managed file under --dangerously-skip-permissions — the hook check would pass vacuously; stderr: $(head -c 400 "$tmp/stderr")"
hooked=0
for doc in "$POLICY_DIR"/testdata/*-managed-settings.json; do
    grep -q '"allowManagedHooksOnly": true' "$doc" || continue
    ! grep -q '"disableBypassPermissionsMode"' "$doc" || continue
    dir="$tmp/settings-testdata-$(basename "$doc" .json)"
    [ "$(hook_fired "$dir")" != fired ] || die "$(basename "$doc"): a repository hook still fired under --dangerously-skip-permissions although the file sets allowManagedHooksOnly"
    hooked=$((hooked + 1))
done
[ "$hooked" -gt 0 ] || die "no bypass-launchable document under $POLICY_DIR/testdata sets allowManagedHooksOnly — the hook check has nothing to prove"
echo "ok: Claude Code $pin runs no repository hook under --dangerously-skip-permissions with $hooked managed-settings document(s) that set allowManagedHooksOnly"

# 7. Denied tools. An attended-rules document's permissions.deny has to take each built-in tool it names out of
# the session: the init event lists the session's tools before any model call, so this is offline too. A tool
# the CLI does not list without the managed file proves nothing, so that control is required.
tools_of() { # <settings dir or ""> -> the init event's tools array, as JSON text
    local mount=() out
    [ -z "$1" ] || mount=(-v "$1:/etc/claude-code:ro")
    # T: marks a recognised init event with a tools array (an empty one is a valid answer: every tool
    # denied); no such event means the run proved nothing, so it fails instead of reading as "no tools".
    out="$(docker run --rm --network none --user node --cap-drop ALL --security-opt no-new-privileges \
        --read-only --tmpfs /tmp -v "$amd_exe:/usr/local/bin/claude:ro" ${mount[@]+"${mount[@]}"} \
        -e HOME=/tmp -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 "$image" \
        claude --print hi --output-format stream-json --verbose 2>"$tmp/stderr" \
        | sed -nE 's/.*"subtype":"init".*"tools":\[([^]]*)\].*/T:\1/p' | head -1 || true)"
    case "$out" in T:*) echo "${out#T:}" ;; *) die "Claude Code $pin printed no init event with a tools array (settings: ${1:-none}) — the deny check would pass without evidence; stderr: $(head -c 400 "$tmp/stderr")" ;; esac
}
unfiled="$(tools_of "")"
denied=0
for doc in "$POLICY_DIR"/testdata/attended-rules/*.json; do
    names="$(sed -n '/"deny": \[/,/\]/p' "$doc" | grep -v ':' | grep -oE '"[A-Za-z0-9_.-]+"' | tr -d '"' | grep -v '^mcp__' || true)"
    dir="$tmp/settings-attended-rules-$(basename "$doc" .json)"
    listed="$(tools_of "$dir")"
    for n in $names; do
        grep -qF "\"$n\"" <<<"$unfiled" || die "$(basename "$doc"): '$n' is not among the tools Claude Code $pin lists without a managed file — the deny check would pass vacuously; stderr: $(head -c 400 "$tmp/stderr")"
        ! grep -qF "\"$n\"" <<<"$listed" || die "attended-rules/$(basename "$doc"): Claude Code $pin still lists '$n', which the file's permissions.deny names — the deny list is not honoured"
        denied=$((denied + 1))
    done
done
[ "$denied" -gt 0 ] || die "no attended-rules document denies a built-in tool — the deny check has nothing to prove"
echo "ok: Claude Code $pin drops every built-in tool the attended-rules documents deny ($denied checks)"

# 8. Ask and repository allow. An attended-rules document's permissions.ask has to hold a tool back even under
# --dangerously-skip-permissions (the no-level, L2 and L2-locked bases launch bypass), and with
# allowManagedPermissionRulesOnly a repository's own permissions.allow must not lift it. A tiny Messages API
# stand-in (node, which the image has) asks the CLI to call WebFetch; `--print` records a call it refused in
# the result event's permission_denials, so no model, network or credential is needed. A control run without
# the managed file must let WebFetch through under each condition, or the assertions prove nothing.
mkdir -p "$tmp/ws-empty" "$tmp/ws-allow/.claude"
echo '{"permissions":{"allow":["WebFetch"]}}' > "$tmp/ws-allow/.claude/settings.json"
echo '{"hasCompletedOnboarding":true,"projects":{"/ws":{"hasTrustDialogAccepted":true,"hasCompletedProjectOnboarding":true}}}' > "$tmp/claude.json"
cat > "$tmp/stub.js" <<'JS'
const http = require("http");
const ev = (n, d) => "event: " + n + "\ndata: " + JSON.stringify(d) + "\n\n";
http.createServer((req, res) => {
  let raw = "";
  req.on("data", (c) => (raw += c));
  req.on("end", () => {
    const body = JSON.parse(raw || "{}");
    if (!req.url.startsWith("/v1/messages") || req.url.includes("count_tokens")) { res.writeHead(200, { "content-type": "application/json" }); return res.end('{"input_tokens":1}'); }
    const last = (body.messages || []).slice(-1)[0];
    const answered = last && Array.isArray(last.content) && last.content.some((c) => c.type === "tool_result");
    const call = !answered && (body.tools || []).some((t) => t.name === "WebFetch");
    const block = call ? { type: "tool_use", id: "toolu_probe1", name: "WebFetch", input: {} } : { type: "text", text: "" };
    const delta = call ? { type: "input_json_delta", partial_json: '{"url":"https://example.com/","prompt":"summarise"}' } : { type: "text_delta", text: "ok" };
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.end(ev("message_start", { type: "message_start", message: { id: "m", type: "message", role: "assistant", model: "claude-sonnet-4-5", content: [], stop_reason: null, stop_sequence: null, usage: { input_tokens: 1, output_tokens: 1 } } })
      + ev("content_block_start", { type: "content_block_start", index: 0, content_block: block })
      + ev("content_block_delta", { type: "content_block_delta", index: 0, delta })
      + ev("content_block_stop", { type: "content_block_stop", index: 0 })
      + ev("message_delta", { type: "message_delta", delta: { stop_reason: call ? "tool_use" : "end_turn", stop_sequence: null }, usage: { output_tokens: 1 } })
      + ev("message_stop", { type: "message_stop" }));
  });
}).listen(8080, "127.0.0.1");
JS
chmod -R a+rX "$tmp/ws-empty" "$tmp/ws-allow" "$tmp/stub.js" "$tmp/claude.json"
webfetch_of() { # <settings dir or ""> <workspace dir> [claude args...] -> denied | ran | "" (no result event)
    local mount=() ws="$2" out; [ -z "$1" ] || mount=(-v "$1:/etc/claude-code:ro"); shift 2
    out="$(docker run --rm --network none --user node --cap-drop ALL --security-opt no-new-privileges \
        --read-only --tmpfs /tmp -v "$amd_exe:/usr/local/bin/claude:ro" ${mount[@]+"${mount[@]}"} \
        -v "$tmp/stub.js:/probe/stub.js:ro" -v "$tmp/claude.json:/probe/claude.json:ro" -v "$ws:/ws:ro" -w /ws \
        -e HOME=/tmp -e CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 -e ANTHROPIC_BASE_URL=http://127.0.0.1:8080 -e ANTHROPIC_AUTH_TOKEN=probe-token \
        --entrypoint sh "$image" -c 'cp /probe/claude.json /tmp/.claude.json; node /probe/stub.js & sleep 1; timeout 60 claude --print "run the probe" --output-format stream-json --verbose "$@"' sh "$@" 2>"$tmp/stderr" \
        | grep -oE '"permission_denials":\[[^]]*\]' | head -1 || true)"
    case "$out" in "") ;; *WebFetch*) echo denied ;; *) echo ran ;; esac
}
[ "$(webfetch_of "" "$tmp/ws-empty" --dangerously-skip-permissions)" = ran ] || die "WebFetch did not run under --dangerously-skip-permissions without a managed file — the ask-under-bypass check would pass vacuously; stderr: $(head -c 400 "$tmp/stderr")"
[ "$(webfetch_of "" "$tmp/ws-allow")" = ran ] || die "a repository permissions.allow did not let WebFetch run without a managed file — the repository-allow check would pass vacuously; stderr: $(head -c 400 "$tmp/stderr")"
asked=0; allowed=0
for doc in "$POLICY_DIR"/testdata/attended-rules/*.json; do
    grep -q '"WebFetch"' <(sed -n '/"ask": \[/,/\]/p' "$doc") || continue
    dir="$tmp/settings-attended-rules-$(basename "$doc" .json)"
    if ! grep -q '"disableBypassPermissionsMode"' "$doc"; then
        [ "$(webfetch_of "$dir" "$tmp/ws-empty" --dangerously-skip-permissions)" = denied ] || die "attended-rules/$(basename "$doc"): WebFetch ran under --dangerously-skip-permissions although the file's permissions.ask names it — ask no longer beats bypass; stderr: $(head -c 400 "$tmp/stderr")"
        asked=$((asked + 1))
    fi
    if grep -q '"allowManagedPermissionRulesOnly": true' "$doc"; then
        [ "$(webfetch_of "$dir" "$tmp/ws-allow")" = denied ] || die "attended-rules/$(basename "$doc"): a repository permissions.allow let WebFetch run although the file sets allowManagedPermissionRulesOnly and asks for it; stderr: $(head -c 400 "$tmp/stderr")"
        allowed=$((allowed + 1))
    fi
done
[ "$asked" -gt 0 ] && [ "$allowed" -gt 0 ] || die "no attended-rules document asks for WebFetch with bypass launchable and allowManagedPermissionRulesOnly — the ask and repository-allow checks have nothing to prove"
echo "ok: Claude Code $pin still asks for WebFetch under --dangerously-skip-permissions ($asked document(s)) and ignores a repository allow ($allowed document(s))"
