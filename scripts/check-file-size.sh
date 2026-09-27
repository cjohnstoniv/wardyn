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
# that are cohesive as-is (see CONTRIBUTING.md "Large files") — new entries
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
  # driver.go came OFF this list 2026-09-21: split by seam (driver_exec.go /
  # driver_network.go) instead of raising its frozen cap for open PR headroom —
  # it is gated at the plain 1000-line threshold like any other file now.
  ["./internal/api/workspace_run.go"]=1180 # 1092 at freeze
  ["./internal/api/setup.go"]=1120         # 1028 at freeze
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
  # The v0.5.0 k8s/cloud merge grew four pre-existing, cohesive files past 1000
  # (the k8s runner substrate + envbuild wiring, and the new-run wizard's k8s
  # runner flavor + its tests). Caps frozen at +~8% so they stop growing; split
  # by seam when next substantially touched.
  # workspaces.go was the first of them to hit its cap and got split by seam
  # (workspace_requirements.go / workspace_envcode.go) rather than re-justified —
  # at 800 lines it came OFF this list and is gated at the plain threshold now.
  ["./internal/envbuild/builder.go"]=1100 # 1020 at v0.5 merge
)

# The walk is git's, not find's: tracked files plus untracked-but-not-ignored
# ones (-co --exclude-standard), so a gitignored local/ tree can never false-red
# the gate (R3-ASM-2) while a brand-new file is still caught before `git add`.
# Outside a checkout the walk would be EMPTY and the gate would pass vacuously,
# so refuse to run there instead of printing OK over nothing.
git rev-parse --git-dir >/dev/null 2>&1 || { echo "FAIL: check-file-size must run inside a git checkout (git ls-files is the walk)" >&2; exit 1; }

fail=0
while IFS= read -r f; do
  case "$f" in
    *_test.go) continue ;;
    ui/*.go) continue ;;
    *.go|ui/src/*.ts|ui/src/*.tsx|scripts/*.sh) ;;
    *) continue ;;
  esac
  f="./$f"
  lines=$(wc -l <"$f")
  cap=${ALLOWLIST[$f]:-$THRESHOLD}
  if ((lines > cap)); then
    if [[ -n "${ALLOWLIST[$f]:-}" ]]; then
      echo "FAIL: $f has $lines lines — allowlisted legacy file grew past its frozen cap ($cap). Split it (or re-justify in CONTRIBUTING.md 'Large files' AND raise the cap here in the same change)." >&2
    else
      echo "FAIL: $f has $lines lines (> $THRESHOLD). Split it by seam; the allowlist is for pre-existing files only." >&2
    fi
    fail=1
  fi
done < <(git ls-files -co --exclude-standard)

if ((fail)); then
  exit 1
fi
echo "check-file-size: OK (no non-test .go, ui/src .ts/.tsx or scripts .sh file over $THRESHOLD lines outside the frozen allowlist)"
