#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Runs the distinct-UID permission proofs as root in a throwaway container:
# TestFSStore_DifferentUIDs (internal/recording) and
# TestCopyToDir_DifferentUIDPermissions (cmd/wardyn-rec) really drop to the
# shipped daemon and agent UIDs, so they skip on every ordinary (non-root) test
# run, including every CI suite. A skip here is a failure: the proof must have
# run and passed, else a regression in the recording store's lock/marker modes or
# the rooted copy fallback would merge green with the proof never executed.
#
# Usage: scripts/test-root-uid.sh   (exit 0 = both ran and passed). Needs go and
# a docker daemon; the static test binaries run in busybox, offline.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"
source "${REPO_ROOT}/scripts/lib/common.sh"

IMAGE="${WARDYN_ROOT_UID_IMAGE:-busybox:1.38}" # the pin ci.yml's base-image mirror primes
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
# The tests drop to uid 1000 and 65532 inside the container and must still reach the binary.
chmod 755 "${WORK}"

log "building the test binaries"
CGO_ENABLED=0 go test -c -o "${WORK}/recording.test" ./internal/recording
CGO_ENABLED=0 go test -c -o "${WORK}/wardyn-rec.test" ./cmd/wardyn-rec
chmod 755 "${WORK}"/*.test

ensure_image "${IMAGE}" || die "cannot obtain ${IMAGE}"
for proof in recording.test:TestFSStore_DifferentUIDs wardyn-rec.test:TestCopyToDir_DifferentUIDPermissions; do
  bin="${proof%%:*}" name="${proof#*:}"
  log "running ${name} as root"
  rc=0
  out="$(docker run --rm --pull=never --network none --user 0:0 -v "${WORK}:/w:ro" "${IMAGE}" \
    "/w/${bin}" -test.run "^${name}\$" -test.count=1 -test.v 2>&1)" || rc=$?
  printf '%s\n' "${out}" | tail -n 12
  [ "${rc}" -eq 0 ] || die "${name} failed (exit ${rc})"
  # A here-string, not a pipe: grep -q closing early would SIGPIPE a producer under pipefail.
  grep -q "^--- PASS: ${name} " <<<"${out}" || die "${name} did not report PASS: a skip is a failure here"
done
log "both distinct-UID proofs ran and passed"
