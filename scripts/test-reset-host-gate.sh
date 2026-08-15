#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for W29-S1-6: `make reset` (scripts/up.sh's cmd_reset) must
# NOT silently stop a live host-mode wardynd off the SAME WARDYN_FORCE_RESET=1
# flag that headlessly confirms the (unrelated) compose-volume wipe.
# docs/TRY-IT.md promises reset "does not touch a host-mode daemon" — before
# this fix, `WARDYN_FORCE_RESET=1 make reset` broke that promise by reusing
# the one _confirm gate (keyed on WARDYN_FORCE_RESET) for BOTH prompts, so the
# headless wipe-confirmation silently auto-answered the host-stop prompt too.
#
# Sources the REAL cmd_reset (minus its dispatch tail) with make/kill/docker
# stubbed, and asserts:
#   - WARDYN_FORCE_RESET=1 alone, non-interactive: stop-host is NOT invoked.
#   - WARDYN_FORCE_RESET=1 + WARDYN_FORCE_STOP_HOST=1: stop-host IS invoked.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UP_SH="${REPO_ROOT}/scripts/up.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Strip the dispatch tail so sourcing defines functions only — it must not
# immediately run a cmd_* the way `scripts/up.sh` invoked directly would.
FUNCS="${WORK}/up_functions.sh"
sed '/^# ── dispatch/,$d' "${UP_SH}" > "${FUNCS}"

# run_case NAME force_reset force_stop_host want_stop_host_called
run_case() {
  local name="$1" force_reset="$2" force_stop_host="$3" want="$4"
  local calllog="${WORK}/calls-${name}"
  : > "${calllog}"

  (
    set -euo pipefail
    HOME="${WORK}/home-${name}"
    mkdir -p "${HOME}/.wardyn"
    # The PID content is irrelevant — kill() below is stubbed to always report
    # "alive" so cmd_reset always reaches the host-stop branch under test.
    echo 999999 > "${HOME}/.wardyn/host-wardynd.pid"

    # Stub the effectful commands BEFORE sourcing: `make`/`docker` must never
    # really run; `kill -0` must report the fake PID as live without one.
    kill() { return 0; }
    make() { echo "make $*" >> "${calllog}"; }
    docker() { :; }

    # shellcheck source=/dev/null
    . "${FUNCS}"
    # cmd_up (via `compose down -v && cmd_up`) would otherwise build/launch
    # the real stack — override AFTER sourcing so it isn't clobbered by the
    # real definition FUNCS just loaded.
    cmd_up() { echo "cmd_up" >> "${calllog}"; }
    REPO_ROOT="${WORK}/fake-repo"

    export WARDYN_FORCE_RESET="${force_reset}"
    if [ -n "${force_stop_host}" ]; then
      export WARDYN_FORCE_STOP_HOST="${force_stop_host}"
    else
      unset WARDYN_FORCE_STOP_HOST 2>/dev/null || true
    fi

    cmd_reset < /dev/null
  )

  local got=0
  grep -q -- '-C .*stop-host' "${calllog}" && got=1
  if [ "${got}" != "${want}" ]; then
    fail "${name}: stop-host called=${got}, want ${want} — calls were: $(cat "${calllog}")"
  fi
  echo "ok  ${name}"
}

run_case "force-reset-only-must-not-stop-host"       1 ""  0
run_case "force-reset-plus-force-stop-host-does-stop" 1 "1" 1

echo "PASS scripts/test-reset-host-gate.sh"
