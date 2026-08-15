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
for fn in pick_policy composer_wants_llm resolve_default_policy wardyn_cli_prefix; do
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
# override, .env's auto-picked value must upgrade to composer-dev.json. Before
# this fix, "only decide when .env has nothing" left this wedged on demo.json.
got="$(resolve_default_policy "${env_file}" "" '{}' "1")"
[ "$got" = "/examples/policies/composer-dev.json" ] || fail "re-pick after a model signal got '$got', want composer-dev.json"
[ "$(env_get "${env_file}" WARDYN_DEFAULT_POLICY)" = "/examples/policies/composer-dev.json" ] || fail "composer-dev.json pick did not persist to .env"

# 4) An explicit operator override wins outright and clears the auto marker.
got="$(resolve_default_policy "${env_file}" "/my/custom.json" '{}' "1")"
[ "$got" = "/my/custom.json" ] || fail "explicit override not honored, got '$got'"
[ "$(env_get "${env_file}" WARDYN_DEFAULT_POLICY_AUTO)" = "0" ] || fail "explicit override did not clear the auto marker"

# 5) A later plain run (no override) must leave the operator's value alone,
# even with a model signal present — the marker says it's no longer ours.
got="$(resolve_default_policy "${env_file}" "" '{}' "1")"
[ "$got" = "/my/custom.json" ] || fail "auto-pick clobbered an operator override, got '$got'"

# 6) wardyn_cli_prefix: no bin/wardyn extracted -> bare "wardyn" fallback.
got="$(wardyn_cli_prefix "${dir}")"
[ "$got" = "wardyn" ] || fail "cli prefix without bin/wardyn got '$got'"

# 7) bin/wardyn present (what seed_host_proxy leaves behind) -> ./bin/wardyn.
mkdir -p "${dir}/bin"
: > "${dir}/bin/wardyn"; chmod +x "${dir}/bin/wardyn"
got="$(wardyn_cli_prefix "${dir}")"
[ "$got" = "./bin/wardyn" ] || fail "cli prefix with bin/wardyn got '$got'"

echo "test-up-policy: self-test PASS"
