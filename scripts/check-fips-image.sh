#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Prove that a pushed wardynd-fips image holds a binary built against the pinned
# Go Cryptographic Module snapshot:
#   scripts/check-fips-image.sh <image-ref> <snapshot>
# <image-ref> should name a digest. For each platform in the image index the
# script copies /wardynd out (no container is started, so no emulation is
# needed) and requires `go version -m` to print exactly `build GOFIPS140=<snapshot>`.
# GODEBUG=fips140=only at run time is not this check: it selects no snapshot and
# passes for an ordinary build.
# Exit 0 only when every platform matches.
set -euo pipefail
REF="${1:?usage: check-fips-image.sh <image-ref> <snapshot>}"
PIN="${2:?usage: check-fips-image.sh <image-ref> <snapshot>}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for platform in linux/amd64 linux/arm64; do
  cid="$(docker create --platform "$platform" "$REF" /wardynd)"
  docker cp "$cid:/wardynd" "$work/wardynd"
  docker rm "$cid" >/dev/null
  go version -m "$work/wardynd" > "$work/info"
  if awk -v want="GOFIPS140=${PIN}" '$1 == "build" && $2 == want { found = 1 } END { exit !found }' "$work/info"; then
    echo "OK   ${platform}: build GOFIPS140=${PIN}"
  else
    echo "FAIL ${platform}: no 'build GOFIPS140=${PIN}' line in go version -m of ${REF}:/wardynd" >&2
    grep GOFIPS140 "$work/info" >&2 || echo "     (no GOFIPS140 line at all: an ordinary build)" >&2
    exit 1
  fi
done
