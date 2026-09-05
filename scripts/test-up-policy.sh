#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# No-docker, no-network regression coverage for the pure host-side decisions
# scripts/up.sh's cmd_up delegates to: resolve_default_policy (the
# WARDYN_DEFAULT_POLICY auto-pick / re-pick / WARDYN_DEFAULT_POLICY_AUTO
# marker logic) and wardyn_cli_prefix (the CLI hand-off prefix). Lives in its
# own file rather than an embedded `up.sh self-test` because scripts/up.sh is
# on check-file-size.sh's frozen allowlist — see CONTRIBUTING.md "Large files".
#
# EXTRACTS the functions under test straight from scripts/up.sh (sed between
# `funcname() {` and the matching top-level `}`) rather than sourcing the
# whole file, which would execute its dispatch tail. This is a live test
# against the real source, not a copy: a change to either function's behavior
# in up.sh is picked up here with no edit needed.
#
# Usage: scripts/test-up-policy.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UP_SH="${REPO_ROOT}/scripts/up.sh"

# shellcheck source=lib/common.sh
. "${REPO_ROOT}/scripts/lib/common.sh"  # env_get / env_set / log / warn / die

extract_func() {  # $1=function name -> its body, verbatim
  sed -n "/^$1() {/,/^}/p" "${UP_SH}"
}
for fn in pick_policy host_llm_key_present llm_ready_from_status llm_ready_from_probe resolve_default_policy wardyn_cli_prefix ensure_age_key_or_die refuse_local_mode_with_oidc; do
  body="$(extract_func "$fn")"
  [ -n "$body" ] || { echo "test-up-policy: '$fn' not found in ${UP_SH} (renamed/removed?)" >&2; exit 1; }
  eval "$body"
done

fail() { echo "test-up-policy: FAIL: $*" >&2; exit 1; }

dir="$(mktemp -d)"; trap 'rm -rf "$dir"' EXIT
env_file="${dir}/.env"
: > "${env_file}"

# 1) First run, no model signal: demo.json, marked auto.
got="$(resolve_default_policy "${env_file}" "" '{}' "")"
[ "$got" = "/examples/policies/demo.json" ] || fail "first pick got '$got', want demo.json"
[ "$(env_get "${env_file}" WARDYN_DEFAULT_POLICY_AUTO)" = "1" ] || fail "first pick did not mark WARDYN_DEFAULT_POLICY_AUTO=1"

# 2) Re-run, still no model signal: idempotent, stays marked auto.
got="$(resolve_default_policy "${env_file}" "" '{}' "")"
[ "$got" = "/examples/policies/demo.json" ] || fail "idempotent re-pick got '$got'"

# 3) THE regression: a model path shows up later (managed subscription
# connected, or an API key exported) — with NO explicit WARDYN_DEFAULT_POLICY
# override, .env's auto-picked value must upgrade to claude-llm.json. Before
# this fix, "only decide when .env has nothing" left this wedged on demo.json.
got="$(resolve_default_policy "${env_file}" "" '{}' "1")"
[ "$got" = "/examples/policies/claude-llm.json" ] || fail "re-pick after a model signal got '$got', want claude-llm.json"
[ "$(env_get "${env_file}" WARDYN_DEFAULT_POLICY)" = "/examples/policies/claude-llm.json" ] || fail "claude-llm.json pick did not persist to .env"

# 3b) W1-S1-3 regression: llm_ready_from_status is the signal cmd_up uses when
# host_llm_key_present sees NOTHING in this process's env — a subscription
# connected in a PRIOR `up` (no token re-supplied this run) or a key added
# through the UI. Neither ever exports *_API_KEY, so host_llm_key_present alone
# must still say "no signal" here...
[ -z "$(host_llm_key_present "${env_file}")" ] || fail "host_llm_key_present found a signal from an empty env — test setup is wrong"
# ...while llm_ready_from_status reads it straight off a canned /setup/status
# body (compact encoding/json.Marshal shape, no space after the colon).
got="$(llm_ready_from_status '{"ready":true,"llm_ready":true,"providers":[]}')"
[ "$got" = "1" ] || fail "llm_ready_from_status missed a true body, got '$got'"
got="$(llm_ready_from_status '{"ready":true,"llm_ready":false,"providers":[]}')"
[ -z "$got" ] || fail "llm_ready_from_status false-positived on llm_ready:false, got '$got'"
got="$(llm_ready_from_status '')"
[ -z "$got" ] || fail "llm_ready_from_status false-positived on an empty/unreachable-daemon body, got '$got'"
# ...and feeding THAT into resolve_default_policy — exactly what cmd_up does
# with the daemon's live answer once wardynd is confirmed healthy — is what
# actually upgrades the ceiling on a plain re-run with no token in this
# process's env at all.
got="$(resolve_default_policy "${env_file}2" "" '{}' "$(llm_ready_from_status '{"llm_ready":true}')")"
[ "$got" = "/examples/policies/claude-llm.json" ] || fail "resolve_default_policy fed by llm_ready_from_status got '$got', want claude-llm.json"

