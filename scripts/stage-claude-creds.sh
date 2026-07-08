#!/usr/bin/env bash
# Stage Claude subscription credentials for Wardyn's composer subscription mode.
#
# What this does (and why):
#   1. COPIES ~/.claude and ~/.claude.json to a staging dir OUTSIDE any repo
#      (default ~/.wardyn/claude-creds). Copies, never the live dir: a run's
#      read-only bind mount protects the copies from tampering, and the live
#      ~/.claude is never exposed to a sandbox at all. Outside the repo tree on
#      purpose — a gitignored in-tree dir is one `git add -f` away from leaking
#      a long-lived OAuth token into history.
#   2. GENERATES the composer-capable subscription ceiling policy
#      (~/.wardyn/composer-dev-subscription.json) from the committed template,
#      substituting this machine's staging dir. Mount sources are machine-
#      specific, so no committed example carries a real path.
#
# A composed run receives these mounts ONLY when the human ticks "Use my Claude
# subscription" on that request (per-run opt-in) — staging alone grants nothing.
#
# SENTINEL (default): the staged .credentials.json is SANITIZED into an inert
# sentinel — refresh token blanked, expiry pinned far out — because wardynd
# injects the operator's LIVE OAuth token proxy-side per request (the sandbox's
# copy is never used to reach Anthropic; it only lets `claude` start). So the
# durable rotating secret is NOT resident in the sandbox, and the copy can never
# go stale. (~/.claude.json still carries MCP config + history; read-only
# mitigates tampering, not reading — prefer the api-key path if that matters.)
#
# ESCAPE HATCH: set WARDYN_SUBSCRIPTION_INJECT=off (for BOTH this script and
# wardynd) to keep the legacy resident-copy behavior — a real, refreshable
# credential is staged and used directly. That copy CAN go stale; re-stage to
# refresh. Do not mix modes: re-run this script whenever you flip the env var.
#
# Usage: [WARDYN_SUBSCRIPTION_INJECT=off] scripts/stage-claude-creds.sh [staging-dir]
#        (re-run to refresh copies)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="${ROOT}/examples/policies/composer-dev-subscription.template.json"
DEST="${1:-${HOME}/.wardyn/claude-creds}"
POLICY="$(dirname "${DEST}")/composer-dev-subscription.json"

