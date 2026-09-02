#!/usr/bin/env bash
# F10-install-trust-path probe — stub-driven END-TO-END execution of the root
# install.sh (no docker, no network, no root). Every external the script
# touches (docker, curl, chmod, sha256sum) is a stub on a private PATH, so the
# script's own control flow is what runs.
#
# Promoted to scripts/ (its intended destination) so it is trackable: /local/ is
# gitignored (.gitignore:64). NOT yet wired into the Makefile `test-scripts`
# target next to scripts/test-install-sh.sh (Makefile:437) — T2..T5 fail on this
# tree by design, so wiring it in would red the gate; wire it once they are fixed.
#
# Run (read-only — no docker, no network, no writes outside mktemp):
#   bash scripts/test-install-sh-trust.sh
#
# Exit 0 = every invariant held. Exit 1 = at least one FAILED. Each case is a
# separately-named invariant; the summary line at the end lists which fell.
#
# Expected TODAY (hardening-base 80538b10 / feat/v0.7-profiles fa910735):
#   T1 PASS   happy path mints a 48-hex admin token and an AGE key
#   T2 FAIL   host without `sha256sum` (macOS default) -> empty admin token   (H1)
#   T3 FAIL   second `docker run … -gen-age-key` fails -> sha256("") token    (H2)
#   T4 FAIL   .env is created world-readable before chmod 600                (H6)
#   T5 FAIL   upgrade path keeps a pre-existing EMPTY admin token            (H16)
#   T6 SKIP   tampered compose file is installed (accepted-risk unless
#             F10_EXPECT_COMPOSE_INTEGRITY=1, then FAIL)                     (H3)
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL="${ROOT}/install.sh"
[ -f "${INSTALL}" ] || { echo "install.sh not found at ${INSTALL}" >&2; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
FAILED=()
PASSED=()
SKIPPED=()

pass() { PASSED+=("$1"); echo "PASS  $1"; }
fail() { FAILED+=("$1"); echo "FAIL  $1 — $2"; }
skip() { SKIPPED+=("$1"); echo "SKIP  $1 — $2"; }

EMPTY_SHA48="$(printf '' | sha256sum | cut -c1-48)"   # e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934c

# ── stub toolchain ─────────────────────────────────────────────────────────
# make_stubs DIR MODE — MODE selects behaviors; state lives in DIR/state.
make_stubs() {
  local bin="$1" mode="$2"
  mkdir -p "${bin}" "${bin}/state"
  # docker: info / compose version / ps -a / run … -gen-age-key / compose pull / compose up
  cat > "${bin}/docker" <<'EOF'
#!/usr/bin/env bash
state="$(dirname "$0")/state"
case "$1" in
  info) exit 0 ;;
  compose)
    shift
    case "$1" in
      version) echo "Docker Compose version v2.99.0"; exit 0 ;;
      pull|up) echo "compose $*" >> "${state}/compose.log"; exit 0 ;;
      *) exit 0 ;;
    esac ;;
  ps) exit 0 ;;   # prints no container names -> no "already exists" die
  run)
    n=$(( $(cat "${state}/runs" 2>/dev/null || echo 0) + 1 )); echo "$n" > "${state}/runs"
    if [ -f "${state}/fail-second-run" ] && [ "$n" -eq 2 ]; then
      echo "docker: transient failure" >&2; exit 125
    fi
    # A real key from `wardynd -gen-age-key` is bech32 after the prefix; the
    # script only greps the prefix, so a fixed suffix per call is enough.
    echo "AGE-SECRET-KEY-1STUB${n}QPZRY9X8GF2TVDW0S3JN54KHCE6MUA7LQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L"
    exit 0 ;;
  *) exit 0 ;;
esac
EOF
  # curl: release listing, the compose file (optionally tampered), CLI asset 404.
  cat > "${bin}/curl" <<'EOF'
