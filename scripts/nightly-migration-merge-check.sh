#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# nightly-migration-merge-check.sh — catches the collision
# scripts/check-migration-numbers.sh cannot: two PRs open AT THE SAME TIME,
# each picking the next free migration number off origin/main as it stood
# when THAT PR ran its own gate. Both pass check-migration-numbers.sh in
# isolation; the collision only exists once both are merged.
#
# Two things, both read-only against GitHub (nothing here pushes or merges
# for real):
#
#   1. STACKED-BASE RETARGET CHECK — an open PR whose base branch is another
#      PR's branch, where that base branch is no longer an open PR's head
#      (merged or closed), should have been retargeted to main and was not.
#
#   2. OCTOPUS SCRATCH MERGE — every open, non-draft, main-targeted,
#      individually-mergeable PR is merged together (git's actual octopus
#      strategy: one `git merge` of every head at once) into a throwaway
#      worktree off origin/main, then `go vet` under the three tag sets,
#      `go test ./internal/db -run Migrat`, and the internal/api AST guards
#      (TestRunPolicySpec_EveryFieldIsBoundedOrDeclaredPassThrough,
#      TestOnlyThreeArmsSetBlocking) run against the result. If the octopus
#      merge conflicts or a check fails, it falls back to merging the same
#      PRs ONE AT A TIME, in number order, re-running the checks after each,
#      and reports the FIRST PR whose addition was the one that broke it —
#      "the first bad pair" being (everything merged before it, that PR).
#
# Informational, not a release gate: reports via GITHUB_STEP_SUMMARY when
# present and exits non-zero so the nightly job goes red with a name attached,
# never silently.
#
# Needs: gh (authenticated via GH_TOKEN), git, go. WARDYN_TEST_PG, if set, is
# passed through so the Postgres-gated migration tests run for real instead of
# self-skipping (nightly.yml provides a throwaway postgres:17 service for
# this, the same way ci.yml's test-pg job does).
set -euo pipefail
cd "$(dirname "$0")/.."

: "${GH_TOKEN:?GH_TOKEN is required (gh pr list needs it)}"
REPO="${GITHUB_REPOSITORY:-cjohnstoniv/wardyn}"
SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/null}"
AST_GUARD_TESTS='TestRunPolicySpec_EveryFieldIsBoundedOrDeclaredPassThrough|TestOnlyThreeArmsSetBlocking'

git fetch --quiet origin main
git fetch --quiet origin master 2>/dev/null || true

# Every open PR targeting main, whatever its state — the stacked-base check
# below needs drafts and not-yet-mergeable PRs too, so the mergeable filter
# is applied later, only for the octopus phase.
prs_json="$(gh pr list --repo "$REPO" --state open --limit 200 \
  --json number,headRefName,baseRefName,mergeable,isDraft)"

