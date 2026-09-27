#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0

# Pre-release memory stress for the wardyn-proxy sidecar's inspection path.
#
# Runs internal/egress/proxy's TestStressInspectionUnderProxyCgroup (four
# concurrent in-cap LLM bodies plus one push at the highest
# max_inspect_pack_mib) inside a container with the sidecar's hard 256 MiB
# memory cap, swap pinned equal, and the GC soft limit wardyn-proxy derives from
# that cap. Fails if any request is refused or the container is OOM-killed.
#
# Usage: scripts/stress-proxy-cgroup.sh   (exit 0 = PASS). Needs go, git and a
# docker daemon; uses busybox:latest to run the static test binary.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
source "${REPO_ROOT}/scripts/lib/common.sh"

MEM_BYTES=$((256 << 20))            # internal/runner/docker's proxyMemoryMiB
GOMEMLIMIT_BYTES=$((MEM_BYTES * 8 / 10)) # cmd/wardyn-proxy's cgroupMemoryHeadroom
IMAGE=busybox:latest
NAME="stress-proxy-cgroup-$$"
WORK="$(mktemp -d)"
cleanup() {
  docker rm -f "${NAME}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

log "building the proxy test binary"
CGO_ENABLED=0 go test -c -o "${WORK}/proxy.test" ./internal/egress/proxy

log "recording a push at the inspection ceiling"
WARDYN_STRESS_RECORD_PUSH="${WORK}/push.bin" \
  "${WORK}/proxy.test" -test.run '^TestStressRecordMaxInspectPush$' -test.count=1 >/dev/null
[ -s "${WORK}/push.bin" ] || die "no push was recorded"

ensure_image "${IMAGE}" || die "cannot obtain ${IMAGE}"
log "running the inspection load under a $((MEM_BYTES >> 20)) MiB cgroup"
rc=0
docker run --name "${NAME}" --pull=never \
  --memory "${MEM_BYTES}" --memory-swap "${MEM_BYTES}" \
  -e GOMEMLIMIT="${GOMEMLIMIT_BYTES}" -e WARDYN_STRESS_PUSH_BODY=/w/push.bin \
  -v "${WORK}:/w:ro" "${IMAGE}" \
  /w/proxy.test -test.run '^TestStressInspectionUnderProxyCgroup$' -test.count=1 -test.v || rc=$?

[ "$(docker inspect -f '{{.State.OOMKilled}}' "${NAME}")" = false ] ||
  die "the proxy's inspection load was OOM-killed under the sidecar's memory cap"
[ "${rc}" -eq 0 ] || die "the stress test failed (exit ${rc})"
log "PASS: inspection load held under the sidecar's memory cap"
