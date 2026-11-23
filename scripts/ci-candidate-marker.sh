#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# ci-candidate-marker.sh — ci.yml's train-suites job asks whether this head is the release candidate, so the nightly it
# dispatches stages images (publish=true). It prints `true` only when HEAD's commit message has a line that is exactly
# `Candidate: v<V>`, V being internal/version/version.go's Version in the same tree, and `false` otherwise; a marker
# naming another version prints `false` and a warning. Run from the repository root at the head commit.
set -euo pipefail
v="$(sed -n 's/^const Version = "\(.*\)"$/\1/p' internal/version/version.go 2>/dev/null || true)"
[ -n "$v" ] || { echo "::error::internal/version/version.go has no Version constant; cannot judge the candidate marker" >&2; exit 1; }
msg="$(git log -1 --format=%B HEAD)"
if grep -qxF "Candidate: v$v" <<<"$msg"; then
  echo true
else
  other="$(grep -xE 'Candidate: v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?' <<<"$msg" | head -1 || true)"
  [ -z "$other" ] || echo "::warning::the head says '$other' but its tree is version $v, so this round does not stage images" >&2
  echo false
fi