# 3c) THE regression this closes: cmd_up's post-boot probe used to hand
# llm_ready_from_status the curl body alone, with no status-code check at
# all — so an SSO 401 body (or any other non-200) was read exactly like an
# unreachable daemon: "no signal", silently. llm_ready_from_probe folds the
# status check IN, on curl's "\n%{http_code}"-suffixed STATUS_RAW encoding
# (the -w '\n%{http_code}' cmd_up's probe now uses), so a 200 body still
# drives the re-pick...
got="$(llm_ready_from_probe '{"ready":true,"llm_ready":true,"providers":[]}
200')"
[ "$got" = "1" ] || fail "llm_ready_from_probe missed a true body behind a 200, got '$got'"
# ...but a non-200 (the SSO 401 case: humanOrAdminAuth rejects the
# unauthenticated in-network curl with a JSON error body that could
# coincidentally still be well-formed) must NOT be read as "not ready" —
# it must be read as "couldn't ask", same treatment either way, but the
# call site now WARNS on this branch instead of staying silent.
got="$(llm_ready_from_probe '{"error":"unauthorized"}
401')"
[ -z "$got" ] || fail "llm_ready_from_probe false-positived (or negatived past a warn) on a 401, got '$got'"
# A 000 (curl couldn't even reach the daemon — the pre-existing failure mode)
# stays "no signal", same as before this fix.
got="$(llm_ready_from_probe '
000')"
[ -z "$got" ] || fail "llm_ready_from_probe false-positived on an unreachable-daemon 000, got '$got'"

# 3d) MODE regression (adversarial-1:C1): deploy/compose/.env holds
# WARDYN_AGE_KEY (the secret-store master key) and WARDYN_ADMIN_TOKEN, and
# up.sh chmods it 600 — but its post-boot policy re-pick runs env_set AFTER the
# last chmod, and env_set's awk-to-tmp-then-mv used to create the tmp under the
# caller's umask. The mv then published the file 0644 inside a 0755 deploy/
# compose/ tree. Exercise the PRODUCT's own path (resolve_default_policy, which
# is what the re-pick calls) rather than env_set directly, so a future re-pick
# that writes the file some other way is still covered.
mode_env="${dir}/.env-mode"
printf 'WARDYN_AGE_KEY=AGE-SECRET-KEY-1TESTONLY\nWARDYN_ADMIN_TOKEN=deadbeef\nWARDYN_DEFAULT_POLICY=/examples/policies/demo.json\nWARDYN_DEFAULT_POLICY_AUTO=1\n' > "${mode_env}"
chmod 600 "${mode_env}"
resolve_default_policy "${mode_env}" "" '{}' "1" >/dev/null
got="$(stat -c '%a' "${mode_env}")"
[ "$got" = "600" ] || fail "the post-boot policy re-pick left ${mode_env} mode ${got}, want 600 — env_set dropped the file mode and published WARDYN_AGE_KEY + WARDYN_ADMIN_TOKEN world-readable"
# ...and the append path (a key not yet in the file) must not regress either.
resolve_default_policy "${mode_env}" "/my/other.json" '{}' "" >/dev/null
got="$(stat -c '%a' "${mode_env}")"
[ "$got" = "600" ] || fail "env_set's append path left ${mode_env} mode ${got}, want 600"
grep -q '^WARDYN_AGE_KEY=AGE-SECRET-KEY-1TESTONLY$' "${mode_env}" || fail "env_set lost unrelated lines while preserving the mode"

