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
# Read-only against GitHub (nothing here pushes or merges for real). Three
# passes:
#
#   1. STACKED-BASE RETARGET CHECK — an open PR whose base branch is another
#      PR's branch, where that base branch is no longer an open PR's head
#      (merged or closed), should have been retargeted to main and was not.
#
#   2. MIGRATION-NUMBER CLAIMS — no merge needed: the migration files each
#      open, non-draft, main-targeted PR adds, flagged when two PRs claim the
#      same prefix or a prefix is at or below origin/main's max.
#
#   3. COMBINED SCRATCH MERGE — the same PRs merged one after another, in
#      number order, into a throwaway worktree off origin/main, then `go vet`
#      under the three tag sets, `go test ./internal/db -run Migrat`, and the
#      internal/api AST guards (TestRunPolicySpec_EveryFieldIsBoundedOrDeclaredPassThrough,
#      TestOnlyThreeArmsSetBlocking) run over the result. Files the checks do
#      not read (CHANGELOG.md, lockfiles, UI) are union-merged; a PR that
#      still does not merge onto the ones before it is set aside and listed,
#      and fails the job only when the conflict is under
#      internal/db/migrations. When the checks fail, a binary search over the
#      merge sequence names the first PR whose addition broke them; that PR is
#      dropped, the rest are merged again on top of the last passing commit,
#      and the loop repeats until the checks pass, so every bad PR is named,
#      not only the first.
#
# Exits non-zero (a red nightly, the PRs named in GITHUB_STEP_SUMMARY) on a
# migration-number claim, a check failure, or a conflict under
# internal/db/migrations. Informational, not a release gate.
#
# Needs: gh (authenticated via GH_TOKEN), git, go, and psql when WARDYN_TEST_PG
# is set. WARDYN_TEST_PG makes the Postgres-gated migration tests run for real
# instead of self-skipping (nightly.yml provides a throwaway postgres:17
# service); its public schema is dropped before every check run, because the
# binary search moves between trees that carry different migration sets.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${GH_TOKEN:?GH_TOKEN is required (gh pr list needs it)}"
REPO="${GITHUB_REPOSITORY:-cjohnstoniv/wardyn}"
SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/null}"
AST_GUARD_TESTS='TestRunPolicySpec_EveryFieldIsBoundedOrDeclaredPassThrough|TestOnlyThreeArmsSetBlocking'
MIGRATIONS_DIR="internal/db/migrations"

git fetch --quiet origin "+refs/heads/main:refs/remotes/origin/main"

# Every open PR, whatever its state — the stacked-base check needs drafts and
# PRs on other bases too.
prs_json="$(gh pr list --repo "$REPO" --state open --limit 200 \
  --json number,headRefName,baseRefName,isDraft)"

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

# No filter on GitHub's `mergeable`: it is computed lazily and comes back
# UNKNOWN for most PRs on a cold query, so filtering on it checks a different,
# sometimes empty, set each night. The scratch merge below finds out for itself.
mapfile -t candidates < <(printf '%s' "$prs_json" \
  | jq -r '.[] | select(.isDraft==false and .baseRefName=="main") | .number' | sort -n)
drafts="$(printf '%s' "$prs_json" | jq '[.[] | select(.isDraft and .baseRefName=="main")] | length')"
other_base="$(printf '%s' "$prs_json" | jq '[.[] | select(.baseRefName!="main")] | length')"

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
  echo "### Candidates"
  echo "${#candidates[@]} open, non-draft PR(s) target main. Not checked: $drafts draft(s) targeting main," \
       "$other_base PR(s) targeting another branch (stacked or release)."
  echo
} >>"$SUMMARY"

if [ "${#candidates[@]}" -eq 0 ]; then
  exit 0
fi

REPO_ROOT="$(pwd)"
WORKDIR="$(mktemp -d)"
LOGDIR="$(mktemp -d)"
# PR heads are fetched into a private ref namespace, force-updated, so a rerun
# after a force-push cannot fail on a non-fast-forward, and the trap removes
# every one of them.
NS="refs/scratch/nightly-$$"
cleanup() {
  cd "$REPO_ROOT"
  git worktree remove --force "$WORKDIR" 2>/dev/null || true
  git for-each-ref --format='delete %(refname)' "$NS" | git update-ref --stdin || true
  rm -rf "$WORKDIR" "$LOGDIR"
}
trap cleanup EXIT
git worktree add --quiet --detach "$WORKDIR" origin/main
cd "$WORKDIR"

