#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# lane-preflight.sh — the cheap merge gates, run locally before a push, so a
# lint, static-analysis, typecheck or scoped-test failure is found in minutes
# rather than after a full hosted CI pass. It stops at the first failing step.
#
# Steps, in order: make lint, make staticcheck, make ui-typecheck, go test of
# the packages holding changed Go files, vitest related for changed UI sources.
# "Changed" is against the merge base with $BASE (default origin/main), plus the
# working tree. A lane stacked on a batch branch sets BASE=origin/batch/NNN.
#
# This is not the merge gate. Hosted CI and `make ci` stay authoritative, and
# the union coverage floor (scripts/cover-union.sh) is not checked here.
#
# Usage: [BASE=<ref>] scripts/lane-preflight.sh
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=lib/common.sh
source scripts/lib/common.sh

BASE="${BASE:-origin/main}"
base=$(git merge-base "${BASE}" HEAD) || die "no merge base with ${BASE} (run: git fetch origin)"
changed=$( { git diff --name-only --diff-filter=d "${base}"; git ls-files --others --exclude-standard; } | sort -u )

go_pkgs=()
while IFS= read -r d; do
  go_pkgs+=("./${d}")
done < <(grep '\.go$' <<<"${changed}" | xargs -r -n1 dirname | sort -u || true)

ui_files=()
while IFS= read -r f; do
  ui_files+=("${f#ui/}")
done < <(grep -E '^ui/src/.*\.(ts|tsx)$' <<<"${changed}" || true)

log "1/5 make lint"
make lint
log "2/5 make staticcheck"
make staticcheck
log "3/5 make ui-typecheck"
make ui-typecheck

if [ "${#go_pkgs[@]}" -gt 0 ]; then
  # A package whose files all sit behind a build tag fails `go test` setup under default tags; drop those.
  mapfile -t go_pkgs < <(go list -e -f '{{if or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' "${go_pkgs[@]}")
fi

if [ "${#go_pkgs[@]}" -gt 0 ]; then
  log "4/5 go test ${go_pkgs[*]}"
  go test -p 4 -count=1 "${go_pkgs[@]}"
else
  log "4/5 go test: no changed Go packages buildable under default tags (build-tagged ones are left to hosted CI), skipped"
fi

if [ "${#ui_files[@]}" -gt 0 ]; then
  log "5/5 vitest related (${#ui_files[@]} files)"
  pnpm -C ui exec vitest related --run --maxWorkers=4 "${ui_files[@]}"
else
  log "5/5 vitest related: no changed UI sources, skipped"
fi

log "preflight passed (not a substitute for hosted CI or the coverage floor)"