# 4) An explicit operator override wins outright and clears the auto marker.
got="$(resolve_default_policy "${env_file}" "/my/custom.json" '{}' "1")"
[ "$got" = "/my/custom.json" ] || fail "explicit override not honored, got '$got'"
[ "$(env_get "${env_file}" WARDYN_DEFAULT_POLICY_AUTO)" = "0" ] || fail "explicit override did not clear the auto marker"

# 5) A later plain run (no override) must leave the operator's value alone,
# even with a model signal present — the marker says it's no longer ours.
got="$(resolve_default_policy "${env_file}" "" '{}' "1")"
[ "$got" = "/my/custom.json" ] || fail "auto-pick clobbered an operator override, got '$got'"

# 5a) F150 — a FAILED age-key mint refuses the stack instead of shrugging.
# cmd_up used to warn twice ("this wardynd build may predate the flag";
# "Continuing with an ephemeral key — fine for now, but secrets won't survive a
# container restart") and boot anyway. On this compose topology — persistent
# postgres volume, restart: unless-stopped — an ephemeral key does not lose
# secrets on restart, it makes wardynd UNBOOTABLE on its second start
# (loadOrCreateSecret fails closed rather than overwrite), with the rows written
# under the first boot's identity unrecoverable. install.sh dies on this and the
# Helm chart fails the render on it; up.sh was the front door that did not.
#
# The `docker` shell function below is the whole fixture: no daemon is reachable
# from this test and none is needed. A function beats PATH lookup, so the
# extracted ensure_age_key_or_die calls THIS.
MINT_MODE=ok
MINT_CALLS=0
docker() {  # only `run … -gen-age-key` is ever reached from this function
  MINT_CALLS=$((MINT_CALLS + 1))
  case "${MINT_MODE}" in
    ok)     echo "AGE-SECRET-KEY-1TESTONLYQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L" ;;
    silent) : ;;                                            # exit 0, prints nothing
    boom)   echo "Unable to find image 'wardyn/wardynd:local' locally" >&2
            echo "docker: Error response from daemon: pull access denied." >&2
            return 125 ;;
  esac
}

age_env="${dir}/.env-age"

# Already persisted: no mint at all, and the existing key is left alone.
printf 'WARDYN_AGE_KEY=AGE-SECRET-KEY-1ALREADYHERE\n' > "${age_env}"
MINT_CALLS=0
ensure_age_key_or_die "${age_env}" >/dev/null 2>&1 \
  || fail "ensure_age_key_or_die refused an .env that already carries a persisted key"
[ "${MINT_CALLS}" = 0 ] || fail "ensure_age_key_or_die re-minted over an existing WARDYN_AGE_KEY (${MINT_CALLS} calls)"
grep -q '^WARDYN_AGE_KEY=AGE-SECRET-KEY-1ALREADYHERE$' "${age_env}" || fail "ensure_age_key_or_die overwrote an existing key"

# A working mint persists the key it got.
: > "${age_env}"
MINT_MODE=ok
ensure_age_key_or_die "${age_env}" >/dev/null 2>&1 \
  || fail "ensure_age_key_or_die refused a successful mint"
[ "$(env_get "${age_env}" WARDYN_AGE_KEY)" = "AGE-SECRET-KEY-1TESTONLYQPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L" ] \
  || fail "a successful mint was not persisted to .env"

# THE regression: a mint that yields no key must stop the stack, not warn it on.
for mode in silent boom; do
  : > "${age_env}"
  MINT_MODE="${mode}"
  out="$( ( ensure_age_key_or_die "${age_env}" ) 2>&1 )" && st=0 || st=$?
  [ "${st}" != 0 ] || fail "a failed age-key mint (${mode}) returned SUCCESS — up.sh would boot the stack with an ephemeral key: ${out}"
  case "${out}" in
    *"fine for now"*) fail "the failed-mint message (${mode}) still reassures with 'fine for now': ${out}" ;;
  esac
  case "${out}" in
    *"crash-loops on its SECOND start"*) ;;
    *) fail "the failed-mint message (${mode}) does not state the real consequence (an unbootable daemon, not lost secrets): ${out}" ;;
  esac
  case "${out}" in
    *"WARDYN_AGE_KEY=AGE-SECRET-KEY-"*) ;;
    *) fail "the failed-mint message (${mode}) names no way out: ${out}" ;;
  esac
  [ -z "$(env_get "${age_env}" WARDYN_AGE_KEY)" ] || fail "a failed mint wrote a WARDYN_AGE_KEY anyway (${mode})"