# The scratch merges are real merge commits, and a GitHub-hosted runner has no
# git identity: without one every `git merge` exits 128 ("Committer identity
# unknown"). Environment, not `git config`, so nothing is written to the repo.
export GIT_AUTHOR_NAME=wardyn-nightly GIT_AUTHOR_EMAIL=nightly@invalid
export GIT_COMMITTER_NAME=wardyn-nightly GIT_COMMITTER_EMAIL=nightly@invalid

# Nearly every PR adds a CHANGELOG bullet in the same spot, so a plain merge
# would set most of them aside over CHANGELOG.md alone and the checks would
# never see them. Only Go sources, go.mod/go.sum and the embedded SQL
# migrations feed the checks, so every other file is union-merged (both
# sides' lines kept): garbage for a lockfile, but nothing here reads one.
printf '%s\n' '* merge=union' '*.go merge=text' '*.sql merge=text' 'go.mod merge=text' 'go.sum merge=text' \
  >"$LOGDIR/attributes"

refspecs=()
for n in "${candidates[@]}"; do refspecs+=("+pull/$n/head:$NS/pr-$n"); done
git fetch --quiet origin "${refspecs[@]}"

status=0

# ── 2. migration-number claims ──────────────────────────────────────────────
main_max=0
while IFS= read -r name; do
  [[ "$(basename "$name")" =~ ^([0-9]{4})_ ]] || continue
  prefix=$((10#${BASH_REMATCH[1]}))
  if (( prefix > main_max )); then main_max=$prefix; fi
done <<<"$(git ls-tree --name-only -r origin/main -- "$MIGRATIONS_DIR")"

declare -A claimants=()
claim_findings=()
for n in "${candidates[@]}"; do
  while IFS= read -r p; do
    [ -z "$p" ] && continue
    claimants[$p]+=" #$n"
    if (( 10#$p <= main_max )); then
      claim_findings+=("PR #$n adds migration $p, at or below origin/main's max ($(printf '%04d' "$main_max"))")
    fi
  done < <(git diff --name-only --diff-filter=A "origin/main...$NS/pr-$n" -- "$MIGRATIONS_DIR" \
    | xargs -r -n1 basename | sed -n 's/^\([0-9]\{4\}\)_.*\.sql$/\1/p' | sort -u)
done
for p in $(printf '%s\n' "${!claimants[@]}" | sort); do
  read -ra who <<<"${claimants[$p]}"
  if (( ${#who[@]} > 1 )); then
    claim_findings+=("migration $p is claimed by ${#who[@]} PRs:${claimants[$p]}")
  fi
done

{
  echo "### Migration-number claims"
  if [ "${#claim_findings[@]}" -eq 0 ]; then
    echo "No two PRs add the same migration prefix, and none is at or below origin/main's max ($(printf '%04d' "$main_max"))."
  else
    printf -- '- %s\n' "${claim_findings[@]}"
  fi
  echo
} >>"$SUMMARY"
for f in "${claim_findings[@]}"; do
  echo "::error::nightly-migration-merge-check: $f"
  status=1
done

# ── 3. combined scratch merge ───────────────────────────────────────────────
# run_checks: the three checks the issue names, in the order it names them.
# Sets check_label to the one that failed and keeps its output in
# $LOGDIR/fail.log (a whole `go vet ./...` failure can be pages long).
run_checks() {
  local log="$LOGDIR/checks.log"
  if [ -n "${WARDYN_TEST_PG:-}" ] \
      && ! psql -q "$WARDYN_TEST_PG" -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >"$log" 2>&1; then
    check_label="resetting the WARDYN_TEST_PG database"
  elif ! go vet ./... >"$log" 2>&1 || ! go vet -tags docker ./... >>"$log" 2>&1 \
      || ! go vet -tags k8s ./... >>"$log" 2>&1; then
    check_label="go vet"
  elif ! go test ./internal/db -run Migrat -count=1 >"$log" 2>&1; then
    check_label="go test ./internal/db -run Migrat"
  elif ! go test ./internal/api -run "$AST_GUARD_TESTS" -count=1 >"$log" 2>&1; then
    check_label="internal/api AST guards"
  else
    return 0
  fi
  cp "$log" "$LOGDIR/fail.log"
  return 1
}

# merge_in_turn PR...: merges each PR onto HEAD in turn. Fills merged[] and
# merged_sha[] (HEAD after each merge), and conflicts[] ("N|files") with each
# PR that did not merge and was set aside.
merge_in_turn() {
  merged=(); merged_sha=(); conflicts=()
  local n files
  for n in "$@"; do
    if git -c core.attributesFile="$LOGDIR/attributes" merge --quiet --no-edit "$NS/pr-$n" \
        >"$LOGDIR/merge.log" 2>&1; then
      merged+=("$n")
      merged_sha+=("$(git rev-parse HEAD)")
    else
      files="$(git diff --name-only --diff-filter=U | paste -sd' ')"
      if [ -z "$files" ]; then
        echo "::error::nightly-migration-merge-check: git merge of PR #$n failed without a conflict"
        cat "$LOGDIR/merge.log"
        exit 1
      fi
      git merge --abort
      conflicts+=("$n|$files")
    fi
  done
}

# fail_tail: the end of the last failing check's output, fenced.
fail_tail() {
  echo '```text'
  tail -n 40 "$LOGDIR/fail.log"
  echo '```'
}

# Every culprit below is found relative to origin/main, so origin/main has to
# pass first; otherwise every round would blame its first PR.
if ! run_checks; then
  {
    echo "### Combined scratch merge"
    echo "origin/main itself fails $check_label, so no PR can be blamed:"
    fail_tail
  } >>"$SUMMARY"
  echo "::error::nightly-migration-merge-check: origin/main itself fails $check_label"
  fail_tail
  exit 1
fi

base_ref=origin/main   # the last commit known to pass the checks
base_count=0           # PRs merged into base_ref
base_last=""           # the last of them
remaining=("${candidates[@]}")
: >"$LOGDIR/failures.md"
while :; do
  git reset --hard --quiet "$base_ref"
  merge_in_turn "${remaining[@]}"
  if [ "${#merged[@]}" -eq 0 ] || run_checks; then
    break
  fi
  # merged_sha[hi] fails; merged_sha[lo] passes (base_ref when lo is -1).
  lo=-1
  hi=$(( ${#merged[@]} - 1 ))
  fail_label="$check_label"
  while (( hi - lo > 1 )); do
    mid=$(( (lo + hi) / 2 ))
    git reset --hard --quiet "${merged_sha[$mid]}"
    if run_checks; then lo=$mid; else hi=$mid; fail_label="$check_label"; fi
  done
  culprit="${merged[$hi]}"
  if (( lo >= 0 )); then
    base_ref="${merged_sha[$lo]}"
    base_count=$((base_count + lo + 1))
    base_last="${merged[$lo]}"
  fi
  on_top="origin/main"
  if (( base_count > 0 )); then on_top="origin/main plus $base_count earlier PR(s), the last #$base_last"; fi
  {
    echo "#### PR #$culprit breaks $fail_label"
    echo "On top of $on_top."
    fail_tail
    echo
  } >>"$LOGDIR/failures.md"
  echo "::error::nightly-migration-merge-check: PR #$culprit breaks $fail_label on top of $on_top"
  fail_tail
  status=1
  # Everything not yet in base_ref except the culprit goes round again, the
  # PRs that conflicted this round included: the culprit may be what they
  # conflicted with.
  declare -A drop=()
  for (( i = 0; i <= hi; i++ )); do drop[${merged[$i]}]=1; done
  next=()
  for n in "${remaining[@]}"; do
    if [ -z "${drop[$n]:-}" ]; then next+=("$n"); fi
  done
  remaining=("${next[@]}")
done

conflict_lines=()
for c in "${conflicts[@]}"; do
  n="${c%%|*}"
  files="${c#*|}"
  if [[ " $files" == *" $MIGRATIONS_DIR/"* ]]; then
    conflict_lines+=("**#$n** (under \`$MIGRATIONS_DIR\`): $files")
    echo "::error::nightly-migration-merge-check: PR #$n conflicts under $MIGRATIONS_DIR: $files"
    status=1
  else
    conflict_lines+=("#$n: $files")
  fi
done

{
  echo "### Combined scratch merge"
  echo "$((base_count + ${#merged[@]})) of ${#candidates[@]} PR(s) merged together onto origin/main, one at a time in" \
       "number order. With the PRs below set aside, the result passes go vet (all tag sets)," \
       "\`go test ./internal/db -run Migrat\` and the internal/api AST guards."
  echo
  if [ -s "$LOGDIR/failures.md" ]; then
    echo "Each PR below breaks a check on top of the PRs merged before it:"
    echo
    cat "$LOGDIR/failures.md"
  fi
  if [ "${#conflict_lines[@]}" -gt 0 ]; then
    echo "Set aside because they do not merge onto the PRs before them (only a conflict under \`$MIGRATIONS_DIR\` fails this job):"
    echo
    printf -- '- %s\n' "${conflict_lines[@]}"
  fi
} >>"$SUMMARY"

exit "$status"
