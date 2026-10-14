#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# No-docker, no-network regression coverage for scripts/lib/runner-limits.sh:
# wardyn_ensure_runner_limits, which setup and the walk scripts call after boot
# so the server's remote-hosted runner has limits before the first launch.
#
# SOURCES the helper under test, a live test against the real file. `wardyn`,
# `docker` and `df` are shell functions here: the helper only ever runs the
# commands it is given, so each case states what the server and the host say
# and reads back what the helper asked the CLI to set. The proposal's own
# numbers are the CLI's (cmd/wardyn TestRunnerRemotePropose); this pins which
# disk number the helper hands it, that nothing is set for a runner that has
# limits, and that an amount nothing measured fails naming the variable.
#
# Usage: scripts/test-runner-limits.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LIB="${REPO_ROOT}/scripts/lib/runner-limits.sh"
[ -f "${LIB}" ] || { echo "test-runner-limits: ${LIB} not found (renamed/removed?)" >&2; exit 1; }
# shellcheck source=scripts/lib/runner-limits.sh
. "${LIB}"

fail() { echo "test-runner-limits: FAIL: $*" >&2; exit 1; }
dir="$(mktemp -d)"; trap 'rm -rf "$dir"' EXIT
calls="${dir}/calls"; err="${dir}/err"