#!/usr/bin/env bash
state="$(dirname "$0")/state"
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
case "${url}" in
  *api.github.com/repos/*/releases?per_page=1*) echo '[{"tag_name": "v0.6.6"}]'; exit 0 ;;
  *raw.githubusercontent.com/*/deploy/compose/docker-compose.yaml)
    body='services: {wardynd: {image: "${WARDYN_WARDYND_IMAGE}"}}'
    [ -f "${state}/tamper-compose" ] && body='services: {wardynd: {image: "evil/wardynd:latest", ports: ["0.0.0.0:8080:8080"], environment: {WARDYN_LOCAL_MODE: "true"}}}'
    [ -n "${out}" ] && printf '%s\n' "${body}" > "${out}"
    exit 0 ;;
  *releases/download/*) exit 22 ;;   # CLI + SHA256SUMS: not served -> the script's skip path
  *) exit 22 ;;
esac
EOF
  # chmod: record the mode .env had BEFORE the script's own chmod 600 ran.
  cat > "${bin}/chmod" <<'EOF'
#!/usr/bin/env bash
state="$(dirname "$0")/state"
for a in "$@"; do
  case "$a" in
    *.env) [ -e "$a" ] && stat -c '%a' "$a" >> "${state}/env-mode-before-chmod" ;;
  esac
done
exec /bin/chmod "$@"
EOF
  case "${mode}" in
    no-sha256sum)
      # Emulate a host without GNU coreutils (macOS): `sha256sum` is not found.
      cat > "${bin}/sha256sum" <<'EOF'
#!/usr/bin/env bash
echo "sh: sha256sum: not found" >&2
exit 127
EOF
      ;;
  esac
  chmod +x "${bin}"/*
}

# run_install STUBDIR HOMEDIR [extra env…] — runs install.sh under the stubs.
run_install() {
  local bin="$1" home="$2"; shift 2
  env -i PATH="${bin}:/usr/bin:/bin" HOME="${home}" WARDYN_HOME="${home}/.wardyn" \
      WARDYN_VERSION=v0.6.6 "$@" sh "${INSTALL}" > "${home}/install.out" 2>&1
}

env_val() { grep -E "^$2=" "$1" | tail -1 | cut -d= -f2-; }

# ── T1: happy path ─────────────────────────────────────────────────────────
t="T1 happy path mints KEY + 48-hex admin token"
d="${WORK}/t1"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"
if run_install "${d}/bin" "${d}/home"; then
  key="$(env_val "${d}/home/.wardyn/.env" WARDYN_AGE_KEY)"
  tok="$(env_val "${d}/home/.wardyn/.env" WARDYN_ADMIN_TOKEN)"
  if [[ "${key}" == AGE-SECRET-KEY-1* ]] && [[ "${tok}" =~ ^[0-9a-f]{48}$ ]]; then pass "${t}"
  else fail "${t}" "key='${key}' token='${tok}'"; fi
else fail "${t}" "install.sh exited $? — $(tail -3 "${d}/home/install.out" | tr '\n' ' ')"; fi

# ── T2: host without sha256sum (macOS default) ─────────────────────────────
# install.sh:188-189 has a shasum fallback for the CLI checksum; install.sh:89
# (the ADMIN TOKEN) does not. sh has no pipefail, so the pipeline's status is
# `cut`'s (0) and set -e never fires: TOKEN="" lands in .env, and the compose
# file's `${WARDYN_ADMIN_TOKEN:-demo-admin-token}` (docker-compose.yaml:205)
# substitutes the PUBLISHED demo token for an empty value.
t="T2 admin token is minted on a host with no sha256sum (macOS)"
d="${WORK}/t2"; make_stubs "${d}/bin" no-sha256sum; mkdir -p "${d}/home"
if run_install "${d}/bin" "${d}/home"; then
  tok="$(env_val "${d}/home/.wardyn/.env" WARDYN_ADMIN_TOKEN)"
  if [[ "${tok}" =~ ^[0-9a-f]{48}$ ]]; then pass "${t}"
  else fail "${t}" "WARDYN_ADMIN_TOKEN='${tok}' (empty -> compose falls through to demo-admin-token)"; fi
else
  # Dying here would ALSO be acceptable (fail closed); only a silent empty token is the defect.
  pass "${t} (script refused instead: $(tail -1 "${d}/home/install.out"))"
fi

# ── T3: the second -gen-age-key run fails ──────────────────────────────────
# install.sh:88 dies when the FIRST run yields no key; install.sh:89 has no such
# guard, so a transient docker failure on the SECOND run hashes an empty
# stdin: the token becomes the first 48 hex of sha256(""), a public constant.
t="T3 admin token is never sha256(\"\") when the second docker run fails"
d="${WORK}/t3"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"; : > "${d}/bin/state/fail-second-run"
if run_install "${d}/bin" "${d}/home"; then
  tok="$(env_val "${d}/home/.wardyn/.env" WARDYN_ADMIN_TOKEN)"
  if [ "${tok}" = "${EMPTY_SHA48}" ] || [ -z "${tok}" ]; then
    fail "${t}" "token='${tok}' == sha256('')[0:48]='${EMPTY_SHA48}'"
  else pass "${t}"; fi
else pass "${t} (script refused instead)"; fi

# ── T4: .env must never exist world-readable ───────────────────────────────
# install.sh:91 `cat > .env` runs under the caller's umask (022 by default),
# then install.sh:115 chmod 600. The file holds WARDYN_AGE_KEY (the secret
# store master key) and WARDYN_ADMIN_TOKEN for that window. The upgrade path's
# `> .env.tmp && mv` (install.sh:130-131,138) has the same shape. Fix shape:
# `umask 077` before the first write (or `(umask 077; cat > .env)`).
t="T4 .env is 0600 at creation (umask 077), not only after a later chmod"
d="${WORK}/t4"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"
( umask 022; run_install "${d}/bin" "${d}/home" ) || true
before="$(head -1 "${d}/bin/state/env-mode-before-chmod" 2>/dev/null || echo '?')"
if [ "${before}" = "600" ]; then pass "${t}"
else fail "${t}" ".env mode before install.sh's own chmod was ${before}"; fi

# ── T5: upgrade path must not carry an EMPTY admin token forward ───────────
# install.sh:117-147 rewrites only the version-derived lines. A box that
# installed under T2/T3 stays on the empty/constant token across every
# upgrade, and the "Upgrading" banner says nothing.
t="T5 upgrade path refuses or re-mints an empty WARDYN_ADMIN_TOKEN"
d="${WORK}/t5"; make_stubs "${d}/bin" default; mkdir -p "${d}/home/.wardyn"
cat > "${d}/home/.wardyn/.env" <<EOF
# Generated by install.sh for Wardyn v0.6.5. Safe to edit.
WARDYN_AGE_KEY=AGE-SECRET-KEY-1STUBOLDQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7LQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L
WARDYN_ADMIN_TOKEN=
WARDYN_UP_PORT=8080
EOF
if run_install "${d}/bin" "${d}/home"; then
  tok="$(env_val "${d}/home/.wardyn/.env" WARDYN_ADMIN_TOKEN)"
  if [[ "${tok}" =~ ^[0-9a-f]{48}$ ]]; then pass "${t}"
  else fail "${t}" "after upgrade WARDYN_ADMIN_TOKEN='${tok}'"; fi
else pass "${t} (script refused instead)"; fi

# ── T6: tampered compose definition ────────────────────────────────────────
# install.sh:80 fetches deploy/compose/docker-compose.yaml from a MUTABLE tag
# ref over raw.githubusercontent with no digest/SHA256SUMS check; the file is
# not among the cosign-signed release assets (release.yml:444-452,477-478).
# Once a digest is introduced (SHA256SUMS row for docker-compose.yaml, or a
# pinned sha256 in install.sh), this case asserts the fetch fails closed.
t="T6 tampered compose file is refused (digest/SHA256SUMS mismatch)"
d="${WORK}/t6"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"; : > "${d}/bin/state/tamper-compose"
if [ "${F10_EXPECT_COMPOSE_INTEGRITY:-0}" != "1" ]; then
  skip "${t}" "ACCEPTED RISK until a digest exists (see ACCEPTED-RISK-compose-fetch.md); set F10_EXPECT_COMPOSE_INTEGRITY=1 to enforce"
elif run_install "${d}/bin" "${d}/home"; then
  fail "${t}" "install.sh accepted a compose file naming evil/wardynd:latest on 0.0.0.0 with WARDYN_LOCAL_MODE=true"
else
  if grep -qi "checksum\|digest\|mismatch" "${d}/home/install.out"; then pass "${t}"
  else fail "${t}" "install.sh died for an unrelated reason: $(tail -1 "${d}/home/install.out")"; fi
fi

echo
echo "passed=${#PASSED[@]} failed=${#FAILED[@]} skipped=${#SKIPPED[@]}"
if [ "${#FAILED[@]}" -gt 0 ]; then
  printf 'FAILED: %s\n' "${FAILED[@]}"
  exit 1
fi
exit 0
