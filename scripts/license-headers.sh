#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# SPDX header gate + fixer: every tracked .go/.ts/.tsx/.css/.sh file (excluding
# generated, vendored, and the MIT-origin shadcn ui/ primitives) must carry the
# SPDX/copyright header — the exact scope is license_scope_files() in
# scripts/lib/common.sh. YAML, Dockerfiles and .mjs are outside this gate, so a
# green run does not mean "every tracked file is marked".
#
# The header above is HAND-maintained: addlicense's has-a-license test is a
# substring scan for "copyright" over the first 1000 bytes, which this file's own
# prose trips, so --fix silently skips it and -check silently passes it.
#
#   scripts/license-headers.sh          # CHECK — what `make license-headers` / CI runs
#   scripts/license-headers.sh --fix    # APPLY — `make license-headers ARGS=fix`
#
# addlicense is `go run` at a PINNED version only (the zero-install pattern the
# staticcheck/govulncheck targets already use). The gate and the fixer therefore
# cannot resolve DIFFERENT addlicense builds and disagree, which the previous
# split pair (an ADDLICENSE env / on-PATH / GOPATH ladder in each) allowed.
# That ladder was the offline escape hatch, and its removal is deliberate: this
# gate now needs a warm module cache or a reachable module proxy. Air-gapped?
# Pre-warm with `go install github.com/google/addlicense@v1.1.1` once (which
# populates the same cache `go run` reads) rather than reintroducing an override.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
source "$ROOT/scripts/lib/common.sh"

ADDL=github.com/google/addlicense@v1.1.1
ARGS=(-s=only -l apache -c "The Wardyn Authors" -y 2025)

# File scope is defined once in scripts/lib/common.sh.
mapfile -t FILES < <(license_scope_files)

if [ "${1:-}" = "--fix" ]; then
  go run "$ADDL" "${ARGS[@]}" "${FILES[@]}"
  echo "license-headers: applied to ${#FILES[@]} files (idempotent)"
else
  go run "$ADDL" -check "${ARGS[@]}" "${FILES[@]}"
  echo "license-headers: PASS (${#FILES[@]} files)"
fi
