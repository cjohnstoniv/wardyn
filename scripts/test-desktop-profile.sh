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
. "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

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


# ── 7. the m-prime (member-mode) envelope's own invariants ──────────────────
# Sections 1-3 prove it PARSES and that every var it sets is documented. Those
# would pass on a file that merely mentions the right variable names. These
# check what the profile actually MEANS, which is what an MDM copies fleet-wide.
MPRIME="${DESK_DIR}/wardyn.env.m-prime.example"
[ -f "${MPRIME}" ] || fail "${MPRIME} not found — the member-mode profile docs/DESKTOP.md documents has no shipped envelope"


# (a) It is actually member mode, with SSO. Either half alone is not m'.
[ "$(env_get "${MPRIME}" WARDYN_MEMBER_MODE)" = "true" ] \
  || fail "m-prime envelope does not set WARDYN_MEMBER_MODE=true"
[ "$(env_get "${MPRIME}" WARDYN_LOCAL_MODE)" = "false" ] \
  || fail "m-prime envelope must set WARDYN_LOCAL_MODE=false — a configured issuer plus explicit local mode is refused at boot, and would silently disable the SSO RBAC that makes this profile mean anything"
[ -n "$(env_get "${MPRIME}" WARDYN_OIDC_ISSUER)" ] \
  || fail "m-prime envelope sets no WARDYN_OIDC_ISSUER — OIDC is mandatory on m', it is the only thing authenticating the member"

# (b) The member can actually mount their own project directory. Unset means
#     "members may not mount host directories at all", which turns off the one
#     power m' exists to add.
roots="$(env_get "${MPRIME}" WARDYN_MEMBER_WORKSPACE_ROOTS)"
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

# ── 7c. every envelope pins BOTH images, by DIGEST ─────────────────────────
# Presence is asserted as hard as the format. A check that only validates the
# VALUE passes vacuously on an unset variable — which is the exact defect here:
# deploy/desktop/ set WARDYN_PROXY_IMAGE nowhere, so the base compose file
# handed wardynd `wardyn/wardyn-proxy:local`, a ref no laptop can resolve, and
# EVERY run's egress sidecar was unresolvable. The stack still reached healthy
# and the console still loaded; the first RUN was the first symptom.
#
# Digest, not tag: publish-image.yml pushes wardynd:latest on every push to
# main, so a mutable tag has managed laptops tracking tip-of-main, unreleased,
# several times a day, on a 300s timer.
for f in "${ENV_EXAMPLES[@]}"; do
  n="$(basename "${f}")"
  for v in WARDYN_WARDYND_IMAGE WARDYN_PROXY_IMAGE; do
    val="$(grep -E "^${v}=" "${f}" | tail -1 | cut -d= -f2-)"
    [ -n "${val}" ] \
      || fail "${n} does not set ${v} — the launcher then falls back to a mutable tag (wardynd) or to wardyn/wardyn-proxy:local, which no laptop can pull, making every run's egress sidecar unresolvable"
    case "${val}" in
      *@sha256:*) ;;
      *) fail "${n}: ${v}='${val}' is not digest-pinned — a mutable tag means a rebuild upstream silently changes what every laptop runs, on a 300s timer" ;;
    esac
  done
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

# ── 9. the Linux/systemd lane ──────────────────────────────────────────────
# deploy/desktop/install.sh hard-refused every non-Darwin host, so the tier had
# no Linux path at all. These assert the units exist, still carry the
# placeholder install.sh's sed looks for, and — where systemd is present —
# actually parse.
UNIT="${DESK_DIR}/wardyn.service"
TIMER="${DESK_DIR}/wardyn.timer"
[ -f "${UNIT}" ]  || fail "${UNIT} not found — the Linux converge job has no unit"
[ -f "${TIMER}" ] || fail "${TIMER} not found — wardyn.service is a oneshot; without the timer nothing ever fires it"

grep -q '__WARDYN_DESKTOP_SH__' "${UNIT}" \
  || fail "${UNIT} lost its install.sh placeholder (__WARDYN_DESKTOP_SH__) — install.sh's sed would silently no-op and ExecStart would point at nothing"
