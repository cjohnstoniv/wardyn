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

# license_scope_files — the SINGLE source of truth for which tracked files must
# carry the SPDX/copyright header, shared by the CI gate
# (check-license-headers.sh) and the local fixer (add-license-headers.sh) so a
# future edit to any exclude pattern updates both at once. Excludes generated
# (*.gen.go/_gen.go/zz_generated), vendored (ui/node_modules, ui/dist), and the
# MIT-origin shadcn primitives (ui/src/app/components/ui/). Run from the repo
# root (both callers `cd` there first). Emits one path per line.
license_scope_files() {
  git ls-files '*.go' '*.ts' '*.tsx' '*.css' \
    | grep -vE '^ui/(node_modules|dist)/' \
    | grep -vE '\.gen\.go$|_gen\.go$|zz_generated' \
    | grep -vE '^ui/src/app/components/ui/'
}

# wardyn_pick_docker_host — export the same daemon preference as
# scripts/setup.sh's pick_daemon: honor an explicit DOCKER_HOST, else the
# dedicated tier-capable native dockerd if present, else the default socket.
# Provisioners (up.sh pg) and consumers (e2e-backend.sh, run-local.sh) must
# agree on the daemon, or `docker exec wardyn-test-pg` hits a different store
# than the one that created the container (real failure on dual-daemon boxes).
#
# It also derives WARDYN_DOCKER_SOCK — the host socket the compose wardynd
# bind-mounts and DRIVES — from the chosen DOCKER_HOST when unset. Without this,
# a dual-daemon box operates the stack on the native daemon yet mounts the
# default /var/run/docker.sock into wardynd, so the UI silently collapses to
# Fence (runsc/kata invisible). This runs even when DOCKER_HOST was pre-set, so
# `export DOCKER_HOST=...` alone is enough. (scripts/ci-run.sh relied on its own
# copy of this derivation; now it inherits this one.)
#
# Rancher Desktop: its daemon runs INSIDE a VM and its host-side context endpoint
# is ~/.rd/docker.sock, which the daemon CANNOT bind-mount ("operation not
# supported") — the working mount is /var/run/docker.sock INSIDE the VM. So both
# when DOCKER_HOST points at that host socket AND when it is unset (we peek at the
# active docker context to detect Rancher), WARDYN_DOCKER_SOCK is forced to the
# in-VM path. We deliberately do NOT rewrite DOCKER_HOST from the context — the
# CLI already resolves it — so Docker Desktop / native hosts are unaffected (unset
# DOCKER_HOST => WARDYN_DOCKER_SOCK left unset => compose's /var/run/docker.sock
# default, unchanged).
wardyn_pick_docker_host() {
  if [ -z "${DOCKER_HOST:-}" ]; then
    for _wpd_s in /run/wardyn-docker.sock /var/run/wardyn-docker.sock; do
      [ -S "${_wpd_s}" ] && { DOCKER_HOST="unix://${_wpd_s}"; export DOCKER_HOST; break; }
    done
    unset _wpd_s
  fi
  if [ -z "${WARDYN_DOCKER_SOCK:-}" ]; then
    _wpd_sock=""
    case "${DOCKER_HOST:-}" in
      unix://*) _wpd_sock="${DOCKER_HOST#unix://}" ;;
      "")
        # DOCKER_HOST unset: the CLI resolves the daemon via the active context.
        # Peek at it ONLY to detect an engine whose host socket is not
        # bind-mountable; otherwise leave WARDYN_DOCKER_SOCK unset (compose default).
        if command -v docker >/dev/null 2>&1; then
          _wpd_ep="$(docker context inspect -f '{{ .Endpoints.docker.Host }}' 2>/dev/null || true)"
          case "${_wpd_ep}" in
            *".rd/docker.sock") _wpd_sock="/var/run/docker.sock" ;;  # Rancher Desktop (in-VM path)
          esac
          unset _wpd_ep
        fi
        ;;
    esac
    # Rancher Desktop remap for an explicit DOCKER_HOST=unix://…/.rd/docker.sock.
    case "${_wpd_sock}" in
      *".rd/docker.sock") _wpd_sock="/var/run/docker.sock" ;;
    esac
    [ -n "${_wpd_sock}" ] && export WARDYN_DOCKER_SOCK="${_wpd_sock}"
    unset _wpd_sock
  fi
  return 0
}
