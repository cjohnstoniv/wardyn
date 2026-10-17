# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
# shellcheck shell=sh

# scripts/lib/runner-limits.sh: give the server's remote-hosted runner its limits after boot.
#
# A runner with no limits takes no run, and the server sets a remote-hosted runner's limits at its first
# boot only when it could measure every amount. With Docker Desktop, or with wardynd in a container (the
# compose stack), it cannot see the disk, so setup and the walk scripts set the limits here, where the
# person or the script can see them. Nothing is guessed, by the server or by this file: an amount
# nothing measured has to be given.
#
# POSIX sh (scripts/up.sh is /bin/sh) and no JSON tool: the CLI answers in plain lines.
#
#   wardyn_ensure_runner_limits
#
# The caller provides `wardyn`: the CLI as a binary on PATH or a shell function, reaching the server as
# an administrator. Every call gets /dev/null as its input, so a `docker compose exec` wrapper never
# reads the answers a person types here. For every remote-hosted runner with no limits (`wardyn runner remote list
# --no-limits`) it sets, with `wardyn runner set`:
#
#   WARDYN_SETUP_RUNNER_LIMITS   those flags verbatim, when set (the walk scripts size theirs for the walk);
#   otherwise the proposal `wardyn runner remote propose` prints: half of the CPU and memory the runner
#   reports and half of its free disk, Runs at once half its CPUs, per run the total divided by that.
#
# The free disk is, in order: WARDYN_SETUP_RUNNER_DISK_GB when set; what the runner reports; for the
# Docker runner, the free space under Docker's data root when the engine this shell's docker CLI uses (its
# selected context, DOCKER_CONTEXT or DOCKER_HOST) is a unix socket here and is not Docker Desktop; on a terminal, what the person types (there is no default). With none of these
# it fails and names the variable. On a terminal, with no variable given, the person is shown the
# proposal and may accept it or type each amount. A runner that reports no CPU or memory has no
# proposal: only WARDYN_SETUP_RUNNER_LIMITS (or a person at `wardyn runner set`) gives it limits. A runner that already has limits is left as it is,
# including limits an administrator declared while a proposal waited (`wardyn runner set --if-unset`), and so is a row that is not set up on
# this server. It returns non-zero when a runner
# is left without limits; the caller decides whether that ends it.

_rl_say() { printf '  runner limits: %s\n' "$*" >&2; }

# _rl_interactive: a person is at the keyboard (every prompt here is written to stderr).
_rl_interactive() { [ -t 0 ] && [ -t 2 ]; }

# _rl_is_socket PATH: PATH is a unix socket on this host.
_rl_is_socket() { [ -S "$1" ]; }

# _rl_host_free_disk_gb ID: the GB free under Docker's data root, when this shell can see the engine's
# own disk; fails (and prints nothing) when it cannot. The engine is the one the docker CLI talks to:
# `docker context inspect` with no name answers for the context the CLI would use, so DOCKER_CONTEXT, a
# selected context and DOCKER_HOST are weighed the way the CLI weighs them. Only a unix socket that is
# there on this host counts as a local engine.
_rl_host_free_disk_gb() {
  [ "$1" = docker ] || return 1
  command -v docker >/dev/null 2>&1 || return 1
  _rl_ep="$(docker context inspect -f '{{.Endpoints.docker.Host}}' 2>/dev/null)" || return 1
  case "${_rl_ep}" in unix://?*) ;; *) return 1 ;; esac
  _rl_is_socket "${_rl_ep#unix://}" || return 1
  _rl_os="$(docker info -f '{{.OperatingSystem}}' 2>/dev/null)" || return 1
  # Docker Desktop's data root is a path inside its VM, never this host's.
  if [ -z "${_rl_os}" ] || [ "${_rl_os}" = "Docker Desktop" ]; then return 1; fi
  _rl_root="$(docker info -f '{{.DockerRootDir}}' 2>/dev/null)" || return 1
  if [ -z "${_rl_root}" ] || [ ! -d "${_rl_root}" ]; then return 1; fi
  _rl_gb="$(df -Pk "${_rl_root}" 2>/dev/null | awk 'NR==2 {print int($4 / 1048576)}')"
  case "${_rl_gb}" in "" | *[!0-9]* | 0) return 1 ;; esac
  printf '%s\n' "${_rl_gb}"
}

# _rl_whole NAME VALUE: VALUE is a whole number of GB, 1 or more.
_rl_whole() {
  case "$2" in "" | *[!0-9]* | 0*) _rl_say "$1 is a whole number of GB, 1 or more (got '$2')"; return 1 ;; esac
}

