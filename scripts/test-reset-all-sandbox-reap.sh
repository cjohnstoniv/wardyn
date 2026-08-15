#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Regression test for W29-S1-3: reset-all's manifest must surface live per-run
# sandbox containers (label wardyn.managed=true) and their internal networks
# (wardyn-int-*) — objects `compose down` never touches because wardynd
# creates them outside the compose project (internal/runner/docker/naming.go).
# Before the fix, `scripts/up.sh reset-all --dry-run` never mentioned them at
# all, so "FULL undo ... leaves the box clean" was false whenever a run was
# mid-flight.
#
# Uses REAL docker objects (a throwaway container + network, labeled/named
# exactly as wardynd's docker runner would) and ONLY ever calls reset-all
# with --dry-run, which is read-only by construction (exits before the
# consent prompt / compose down / any docker rm) — safe to run against a real
# daemon that may have unrelated stacks running.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MARKER="wardyn-test-resetall-$$"
CONTAINER="${MARKER}"
NETWORK="wardyn-int-${MARKER}"

cleanup() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail=0

# A throwaway per-run sandbox container: same label wardynd stamps on every
# object it owns (wardynLabels, naming.go), just not actually attached to
# wardynd — reset-all's selector is label-only, so this is sufficient.
docker create --label wardyn.managed=true --name "$CONTAINER" busybox:latest sleep 300 >/dev/null

# A throwaway per-run internal network, named exactly as internalNetName()
# would (wardyn-int-<runID>) — the reset-all filter matches on the prefix.
docker network create --internal "$NETWORK" >/dev/null

out="$(./scripts/up.sh reset-all --dry-run 2>&1)"

check() {  # $1=pattern $2=description
    if echo "$out" | grep -qE "$1"; then
        echo "ok: $2"
    else
        echo "FAIL: $2 — manifest did not match /$1/" >&2
        echo "$out" >&2
        fail=1
    fi
}

check '\[present\] live per-run sandbox containers: [1-9]' "manifest reports the live sandbox container as present"
check '\[present\] per-run internal networks: [1-9]' "manifest reports the live wardyn-int-* network as present"

echo "--- test-reset-all-sandbox-reap: $([[ $fail -eq 0 ]] && echo PASS || echo FAIL) ---"
exit "$fail"