HEADER="ID  NAME  SET UP HERE  ONLINE  PEOPLE'S POOLS  POOLS"
# What the fake server says: the runners with no limits, and whether the runner reports its free disk.
unset_runners="" reports_disk=0 reports_size=1 set_rc=0 noisy=0
# The proposal the fake CLI prints: half of the free disk it was handed, everything else fixed.
proposal() { echo "--max-runs 4 --total-cpu 4 --total-memory 8 --total-disk $(($1 / 2)) --per-run-cpu 1 --per-run-memory 2 --per-run-disk $(($1 / 8))"; }
wardyn() {
  echo "$*" >>"${calls}"
  case "$*" in
    "runner remote list --no-limits") echo "${HEADER}"; for r in ${unset_runners}; do echo "${r}  ${r}  yes  yes  off  Organization"; done ;;
    "runner remote propose docker"*)
      if [ "${reports_size}" = 0 ]; then echo "wardyn: the runner reports no CPU or memory to propose from: set its limits with \`wardyn runner set\`" >&2; return 1; fi
      [ "${noisy}" = 1 ] && echo "wardyn: notice: this server is older than the CLI" >&2
      case "$*" in *--free-disk*) proposal "${!#}"; return ;; esac
      if [ "${reports_disk}" = 1 ]; then proposal 400; else echo "wardyn: the runner does not report its free disk: pass --free-disk" >&2; return 1; fi ;;
    "runner set docker "*) return "${set_rc}" ;;
    *) fail "the helper ran an unexpected command: wardyn $*" ;;
  esac
}
# The host: which engine `docker info` names, and the free space `df` reports under its data root.
engine="Docker Desktop" free_kb=0
docker() { case "$*" in *OperatingSystem*) echo "${engine}" ;; *DockerRootDir*) echo "${dir}" ;; *) return 1 ;; esac; }
df() { printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/x 0 0 %s 1%% /\n' "${free_kb}"; }

# run: one call of the helper with no terminal, its stderr kept; echoes the helper's exit code.
run() { : >"${calls}"; local rc=0; wardyn_ensure_runner_limits </dev/null 2>"${err}" || rc=$?; echo "${rc}"; }
set_call() { grep '^runner set ' "${calls}" || true; }
unset WARDYN_SETUP_RUNNER_LIMITS WARDYN_SETUP_RUNNER_DISK_GB DOCKER_HOST

# 1) Every runner has limits: nothing is proposed or set, and it says so (a re-run of setup changes nothing).
[ "$(run)" = 0 ] || fail "a server whose runners all have limits made the helper fail: $(cat "${err}")"
if [ -n "$(set_call)" ] || grep -q propose "${calls}"; then fail "limits were proposed or set for a runner that already has them: $(cat "${calls}")"; fi
grep -q "already has its limits; nothing changed" "${err}" || fail "the helper did not say that nothing changed: $(cat "${err}")"

# 2) The runner reports everything: its own numbers, no --free-disk.
unset_runners=docker reports_disk=1
[ "$(run)" = 0 ] || fail "a runner that reports everything was not given limits: $(cat "${err}")"
[ "$(set_call)" = "runner set docker $(proposal 400)" ] || fail "set ran as '$(set_call)', want the runner's own proposal"

# 2b) A line the CLI writes to stderr on a successful propose never becomes a flag: set gets exactly the
# proposal.
noisy=1
[ "$(run)" = 0 ] || fail "a propose that also wrote to stderr made the helper fail: $(cat "${err}")"
[ "$(set_call)" = "runner set docker $(proposal 400)" ] || fail "with stderr on a successful propose, set ran as '$(set_call)', want exactly the proposal"
noisy=0

# 3) Nothing measured the disk and no variable gives it: it fails, names the variable, and sets nothing.
reports_disk=0
[ "$(run)" != 0 ] || fail "an unmeasured disk with no WARDYN_SETUP_RUNNER_DISK_GB was accepted"
grep -q "WARDYN_SETUP_RUNNER_DISK_GB" "${err}" || fail "the failure does not name WARDYN_SETUP_RUNNER_DISK_GB: $(cat "${err}")"
[ -z "$(set_call)" ] || fail "limits were set though the disk was never given: $(set_call)"

# 4) WARDYN_SETUP_RUNNER_DISK_GB gives it, and wins over what the runner reports; a bad value is refused.
reports_disk=1
[ "$(WARDYN_SETUP_RUNNER_DISK_GB=120 run)" = 0 ] || fail "WARDYN_SETUP_RUNNER_DISK_GB=120 was refused: $(cat "${err}")"
[ "$(set_call)" = "runner set docker $(proposal 120)" ] || fail "set ran as '$(set_call)', want the proposal from 120 GB"
if [ "$(WARDYN_SETUP_RUNNER_DISK_GB=lots run)" = 0 ] || ! grep -q "WARDYN_SETUP_RUNNER_DISK_GB is a whole number" "${err}" || [ -n "$(set_call)" ]; then
  fail "WARDYN_SETUP_RUNNER_DISK_GB=lots was not refused by name: $(cat "${err}")"
fi

# 5) A local engine that is not Docker Desktop: the free space under its data root, as the host sees it.
reports_disk=0 engine="Ubuntu 24.04.1 LTS" free_kb=$((300 * 1048576))
[ "$(run)" = 0 ] || fail "a host that can see Docker's data root was not measured: $(cat "${err}")"
[ "$(set_call)" = "runner set docker $(proposal 300)" ] || fail "set ran as '$(set_call)', want the proposal from the host's 300 GB"
[ "$(DOCKER_HOST=tcp://build:2376 run)" != 0 ] || fail "a remote engine's disk was measured on this host"
engine="Docker Desktop"

# 6) WARDYN_SETUP_RUNNER_LIMITS sets exactly what a walk script gives, and proposes nothing.
walk="--max-runs 2 --total-cpu 2 --total-memory 4 --total-disk 20 --per-run-cpu 1 --per-run-memory 2 --per-run-disk 10"
[ "$(WARDYN_SETUP_RUNNER_LIMITS="${walk}" run)" = 0 ] || fail "WARDYN_SETUP_RUNNER_LIMITS was refused: $(cat "${err}")"
if [ "$(set_call)" != "runner set docker ${walk}" ] || grep -q propose "${calls}"; then fail "set ran as '$(set_call)', want the walk's flags verbatim and no proposal"; fi

# 7) A refused set leaves the runner without limits, and the helper says so and fails.
set_rc=1
if [ "$(WARDYN_SETUP_RUNNER_LIMITS="${walk}" run)" = 0 ] || ! grep -q "takes no run until" "${err}"; then fail "a refused set was reported as done: $(cat "${err}")"; fi
set_rc=0

# 7b) The given flags split into words but never into file names, and the caller's own glob setting
# survives the call.
touch "${dir}/--max-runs-glob-bait"
cd "${dir}"
[ "$(WARDYN_SETUP_RUNNER_LIMITS='--max-runs 2 --total-cpu *' run)" = 0 ] || fail "flags holding a glob were refused: $(cat "${err}")"
[ "$(set_call)" = "runner set docker --max-runs 2 --total-cpu *" ] || fail "a glob in WARDYN_SETUP_RUNNER_LIMITS expanded to file names: '$(set_call)'"
cd - >/dev/null
case $- in *f*) fail "the helper left globbing off for its caller" ;; esac

