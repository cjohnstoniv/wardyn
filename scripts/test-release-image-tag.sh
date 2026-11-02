#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-release-image-tag.sh — release.yml turns github.ref_name into an image
# tag. A vX.Y.Z tag push gives X.Y.Z; a dry-run dispatch on a branch such as
# train/0.10-rc9-w1 must still give a valid tag (`/` is not allowed in one), or
# every image build fails with "invalid reference format".
#
# Daemon-free, network-free: each "Compute the release tag" step's run script
# is pulled out of the workflow and run with a chosen REF_NAME.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WF="$ROOT/.github/workflows/release.yml"
fail() { echo "FAIL: $*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# The run block of every step named "Compute the release tag".
awk '
  /^      - name: Compute the release tag$/ { n++; want = 1; next }
  want && /^        run: \|$/ { grab = 1; next }
  want && /^        run: / { sub(/^        run: /, ""); print > (dir "/step" n ".sh"); want = 0; next }
  grab && /^          / { sub(/^          /, ""); print > (dir "/step" n ".sh"); next }
  grab { grab = 0; want = 0 }
' dir="$WORK" "$WF"
steps="$(find "$WORK" -name 'step*.sh' | wc -l)"
[ "$steps" -eq 2 ] || fail "want 2 'Compute the release tag' steps in release.yml, found $steps"

for s in "$WORK"/step*.sh; do
  while IFS='|' read -r ref want; do
    out="$WORK/out"; : >"$out"
    REF_NAME="$ref" GITHUB_OUTPUT="$out" bash "$s"
    [ "$(cat "$out")" = "tag=$want" ] || fail "$(basename "$s"): ref '$ref': got [$(cat "$out")], want [tag=$want]"
  done <<'CASES'
v0.10.0|0.10.0
v0.10.0-rc.9|0.10.0-rc.9
train/0.10-rc9-w1|train-0.10-rc9-w1
main|main
CASES
done

# The promote job derives the same tag from REF_NAME.
grep -q 'RELEASE_TAG="${RELEASE_TAG//\\//-}"' "$WF" || fail "the promote step does not replace '/' in the tag"

echo "test-release-image-tag: ok"
