#!/usr/bin/env bash
# SPDX header gate: every tracked source file (excluding generated, vendored,
# and the MIT-origin shadcn ui/ primitives) must carry the SPDX/copyright
# header. Run scripts/add-license-headers.sh to fix a failure.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
source "$ROOT/scripts/lib/common.sh"

# Resolve addlicense without depending on it being pre-installed on PATH: an
# on-PATH binary, then a go-installed one, else `go run` the pinned version
# (the same zero-install pattern the staticcheck/govulncheck CI jobs use).
if [ -n "${ADDLICENSE:-}" ]; then ADDL=("$ADDLICENSE")
elif command -v addlicense >/dev/null 2>&1; then ADDL=(addlicense)
elif [ -x "$(go env GOPATH)/bin/addlicense" ]; then ADDL=("$(go env GOPATH)/bin/addlicense")
else ADDL=(go run github.com/google/addlicense@v1.1.1); fi

# File scope is defined once in scripts/lib/common.sh (shared with the fixer).
mapfile -t FILES < <(license_scope_files)

"${ADDL[@]}" -check -s=only -l apache -c "The Wardyn Authors" -y 2025 "${FILES[@]}"
echo "license-headers: PASS (${#FILES[@]} files)"