# _rl_propose ID: prints the proposed `wardyn runner set` flags for ID, and says on stderr where the
# disk number came from.
_rl_propose() {
  if [ -n "${WARDYN_SETUP_RUNNER_DISK_GB:-}" ]; then
    _rl_whole WARDYN_SETUP_RUNNER_DISK_GB "${WARDYN_SETUP_RUNNER_DISK_GB}" || return 1
    _rl_say "$1: free disk ${WARDYN_SETUP_RUNNER_DISK_GB} GB, from WARDYN_SETUP_RUNNER_DISK_GB"
    wardyn runner remote propose "$1" --free-disk "${WARDYN_SETUP_RUNNER_DISK_GB}" </dev/null
    return
  fi
  # The flags are stdout alone: anything the CLI says on stderr never reaches `wardyn runner set`. Its
  # refusal is read by a second call, only when the first failed.
  if _rl_flags="$(wardyn runner remote propose "$1" </dev/null 2>/dev/null)" && [ -n "${_rl_flags}" ]; then
    _rl_say "$1: CPU, memory and free disk as the runner reports them"
    printf '%s\n' "${_rl_flags}"
    return
  fi
  _rl_why="$(wardyn runner remote propose "$1" </dev/null 2>&1 >/dev/null)" || true
  case "${_rl_why}" in
    *"no CPU or memory"*)
      _rl_say "$1 reports no CPU or memory to propose from, so there is no proposal. Set WARDYN_SETUP_RUNNER_LIMITS to the whole set of \`wardyn runner set\` flags."
      return 1
      ;;
  esac
  if _rl_disk="$(_rl_host_free_disk_gb "$1")"; then
    _rl_say "$1: free disk ${_rl_disk} GB, measured under Docker's data root on this host (the server could not see it)"
  elif _rl_interactive; then
    _rl_say "$1: the server could not measure this runner's free disk, and neither can this script."
    printf '  Free disk for runs on %s, in GB: ' "$1" >&2
    read -r _rl_disk || _rl_disk=""
    _rl_whole "the free disk" "${_rl_disk}" || return 1
  else
    _rl_say "$1: its free disk could not be measured. Set WARDYN_SETUP_RUNNER_DISK_GB to the GB of disk free for runs on its host, or WARDYN_SETUP_RUNNER_LIMITS to the whole set of \`wardyn runner set\` flags."
    return 1
  fi
  wardyn runner remote propose "$1" --free-disk "${_rl_disk}" </dev/null
}

# _rl_edit FLAG VALUE...: asks for each amount, Enter keeping the one shown; prints the flags as answered.
_rl_edit() {
  _rl_out=""
  while [ "$#" -ge 2 ]; do
    printf '    %s [%s]: ' "${1#--}" "$2" >&2
    read -r _rl_ans || _rl_ans=""
    _rl_out="${_rl_out} $1 ${_rl_ans:-$2}"
    shift 2
  done
  printf '%s\n' "${_rl_out# }"
}

# _rl_set ID: sets the limits of the one runner ID, which has none.
_rl_set() {
  if [ -n "${WARDYN_SETUP_RUNNER_LIMITS:-}" ]; then
    _rl_flags="${WARDYN_SETUP_RUNNER_LIMITS}"
    _rl_say "$1: the limits given in WARDYN_SETUP_RUNNER_LIMITS"
  else
    _rl_flags="$(_rl_propose "$1")" || _rl_flags=""
    if [ -z "${_rl_flags}" ]; then
      _rl_say "$1 has no limits and takes no run until they are set: wardyn runner set $1 --max-runs N --total-cpu N --total-memory GB --total-disk GB --per-run-cpu N --per-run-memory GB --per-run-disk GB"
      return 1
    fi
    # Asked to accept only when nothing was given: WARDYN_SETUP_RUNNER_DISK_GB is an answer already.
    if _rl_interactive && [ -z "${WARDYN_SETUP_RUNNER_DISK_GB:-}" ]; then
      _rl_say "$1: proposed, half of what it has: ${_rl_flags}"
      printf '  Set these limits? [Y/n] (n: type each amount) ' >&2
      read -r _rl_ans || _rl_ans=""
      case "${_rl_ans}" in
        [nN]*)
          # shellcheck disable=SC2086 # the flags are words: --flag value pairs
          _rl_flags="$(_rl_edit ${_rl_flags})"
          ;;
      esac
    fi
  fi
  _rl_say "$1: wardyn runner set $1 ${_rl_flags}"
  # The flags split into words; never into file names (WARDYN_SETUP_RUNNER_LIMITS is the caller's text).
  _rl_opts=$-
  set -f
  # shellcheck disable=SC2086 # the flags are words: --flag value pairs
  if wardyn runner set "$1" --if-unset ${_rl_flags} </dev/null >&2; then _rl_rc=0; else _rl_rc=$?; fi
  case "${_rl_opts}" in *f*) ;; *) set +f ;; esac
  if [ "${_rl_rc}" -ne 0 ]; then
    # --if-unset loses to a limit declared between the list and the set: then the runner has limits, just
    # not these.
    if _rl_still="$(wardyn runner remote list --no-limits --set-up-here --ids </dev/null 2>/dev/null)" &&
      ! printf '%s\n' "${_rl_still}" | grep -qx -- "$1"; then
      _rl_say "$1: limits were set meanwhile by someone else; they are kept and these were not applied"
      return 0
    fi
    _rl_say "$1: these limits were not set; it takes no run until they are"
    return 1
  fi
}

wardyn_ensure_runner_limits() {
  # Ids, one per line (--ids): a name may hold spaces, so no table is parsed. A row that is not set up on
  # this server is another computer's or cluster's: it cannot take a run here, so it is left out.
  _rl_all="$(wardyn runner remote list --no-limits --ids </dev/null)" &&
    _rl_ids="$(wardyn runner remote list --no-limits --set-up-here --ids </dev/null)" || {
    _rl_say "could not list the server's remote-hosted runners (wardyn runner remote list --no-limits)"
    return 1
  }
  for _rl_id in ${_rl_all}; do
    printf '%s\n' "${_rl_ids}" | grep -qx -- "${_rl_id}" ||
      _rl_say "${_rl_id}: not set up on this server, so it takes no run here and is left as it is"
  done
  if [ -z "${_rl_ids}" ]; then
    _rl_say "every remote-hosted runner set up here already has its limits; nothing changed"
    return 0
  fi
  for _rl_id in ${_rl_ids}; do
    _rl_set "${_rl_id}" || return 1
  done
}
