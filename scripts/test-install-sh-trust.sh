#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# F10-install-trust-path probe — stub-driven END-TO-END execution of the root
# install.sh (no docker, no network, no root). The daemon and the network are
# stubs on a private PATH (docker, curl), so the script's own control flow is
# what runs; chmod is a RECORD-ONLY stub, so a mode the script claims to be BORN
# with cannot have been chmod'd into place behind the assertion's back.
#
# T2 goes further and replaces the PATH itself with a MAC-SHAPED one — a symlink
# farm over /usr/bin and /bin with `sha256sum` REMOVED — because a `sha256sum`
# stub that merely exits 127 is still FOUND by `command -v`, which sends
# install.sh down its die branch instead of through the `shasum -a 256` fallback
# that case exists to exercise.
#
# Wired into `make test-scripts` beside scripts/test-install-sh.sh, which is the
# text-only half of the same coverage. cmd/wardynd/install_sh_trust_guard_test.go
# pins the SHAPE of the fixes below in Go; this file proves they hold when the
# script actually runs.
#
# Run (read-only — no docker, no network, no writes outside mktemp):
#   bash scripts/test-install-sh-trust.sh
#
# Exit 0 = every invariant held. Exit 1 = at least one FAILED. Each case is a
# separately-named invariant; the summary line at the end lists which fell.
#
# Expected:
#   T1 PASS   happy path mints a 48-hex admin token and an AGE key
#   T2 PASS   mac-shaped PATH (no sha256sum) mints via `shasum -a 256`      (H1)
#   T3 PASS   second `docker run … -gen-age-key` fails -> refuses to guess   (H2)
#   T4 PASS   .env is born 0600 with no chmod at all                        (H6)
#   T5 PASS   upgrade re-mints an empty/quoted-empty/placeholder token, leaves
#             a real one alone, and refuses a keyless .env (8 rows)         (H16)
#   T6 SKIP   tampered compose file is installed — a PUBLISHED accepted risk
#             (docs/VERIFY.md; THREAT-MODEL §5 residual 32). Set
#             F10_EXPECT_COMPOSE_INTEGRITY=1 to enforce once a digest exists. (H3)
#   T7 PASS   the documented upgrade — re-run the installer over a RUNNING
#             stack — completes instead of dying on its own containers  (C01)
#   T8 PASS   …while a FRESH install colliding with a foreign stack still refuses
#   T9 PASS   the refusal's printed recipe, run verbatim, yields a SECOND
#             install: own root, own namespace, own port      (adversarial C2/C4)
#   T10 PASS  upgrade MERGES WARDYN_AGENT_IMAGES — a hand-added key survives (C02)
#   T11 PASS  the banner names the port .env pins, not this invocation's   (C04)
#   T12 PASS  upgrade backfills WARDYN_SSH_ADVERTISE from .env's port      (C03)
#   T13 PASS  a QUOTED WARDYN_AGE_KEY — a shape compose accepts — is a key  (C05)
#   T14 PASS  a refused upgrade writes nothing: .env and compose untouched (C06)
#   T15 PASS  every network fetch carries a timeout                    (perf2-C2)
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
  ps) cat "${state}/ps-names" 2>/dev/null; exit 0 ;;   # no file -> no names -> no "already exists" die
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
printf '%s\n' "$*" >> "${state}/curl.log"
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
  # chmod: RECORD-ONLY — it deliberately does not exec the real chmod. install.sh
  # must reach 0600 by creating .env under `umask 077`, so with this stub in
  # place any chmod it might make is a no-op and cannot be what T4 observes.
  cat > "${bin}/chmod" <<'EOF'