# 7c) A runner that reports no CPU or memory has no proposal: it says so, names WARDYN_SETUP_RUNNER_LIMITS,
# and sets nothing, even with the disk given.
reports_size=0
[ "$(run)" != 0 ] || fail "a runner with no CPU or memory reported was given limits"
if ! grep -q "no CPU or memory" "${err}" || ! grep -q "WARDYN_SETUP_RUNNER_LIMITS" "${err}"; then
  fail "the failure for a runner with no CPU or memory reported does not say so and name WARDYN_SETUP_RUNNER_LIMITS: $(cat "${err}")"
fi
if [ "$(WARDYN_SETUP_RUNNER_DISK_GB=100 run)" = 0 ] || [ -n "$(set_call)" ]; then fail "the disk alone gave limits to a runner with no CPU or memory reported: $(set_call)"; fi
reports_size=1

# 8) At a keyboard: the disk is asked for (no default: an empty answer sets nothing), the proposal is
# shown, and declining it asks for each amount, Enter keeping the one shown.
_rl_interactive() { return 0; }
: >"${calls}"; printf '\n' | wardyn_ensure_runner_limits 2>"${err}" && fail "an empty answer for the disk was taken as a number"
[ -z "$(set_call)" ] || fail "limits were set after an empty answer for the disk: $(set_call)"
: >"${calls}"; printf '200\nn\n3\n\n\n\n\n\n\n' | wardyn_ensure_runner_limits 2>"${err}" || fail "the typed amounts were refused: $(cat "${err}")"
want="runner set docker --max-runs 3 --total-cpu 4 --total-memory 8 --total-disk 100 --per-run-cpu 1 --per-run-memory 2 --per-run-disk 25"
[ "$(set_call)" = "${want}" ] || fail "after typing 200 GB and Runs at once 3, set ran as '$(set_call)', want '${want}'"
: >"${calls}"; printf '200\n\n' | wardyn_ensure_runner_limits 2>"${err}" || fail "accepting the proposal failed: $(cat "${err}")"
[ "$(set_call)" = "runner set docker $(proposal 200)" ] || fail "accepting the proposal set '$(set_call)'"
# With WARDYN_SETUP_RUNNER_DISK_GB given there is nothing to ask: no prompt waits on a recorded or scripted terminal.
: >"${calls}"; WARDYN_SETUP_RUNNER_DISK_GB=200 wardyn_ensure_runner_limits </dev/null 2>"${err}" || fail "a given disk at a terminal was refused: $(cat "${err}")"
if grep -q "Set these limits?" "${err}" || [ "$(set_call)" != "runner set docker $(proposal 200)" ]; then fail "with the disk given at a terminal it asked, or set '$(set_call)'"; fi

# 9) setup.sh's host mode gives the runner its limits on its already-running branch too, so a re-run with
# WARDYN_SETUP_RUNNER_DISK_GB set finishes what a first run stopped at.
already="$(awk '/^# Already running\?/,/^fi$/' "${REPO_ROOT}/scripts/setup.sh")"
# shellcheck disable=SC2016 # the literal text of the call in setup.sh
grep -q 'host_runner_limits "\$URL"' <<<"${already}" \
  || fail "scripts/setup.sh's already-running branch no longer calls host_runner_limits: a re-run would report success with a runner that takes no run"

echo "test-runner-limits: self-test PASS"