done
# ...and the daemon's own error text survives into the refusal, so "no such
# image" is distinguishable from "the daemon went away". 2>/dev/null used to
# discard it and the warning asserted ONE cause ("may predate the flag").
MINT_MODE=boom
: > "${age_env}"
out="$( ( ensure_age_key_or_die "${age_env}" ) 2>&1 )" || true
case "${out}" in
  *"pull access denied"*) ;;
  *) fail "the refusal swallowed the mint's own error, so the operator cannot tell WHY it failed: ${out}" ;;
esac
unset -f docker

# 5b) F012 — WARDYN_LOCAL_MODE=true alongside a configured WARDYN_OIDC_ISSUER is
# the one pair resolveLocalMode (cmd/wardynd/boot_flags.go) REFUSES to boot on.
# up.sh used to warn that local mode "bypasses SSO entirely (no login required)"
# — the opposite of what happens — and then spend a full build+boot cycle to die
# on a generic "wardynd did not become healthy".
lm_env="${dir}/.env-lm"

printf 'WARDYN_LOCAL_MODE=true\nWARDYN_OIDC_ISSUER=https://idp.example.com\n' > "${lm_env}"
out="$( ( refuse_local_mode_with_oidc "${lm_env}" ) 2>&1 )" && st=0 || st=$?
[ "${st}" != 0 ] || fail "up.sh proceeds on WARDYN_LOCAL_MODE=true + WARDYN_OIDC_ISSUER — wardynd refuses to boot on exactly that pair"
case "${out}" in
  *"bypasses SSO entirely"*) fail "the refusal still repeats the stale 'bypasses SSO entirely' model: ${out}" ;;
esac
case "${out}" in
  *WARDYN_LOCAL_MODE=false*) ;;
  *) fail "the refusal does not name the keep-SSO remedy: ${out}" ;;
esac
case "${out}" in
  *WARDYN_OIDC_ISSUER*) ;;
  *) fail "the refusal does not name the keep-no-auth remedy: ${out}" ;;
esac
# deploy/compose/docker-compose.yaml's wardynd service does not forward
# WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC, so offering it here as a settable remedy
# would send the operator to a variable this path ignores. If the compose file
# ever forwards it, THIS is the assertion to change.
grep -q 'WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC' "${REPO_ROOT}/deploy/compose/docker-compose.yaml" \
  && fail "docker-compose.yaml now forwards WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC — up.sh's refusal says it does not, and should now offer it as a real override"
case "${out}" in
  *"does not"*"forward it"*) ;;
  *) fail "the refusal names no override caveat — an operator will try WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC in .env and get no effect: ${out}" ;;
esac

# Neither half alone is the refused pair: a local-mode box with no issuer, and
# an SSO box with local mode off, must both pass straight through.
printf 'WARDYN_LOCAL_MODE=true\nWARDYN_OIDC_ISSUER=\n' > "${lm_env}"
refuse_local_mode_with_oidc "${lm_env}" >/dev/null 2>&1 \
  || fail "refused a plain local-mode .env with no OIDC issuer — that is the default compose posture"
printf 'WARDYN_LOCAL_MODE=false\nWARDYN_OIDC_ISSUER=https://idp.example.com\n' > "${lm_env}"
refuse_local_mode_with_oidc "${lm_env}" >/dev/null 2>&1 \
  || fail "refused a correctly-configured SSO .env (local mode off)"


# 6) wardyn_cli_prefix: no bin/wardyn extracted -> bare "wardyn" fallback.
got="$(wardyn_cli_prefix "${dir}")"
[ "$got" = "wardyn" ] || fail "cli prefix without bin/wardyn got '$got'"

# 7) bin/wardyn present (what seed_host_proxy leaves behind) -> ./bin/wardyn.
mkdir -p "${dir}/bin"
: > "${dir}/bin/wardyn"; chmod +x "${dir}/bin/wardyn"
got="$(wardyn_cli_prefix "${dir}")"
[ "$got" = "./bin/wardyn" ] || fail "cli prefix with bin/wardyn got '$got'"

echo "test-up-policy: self-test PASS"
