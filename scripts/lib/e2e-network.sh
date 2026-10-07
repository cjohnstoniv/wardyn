#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# A port-only override must not reopen the host's interfaces. Pin localhost too,
# so real-tmux safety never depends on DNS or a machine's hosts file.
e2e_listen_addr() {
  if [[ "$1" =~ ^:[0-9]+$ || "$1" == localhost:* ]]; then
    printf '127.0.0.1:%s\n' "${1##*:}"
  else
    printf '%s\n' "$1"
  fi
}

e2e_base_url() {
  case "${1%:*}" in
    127.0.0.1|0.0.0.0) printf 'http://localhost:%s\n' "${1##*:}" ;;
    '[::]') printf 'http://[::1]:%s\n' "${1##*:}" ;;
    *) printf 'http://%s\n' "$1" ;;
  esac
}

e2e_require_loopback() {
  python3 - "$@" <<'PY'
import ipaddress
import sys

for addr in sys.argv[1:]:
    host = addr.rsplit(":", 1)[0].strip("[]")
    try:
        if ipaddress.ip_address(host).is_loopback:
            continue
    except ValueError:
        pass
    sys.exit(f"WARDYN_E2E_TMUX=1 refuses non-loopback listener {addr}; use a literal loopback address")
PY
}
