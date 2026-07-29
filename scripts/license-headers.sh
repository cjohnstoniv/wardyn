#!/usr/bin/env bash
# SPDX header gate + fixer: every tracked source file (excluding generated,
# vendored, and the MIT-origin shadcn ui/ primitives) must carry the
# SPDX/copyright header.
#
#   scripts/license-headers.sh          # CHECK — what `make license-headers` / CI runs
#   scripts/license-headers.sh --fix    # APPLY — `make license-headers ARGS=fix`
#
# addlicense is `go run` at a PINNED version only (the zero-install pattern the
# staticcheck/govulncheck targets already use). The gate and the fixer therefore
# cannot resolve DIFFERENT addlicense builds and disagree, which the previous
# split pair (an ADDLICENSE env / on-PATH / GOPATH ladder in each) allowed.
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
