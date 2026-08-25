#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# No-docker, no-network honesty checks for the desktop tier's install lane
# (deploy/desktop/). Catches the failure modes that pass a code review but
# break the actual laptop install: a var the example envelope sets that
# docs/ENV.md never documents (DESKTOP.md's own promise — "Every variable ...
# already exists and is already documented"), a policy path that has drifted
# from the MDM file table, a plist that isn't valid XML, an install script
# with a syntax error. Daemon-free by construction — see Makefile test-scripts.
#
# Usage: scripts/test-desktop-profile.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DESK_DIR="${REPO_ROOT}/deploy/desktop"
ENV_EXAMPLE="${DESK_DIR}/wardyn.env.example"
ENV_MD="${REPO_ROOT}/docs/ENV.md"

fail() { echo "test-desktop-profile: FAIL: $*" >&2; exit 1; }

# ── 1. wardyn.env.example parses as shell/env syntax ────────────────────────
# Every uncommented, non-blank line must be a bare KEY=VALUE assignment (what
# an MDM env-rendering step and `docker compose --env-file` both expect) —
# catches a stray shell-ism (export, quotes MDM won't strip, a heredoc) that
# would silently corrupt every device's envelope.
[ -f "${ENV_EXAMPLE}" ] || fail "${ENV_EXAMPLE} not found"
bad_lines="$(grep -vE '^\s*(#.*)?$' "${ENV_EXAMPLE}" | grep -vE '^[A-Za-z_][A-Za-z0-9_]*=' || true)"
[ -z "${bad_lines}" ] || fail "wardyn.env.example has non-KEY=VALUE lines:
${bad_lines}"

# ── 2. every var it SETS (uncommented) is a real documented var ─────────────
# DESKTOP.md's own claim: "There is NOTHING NEW here ... already documented in
# docs/ENV.md". A var present here but absent from ENV.md is either a typo an
# operator would silently misconfigure, or an env-doc gap — either way, this
# is the guard that catches it before a device ships it.
[ -f "${ENV_MD}" ] || fail "${ENV_MD} not found"
vars="$(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "${ENV_EXAMPLE}" | tr -d '=' | sort -u)"
[ -n "${vars}" ] || fail "wardyn.env.example set no variables — parsing regressed"
missing=""
while IFS= read -r v; do
  grep -qE "\`${v}\`" "${ENV_MD}" || missing="${missing}${v}\n"
done <<<"${vars}"
[ -z "${missing}" ] || fail "vars set in wardyn.env.example but not documented in docs/ENV.md:
$(printf '%b' "${missing}")"

# ── 3. the policy path resolves per the MDM file table ──────────────────────
policy_line="$(grep -E '^WARDYN_DEFAULT_POLICY=' "${ENV_EXAMPLE}" || true)"
[ -n "${policy_line}" ] || fail "WARDYN_DEFAULT_POLICY not set in wardyn.env.example"
policy_val="${policy_line#WARDYN_DEFAULT_POLICY=}"
[ "${policy_val}" = "/etc/wardyn/policy.json" ] || fail "WARDYN_DEFAULT_POLICY='${policy_val}', want /etc/wardyn/policy.json (docs/DESKTOP.md 'The MDM file table')"
grep -q '/etc/wardyn/policy.json' "${REPO_ROOT}/docs/DESKTOP.md" \
  || fail "docs/DESKTOP.md no longer mentions /etc/wardyn/policy.json — the MDM file table and the example envelope have drifted apart"

# ── 4. the included stack still mounts that managed dir, and the desktop
#       entrypoint still sets it ────────────────────────────────────────────
# The mount lives in the INCLUDED file, not the desktop one: re-declaring
# services.wardynd beside an `include:` that already declares it is a collision
# older Compose refuses outright ("conflicts with imported resource"), which
# broke the desktop-envelope CI job while passing on a newer local Compose.
grep -qE '^\s*-\s*\$\{WARDYN_MANAGED_DIR:-[^}]+\}:/etc/wardyn:ro\s*$' "${REPO_ROOT}/deploy/compose/docker-compose.yaml" \
  || fail "deploy/compose/docker-compose.yaml no longer bind-mounts \${WARDYN_MANAGED_DIR}:/etc/wardyn:ro — WARDYN_DEFAULT_POLICY would point at a path the container can't see"
grep -qE '^\s*export\s+WARDYN_MANAGED_DIR=' "${DESK_DIR}/wardyn-desktop.sh" \
  || fail "deploy/desktop/wardyn-desktop.sh no longer exports WARDYN_MANAGED_DIR — the managed dir would mount as the empty default"
grep -qE '^\s*-\s*path:\s*\.\./compose/docker-compose\.yaml\s*$' "${DESK_DIR}/docker-compose.yaml" \
  || fail "deploy/desktop/docker-compose.yaml no longer includes ../compose/docker-compose.yaml — it would no longer be a configuration of the same stack"
if grep -qE '^\s*services:\s*$' "${DESK_DIR}/docker-compose.yaml"; then
  fail "deploy/desktop/docker-compose.yaml re-declares services: beside its include: — that collides with the imported stack on older Compose (services.wardynd conflicts with imported resource)"
fi

# ── 5. the plist is valid XML ────────────────────────────────────────────────
PLIST="${DESK_DIR}/com.wardyn.daemon.plist"
[ -f "${PLIST}" ] || fail "${PLIST} not found"
if command -v xmllint >/dev/null 2>&1; then
  xmllint --noout "${PLIST}" || fail "${PLIST} is not valid XML (xmllint)"
else
  python3 -c "import xml.dom.minidom as m; m.parse('${PLIST}')" || fail "${PLIST} is not valid XML (python3 xml.dom.minidom)"
fi
grep -q '__WARDYN_DESKTOP_SH__' "${PLIST}" || fail "${PLIST} lost its install.sh placeholder (__WARDYN_DESKTOP_SH__) — install.sh's sed substitution would silently no-op"

# ── 6. install.sh and wardyn-desktop.sh: syntax, and shellcheck if present ──
for f in install.sh wardyn-desktop.sh; do
  s="${DESK_DIR}/${f}"
  [ -x "${s}" ] || fail "${s} missing or not executable"
  bash -n "${s}" || fail "${s} failed bash -n"
  if command -v shellcheck >/dev/null 2>&1; then
    shellcheck "${s}" || fail "${s} failed shellcheck"
  fi
done

echo "test-desktop-profile: self-test PASS"
