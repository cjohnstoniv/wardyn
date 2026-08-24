#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for W29-S1-1 and W29-S1-5, both in the reset paths'
# site-config warning (scripts/up-reset.sh's _capture_hint):
#
#   W29-S1-1: plain `make reset` (cmd_reset) warned only about "Postgres … and
#             recordings" and never named site-config/secrets or printed a
#             capture command — that text existed only on cmd_reset_all. The
#             corporate baseline (upstream proxy, artifact mirrors, SCM hosts)
#             lives in the Postgres volume, so the wipe took it unannounced and
#             the stack came back up healthy with sandbox egress unconfigured.
#
#   W29-S1-5: the capture hint printed whenever volumes existed, with no check
#             that wardynd was running. `wardyn site-config get` runs INSIDE the
#             wardynd container, so on an already-down stack — the normal state
#             one line under a manifest reporting 0 running containers — the
#             operator got a command that could only print a connection error.
#
# Sources the REAL cmd_reset (minus up.sh's dispatch tail) with the effectful
# commands stubbed, and drives _capture_hint's one branch through a stubbed
# `compose ps --status running wardynd`.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UP_SH="${REPO_ROOT}/scripts/up.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Strip the dispatch tail so sourcing defines functions only.
FUNCS="${WORK}/up_functions.sh"
sed '/^# ── dispatch/,$d' "${UP_SH}" > "${FUNCS}"

# run_case NAME wardynd_running
# Echoes cmd_reset's combined output for the caller to assert on.
run_case() {
  local name="$1" running="$2"
  (
    set -euo pipefail
    HOME="${WORK}/home-${name}"
    mkdir -p "${HOME}/.wardyn"   # no host-wardynd.pid => host-stop branch skipped

    kill() { return 0; }
    make() { :; }
    docker() { :; }

    # shellcheck source=/dev/null
    . "${FUNCS}"

    # AFTER sourcing, so the real definitions don't clobber these.
    # `compose ps -q --status running wardynd` is the ONLY thing _capture_hint
    # branches on; every other compose call is inert here.
    if [ "${running}" = 1 ]; then
      compose() { case "$*" in *"--status running"*) echo "deadbeefcafe" ;; *) : ;; esac; }
    else
      compose() { :; }
    fi
    cmd_up() { :; }
    REPO_ROOT="${WORK}/fake-repo"

    # No WARDYN_FORCE_RESET and stdin closed => _confirm declines, so cmd_reset
    # exits BEFORE removing anything. The warnings under test all print first.
    unset WARDYN_FORCE_RESET 2>/dev/null || true
    # cmd_reset's decline path calls `exit 2`, which terminates this subshell
    # directly — hence the `|| true` on the CALLER below, not here.
    cmd_reset < /dev/null
  ) 2>&1
}

# W29-S1-1: plain reset must name what else the volume wipe destroys.
out_up="$(run_case wardynd-running 1 || true)"
case "${out_up}" in
  *site-config*) ;;
  *) fail "plain reset never names site-config — output was: ${out_up}" ;;
esac

# W29-S1-5: with wardynd up, the capture command is the one worth printing.
case "${out_up}" in
  *"exec -T wardynd wardyn site-config get"*) echo "ok  running: capture command printed" ;;
  *) fail "wardynd running but no capture command printed — output was: ${out_up}" ;;
esac

# W29-S1-5: with wardynd down, that command could only error, so it must NOT
# be printed — and the operator must be told why.
out_down="$(run_case wardynd-down 0 || true)"
case "${out_down}" in
  *"exec -T wardynd wardyn site-config get"*)
    fail "wardynd is down but the in-container capture command was printed anyway — output was: ${out_down}" ;;
esac
case "${out_down}" in
  *site-config*) ;;
  *) fail "wardynd down: reset still must name site-config — output was: ${out_down}" ;;
esac
case "${out_down}" in
  *"NOT running"*) echo "ok  down: command withheld, reason given" ;;
  *) fail "wardynd down: no explanation printed — output was: ${out_down}" ;;
esac

echo "PASS scripts/test-reset-capture-hint.sh"
