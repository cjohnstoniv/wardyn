#!/usr/bin/env bash
# SPDX header gate: every tracked source file (excluding generated, vendored,
# and the MIT-origin shadcn ui/ primitives) must carry the SPDX/copyright
# header. Run scripts/add-license-headers.sh to fix a failure.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ADDLICENSE="${ADDLICENSE:-addlicense}"
command -v "$ADDLICENSE" >/dev/null 2>&1 || ADDLICENSE="$(go env GOPATH)/bin/addlicense"
[ -x "$ADDLICENSE" ] || { echo "addlicense not found — go install github.com/google/addlicense@v1.1.1"; exit 1; }

# The single source of truth for which files are in scope (shared with the fixer).
mapfile -t FILES < <(git ls-files '*.go' '*.ts' '*.tsx' '*.css' \
  | grep -vE '^ui/(node_modules|dist)/' \
  | grep -vE '\.gen\.go$|_gen\.go$|zz_generated' \
  | grep -vE '^ui/src/app/components/ui/')

"$ADDLICENSE" -check -s=only -l apache -c "The Wardyn Authors" -y 2025 "${FILES[@]}"
echo "license-headers: PASS (${#FILES[@]} files)"
