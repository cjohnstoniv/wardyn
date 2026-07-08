#!/usr/bin/env bash
# Add the SPDX/copyright header to any tracked source file missing it. Idempotent.
# CI enforces this via scripts/check-license-headers.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ADDLICENSE="${ADDLICENSE:-addlicense}"
command -v "$ADDLICENSE" >/dev/null 2>&1 || ADDLICENSE="$(go env GOPATH)/bin/addlicense"
[ -x "$ADDLICENSE" ] || { echo "addlicense not found — go install github.com/google/addlicense@v1.1.1"; exit 1; }

mapfile -t FILES < <(git ls-files '*.go' '*.ts' '*.tsx' '*.css' \
  | grep -vE '^ui/(node_modules|dist)/' \
  | grep -vE '\.gen\.go$|_gen\.go$|zz_generated' \
  | grep -vE '^ui/src/app/components/ui/')

"$ADDLICENSE" -s=only -l apache -c "The Wardyn Authors" -y 2025 "${FILES[@]}"
echo "headers applied to ${#FILES[@]} files (idempotent)"
