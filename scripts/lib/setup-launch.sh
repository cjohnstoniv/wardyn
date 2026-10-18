# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=bash

# scripts/lib/setup-launch.sh — host-mode wardynd launch helpers shared by
# scripts/setup.sh. A separate file (not inline in setup.sh) because setup.sh
# is on scripts/check-file-size.sh's frozen allowlist: new logic goes in a NEW
# file rather than growing the capped one further.

# finish_unhealthy_launch PIDFILE WPID LOGFILE — the tail of setup.sh's
# launch_wardynd wait when /healthz never answered 200: states what happened
# and decides whether PIDFILE survives. B12b-F4: an unconditional `rm -f
# PIDFILE` here orphaned a wardynd that was still alive and simply still
# starting past this script's own wait budget — `make stop-host` and the
# already-running check at the top of setup.sh both key off PIDFILE, so
# deleting it left no way to find or stop that process short of a manual
# `kill`, and a later `make setup` would try to launch a SECOND wardynd onto
# the same port instead of noticing the first. Keep PIDFILE whenever the
# process it names is still actually running; only delete it once the process
# has genuinely exited. Extracted into its own function (here, not inline) so
# scripts/test-setup-launch.sh can drive both arms with no daemon.
finish_unhealthy_launch() {
  _ful_pidfile=$1 _ful_wpid=$2 _ful_logfile=$3
  if kill -0 "${_ful_wpid}" 2>/dev/null; then
    warn "wardynd is still starting (PID ${_ful_wpid}) — watch ${_ful_logfile}; stop with make stop-host"
  else
    rm -f "${_ful_pidfile}"
    warn "wardynd did not become healthy — last log lines:"
  fi
  tail -n 15 "${_ful_logfile}" 2>/dev/null | sed 's/^/    /'
  warn "Full log: ${_ful_logfile}"
  unset _ful_pidfile _ful_wpid _ful_logfile
}

# host_runner_limits URL — gives the host-mode server's runner its limits
# (scripts/lib/runner-limits.sh, sourced by setup.sh before this is called).
# setup.sh calls it after a launch AND on its already-running branch, so a
# re-run with WARDYN_SETUP_RUNNER_DISK_GB set finishes what a first run
# stopped at; a runner that already has limits is left alone.
host_runner_limits() {
  _hrl_url=$1
  # shellcheck disable=SC2317 # called by wardyn_ensure_runner_limits
  wardyn() { WARDYN_URL="${_hrl_url}" ./bin/wardyn "$@"; }
  wardyn_ensure_runner_limits \
    || die "the server's runner has no limits, so no run can start — set WARDYN_SETUP_RUNNER_DISK_GB and run make setup again, or set them by hand (docs/operations/runner-pools.md#a-remote-hosted-runners-limits); wardynd keeps running (make stop-host)"
  unset -f wardyn
  unset _hrl_url
}

# local_build_needed — true when ./bin/wardynd, ./bin/wardyn or the UI bundle is missing, or any
# non-test Go source (or go.mod/go.sum) is newer than ./bin/wardyn. An old CLI left by an earlier
# checkout lacks the flags setup.sh passes it, so a present binary is not enough.
local_build_needed() {
  [ ! -x ./bin/wardynd ] || [ ! -x ./bin/wardyn ] || [ ! -f ./ui/dist/index.html ] && return 0
  [ -n "$(find cmd internal pkg go.mod go.sum -type f \( -name '*.go' -o -name go.mod -o -name go.sum \) ! -name '*_test.go' -newer ./bin/wardyn -print -quit 2>/dev/null)" ]
}

# stale_secrets_wipe_consent ENV_FILE — may setup.sh TRUNCATE the secrets table after
# wardynd could not decrypt it? 0 = yes, 1 = no (with the reason printed). The wipe
# destroys every stored credential and the boot keys, so it needs the same consent as the
# volume wipe, and is refused when the decrypt failed because THIS shell handed wardynd a
# different key than ENV_FILE's: the stored secrets are then still readable with ENV_FILE's.
# ask_yn comes from setup.sh.
stale_secrets_wipe_consent() {
  _ssw_envkey=$(grep -E '^WARDYN_AGE_KEY=' "$1" 2>/dev/null | head -1 | cut -d= -f2-) || true
  if [ -n "${WARDYN_AGE_KEY_FILE:-}${WARDYN_PLATFORM_KEY_FILE:-}" ] || { [ -n "${WARDYN_AGE_KEY:-}" ] && [ "${WARDYN_AGE_KEY}" != "${_ssw_envkey}" ]; }; then
    warn "Your shell sets WARDYN_AGE_KEY, WARDYN_AGE_KEY_FILE or WARDYN_PLATFORM_KEY_FILE to a key other than the one in $1; wardynd used it, so the stored secrets may be fine under that file's key."
    warn "Nothing was cleared. Unset those variables and run make setup again."
    unset _ssw_envkey
    return 1
  fi
  unset _ssw_envkey
  if [ "${WARDYN_FORCE_RESET:-}" = 1 ]; then warn "WARDYN_FORCE_RESET=1 - clearing the secrets table."; return 0; fi
  warn "Clearing the secrets table DESTROYS every stored credential (model providers, SCM) and the boot keys."
  [ -t 0 ] && ask_yn "Clear the secrets table (audit log, runs and recordings are kept)?" n
}
