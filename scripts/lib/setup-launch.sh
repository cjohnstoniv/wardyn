# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

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