# The plist's RunAtLoad + StartInterval 300 has to survive the translation, or
# the two platforms converge on different schedules.
grep -q '^OnBootSec=' "${TIMER}"       || fail "${TIMER} sets no OnBootSec — the launchd side converges at load (RunAtLoad); Linux must too"
grep -q '^OnUnitActiveSec=300' "${TIMER}" || fail "${TIMER} does not re-assert every 300s — the plist's StartInterval is 300, and the two platforms must not drift"
grep -q '^Type=oneshot' "${UNIT}"      || fail "${UNIT} is not Type=oneshot — 'wardyn-desktop.sh up' converges and exits, and a Restart= would fight the timer"
grep -q '^User=root' "${UNIT}"         || fail "${UNIT} does not run as root — it reads /etc/wardyn/age.key (0600) and must match the macOS LaunchDaemon"

# install.sh must actually branch, not just stop refusing Linux.
DESK_INSTALL="${DESK_DIR}/install.sh"
# Anchored on the ENABLE, not the string: "wardyn.timer" also appears in the
# uninstall path and in the destination path, so a bare match survives deleting
# the registration entirely.
grep -q 'systemctl enable --now wardyn.timer' "${DESK_INSTALL}" \
  || fail "${DESK_INSTALL} never ENABLES wardyn.timer — the Linux branch would install unit files that nothing ever fires"
grep -q 'chown root:root' "${DESK_INSTALL}" \
  || fail "${DESK_INSTALL} has no root:root chown for the Linux units — 'wheel' does not exist on Debian/Ubuntu and would hard-fail under set -euo pipefail"
# Same: "--uninstall" appears in the usage header and the closing hints, so
# match the DISPATCH that makes it do anything.
grep -q 'MODE=uninstall' "${DESK_INSTALL}" \
  || fail "${DESK_INSTALL} has no uninstall dispatch (grep -rn uninstall deploy/ used to return nothing at all)"
grep -q 'down --purge' "${DESK_INSTALL}" \
  || fail "${DESK_INSTALL}'s uninstall cannot purge — the age.key/volume decision has no implementation"

# ...and if this host runs systemd, the RENDERED units must parse.
if command -v systemd-analyze >/dev/null 2>&1; then
  _t="$(mktemp -d)"
  sed "s#__WARDYN_DESKTOP_SH__#${DESK_DIR}/wardyn-desktop.sh#" "${UNIT}" > "${_t}/wardyn.service"
  cp "${TIMER}" "${_t}/wardyn.timer"
  # Filter this HOST's unrelated unit warnings; only our two files' verdict counts.
  if ! systemd-analyze verify "${_t}/wardyn.service" "${_t}/wardyn.timer" 2>&1 | grep -vE 'docker\.socket|legacy directory' | grep -q .; then
    :  # no output => clean
  else
    systemd-analyze verify "${_t}/wardyn.service" "${_t}/wardyn.timer" 2>&1 | grep -vE 'docker\.socket|legacy directory' >&2
    rm -rf "${_t}"
    fail "the rendered systemd units do not verify"
  fi
  rm -rf "${_t}"
  echo "test-desktop-profile: systemd units verify"
else
  echo "test-desktop-profile: systemd-analyze absent — unit syntax not verified on this host"
fi

# ── 10. the packaging script's clean-tree guarantee ────────────────────────
# deploy/compose/.env is a real file on a maintainer's box — 0600, gitignored,
# carrying a LIVE WARDYN_AGE_KEY. If the payload is ever built by copying the
# working directory instead of exporting from git, that key ships to every
# managed laptop and every device's secret store becomes decryptable by anyone
# holding the package. docs/DESKTOP.md says age.key travels "never via MDM".
PKGR="${REPO_ROOT}/scripts/build-desktop-package.sh"
[ -x "${PKGR}" ] || fail "${PKGR} missing or not executable"
grep -q 'git archive HEAD' "${PKGR}" \
  || fail "${PKGR} no longer exports from git — a working-directory copy picks up deploy/compose/.env and its LIVE age key"
