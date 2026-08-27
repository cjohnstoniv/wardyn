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
ENV_MD="${REPO_ROOT}/docs/ENV.md"

fail() { echo "test-desktop-profile: FAIL: $*" >&2; exit 1; }

# Sections 1-3 run over EVERY shipped envelope variant, not just the a-prime
# one. They used to read a single hardcoded wardyn.env.example, so the m-prime
# (member-mode) envelope would have shipped with no syntax check, no ENV.md
# parity check and no policy-path check — while still printing PASS.
#
# The glob is `wardyn.env*.example`, NOT `*.env.example`: the m-prime file is
# named wardyn.env.m-prime.example, which does NOT end in `.env.example`. The
# narrower glob would match only the a-prime file, iterate once, skip m-prime
# entirely, and still pass — which is the exact hole this loop exists to close.
# The count assertion below is what makes that unfixable by a later rename.
shopt -s nullglob
ENV_EXAMPLES=("${DESK_DIR}"/wardyn.env*.example)
shopt -u nullglob
[ "${#ENV_EXAMPLES[@]}" -ge 2 ] \
  || fail "expected at least 2 envelope variants under ${DESK_DIR} (a-prime + m-prime), found ${#ENV_EXAMPLES[@]}: ${ENV_EXAMPLES[*]:-<none>}. A variant that the glob misses is a variant with NO syntax, parity or policy-path check."

for ENV_EXAMPLE in "${ENV_EXAMPLES[@]}"; do
ENV_NAME="$(basename "${ENV_EXAMPLE}")"

# ── 1. wardyn.env.example parses as shell/env syntax ────────────────────────
# Every uncommented, non-blank line must be a bare KEY=VALUE assignment (what
# an MDM env-rendering step and `docker compose --env-file` both expect) —
# catches a stray shell-ism (export, quotes MDM won't strip, a heredoc) that
# would silently corrupt every device's envelope.
[ -f "${ENV_EXAMPLE}" ] || fail "${ENV_EXAMPLE} not found"
bad_lines="$(grep -vE '^\s*(#.*)?$' "${ENV_EXAMPLE}" | grep -vE '^[A-Za-z_][A-Za-z0-9_]*=' || true)"
[ -z "${bad_lines}" ] || fail "${ENV_NAME} has non-KEY=VALUE lines:
${bad_lines}"

# ── 2. every var it SETS (uncommented) is a real documented var ─────────────
# DESKTOP.md's own claim: "There is NOTHING NEW here ... already documented in
# docs/ENV.md". A var present here but absent from ENV.md is either a typo an
# operator would silently misconfigure, or an env-doc gap — either way, this
# is the guard that catches it before a device ships it.
[ -f "${ENV_MD}" ] || fail "${ENV_MD} not found"
vars="$(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "${ENV_EXAMPLE}" | tr -d '=' | sort -u)"
[ -n "${vars}" ] || fail "${ENV_NAME} set no variables — parsing regressed"
missing=""
while IFS= read -r v; do
  grep -qE "\`${v}\`" "${ENV_MD}" || missing="${missing}${v}\n"
done <<<"${vars}"
[ -z "${missing}" ] || fail "vars set in ${ENV_NAME} but not documented in docs/ENV.md:
$(printf '%b' "${missing}")"

# ── 3. the policy path resolves per the MDM file table ──────────────────────
policy_line="$(grep -E '^WARDYN_DEFAULT_POLICY=' "${ENV_EXAMPLE}" || true)"
[ -n "${policy_line}" ] || fail "WARDYN_DEFAULT_POLICY not set in ${ENV_NAME}"
policy_val="${policy_line#WARDYN_DEFAULT_POLICY=}"
[ "${policy_val}" = "/etc/wardyn/policy.json" ] || fail "${ENV_NAME}: WARDYN_DEFAULT_POLICY='${policy_val}', want /etc/wardyn/policy.json (docs/DESKTOP.md 'The MDM file table')"
grep -q '/etc/wardyn/policy.json' "${REPO_ROOT}/docs/DESKTOP.md" \
  || fail "docs/DESKTOP.md no longer mentions /etc/wardyn/policy.json — the MDM file table and the example envelope have drifted apart"

done

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

# ── 7. the m-prime (member-mode) envelope's own invariants ──────────────────
# Sections 1-3 prove it PARSES and that every var it sets is documented. Those
# would pass on a file that merely mentions the right variable names. These
# check what the profile actually MEANS, which is what an MDM copies fleet-wide.
MPRIME="${DESK_DIR}/wardyn.env.m-prime.example"
[ -f "${MPRIME}" ] || fail "${MPRIME} not found — the member-mode profile docs/DESKTOP.md documents has no shipped envelope"

mp_get() { grep -E "^$1=" "${MPRIME}" | head -1 | cut -d= -f2-; }

# (a) It is actually member mode, with SSO. Either half alone is not m'.
[ "$(mp_get WARDYN_MEMBER_MODE)" = "true" ] \
  || fail "m-prime envelope does not set WARDYN_MEMBER_MODE=true"
[ "$(mp_get WARDYN_LOCAL_MODE)" = "false" ] \
  || fail "m-prime envelope must set WARDYN_LOCAL_MODE=false — a configured issuer plus explicit local mode is refused at boot, and would silently disable the SSO RBAC that makes this profile mean anything"
[ -n "$(mp_get WARDYN_OIDC_ISSUER)" ] \
  || fail "m-prime envelope sets no WARDYN_OIDC_ISSUER — OIDC is mandatory on m', it is the only thing authenticating the member"

