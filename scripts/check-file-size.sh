#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-file-size.sh — hold the line on god-files.
#
# FAILS when any NON-allowlisted, non-test .go file (or ui/src/**/*.ts,*.tsx,
# or scripts/**/*.sh file) exceeds THRESHOLD lines, or when an allowlisted
# legacy file grows
# materially past its frozen cap (current size at gate introduction + ~8%
# headroom). The allowlist is the small set of pre-existing 1000+ line files
# that are cohesive as-is (see ARCHITECTURE.md "Large files") — new entries
# need the same written justification there, not a silent edit here.
#
# Companion to .golangci.yml (funlen/gocyclo/gocognit gate functions; this
# gates files). Run via `make lint`.
set -euo pipefail
cd "$(dirname "$0")/.."

THRESHOLD=1000

# path -> frozen cap (lines at 2026-07-16 + headroom). Shrinking is always fine.
declare -A ALLOWLIST=(
  ["./internal/workspacescan/detect.go"]=1340 # 1244 at freeze
  ["./internal/runner/docker/driver.go"]=1230 # 1137 at freeze
  ["./internal/api/workspace_run.go"]=1180 # 1092 at freeze
  ["./internal/api/setup.go"]=1120         # 1028 at freeze
  ["./ui/src/app/components/screens/import-workspace/import-panel.tsx"]=1420 # 1312 at freeze
  ["./ui/src/app/components/screens/setup/step-bodies.tsx"]=1150 # 1062 at freeze
  # The build-plumbing shell was unpoliced until 2026-07-29 and the three biggest
  # files are already near THRESHOLD. Their caps are frozen BELOW it so they stop
  # growing without forcing a refactor now; every other scripts/*.sh is gated at
  # the plain 1000-line threshold.
  ["./scripts/up.sh"]=990         # 918 at freeze
  ["./scripts/setup.sh"]=890      # 828 at freeze
  ["./scripts/test-drive.sh"]=870 # 806 at freeze
  # compose.go and store.go were decomposed below 1000 in R4 (llmcred.go /
  # pagination.go splits) and came OFF this list — they are gated at the plain
  # 1000-line threshold like any other file now.
)

fail=0
while IFS= read -r f; do
  lines=$(wc -l <"$f")
  cap=${ALLOWLIST[$f]:-$THRESHOLD}
  if ((lines > cap)); then
    if [[ -n "${ALLOWLIST[$f]:-}" ]]; then
      echo "FAIL: $f has $lines lines — allowlisted legacy file grew past its frozen cap ($cap). Split it (or re-justify in ARCHITECTURE.md 'Large files' AND raise the cap here in the same change)." >&2
    else
      echo "FAIL: $f has $lines lines (> $THRESHOLD). Split it by seam; the allowlist is for pre-existing files only." >&2
    fi
    fail=1
  fi
done < <(find . -path './.claude' -prune -o \
              \( \( -name '*.go' ! -name '*_test.go' ! -path './.git/*' ! -path './ui/*' \) \
                 -o \( -path './ui/src/*' \( -name '*.ts' -o -name '*.tsx' \) \) \
                 -o \( -path './scripts/*' -name '*.sh' \) \) -print)

if ((fail)); then
  exit 1
fi
echo "check-file-size: OK (no non-test .go, ui/src .ts/.tsx or scripts .sh file over $THRESHOLD lines outside the frozen allowlist)"