#!/usr/bin/env bash
state="$(dirname "$0")/state"
echo "chmod $*" >> "${state}/chmod.log"
exit 0
EOF
  case "${mode}" in
    no-sha256sum)
      # A mac-shaped PATH: macOS ships perl's `shasum` and no GNU coreutils, so
      # `command -v sha256sum` must FAIL. A stub that exits 127 does not emulate
      # that — command -v still finds it — so build a symlink farm over the real
      # /usr/bin and /bin and take `sha256sum` out of it. T2 runs with
      # PATH="${bin}:${macbin}", so the stubs above still win and nothing else
      # about the host changes.
      local macbin="${bin}/macbin" real_sha
      mkdir -p "${macbin}"
      ln -sf -t "${macbin}" /usr/bin/* /bin/* 2>/dev/null || true
      rm -f "${macbin}/sha256sum"
      if [ ! -e "${macbin}/shasum" ]; then
        # This host has no perl `shasum` to symlink, so shim one onto the real
        # sha256sum by ABSOLUTE path — install.sh still cannot see one on PATH.
        real_sha="$(command -v sha256sum)"
        cat > "${macbin}/shasum" <<EOF
#!/usr/bin/env bash
# \`shasum -a 256 [FILE]\` -> ${real_sha}
args=()
while [ \$# -gt 0 ]; do
  case "\$1" in
    -a) shift 2 ;;
    -a*) shift ;;
    *) args+=("\$1"); shift ;;
  esac
done
exec "${real_sha}" "\${args[@]}"
EOF
        chmod +x "${macbin}/shasum"
      fi
      ;;
  esac
  chmod +x "${bin}"/*
}

# run_install STUBDIR HOMEDIR [extra env…] — runs install.sh under the stubs.
# Set PATHX first to replace the stub PATH wholesale; T2 is the only caller that
# does, and it resets it after. Set WHOMEX=none to drop WARDYN_HOME, so the run
# lands wherever install.sh's OWN default root resolves — which is the whole
# subject of T9, where a second namespace must not reuse the first install's
# ~/.wardyn.
PATHX=""
WHOMEX=""
run_install() {
  local bin="$1" home="$2"; shift 2
  local -a envx=(PATH="${PATHX:-${bin}:/usr/bin:/bin}" HOME="${home}" WARDYN_VERSION=v0.6.6)
  [ "${WHOMEX}" = none ] || envx+=(WARDYN_HOME="${home}/.wardyn")
  env -i "${envx[@]}" "$@" sh "${INSTALL}" > "${home}/install.out" 2>&1
}

# upgrade_fixture DIR [KEY=VALUE…] — a live 0.6.5-era install at DIR/home/.wardyn:
# stubs and an .env with a real store key and a real admin token. Extra
# KEY=VALUE lines are appended to the .env verbatim. It deliberately leaves the
# docker stub reporting NO containers: a case pinning some other upgrade defect
# must reach the upgrade branch, not be satisfied by the existing-stack refusal
# T7 owns.
upgrade_fixture() {
  local d="$1"; shift
  make_stubs "${d}/bin" default
  mkdir -p "${d}/home/.wardyn"
  {
    echo "# Generated by install.sh for Wardyn v0.6.5. Safe to edit."
    echo "WARDYN_AGE_KEY=AGE-SECRET-KEY-1STUBOLDQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7LQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L"
    echo "WARDYN_ADMIN_TOKEN=a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718"
    printf '%s\n' "$@"
  } > "${d}/home/.wardyn/.env"
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
# install_cli's CLI checksum had a shasum fallback; the ADMIN TOKEN derivation
# did not. sh has no pipefail, so the pipeline's status was `cut`'s (0) and
# set -e never fired: TOKEN="" landed in .env, and the compose file's
# `${WARDYN_ADMIN_TOKEN:-demo-admin-token}` substitutes the PUBLISHED demo token
# for an empty value. Both sites now route through install.sh's sha256_hex.
#
# This case runs on the MAC-SHAPED PATH make_stubs builds: no `sha256sum` on it
# at all, so `command -v sha256sum` fails and `shasum -a 256` is the code that
# actually hashes. Refusing to install is NOT accepted here — a mac must get a
# working token, and an earlier version of this case that accepted either
# outcome passed while the fallback was never once executed.
t="T2 admin token is minted via \`shasum -a 256\` on a mac-shaped PATH (no sha256sum)"
d="${WORK}/t2"; make_stubs "${d}/bin" no-sha256sum; mkdir -p "${d}/home"
PATHX="${d}/bin:${d}/bin/macbin"
if run_install "${d}/bin" "${d}/home"; then
  tok="$(env_val "${d}/home/.wardyn/.env" WARDYN_ADMIN_TOKEN)"
  if [[ "${tok}" =~ ^[0-9a-f]{48}$ ]]; then pass "${t}"
  else fail "${t}" "WARDYN_ADMIN_TOKEN='${tok}' (empty -> compose falls through to demo-admin-token)"; fi
else
  fail "${t}" "install.sh exited $? and minted nothing — $(tail -2 "${d}/home/install.out" | tr '\n' ' ')"
fi
PATHX=""

# ── T3: the second -gen-age-key run fails ──────────────────────────────────
# install.sh dies when the FIRST `-gen-age-key` run yields no key; the token
# derivation had no such guard, so a transient docker failure on the SECOND run
# hashed an empty stdin and the token became the first 48 hex of sha256("") — a
# public constant. mint_admin_token now checks the mint before hashing it.
t="T3 admin token is never sha256(\"\") when the second docker run fails"
d="${WORK}/t3"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"; : > "${d}/bin/state/fail-second-run"
if run_install "${d}/bin" "${d}/home"; then
  tok="$(env_val "${d}/home/.wardyn/.env" WARDYN_ADMIN_TOKEN)"
  if [ "${tok}" = "${EMPTY_SHA48}" ] || [ -z "${tok}" ]; then
    fail "${t}" "token='${tok}' == sha256('')[0:48]='${EMPTY_SHA48}'"
  else pass "${t}"; fi
else pass "${t} (script refused instead)"; fi

# ── T4: .env must never exist world-readable ───────────────────────────────
# `cat > .env` ran under the caller's umask (022 by default) and was chmod 600
# only afterwards. The file holds WARDYN_AGE_KEY (the secret-store master key)
# and WARDYN_ADMIN_TOKEN for that window, and the upgrade path's env_set
# `> .env.tmp && mv` had the same shape. install.sh now sets `umask 077` around
# the whole .env block and restores OLD_UMASK after it.
#
# The whole run happens under `umask 022` with the RECORD-ONLY chmod stub, so
# nothing but install.sh's own umask can produce a 0600 file — which is why
# install.sh needs no `chmod 600 .env` and no longer carries one. The stub's log
# is read only to name what it swallowed when this fails.
t="T4 .env is born 0600 under umask 022, with every chmod stubbed out"
d="${WORK}/t4"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"
( umask 022; run_install "${d}/bin" "${d}/home" ) || true
mode="$(stat -c '%a' "${d}/home/.wardyn/.env" 2>/dev/null || echo '?')"
if [ "${mode}" = "600" ]; then pass "${t}"
else fail "${t}" ".env is ${mode} after the run; chmod calls the stub swallowed: $(tr '\n' ';' < "${d}/bin/state/chmod.log" 2>/dev/null || echo none)"; fi

# ── T5: upgrade path must not carry a non-credential admin token forward ───
# The upgrade branch rewrites only the version-derived lines, so a box that
# installed under T2/T3 stayed on the empty token across every upgrade and the
# "Upgrading" banner said nothing. It now re-mints and says so — and it decides
# by READING THE VALUE: the `grep -qE '^WARDYN_ADMIN_TOKEN=.'` predicate this
# replaced only asked whether SOME character followed the `=`, so `""`, `''`, a
# lone space and both placeholders an operator is likely to have pasted in all
# counted as credentials. A real token must survive untouched, so that row runs
# here too — a re-mint predicate that fires on everything is not a fix.
#
# Each row also asserts the mode: the fixture is a pre-`umask 077` install's
# 0644 .env, and install.sh has no chmod left, so 600 can only come from the
# `.env.tmp` + `mv` rewrite being born under the umask.
t5_case() { # LABEL EXISTING-VALUE EXPECT(remint|keep)
  local label="$1" existing="$2" expect="$3" t d tok mode
  t="T5/${label} upgrade re-mints WARDYN_ADMIN_TOKEN=${existing}"
  [ "${expect}" = keep ] && t="T5/${label} upgrade leaves a real WARDYN_ADMIN_TOKEN alone"
  d="${WORK}/t5-${label}"
  make_stubs "${d}/bin" default; mkdir -p "${d}/home/.wardyn"
  cat > "${d}/home/.wardyn/.env" <<EOF
# Generated by install.sh for Wardyn v0.6.5. Safe to edit.
WARDYN_AGE_KEY=AGE-SECRET-KEY-1STUBOLDQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7LQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L
WARDYN_ADMIN_TOKEN=${existing}
WARDYN_UP_PORT=8080
EOF
  /bin/chmod 644 "${d}/home/.wardyn/.env"
  if ! run_install "${d}/bin" "${d}/home"; then
    fail "${t}" "install.sh exited non-zero — $(tail -2 "${d}/home/install.out" | tr '\n' ' ')"; return
  fi
  tok="$(env_val "${d}/home/.wardyn/.env" WARDYN_ADMIN_TOKEN)"
  mode="$(stat -c '%a' "${d}/home/.wardyn/.env" 2>/dev/null || echo '?')"
  if [ "${expect}" = keep ]; then
    [ "${tok}" = "${existing}" ] || { fail "${t}" "re-minted a real token: '${existing}' -> '${tok}'"; return; }
  elif ! [[ "${tok}" =~ ^[0-9a-f]{48}$ ]]; then
    fail "${t}" "left WARDYN_ADMIN_TOKEN='${tok}' (was '${existing}') — compose substitutes its published demo token for an empty value"; return
  fi
  [ "${mode}" = "600" ] || { fail "${t}" "the rewritten .env is ${mode}, not 600"; return; }
  pass "${t}"
}
t5_case empty    ''                 remint
t5_case dquoted  '""'               remint
t5_case squoted  "''"               remint
t5_case spaced   ' '                remint
t5_case demo     'demo-admin-token' remint
t5_case changeme 'change-me'        remint
t5_case real     'a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718' keep

# The upgrade path also refuses an .env whose store key is gone: minting a fresh
# WARDYN_AGE_KEY there would orphan every secret already written under the old
# one, so this is the condition install.sh stops on rather than repairs.
t="T5/nokey upgrade refuses an .env with no WARDYN_AGE_KEY"
d="${WORK}/t5-nokey"; make_stubs "${d}/bin" default; mkdir -p "${d}/home/.wardyn"
cat > "${d}/home/.wardyn/.env" <<EOF
# Generated by install.sh for Wardyn v0.6.5. Safe to edit.
WARDYN_ADMIN_TOKEN=a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718
WARDYN_UP_PORT=8080
EOF
if run_install "${d}/bin" "${d}/home"; then
  fail "${t}" "install.sh completed against an .env with no WARDYN_AGE_KEY line"
elif grep -q 'WARDYN_AGE_KEY' "${d}/home/install.out"; then pass "${t}"
else fail "${t}" "died for an unrelated reason: $(tail -1 "${d}/home/install.out")"; fi

# ── T6: tampered compose definition ────────────────────────────────────────
# install.sh fetches deploy/compose/docker-compose.yaml from a MUTABLE tag ref
# over raw.githubusercontent with no digest/SHA256SUMS check; the file is not
# among the cosign-signed release assets (release.yml's checksums +
# cosign-sign-blob steps). Accepted and published for 0.7 — docs/VERIFY.md and
# THREAT-MODEL §5 residual 32. Once a digest is introduced (a SHA256SUMS row for
# docker-compose.yaml, or a pinned sha256 in install.sh), this case asserts the
# fetch fails closed.
t="T6 tampered compose file is refused (digest/SHA256SUMS mismatch)"
d="${WORK}/t6"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"; : > "${d}/bin/state/tamper-compose"
if [ "${F10_EXPECT_COMPOSE_INTEGRITY:-0}" != "1" ]; then
  skip "${t}" "ACCEPTED RISK until a digest exists (docs/VERIFY.md; THREAT-MODEL §5 residual 32); set F10_EXPECT_COMPOSE_INTEGRITY=1 to enforce"
elif run_install "${d}/bin" "${d}/home"; then
  fail "${t}" "install.sh accepted a compose file naming evil/wardynd:latest on 0.0.0.0 with WARDYN_LOCAL_MODE=true"
else
  if grep -qi "checksum\|digest\|mismatch" "${d}/home/install.out"; then pass "${t}"
  else fail "${t}" "install.sh died for an unrelated reason: $(tail -1 "${d}/home/install.out")"; fi
fi

# ── T7: the upgrade the closing banner documents ───────────────────────────
# install.sh:328 tells the operator "Upgrade: re-run this installer at the new
# version". Every box that has ever run it has ${NS}-api on its daemon, and the
# existing-stack pre-flight fired on OUR OWN containers before the upgrade
# branch could be reached — so the documented procedure exited 1 with a message
# about a name collision, on every live install. T5's rows never saw it: their
# docker stub's `ps` printed nothing, which is the one state an upgrade is never
# in. The guard is now gated on .env NOT existing.
t="T7 the documented upgrade completes with the stack's own containers running"
d="${WORK}/t7"; upgrade_fixture "${d}" 'WARDYN_UP_PORT=8080'
printf 'wardyn-api\nwardyn-postgres\n' > "${d}/bin/state/ps-names"   # the state a real upgrade is in
if run_install "${d}/bin" "${d}/home"; then
  img="$(env_val "${d}/home/.wardyn/.env" WARDYN_WARDYND_IMAGE)"
  if [ "${img}" = "ghcr.io/cjohnstoniv/wardynd:0.6.6" ]; then pass "${t}"
  else fail "${t}" "the upgrade ran but left WARDYN_WARDYND_IMAGE='${img}'"; fi
else
  fail "${t}" "install.sh exited $? on the procedure it prints — $(tail -4 "${d}/home/install.out" | tr '\n' ' ')"
fi

# ── T8: …and the collision it was written for still refuses ────────────────
# The counterweight to T7: gating the pre-flight on .env must not delete it. A
# FRESH install (no .env of ours anywhere) that finds a `wardyn-api` — a clone's
# stack, or another operator's — still stops, because compose would otherwise
# adopt containers this install does not own.
t="T8 a fresh install still refuses to collide with a stack it does not own"
d="${WORK}/t8"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"
printf 'wardyn-api\n' > "${d}/bin/state/ps-names"
if run_install "${d}/bin" "${d}/home"; then
  fail "${t}" "install.sh installed straight over a foreign 'wardyn' stack"
elif grep -q "already exists" "${d}/home/install.out"; then pass "${t}"
else fail "${t}" "died for an unrelated reason: $(tail -1 "${d}/home/install.out")"; fi

# ── T9: the refusal's own remedy, executed ─────────────────────────────────
# The message printed `WARDYN_NS=… curl … | sh`: a command-prefix assignment
# applies to `curl`, NOT to the piped `sh` (/bin/sh is dash on most Linux), so
# pasting it verbatim re-ran the installer with a DEFAULT environment and
# reproduced the identical error. And even exported, it named no WARDYN_HOME —
# so the "second install" landed in the FIRST one's ~/.wardyn and merely
# redeployed it, sharing its age key, its admin token and its ports.
#
# This case does not read the message, it RUNS it: the only substitution is
# `curl -fsSL <this-url>` -> `cat install.sh`, which keeps the pipe and the
# assignments exactly where the message puts them.
t="T9 the printed 'install alongside it' recipe really produces a second install"
d="${WORK}/t9"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"
printf 'wardyn-api\n' > "${d}/bin/state/ps-names"
WHOMEX=none; run_install "${d}/bin" "${d}/home"; WHOMEX=""
recipe="$(sed -n '/under a different name and free ports:/,/sh$/p' "${d}/home/install.out" | sed '1d')"
recipe="${recipe/curl -fsSL <this-url>/cat ${INSTALL}}"
if [ -z "${recipe}" ] || [[ "${recipe}" == *"<this-url>"* ]]; then
  fail "${t}" "no runnable recipe in the refusal: $(tr '\n' ' ' <<< "${recipe}")"
else
  env -i PATH="${d}/bin:/usr/bin:/bin" HOME="${d}/home" WARDYN_VERSION=v0.6.6 \
      bash -c "${recipe}" > "${d}/home/recipe.out" 2>&1
  second="${d}/home/.wardyn2/.env"
  if [ ! -f "${second}" ]; then
    fail "${t}" "the recipe created no ~/.wardyn2 install — $(tail -2 "${d}/home/recipe.out" | tr '\n' ' ')"
  elif [ -e "${d}/home/.wardyn/.env" ]; then
    fail "${t}" "the recipe wrote into the FIRST install's ~/.wardyn as well"
  else
    ns="$(env_val "${second}" WARDYN_NS)"; port="$(env_val "${second}" WARDYN_UP_PORT)"
    if [ "${ns}" = wardyn2 ] && [ "${port}" = 8090 ]; then pass "${t}"
    else fail "${t}" "second install pinned WARDYN_NS='${ns}' WARDYN_UP_PORT='${port}'"; fi
  fi
fi

# ── T10: WARDYN_AGENT_IMAGES is a merge, not a rewrite ─────────────────────
# docs/UI-SANDBOXES.md tells operators to add their own images to this map. The
# upgrade branch env_set it wholesale from the three names this project
# publishes, so every hand-added entry vanished on the next upgrade — silently,
# under a comment promising "any hand edits — untouched". Rows still pointing at
# ghcr.io/<owner>/ are bumped; everything else survives; a published row the
# file never had (0.6.x seeded no claude-code) is added.
t="T10 upgrade merges WARDYN_AGENT_IMAGES and keeps a hand-added entry"
d="${WORK}/t10"; upgrade_fixture "${d}" 'WARDYN_UP_PORT=8080' \
  'WARDYN_AGENT_IMAGES={"codex-cli":"ghcr.io/cjohnstoniv/agent-codex-cli:0.6.5","aws-sso":"ghcr.io/cjohnstoniv/agent-aws-sso:0.6.5","vscode":"registry.corp.example/wardyn/agent-vscode:2.1"}'
if run_install "${d}/bin" "${d}/home"; then
  v="$(env_val "${d}/home/.wardyn/.env" WARDYN_AGENT_IMAGES)"
  if [[ "${v}" != *'"vscode":"registry.corp.example/wardyn/agent-vscode:2.1"'* ]]; then
    fail "${t}" "the operator's vscode image was deleted: ${v}"
  elif [[ "${v}" != *'"codex-cli":"ghcr.io/cjohnstoniv/agent-codex-cli:0.6.6"'* ]]; then
    fail "${t}" "codex-cli was not bumped to the new release: ${v}"
  elif [[ "${v}" != *'"claude-code":"ghcr.io/cjohnstoniv/agent-base:0.6.6"'* ]]; then
    fail "${t}" "claude-code — absent from 0.6.x .env files — was not added: ${v}"
  else pass "${t}"; fi
else fail "${t}" "install.sh exited $? — $(tail -2 "${d}/home/install.out" | tr '\n' ' ')"; fi

# ── T11: the banner describes the install, not the invocation ──────────────
# The upgrade branch leaves the ports in .env untouched BY DESIGN, and the
# banner printed ${WARDYN_PORT:-8080} — the process environment — so the
# documented upgrade of an 8090 install ended by sending the operator to
# http://127.0.0.1:8080, where nothing listens.
t="T11 the closing banner names the port .env pins, not this invocation's"
d="${WORK}/t11"; upgrade_fixture "${d}" 'WARDYN_UP_PORT=8090' 'WARDYN_SSH_PORT=2223'
if run_install "${d}/bin" "${d}/home"; then
  if grep -q 'Wardyn is running: http://127.0.0.1:8090' "${d}/home/install.out"; then pass "${t}"
  else fail "${t}" "banner said: $(grep 'Wardyn is running' "${d}/home/install.out")  (.env pins 8090)"; fi
else fail "${t}" "install.sh exited $? — $(tail -2 "${d}/home/install.out" | tr '\n' ' ')"; fi

# ── T12: the listener backfill reads .env too ──────────────────────────────
# WARDYN_SSH_ADVERTISE is what the console's attach pane and /healthz publish.
# Backfilling it from ${WARDYN_SSH_PORT:-2222} — the process environment — on a
# bare upgrade re-run advertised 2222 for a stack compose publishes on 2223,
# i.e. exactly the install the refusal message's own recipe tells you to make.
t="T12 upgrade backfills WARDYN_SSH_ADVERTISE from .env's WARDYN_SSH_PORT"
d="${WORK}/t12"; upgrade_fixture "${d}" 'WARDYN_UP_PORT=8080' 'WARDYN_SSH_PORT=2223'
if run_install "${d}/bin" "${d}/home"; then
  adv="$(env_val "${d}/home/.wardyn/.env" WARDYN_SSH_ADVERTISE)"
  if [ "${adv}" = "127.0.0.1:2223" ]; then pass "${t}"
  else fail "${t}" "WARDYN_SSH_ADVERTISE='${adv}' while .env publishes WARDYN_SSH_PORT=2223"; fi
else fail "${t}" "install.sh exited $? — $(tail -2 "${d}/home/install.out" | tr '\n' ' ')"; fi

# ── T13: a quoted value is a value ─────────────────────────────────────────
# `WARDYN_AGE_KEY="AGE-SECRET-KEY-…"` is a shape docker compose accepts — it
# strips the quotes and the stack runs. The upgrade's refusal tested the RAW
# line, so it called that key ABSENT and stopped a healthy install with a false
# "has no WARDYN_AGE_KEY" claim. Both readers now share install.sh's env_get.
t="T13 a quoted WARDYN_AGE_KEY is read as present, not missing"
d="${WORK}/t13"; make_stubs "${d}/bin" default; mkdir -p "${d}/home/.wardyn"
cat > "${d}/home/.wardyn/.env" <<'EOF'
# Generated by install.sh for Wardyn v0.6.5. Safe to edit.
WARDYN_AGE_KEY="AGE-SECRET-KEY-1STUBOLDQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7LQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L"
WARDYN_ADMIN_TOKEN="a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718"
WARDYN_UP_PORT=8080
EOF
if run_install "${d}/bin" "${d}/home"; then
  if grep -q 'WARDYN_AGE_KEY' "${d}/home/.wardyn/.env"; then pass "${t}"
  else fail "${t}" "the upgrade ran but the store key is gone from .env"; fi
else fail "${t}" "refused a quoted key: $(grep -m1 error "${d}/home/install.out")"; fi

# ── T14: a refusal must leave the box as it found it ───────────────────────
# The keyless-.env refusal ran AFTER the compose file had been overwritten at
# the new tag and after .env's header and image pins had been rewritten, so the
# operator was told to restore a key while the install had already been half
# moved to a version it could not start. Refusals now precede the first write.
t="T14 a refused upgrade writes nothing — .env and the compose file are untouched"
d="${WORK}/t14"; make_stubs "${d}/bin" default; mkdir -p "${d}/home/.wardyn"
cat > "${d}/home/.wardyn/.env" <<'EOF'
# Generated by install.sh for Wardyn v0.6.5. Safe to edit.
WARDYN_ADMIN_TOKEN=a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718
WARDYN_UP_PORT=8080
EOF
printf 'services: {wardynd: {image: "the v0.6.5 compose file"}}\n' > "${d}/home/.wardyn/docker-compose.yaml"
cp "${d}/home/.wardyn/.env" "${d}/env.before"
if run_install "${d}/bin" "${d}/home"; then
  fail "${t}" "install.sh completed against an .env with no WARDYN_AGE_KEY line"
elif ! cmp -s "${d}/home/.wardyn/.env" "${d}/env.before"; then
  fail "${t}" "the refusal still rewrote .env: $(diff "${d}/env.before" "${d}/home/.wardyn/.env" | tr '\n' ' ')"
elif ! grep -q 'the v0.6.5 compose file' "${d}/home/.wardyn/docker-compose.yaml"; then
  fail "${t}" "the refusal still replaced docker-compose.yaml with the new tag's"
else pass "${t}"; fi

# ── T15: no fetch may wait forever ─────────────────────────────────────────
# curl's default is to wait indefinitely, and all four fetches ran with 2>/dev/null:
# a peer that completes the handshake and then answers nothing hangs
# `curl … | sh` with no output and no exit, which is indistinguishable from a
# machine that has stopped. The stub records every invocation, so this asserts
# the options the script ACTUALLY passed, not the ones its text mentions.
t="T15 every network fetch is bounded by a timeout"
d="${WORK}/t15"; make_stubs "${d}/bin" default; mkdir -p "${d}/home"
run_install "${d}/bin" "${d}/home"
log="${d}/bin/state/curl.log"
if [ ! -s "${log}" ]; then fail "${t}" "no curl invocation was recorded at all"
else
  unbounded="$(grep -v -- '--max-time' "${log}" | head -3)"
  if [ -z "${unbounded}" ]; then pass "${t}"
  else fail "${t}" "$(wc -l < "${log}") fetches, unbounded: $(tr '\n' ';' <<< "${unbounded}")"; fi
fi

echo
echo "passed=${#PASSED[@]} failed=${#FAILED[@]} skipped=${#SKIPPED[@]}"
if [ "${#FAILED[@]}" -gt 0 ]; then
  printf 'FAILED: %s\n' "${FAILED[@]}"
  exit 1
fi
exit 0
