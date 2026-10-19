#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Pins that every long-lived compose service other than the privileged tetragon
# sensor drops NET_RAW. Local mode trusts the published port's source address on
# the control-plane network (WARDYN_LOCAL_TRUST_FORWARDER); without NET_RAW no
# service there can forge that address on the bridge. Daemon-free: it reads
# `docker compose config`, so it needs the compose plugin but no running daemon.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "FAIL: $*" >&2; echo "--- test-compose-cap-drop: FAIL ---"; exit 1; }

if ! docker compose version >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: needs the docker compose plugin and jq"
  exit 0
fi

cfg="$(docker compose --profile groundtruth --profile sso -f "$ROOT/deploy/compose/docker-compose.yaml" config --format json)" \
  || fail "docker compose config did not parse deploy/compose/docker-compose.yaml"

for svc in wardynd registry postgres dex wardyn-tetragon-ingest; do
  echo "$cfg" | jq -e --arg s "$svc" '.services[$s].cap_drop // [] | index("NET_RAW") != null' >/dev/null \
    || fail "service $svc does not drop NET_RAW (cap_drop: [NET_RAW]); see the local-mode forwarder residual in threatmodel/THREAT-MODEL.md"
  echo "ok: $svc drops NET_RAW"
done

# The sensor needs the full privilege set; a cap_drop there would be a different change.
echo "$cfg" | jq -e '.services.tetragon.privileged == true' >/dev/null \
  || fail "tetragon is no longer the privileged host sensor; revisit which services this test covers"
echo "--- test-compose-cap-drop: PASS ---"