# Matched on the whole find EXPRESSION, not on "name '.env'" alone: that
# substring also appears in the script's own explanatory comment, so the
# narrower match stayed green with the real check deleted.
grep -qF -e "-name '.env' -o -name '*.env'" "${PKGR}" \
  || fail "${PKGR} lost the literal '.env' assertion — a bare *.env glob does NOT match a file named '.env', which is the only one this stops"
grep -q 'AGE-SECRET-KEY-1' "${PKGR}" \
  || fail "${PKGR} no longer scans the payload for a real age identity"
# The `deploy/` level in the payload is load-bearing: wardyn-desktop.sh computes
# REPO_ROOT as ../.. from itself, so a payload rooted at .../wardyn/desktop/
# resolves to /usr/local/lib and the wrapper dies sourcing common.sh.
# The payload carries a COMPILED Go binary, so neither package is
# arch-independent. Both were first written `Architecture: all` / `BuildArch:
# noarch`. rpmbuild refuses that outright; dpkg does NOT — and that is the worse
# failure, because an `all` .deb installs happily on arm64 and only then does the
# CLI fail to run.
grep -qF 'Architecture: ${GOARCH_PKG}' "${PKGR}" \
  || fail "${PKGR}'s .deb no longer declares a real architecture — an 'all' package ships an amd64 binary to arm64 hosts and dpkg will not stop it"
grep -qF 'BuildArch:      ${RPM_ARCH}' "${PKGR}" \
  || fail "${PKGR}'s .rpm no longer declares a real architecture"
grep -q 'PREFIX="${PAYLOAD}/usr/local/lib/wardyn"' "${PKGR}" \
  || fail "${PKGR}'s payload prefix changed — wardyn-desktop.sh's REPO_ROOT='../..' depends on the deploy/ level being present"

echo "test-desktop-profile: m-prime invariants PASS"

# ── F185: /etc/wardyn/secret.env is a trust boundary, not formatting ────────
#
# wardyn-desktop.sh dot-sources it AS ROOT on every timer tick, out of a 0755
# directory install.sh creates. `.` EXECUTES the file, so its mode and owner
# decide who chooses the code root runs — and docs/DESKTOP.md's MDM file table
# asserted 0600 with nothing enforcing it. Driven for real: each fixture's
# secret.env carries a `touch PWNED` line, so "was it sourced" is observable.
#
# Daemon-free by construction, like the rest of this file: `docker` is a PATH
# stub that answers everything with success and DOCKER_HOST points at a socket
# that does not exist, so nothing past the gate can reach a real daemon.
DESKTOP_SH="${DESK_DIR}/wardyn-desktop.sh"
[ -f "${DESKTOP_SH}" ] || fail "${DESKTOP_SH} not found"
F185_TMP="$(mktemp -d)"
trap 'rm -rf "${F185_TMP}"' EXIT
mkdir -p "${F185_TMP}/bin"
printf '#!/usr/bin/env bash\nexit 0\n' > "${F185_TMP}/bin/docker"
# curl too: the launcher's wait_healthy polls /healthz for up to 60s before it
# gives up, and this fixture has no stack. A stub that answers immediately keeps
# these three cases sub-second AND keeps the run entirely off the network.
printf '#!/usr/bin/env bash\nexit 0\n' > "${F185_TMP}/bin/curl"
chmod +x "${F185_TMP}/bin/docker" "${F185_TMP}/bin/curl"

f185_fixture() { # MODE -> managed dir
  local mode="$1" md
  md="$(mktemp -d -p "${F185_TMP}")"
  printf 'WARDYN_WARDYND_IMAGE=ghcr.io/example/wardynd@sha256:aa\n' > "${md}/wardyn.env"
  printf 'AGE-SECRET-KEY-1FAKE\n' > "${md}/age.key"
  # secret.env IS a shell script, which is what dot-sourcing means.
  printf 'WARDYN_OIDC_CLIENT_SECRET=s3cret\ntouch "%s/PWNED"\n' "${md}" > "${md}/secret.env"
  chmod "${mode}" "${md}/secret.env"
  printf '%s' "${md}"
}
f185_run() { # MANAGED_DIR -> combined output (never fatal here)
  timeout 30 env PATH="${F185_TMP}/bin:${PATH}" \
      DOCKER_HOST="unix:///nonexistent-f185.sock" \
      WARDYN_MANAGED_DIR="$1" \
      "${DESKTOP_SH}" up 2>&1 || true
}

