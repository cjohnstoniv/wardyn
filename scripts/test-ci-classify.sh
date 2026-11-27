#!/usr/bin/env bash
# Copyright 2026 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-ci-classify.sh — daemon-free test of ci.yml's "Classify the changed paths" step. The step's own run block is
# run on a throwaway repository whose last commit changes one path, and the outputs it writes are checked. A change
# under deploy/macos-guest/ must select the macOS job: it is the only job that builds the guest files.
# Usage: scripts/test-ci-classify.sh   (exit 0 = PASS, non-zero = FAIL)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WF="$ROOT/.github/workflows/ci.yml"
fail() { echo "test-ci-classify: FAIL: $*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# The run block of the one step named "Classify the changed paths", blank lines kept.
awk '
  /^      - name: Classify the changed paths$/ { want = 1; next }
  want && /^        run: \|$/ { grab = 1; next }
  grab && /^          / { sub(/^          /, ""); print; next }
  grab && /^$/ { print; next }
  grab { exit }
' "$WF" >"$WORK/classify.sh"
grep -q 'macos=' "$WORK/classify.sh" || fail "ci.yml has no 'Classify the changed paths' run block that sets macos"

# git with no user or system config (no hooks, no signing), and an identity for the two commits.
git_() { GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null git -C "$WORK/repo" -c user.name=test -c user.email=test@example.invalid "$@"; }

# classify PATH prints what the step writes to GITHUB_OUTPUT for a pull request whose one change is PATH.
classify() {
  rm -rf "$WORK/repo"
  mkdir -p "$WORK/repo"
  git_ init -q
  git_ commit -q --allow-empty -m base
  mkdir -p "$WORK/repo/$(dirname "$1")"
  echo x >"$WORK/repo/$1"
  git_ add -- "$1"
  git_ commit -q -m change
  : >"$WORK/out"
  (cd "$WORK/repo" && EVENT=pull_request HEAD_REF=lane/test BASE_REF=main GITHUB_OUTPUT="$WORK/out" bash "$WORK/classify.sh" >/dev/null)
  cat "$WORK/out"
}

while IFS='|' read -r path want; do
  got="$(classify "$path")"
  grep -qx "$want" <<<"$got" || fail "$path: want $want, the step wrote: $(tr '\n' ' ' <<<"$got")"
done <<'CASES'
deploy/macos-guest/setup.sh|macos=true
internal/macvm/vm.go|macos=true
docs/README.md|macos=false
docs/README.md|code=false
CASES

echo "test-ci-classify: PASS"
