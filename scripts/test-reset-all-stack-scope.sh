#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test (#2220): reset-all reaps the sandbox containers and per-run networks of ITS OWN
# stack only. Every wardynd or runnerd on a daemon stamps wardyn.managed=true, so the old label-only
# selector also removed a second stack's live sandboxes (WARDYN_NS=job-a, an e2e backend, a laptop
# runnerd). Since 0.9.4 each object names its deployment (the control-plane network); one that names
# another is kept and listed, one with no such label predates that and is reaped as before.
#
# Daemon-free: sources the real cmd_reset_all with docker/make stubbed, like test-reset-network-ns.sh.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

FUNCS="${WORK}/up_functions.sh"
sed '/^# ── dispatch/,$d' "${REPO_ROOT}/scripts/up.sh" > "${FUNCS}"

# run_case NAME NS OWN FOREIGN [--dry-run]: the stubbed daemon holds an unlabelled sandbox, one
# labelled OWN and one labelled FOREIGN, with the same three per-run networks.
run_case() {
  local name="$1" ns="$2" own="$3" foreign="$4" dry="${5:-}"
  local calllog="${WORK}/docker-${name}" out="${WORK}/out-${name}"
  : > "${calllog}"
  (
    set -euo pipefail
    HOME="${WORK}/home-${name}"; mkdir -p "${HOME}/.wardyn"
    docker() {
      echo "docker $*" >> "${calllog}"
      case "$1 ${2:-}" in
        "ps -a") printf 'c-old \nc-own %s\nc-other %s\n' "${own}" "${foreign}" ;;
        "network ls") printf 'n-old \nn-own %s\nn-other %s\n' "${own}" "${foreign}" ;;
      esac
    }
    make() { echo "make $*" >> "${calllog}"; }
    # shellcheck source=/dev/null
    . "${FUNCS}"
    if [ -n "${ns}" ]; then export WARDYN_NS="${ns}"; else unset WARDYN_NS; fi
    export WARDYN_FORCE_RESET=1
    cmd_reset_all ${dry} < /dev/null
  ) > "${out}" 2>&1 || fail "${name}: cmd_reset_all exited non-zero: $(cat "${out}")"
  grep -q '\[present\] live per-run sandbox containers: 2 ' "${out}" \
    || fail "${name}: the manifest does not count exactly this stack's 2 sandbox containers: $(grep 'sandbox containers' "${out}")"
  grep -q '\[present\] per-run internal networks: 2 ' "${out}" \
    || fail "${name}: the manifest does not count exactly this stack's 2 per-run networks: $(grep 'internal networks' "${out}")"
  grep -q '\[keep\].*OTHER Wardyn stacks.*: 2 ' "${out}" \
    || fail "${name}: the other stack's objects are not listed [keep]: $(cat "${out}")"
  [ -n "${dry}" ] && return 0
  grep -qx 'docker rm -f c-old c-own' "${calllog}" \
    || fail "${name}: reset-all did not remove exactly c-old and c-own: $(grep 'docker rm' "${calllog}")"
  grep -qx 'docker network rm n-old n-own' "${calllog}" \
    || fail "${name}: reset-all did not remove exactly n-old and n-own: $(grep 'docker network rm' "${calllog}")"
  if grep -qE 'c-other|n-other' <(grep -E 'docker (rm|network rm)' "${calllog}"); then
    fail "${name}: reset-all removed the other stack's sandbox: $(grep -E 'c-other|n-other' "${calllog}")"
  fi
}

run_case ns-job-a job-a job-a-internal wardyn-internal
run_case ns-job-a-dry job-a job-a-internal wardyn-internal --dry-run
run_case default "" wardyn-internal job-a-internal

echo "test-reset-all-stack-scope: PASS"