# (b) The member can actually mount their own project directory. Unset means
#     "members may not mount host directories at all", which turns off the one
#     power m' exists to add.
roots="$(mp_get WARDYN_MEMBER_WORKSPACE_ROOTS)"
[ -n "${roots}" ] \
  || fail "m-prime envelope leaves WARDYN_MEMBER_WORKSPACE_ROOTS unset — members may then mount NOTHING, which is the one power m' exists to add"

# (c) ...and those roots are NARROW. `/` or a home directory leaves the dotfile
#     deny-list as the only thing between a member and the operator's ~/.ssh,
#     ~/.aws and ~/.claude — and wardynd logs that at boot and starts anyway.
#     Arm (i) of A5's acceptance (a member is refused a mount outside the roots)
#     cannot catch this: it is a property of the FILE MDM copies, not of the
#     running daemon.
IFS=',' read -r -a _roots <<< "${roots}"
for r in "${_roots[@]}"; do
  r="$(echo "${r}" | tr -d '[:space:]')"
  [ -n "${r}" ] || continue
  case "${r}" in
    /|/root|/home|/Users|/home/|/Users/|'$HOME'|'~')
      fail "m-prime WARDYN_MEMBER_WORKSPACE_ROOTS contains '${r}' — a root that wide leaves the dotfile deny-list as the ONLY thing between a member and the operator's credentials. MDM copies this file to every laptop." ;;
  esac
  case "${r}" in
    /*) ;;
    *) fail "m-prime WARDYN_MEMBER_WORKSPACE_ROOTS entry '${r}' is not an absolute path (docs/ENV.md: CSV of absolute paths)" ;;
  esac
done

# (d) The admin token must NOT be in this 0644 file. compose falls back to the
#     PUBLISHED literal `demo-admin-token`, and on a loopback bind that default
#     warns and BOOTS — validateMemberModePosture never looks at the token. So
#     an omitted token silently hands every developer operator rights via
#     `Authorization: Bearer demo-admin-token`, and m''s whole invariant is
#     false on every device. It belongs in secret.env at 0600.
if grep -qE '^WARDYN_ADMIN_TOKEN=' "${MPRIME}"; then
  fail "m-prime envelope SETS WARDYN_ADMIN_TOKEN in the 0644 wardyn.env — it must ship in /etc/wardyn/secret.env at 0600"
fi
grep -q 'secret\.env' "${MPRIME}" \
  || fail "m-prime envelope never names secret.env — the admin token and the OIDC client secret have no stated delivery file, and the compose default is a PUBLISHED token"
grep -q 'WARDYN_ADMIN_TOKEN' "${MPRIME}" \
  || fail "m-prime envelope never mentions WARDYN_ADMIN_TOKEN at all — an operator following it ships the published demo-admin-token to every laptop"

# (e) Both image pins are present and are DIGESTS. A mutable tag on a 300s timer
#     is what A3 exists to close; the proxy pin additionally has no working
#     default at all on this tier.
for v in WARDYN_WARDYND_IMAGE WARDYN_PROXY_IMAGE; do
  val="$(mp_get "${v}")"
  [ -n "${val}" ] || fail "m-prime envelope does not pin ${v} — the launcher then falls back to a mutable tag (wardynd) or an unpullable local-build ref (proxy)"
  case "${val}" in
    *@sha256:*) ;;
    *) fail "m-prime ${v}='${val}' is not digest-pinned; a mutable tag means a rebuild upstream silently changes what every laptop runs" ;;
  esac
done

# ── 7b. the listeners the tier's own promise depends on ────────────────────
# Both listener vars default to EMPTY in the included stack, and empty means
# off — no listener, not even a generated host key. The compose topology
# publishes 127.0.0.1:2222 and :8081 regardless, so an envelope that sets
# neither ships PUBLISHED PORTS THAT REFUSE EVERY CONNECTION while
# docs/DESKTOP.md promises the developer reaches a governed sandbox from their
# own terminal. Phase A used to touch no listener and Phase B no envelope, so
# nothing joined them: that seam is what this asserts.
for f in "${ENV_EXAMPLES[@]}"; do
  grep -qE '^WARDYN_SSH_LISTEN=' "${f}" \
    || fail "$(basename "${f}") sets no WARDYN_SSH_LISTEN — compose still publishes 2222, so the tier ships a port that refuses every connection and `wardyn ssh` does not answer"
  grep -qE '^WARDYN_SSH_ADVERTISE=' "${f}" \
    || fail "$(basename "${f}") sets no WARDYN_SSH_ADVERTISE — the console's 'Attach from your terminal' pane then prints no usable ssh command"
done

# ── 8. no envelope pins an image this project does not publish ─────────────
# release.yml's publish matrix is wardynd, wardyn-proxy, agent-base,
# agent-codex-cli, agent-aws-sso. agent-claude-code is NOT in it — 0.6.2 stopped
# publishing it (it bundles a vendor CLI whose terms are not readable from
# inside the image) and agent-base ships in its place. An envelope naming it
# points every managed laptop at a registry 404, which is what
# wardyn.env.example did for three releases.
#
# Keyed on the ghcr ref, so a LOCAL build (`make agent-images` ->
# wardyn/agent-claude-code:local) that an operator deliberately points at is
# still allowed — that is the supported way to get the vendor CLI.
for f in "${ENV_EXAMPLES[@]}"; do
  if grep -qE 'ghcr\.io/[^"]*/agent-claude-code' "${f}"; then
    fail "$(basename "${f}") pins ghcr.io/.../agent-claude-code, which this project does NOT publish (release.yml's matrix ships agent-base in its place) — every device would 404. Use agent-base, or a locally-built ref."
  fi
done

echo "test-desktop-profile: m-prime invariants PASS"
