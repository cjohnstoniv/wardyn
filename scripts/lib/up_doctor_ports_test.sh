#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for W1-S1-2: `scripts/up.sh doctor` checked WARDYN_UP_PORT
# and postgres's 5432 for a pre-existing listener, but missed two other ports
# the compose stack always publishes: the registry (5010, auto-started via
# postgres/dex/wardynd's depends_on — not opt-in) and wardynd's SSH gateway
# mapping (2222, always published whether or not the gateway is enabled). A
# collision on either used to surface only as a silent compose bring-up
# failure with no preflight warning.
#
# Runs the REAL `scripts/up.sh doctor` (read-only, no side effects) against a
# deterministically-occupied port (a background TCP listener this test binds
# itself) so the assertion does not depend on the host's actual port state.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if ! command -v docker >/dev/null 2>&1; then
  echo "SKIP: docker not available to run scripts/up.sh doctor" >&2
  exit 0
fi

port_file="$(mktemp)"
python3 - "${port_file}" <<'PYEOF' &
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 0))
with open(sys.argv[1], "w") as f:
    f.write(str(s.getsockname()[1]))
s.listen(1)
s.accept()
PYEOF
listener_pid=$!

cleanup() {
  kill "${listener_pid}" >/dev/null 2>&1 || true
  rm -f "${port_file}"
}
trap cleanup EXIT

for _ in $(seq 1 50); do
  [[ -s "${port_file}" ]] && break
  sleep 0.1
done
port="$(cat "${port_file}")"
if [[ -z "${port}" ]]; then
  echo "FAIL: test harness could not bind a listener port" >&2
  exit 1
fi

out="$(WARDYN_REGISTRY_PORT="${port}" "${REPO_ROOT}/scripts/up.sh" doctor 2>&1)"

if ! printf '%s' "${out}" | grep -q "port ${port} already in use"; then
  echo "FAIL: doctor did not warn about the occupied registry port ${port}" >&2
  echo "--- doctor output ---" >&2
  printf '%s\n' "${out}" >&2
  exit 1
fi

# The SSH gateway port check must also exist (asserted against its default,
# since occupying a second real port for it is redundant coverage of the same
# code path already proven above).
if ! printf '%s' "${out}" | grep -qE "port 2222 (free|already in use)"; then
  echo "FAIL: doctor has no port check for the SSH gateway mapping (2222)" >&2
  echo "--- doctor output ---" >&2
  printf '%s\n' "${out}" >&2
  exit 1
fi

echo "ok - doctor warns on an occupied registry port and checks the SSH gateway port"
