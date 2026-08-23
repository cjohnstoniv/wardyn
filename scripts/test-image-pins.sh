#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-image-pins.sh — check-image-pins.sh still SEES a `FROM` that carries a
# flag.
#
# Why this exists: the gate reads the image ref out of the FROM line by field
# position. Adding `FROM --platform=$BUILDPLATFORM <image>` to the cross-
# compiling build stages moved the ref from field 2 to field 3, and the first
# symptom was the gate failing every pinned stage. The DANGEROUS symptom is the
# mirror image — a future parse change that makes the gate skip flagged lines
# entirely would leave it green while an unpinned base slipped in. A security
# gate that stops detecting is worse than no gate, so assert BOTH directions.
#
# Daemon-free, network-free: it runs the real gate against a throwaway tree.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/scripts"
cp "$ROOT/scripts/check-image-pins.sh" "$TMP/scripts/"

fail() { echo "FAIL: $*" >&2; exit 1; }

# The gate resolves its own repo root from its location, so running the COPY
# scans $TMP and nothing else.
run_gate() { (cd "$TMP" && ./scripts/check-image-pins.sh >/dev/null 2>&1); }

# 1. flagged AND pinned => pass. (Regression: the ref used to be read as the
#    flag itself, so this shape failed as "not digest-pinned".)
cat > "$TMP/Dockerfile" <<'EOF'
FROM --platform=$BUILDPLATFORM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc AS builder
FROM builder
EOF
run_gate || fail "a flagged, digest-pinned FROM must PASS"
echo "ok  flagged + pinned passes"

# 2. flagged and UNPINNED => fail. This is the one that matters: the gate must
#    not skip a line just because it carries a flag.
cat > "$TMP/Dockerfile" <<'EOF'
FROM --platform=$BUILDPLATFORM alpine:3.20
EOF
if run_gate; then fail "a flagged, UNPINNED FROM must FAIL — the gate went blind"; fi
echo "ok  flagged + unpinned fails"

# 3. unflagged and unpinned => fail (the gate's original job, unchanged).
cat > "$TMP/Dockerfile" <<'EOF'
FROM alpine:3.20
EOF
if run_gate; then fail "an unflagged, UNPINNED FROM must FAIL"; fi
echo "ok  unflagged + unpinned fails"

echo "test-image-pins: PASS"