# ── 1. stacked-base retarget check ──────────────────────────────────────────
# A PR is "stacked" when its base is neither main/master nor a release/**
# branch. If that base name is not among the currently-open PRs' own head
# branches, the PR it was stacked on has merged or closed, and this one
# should have been retargeted to main — flag it rather than assume the
# author noticed.
stacked_findings=0
open_heads="$(printf '%s' "$prs_json" | jq -r '.[].headRefName')"
while IFS=$'\t' read -r num base; do
  [ -z "$num" ] && continue
  case "$base" in
    main|master|release/*) continue ;;
  esac
  if ! grep -qxF "$base" <<<"$open_heads"; then
    echo "::warning::PR #$num is stacked on '$base', which is not an open PR's branch (merged or closed) — retarget to main"
    stacked_findings=$((stacked_findings + 1))
  fi
done < <(printf '%s' "$prs_json" | jq -r '.[] | [.number, .baseRefName] | @tsv')

{
  echo "## Nightly migration-merge check"
  echo
  echo "### Stacked-base retarget check"
  if [ "$stacked_findings" -eq 0 ]; then
    echo "No open PR is stacked on a branch that is no longer open."
  else
    echo "$stacked_findings PR(s) stacked on a merged/closed base — see the workflow warnings above."
  fi
  echo
} >>"$SUMMARY"

# ── 2. octopus scratch merge ────────────────────────────────────────────────
mapfile -t candidates < <(printf '%s' "$prs_json" \
  | jq -r '.[] | select(.isDraft==false and .baseRefName=="main" and .mergeable=="MERGEABLE") | .number' \
  | sort -n)

if [ "${#candidates[@]}" -eq 0 ]; then
  echo "No open, non-draft, individually-mergeable PRs target main; nothing to octopus-merge." >>"$SUMMARY"
  exit 0
fi

REPO_ROOT="$(pwd)"
WORKDIR="$(mktemp -d)"
BRANCH="scratch/nightly-migration-merge-$$"
trap 'cd "$REPO_ROOT"; git worktree remove --force "$WORKDIR" 2>/dev/null || true; git branch -D "$BRANCH" 2>/dev/null || true; rm -rf "$WORKDIR"' EXIT
git worktree add --quiet -b "$BRANCH" "$WORKDIR" origin/main
cd "$WORKDIR"

for n in "${candidates[@]}"; do
  git fetch --quiet origin "pull/$n/head:pr-$n"
done

# run_checks: the three the issue names, in the order it names them. Its own
# stdout/stderr are logged (not returned — a whole `go vet ./...` failure can
# be pages long), so a caller doing `bad_check="$(run_checks)"` gets back ONLY
# the short label of whichever check failed, never the tool's raw output.
run_checks() {
  if ! go vet ./... >/tmp/checks.log 2>&1 || ! go vet -tags docker ./... >>/tmp/checks.log 2>&1 \
      || ! go vet -tags k8s ./... >>/tmp/checks.log 2>&1; then
    echo "go vet"; return 1
  fi
  if ! go test ./internal/db -run Migrat -count=1 >/tmp/checks.log 2>&1; then
    echo "go test ./internal/db -run Migrat"; return 1
  fi
  if ! go test ./internal/api -run "$AST_GUARD_TESTS" -count=1 >/tmp/checks.log 2>&1; then
    echo "internal/api AST guards"; return 1
  fi
  return 0
}

merge_refs=()
for n in "${candidates[@]}"; do merge_refs+=("pr-$n"); done

octopus_ok=1
if ! git merge --quiet --no-edit "${merge_refs[@]}" >/tmp/octopus-merge.log 2>&1; then
  octopus_ok=0
fi
if [ "$octopus_ok" -eq 1 ]; then
  if ! bad_check="$(run_checks)"; then
    octopus_ok=0
  fi
fi

if [ "$octopus_ok" -eq 1 ]; then
  {
    echo "### Octopus scratch merge (${#candidates[@]} PRs: ${candidates[*]})"
    echo "Merged clean and passed go vet (all tag sets), \`go test ./internal/db -run Migrat\`, and the internal/api AST guards."
  } >>"$SUMMARY"
  exit 0
fi

# The all-at-once merge (or the checks over it) failed. Bisect: reset to
# origin/main and merge candidates ONE AT A TIME, in number order, so the
# FIRST one whose addition breaks something is nameable.
git merge --abort 2>/dev/null || true
git reset --hard origin/main --quiet

# last_good_ref is a real commit (for the reset below); last_good_label is
# just for the report — keeping them separate is load-bearing, since
# "pr-100 (through #100)" is not a valid argument to `git reset --hard`.
last_good_ref="origin/main"
last_good_label="origin/main"
culprit=""
reason=""
for n in "${candidates[@]}"; do
  if ! git merge --quiet --no-edit "pr-$n" >/tmp/pair-merge.log 2>&1; then
    culprit="$n"
    reason="does not merge cleanly onto $last_good_label ($(tail -n1 /tmp/pair-merge.log))"
    break
  fi
  if ! bad_check="$(run_checks)"; then
    culprit="$n"
    reason="merges cleanly onto $last_good_label but breaks: $bad_check"
    git reset --hard "$last_good_ref" --quiet 2>/dev/null || true
    break
  fi
  last_good_ref="$(git rev-parse HEAD)"
  last_good_label="PR #$n"
done

{
  echo "### Octopus scratch merge (${#candidates[@]} PRs: ${candidates[*]})"
  echo
  if [ -n "$culprit" ]; then
    echo "The all-at-once octopus merge failed. First bad pair: **PR #$culprit**, on top of \`$last_good_label\`."
    echo
    echo "$reason"
    echo "::error::nightly-migration-merge-check: first bad pair is PR #$culprit (on top of $last_good_label): $reason"
  else
    echo "The all-at-once octopus merge failed, but every PR merges clean and passes ONE AT A TIME in number order — the" \
         "failure is an interaction between 3+ PRs together, not any single one. Re-run with a narrower candidate set to" \
         "isolate it."
    echo "::error::nightly-migration-merge-check: octopus merge fails only with 3+ PRs combined; no single bad pair"
  fi
} >>"$SUMMARY"

exit 1
