#!/usr/bin/env bash
# SPDX header gate: every tracked source file (excluding generated, vendored,
# and the MIT-origin shadcn ui/ primitives) must carry the SPDX/copyright
# header. Run scripts/add-license-headers.sh to fix a failure.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Resolve addlicense without depending on it being pre-installed on PATH: an
# on-PATH binary, then a go-installed one, else `go run` the pinned version
# (the same zero-install pattern the staticcheck/govulncheck CI jobs use).
if [ -n "${ADDLICENSE:-}" ]; then ADDL=("$ADDLICENSE")
elif command -v addlicense >/dev/null 2>&1; then ADDL=(addlicense)
elif [ -x "$(go env GOPATH)/bin/addlicense" ]; then ADDL=("$(go env GOPATH)/bin/addlicense")
else ADDL=(go run github.com/google/addlicense@v1.1.1); fi

# The single source of truth for which files are in scope (shared with the fixer).
mapfile -t FILES < <(git ls-files '*.go' '*.ts' '*.tsx' '*.css' \
  | grep -vE '^ui/(node_modules|dist)/' \
  | grep -vE '\.gen\.go$|_gen\.go$|zz_generated' \
  | grep -vE '^ui/src/app/components/ui/')

"${ADDL[@]}" -check -s=only -l apache -c "The Wardyn Authors" -y 2025 "${FILES[@]}"
echo "license-headers: PASS (${#FILES[@]} files)"