# 1. group+world WRITABLE: anyone on the box chooses the code root runs.
md="$(f185_fixture 0666)"; out="$(f185_run "${md}")"
[ ! -e "${md}/PWNED" ] \
  || fail "wardyn-desktop.sh SOURCED a world-writable ${md}/secret.env as root — any local account can put a command in that file and have this timer run it (F185)"
printf '%s' "${out}" | grep -qi 'writable' \
  || fail "wardyn-desktop.sh refused a world-writable secret.env without saying the mode was why — the MDM operator cannot fix what is not named (F185). Got: $(printf '%s' "${out}" | tail -1)"

# 2. group/world READABLE (0644 — the mode docs/DESKTOP.md's table forbids):
#    the SIEM bearer token and OIDC client secret were readable by every local
#    account. Not a refusal (nothing has executed), but the exposure already
#    happened, so: re-assert 0600 and say it loudly enough to rotate.
md="$(f185_fixture 0644)"; out="$(f185_run "${md}")"
mode="$(stat -c '%a' "${md}/secret.env" 2>/dev/null || stat -f '%OLp' "${md}/secret.env")"
[ "${mode}" = "600" ] \
  || fail "a 0644 secret.env was left at mode ${mode} — its live credentials stay readable by every local account on the device (F185)"
printf '%s' "${out}" | grep -qi 'readable by every local account' \
  || fail "wardyn-desktop.sh silently re-permed a 0644 secret.env — the credentials in it are org-wide and already exposed, so this has to be loud enough to trigger a rotation (F185)"

# 3. the counterweight: a correct 0600 file must still be sourced, or 1 and 2
#    are satisfied by a launcher that stopped reading secrets at all.
md="$(f185_fixture 0600)"; f185_run "${md}" >/dev/null
[ -e "${md}/PWNED" ] \
  || fail "a root-owned 0600 secret.env was NOT sourced — the audit-sink bearer token and OIDC client secret never reach the stack (F185)"
echo "test-desktop-profile: secret.env trust-boundary invariants PASS"

# ── F113/F184: the enrolment image is disclosed where it is decided ─────────
#
# install.sh's default WARDYN_INSTALL_IMAGE is the CONTINUOUS :latest tag
# publish-image.yml pushes on every merge and never cosign-signs, and it runs
# AS ROOT to mint the device's age identity. The envelope pins
# WARDYN_WARDYND_IMAGE by digest, so a reader of DESKTOP.md would reasonably
# assume the whole lane is pinned. scripts/check-image-pins.sh pins the console
# warning; this pins the doc.
DESKTOP_MD="${REPO_ROOT}/docs/DESKTOP.md"
grep -q 'WARDYN_INSTALL_IMAGE' "${DESKTOP_MD}" \
  || fail "docs/DESKTOP.md never names WARDYN_INSTALL_IMAGE — the one override that pins the enrolment image is undiscoverable from the tier's own doc (F113/F184)"
grep -q 'wardynd:latest' "${DESKTOP_MD}" \
  || fail "docs/DESKTOP.md does not state that the enrolment default is the :latest tag (F113/F184)"
grep -qi 'not.*cosign-signed' "${DESKTOP_MD}" \
  || fail "docs/DESKTOP.md does not say the enrolment image is unsigned — the envelope pins a digest, so a reader assumes the whole lane is verified (F113/F184)"
grep -qF 'is a MUTABLE tag, not a digest' "${DESK_DIR}/install.sh" \
  || fail "deploy/desktop/install.sh no longer warns at the console when the enrolment ref floats — the person who needs that is the one RUNNING it, not the one reading the source (F113/F184)"
echo "test-desktop-profile: enrolment-image disclosure PASS"

echo "test-desktop-profile: self-test PASS"
