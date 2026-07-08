#!/usr/bin/env bash
# Add the SPDX/copyright header to any tracked source file missing it. Idempotent.
# CI enforces this via scripts/check-license-headers.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [ -n "${ADDLICENSE:-}" ]; then ADDL=("$ADDLICENSE")
elif command -v addlicense >/dev/null 2>&1; then ADDL=(addlicense)
elif [ -x "$(go env GOPATH)/bin/addlicense" ]; then ADDL=("$(go env GOPATH)/bin/addlicense")
else ADDL=(go run github.com/google/addlicense@v1.1.1); fi

mapfile -t FILES < <(git ls-files '*.go' '*.ts' '*.tsx' '*.css' \
  | grep -vE '^ui/(node_modules|dist)/' \
  | grep -vE '\.gen\.go$|_gen\.go$|zz_generated' \
  | grep -vE '^ui/src/app/components/ui/')

"${ADDL[@]}" -s=only -l apache -c "The Wardyn Authors" -y 2025 "${FILES[@]}"
echo "headers applied to ${#FILES[@]} files (idempotent)"
