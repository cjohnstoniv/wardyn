#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-image-pins.sh — every externally-pulled image is digest-pinned.
#
# FAILS if a Dockerfile `FROM` (a registry image, not a build-stage alias) or a
# compose registry image lacks an @sha256 digest. A floating tag lets an
# attacker who controls the upstream tag swap the image contents; the digest
# freezes exactly what we build/run.
#
# Exempt:
#   - compose `*:local` tags   — locally BUILT images (they carry a `build:` stanza)
#   - $ALLOWLIST_DOCKERFILE     — the one documented local retag of a :local image
# Run via `make lint`.
set -euo pipefail
cd "$(dirname "$0")/.."

ALLOWLIST_DOCKERFILE="./deploy/images/full/Dockerfile"  # FROM wardyn/agent-claude-code:local (local retag)
fail=0

# ── Dockerfile FROMs ────────────────────────────────────────────────────────
while IFS= read -r df; do
  # Build-stage aliases (FROM <img> AS <alias>) get re-referenced by name later
  # (FROM <alias>); those refs are not external images and need no digest.
  mapfile -t aliases < <(grep -iE '^FROM .+ [Aa][Ss] ' "$df" | sed -E 's/.* [Aa][Ss] +([^ ]+).*/\1/')
  while IFS= read -r line; do
    ref=$(echo "$line" | awk '{print $2}')
    skip=0
    for a in "${aliases[@]:-}"; do
      if [[ -n "$a" && "$ref" == "$a" ]]; then skip=1; break; fi
    done
    ((skip)) && continue
    if [[ "$ref" != *"@sha256:"* ]]; then
      if [[ "$df" == "$ALLOWLIST_DOCKERFILE" ]]; then continue; fi
      echo "FAIL: $df: FROM '$ref' is not digest-pinned (@sha256). Pin it: FROM $ref@sha256:<digest>." >&2
      fail=1
    fi
  done < <(grep -iE '^FROM ' "$df")
done < <(find . \( -name 'Dockerfile' -o -name 'Dockerfile.*' \) ! -path './.git/*' ! -path './ui/node_modules/*')

# ── compose registry images ─────────────────────────────────────────────────
compose="./deploy/compose/docker-compose.yaml"
while IFS= read -r img; do
  [[ "$img" == *:local ]] && continue          # locally-built stanza (has `build:`)
  if [[ "$img" != *"@sha256:"* ]]; then
    echo "FAIL: $compose: image '$img' is not digest-pinned (@sha256). Pin it: '$img@sha256:<digest>'." >&2
    fail=1
  fi
done < <(grep -E '^[[:space:]]*image:' "$compose" | awk '{print $2}')

if ((fail)); then
  exit 1
fi
echo "check-image-pins: OK (all Dockerfile FROMs and compose registry images are digest-pinned)"