[[ -d "${HOME}/.claude" ]]      || { echo "error: ~/.claude not found (is the claude CLI logged in?)" >&2; exit 1; }
[[ -f "${HOME}/.claude.json" ]] || { echo "error: ~/.claude.json not found (the CLI needs BOTH)" >&2; exit 1; }
case "${DEST}" in
  "${ROOT}"/*) echo "error: staging dir ${DEST} is inside the repo tree; pick one outside it" >&2; exit 1;;
esac

mkdir -p "${DEST}"
rm -rf "${DEST}/.claude" "${DEST}/.claude.json"
mkdir -p "${DEST}/.claude"
# Copy only the auth + config the sandbox needs — NOT the operator's volatile
# runtime state (jobs/projects/sessions/…). That state is large, is partly owned
# by OTHER uids (sandboxed agents write agent-owned files there, which breaks a
# plain `cp -a ~/.claude`), and would needlessly expose the operator's host
# history/transcripts INSIDE the sandbox. Denylist those top-level entries; copy
# the rest (.credentials.json, settings.json, plugins, CLAUDE.md, …).
_skip=" jobs projects sessions shell-snapshots todos statsig history.jsonl "
shopt -s nullglob dotglob
for entry in "${HOME}/.claude/"*; do
  base="$(basename "${entry}")"
  case "${_skip}" in *" ${base} "*) continue;; esac
  cp -a "${entry}" "${DEST}/.claude/" 2>/dev/null || echo "note: skipped unreadable ~/.claude/${base}"
done
shopt -u nullglob dotglob
cp -a "${HOME}/.claude.json" "${DEST}/.claude.json"

# Never ship credential BACKUPS into the sandbox — claude keeps a
# .credentials-backup.json (and .bak variants) that carry a REAL, possibly still
# valid refresh token, which the sentinel sanitization below does not touch.
rm -f "${DEST}/.claude/.credentials-backup.json" "${DEST}/.claude/.credentials.json.bak"* 2>/dev/null || true

# Strip the operator's MCP config + tokens from the sandbox copy — ALWAYS, both
# modes. The sandbox cannot reach the operator's host MCP servers (they need host
# network/credentials), and claude would otherwise HANG at startup trying to
# connect every one of them (blocked by the sandbox egress allowlist). mcpOAuth
# also carries real per-server access/refresh tokens + client secrets that must
# not be resident in the sandbox.
python3 - "${DEST}/.claude.json" "${DEST}/.claude/.credentials.json" <<'PY'
import json, sys
cj_json, cred = sys.argv[1], sys.argv[2]
try:
    with open(cj_json) as f:
        d = json.load(f)
    if isinstance(d, dict):
        if d.get("mcpServers"):
            d["mcpServers"] = {}
        for proj in (d.get("projects") or {}).values():
            if isinstance(proj, dict) and proj.get("mcpServers"):
                proj["mcpServers"] = {}
        with open(cj_json, "w") as f:
            json.dump(d, f)
except FileNotFoundError:
    pass
try:
    with open(cred) as f:
        c = json.load(f)
    if isinstance(c, dict) and "mcpOAuth" in c:
        del c["mcpOAuth"]
        with open(cred, "w") as f:
            json.dump(c, f)
except FileNotFoundError:
    pass
PY
echo "sanitized: MCP servers + mcpOAuth tokens stripped from the sandbox copy"

# Sentinel sanitization (default; skipped only in the WARDYN_SUBSCRIPTION_INJECT=off
# escape hatch). Blank the refresh token so no usable rotating secret is resident,
# and pin expiresAt far out so `claude` never client-refreshes — the proxy injects
# the live host token at request time, so the frozen access token's staleness is
# immaterial. Leaving the access token in place lets `claude` start cleanly.
if [[ "$(printf '%s' "${WARDYN_SUBSCRIPTION_INJECT:-}" | tr '[:upper:]' '[:lower:]')" != "off" ]]; then
  CJ="${DEST}/.claude/.credentials.json"
  if [[ -f "${CJ}" ]]; then
    python3 - "${CJ}" <<'PY'
import json, sys
p = sys.argv[1]
with open(p) as f:
    d = json.load(f)
o = d.get("claudeAiOauth")
if isinstance(o, dict):
    o["refreshToken"] = ""            # no usable rotating secret resident
    o["expiresAt"] = 4102444800000    # 2100-01-01 in ms: claude never client-refreshes
with open(p, "w") as f:
    json.dump(d, f)
PY
    echo "sentinel: staged .credentials.json sanitized (refresh token blanked, expiry pinned; proxy injects the live token)"
  fi
else
  echo "escape hatch: WARDYN_SUBSCRIPTION_INJECT=off — staging a REAL resident credential (can go stale; re-run to refresh)"
fi

# Owner-only: the agent runs as uid 1000; on a typical dev box that IS this user.
chmod -R go-rwx "${DEST}"
if [[ "$(id -u)" != "1000" ]]; then
  echo "note: your uid is $(id -u), but the sandbox agent runs as uid 1000 — it may not be able to read the copies."
fi

# Generate the policy: substitute the staging dir and strip template-only "__*"
# keys (wardynd's LoadPolicySpec uses DisallowUnknownFields and would reject them).
python3 - "${TEMPLATE}" "${POLICY}" "${DEST}" <<'PY'
import json, sys
template, out, dest = sys.argv[1], sys.argv[2], sys.argv[3]
with open(template) as f:
    raw = f.read().replace("__WARDYN_CRED_DIR__", dest)
spec = {k: v for k, v in json.loads(raw).items() if not k.startswith("__")}
with open(out, "w") as f:
    json.dump(spec, f, indent=2)
    f.write("\n")
PY

echo "staged: ${DEST}/.claude and ${DEST}/.claude.json (read-only copies, owner-only)"
echo "policy: ${POLICY}"
echo
echo "Use it:  WARDYN_DEFAULT_POLICY=${POLICY} scripts/run-host.sh"
echo "         (run-host.sh auto-picks it up when the file exists)"
echo "Then tick \"Use my Claude subscription\" when composing a run."
