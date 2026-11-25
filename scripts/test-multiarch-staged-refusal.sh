#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-multiarch-staged-refusal.sh — the two-platform claude-code target must refuse a
# staged AWS CLI zip: the image copies that one zip into both platforms, and a zip is one
# architecture's executable. Without the staging option the same target must still reach
# docker buildx.
#
# Daemon-free: a stub docker on PATH records its arguments, so nothing is built. The target runs against a copy of the
# Makefile in an empty directory, so a binary a developer has staged in the checkout cannot change the outcome, and the
# positive case sets AWS_CLI_INSTALL= itself, so one exported in the caller's environment cannot either.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "FAIL: $*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
printf '#!/bin/sh\necho "$@" >> "%s/docker.calls"\n' "$WORK" >"$WORK/docker"
chmod +x "$WORK/docker"

mkdir "$WORK/tree"
cp "$ROOT/Makefile" "$WORK/tree/Makefile"
run() { (cd "$WORK/tree" && PATH="$WORK:$PATH" make agent-image-claude-code-multiarch CLAUDE_CODE_IMAGE=registry.invalid/t/c:1 "$@") >"$WORK/out" 2>&1; }

: >"$WORK/docker.calls"
if run AWS_CLI_INSTALL=staged; then fail "AWS_CLI_INSTALL=staged was accepted at the multiarch target"; fi
grep -q 'AWS_CLI_INSTALL=staged' "$WORK/out" || fail "the refusal does not name AWS_CLI_INSTALL=staged: $(cat "$WORK/out")"
[ ! -s "$WORK/docker.calls" ] || fail "docker ran despite the refusal: $(cat "$WORK/docker.calls")"

run AWS_CLI_INSTALL= || fail "the target refused without the staging option: $(cat "$WORK/out")"
grep -q 'buildx build' "$WORK/docker.calls" || fail "docker buildx was not reached without the staging option"
echo "ok: multiarch target refuses a staged AWS CLI zip and builds without one"
