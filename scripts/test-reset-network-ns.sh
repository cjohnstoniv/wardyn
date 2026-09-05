#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test: reset-all must act on ITS OWN control-plane network.
#
# docker-compose.yaml names that network ${WARDYN_NS:-wardyn}-internal (and
# hands wardynd the same string as WARDYN_INTERNAL_NETWORK), and up.sh's own
# `docker run --network` calls use the same derived form — but up-reset.sh
# hardcoded the literal "wardyn-internal". On a namespaced install
# (WARDYN_NS=wardyn2, the shape deploy/compose/README.md documents for a second
# stack on one host) that inverted the blast radius twice over: reset-all
# reported and REMOVED the DEFAULT install's network — a FOREIGN stack's — and
# never removed its own.
#
# Daemon-free: sources the real cmd_reset_all with docker/make stubbed to log
# every call, exactly like scripts/test-reset-host-gate.sh.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
UP_SH="${REPO_ROOT}/scripts/up.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Sourcing must define functions only, never run the dispatch tail.
FUNCS="${WORK}/up_functions.sh"
sed '/^# ── dispatch/,$d' "${UP_SH}" > "${FUNCS}"

# run_case NAME NS EXTRA_ARGS — captures docker calls + reset-all's manifest.
run_case() {
  local name="$1" ns="$2" dry="$3"
  local calllog="${WORK}/docker-${name}" out="${WORK}/out-${name}"
  : > "${calllog}"

  (
    set -euo pipefail
    HOME="${WORK}/home-${name}"
    mkdir -p "${HOME}/.wardyn"

    # Every effectful command is a stub. `docker` logs its argv and reports
    # "object exists" (exit 0) so both the inspect branch and the removal
    # branch are TAKEN — a stub that reported "absent" would skip the very
    # lines under test.
    docker() { echo "docker $*" >> "${calllog}"; }
    make() { echo "make $*" >> "${calllog}"; }

    # shellcheck source=/dev/null
    . "${FUNCS}"
    export WARDYN_NS="${ns}" WARDYN_FORCE_RESET=1
    if [ "${dry}" = 1 ]; then
      cmd_reset_all --dry-run < /dev/null
    else
      cmd_reset_all < /dev/null
    fi
  ) > "${out}" 2>&1 || fail "${name}: cmd_reset_all exited non-zero — $(cat "${out}")"

  local want="${ns:-wardyn}-internal"
  grep -q "docker network inspect ${want}\b" "${calllog}" \
    || fail "${name}: reset-all never inspected ${want} — docker calls were: $(grep '^docker network' "${calllog}" || true)"
  grep -q "\[present\] docker network ${want} " "${out}" \
    || fail "${name}: manifest did not name ${want} — output was: $(cat "${out}")"

  # The teardown half, not just the read half: the removal used the same literal.
  if [ "${dry}" != 1 ]; then
    grep -q "docker network rm ${want}\b" "${calllog}" \
      || fail "${name}: reset-all never removed ${want} — docker calls were: $(grep '^docker network' "${calllog}" || true)"
  fi

  # And it must never reach for the DEFAULT namespace's objects when it is not
  # the default namespace: that is the foreign-install blast radius. `if grep`,
  # never `grep && fail` — under set -e a bare grep that matches nothing is a
  # failing command, and the negative case is the one that must pass silently.
  if [ "${want}" != "wardyn-internal" ]; then
    if grep -qE 'docker network (inspect|rm) wardyn-internal\b' "${calllog}"; then
      fail "${name}: reset-all touched the FOREIGN default network wardyn-internal — $(grep '^docker network' "${calllog}")"
    fi
    if grep -q 'wardyn-internal' "${out}"; then
      fail "${name}: manifest still names the foreign default network — $(cat "${out}")"
    fi
  fi
  echo "ok  ${name}"
}

run_case "default-ns-dry-run"        ""        1
run_case "namespaced-dry-run"        "wardyn2" 1
run_case "namespaced-removal-path"   "wardyn2" 0

echo "PASS scripts/test-reset-network-ns.sh"
