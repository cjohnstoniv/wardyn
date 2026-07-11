# scripts/lib/common.sh — small shell helpers shared across scripts/*.sh.
#
# wait_healthy/wait_down cover the plain "poll /healthz in a loop" shape used
# by run-local.sh, e2e-backend.sh, run-e2e-subscription.sh and run-e2e-live.sh.
# Scripts with extra gating on top of the plain poll (up.sh's docker-inspect
# container health, demo.sh's compose-health check, test-drive.sh's stack
# health) keep their own bespoke loop — those are not the same shape.
#
# log/warn/die are the ANSI-colored console helpers used by most scripts/*.sh.
# Callers whose tag/color/stream differs from this default (e.g. a script that
# tags its lines "[e2e]" instead of "==>") keep a local override defined AFTER
# sourcing this file — bash lets the later definition win, so behavior for
# those scripts is unchanged.

# wait_healthy URL [TRIES] [SLEEP_SECS] — poll URL/healthz until it answers
# (curl -fsS), sleeping SLEEP_SECS (default 1) between up to TRIES (default 60)
# attempts. Returns 1 on timeout.
wait_healthy() {
  local url="$1" tries="${2:-60}" slp="${3:-1}"
  for _ in $(seq 1 "${tries}"); do
    curl -fsS "${url}/healthz" >/dev/null 2>&1 && return 0
    sleep "${slp}"
  done
  return 1
}

# wait_down URL [TRIES] [SLEEP_SECS] — poll until URL/healthz stops answering.
# Returns 1 on timeout (still answering).
wait_down() {
  local url="$1" tries="${2:-30}" slp="${3:-1}"
  for _ in $(seq 1 "${tries}"); do
    curl -fsS "${url}/healthz" >/dev/null 2>&1 || return 0
    sleep "${slp}"
  done
  return 1
}

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

# wardyn_pick_docker_host — export the same daemon preference as
# scripts/setup.sh's pick_daemon: honor an explicit DOCKER_HOST, else the
# dedicated tier-capable native dockerd if present, else the default socket.
# Provisioners (up.sh pg) and consumers (e2e-backend.sh, run-local.sh) must
# agree on the daemon, or `docker exec wardyn-test-pg` hits a different store
# than the one that created the container (real failure on dual-daemon boxes).
wardyn_pick_docker_host() {
  [ -n "${DOCKER_HOST:-}" ] && return 0
  for _wpd_s in /run/wardyn-docker.sock /var/run/wardyn-docker.sock; do
    [ -S "${_wpd_s}" ] && { DOCKER_HOST="unix://${_wpd_s}"; export DOCKER_HOST; break; }
  done
  unset _wpd_s
  return 0
}
