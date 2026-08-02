#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-image-pins.sh — every image a Dockerfile FROM or a compose file pulls is
# digest-pinned.
#
# FAILS if a Dockerfile `FROM` (a registry image, not a build-stage alias) or a
# compose registry image lacks an @sha256 digest. A floating tag lets an
# attacker who controls the upstream tag swap the image contents; the digest
# freezes exactly what we build/run.
#
# SCOPE is those two file sets only — images pulled from anywhere else (e.g. the
# Makefile's throwaway test infra: registry:2, postgres:17, alpine:latest) are
# deliberately outside this gate, so do not read a green run as "nothing floats".
#
# Exempt:
#   - compose `*:local` tags   — locally BUILT images (they carry a `build:` stanza)
#   - $ALLOWLIST_REF            — the one documented local retag of a :local image
# Run via `make lint`.
set -euo pipefail
cd "$(dirname "$0")/.."

# Matched on the REF, not the file: exempting a whole Dockerfile would silently
# waive every OTHER FROM in it (full/ also pulls digest-pinned toolchain stages).
ALLOWLIST_REF="wardyn/agent-claude-code:local"  # deploy/images/full/Dockerfile's base — a locally BUILT image, so no upstream digest exists
fail=0

# ── Dockerfile FROMs ────────────────────────────────────────────────────────
while IFS= read -r df; do
  # Build-stage aliases (FROM <img> AS <alias>) get re-referenced by name later
  # (FROM <alias>); those refs are not external images and need no digest.
  mapfile -t aliases < <(grep -iE '^FROM .+ [Aa][Ss] ' "$df" | sed -E 's/.* [Aa][Ss] +([^ ]+).*/\1/')
  while IFS= read -r line; do
    ref=$(echo "$line" | awk '{print $2}')
    # `FROM ${VAR}` is BuildKit's global-ARG stage selector (e.g. UI_STAGE picks
    # ui-build vs ui-prebuilt). Resolve it against the Dockerfile's own
    # `ARG VAR=default` so the alias check below sees the stage name. Only the
    # default is knowable here; if it resolves to a registry image rather than a
    # stage alias, the digest requirement below still applies.
    if [[ "$ref" =~ ^\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?$ ]]; then
      argdef=$(grep -iE "^ARG ${BASH_REMATCH[1]}=" "$df" | head -1 | sed -E 's/^[^=]*=//' || true)
      [[ -n "$argdef" ]] && ref="$argdef"
    fi
    skip=0
    for a in "${aliases[@]:-}"; do
      if [[ -n "$a" && "$ref" == "$a" ]]; then skip=1; break; fi
    done
    ((skip)) && continue
    if [[ "$ref" != *"@sha256:"* ]]; then
      if [[ "$ref" == "$ALLOWLIST_REF" ]]; then continue; fi
      echo "FAIL: $df: FROM '$ref' is not digest-pinned (@sha256). Pin it: FROM $ref@sha256:<digest>." >&2
      fail=1
    fi
  done < <(grep -iE '^FROM ' "$df")
done < <(find . \( -name 'Dockerfile' -o -name 'Dockerfile.*' \) ! -path './.git/*' ! -path './ui/node_modules/*' ! -path './.claude/*')

# ── compose registry images (base AND every overlay: ci-run.sh starts the stack
#    with -f docker-compose.yaml -f docker-compose.ci.yaml) ──────────────────
for compose in ./deploy/compose/docker-compose*.yaml; do
  while IFS= read -r img; do
    [[ "$img" == *:local ]] && continue        # locally-built stanza (has `build:`)
    if [[ "$img" != *"@sha256:"* ]]; then
      echo "FAIL: $compose: image '$img' is not digest-pinned (@sha256). Pin it: '$img@sha256:<digest>'." >&2
      fail=1
    fi
  done < <(grep -E '^[[:space:]]*image:' "$compose" | awk '{print $2}')
done

if ((fail)); then
  exit 1
fi
echo "check-image-pins: OK (all Dockerfile FROMs and compose registry images are digest-pinned)"
