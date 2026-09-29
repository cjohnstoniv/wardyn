#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-nightly-migration-merge-check.sh — pins the riskiest logic in
# scripts/nightly-migration-merge-check.sh: the migration-prefix claim
# checks, the binary search that isolates a single bad PR out of several
# good ones, and the conflict handling that only fails the job on a
# conflict under internal/db/migrations (a union-mergeable edit or a real
# conflict elsewhere must not).
#
# `gh` and `go` are both faked (a bin/ directory prepended to PATH):
#   - fake gh answers `gh pr list ...` from a JSON file the case wrote.
#   - fake go answers `vet` and `test ./internal/db|./internal/api` by
#     checking for a marker file in the current tree (VET_BROKEN,
#     DB_TEST_BROKEN, API_TEST_BROKEN) — a "bad" PR is one that adds that
#     marker file, so the real git merge naturally carries it into the
#     combined tree the same way a real regression would.
# Everything else (fetch, worktree, merge, diff) is real git against a
# local bare 'origin', so the merge/conflict/union-attribute behavior under
# test is the genuine thing, not a simulation of it.
#
# Daemon-free, network-free.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A developer's global/system git config (commit.gpgsign, hooks paths, init
# defaults) must not reach the fixture's seed commits.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

fail() { echo "FAIL: $*" >&2; exit 1; }

# ── fake gh / fake go on PATH ────────────────────────────────────────────
mkdir -p "$TMP/bin"

cat >"$TMP/bin/gh" <<'FAKEGH'
#!/usr/bin/env bash
if [ "$1" = "pr" ] && [ "$2" = "list" ]; then
  cat "$FAKE_GH_PRS"
  exit 0
fi
echo "fake gh: unsupported invocation: $*" >&2
exit 1
FAKEGH
chmod +x "$TMP/bin/gh"

cat >"$TMP/bin/go" <<'FAKEGO'
#!/usr/bin/env bash
sub="$1"; shift || true
case "$sub" in
  vet)
    [ -e VET_BROKEN ] && { echo "fake go vet: VET_BROKEN present" >&2; exit 1; }
    exit 0 ;;
  test)
    for a in "$@"; do
      case "$a" in
        ./internal/db)
          [ -e DB_TEST_BROKEN ] && { echo "fake go test: DB_TEST_BROKEN present" >&2; exit 1; }
          exit 0 ;;
        ./internal/api)
          [ -e API_TEST_BROKEN ] && { echo "fake go test: API_TEST_BROKEN present" >&2; exit 1; }
          exit 0 ;;
      esac
    done
    exit 0 ;;
  *)
    echo "fake go: unsupported subcommand '$sub'" >&2
    exit 1 ;;
esac
FAKEGO
chmod +x "$TMP/bin/go"

export PATH="$TMP/bin:$PATH"

# ── repo builders ────────────────────────────────────────────────────────
# new_case DIR: a bare 'origin' whose main has migrations 0001..0003, plus a
# 'seed' clone (used to build PR branches) and a 'work' clone with the gate
# script copied in (used to run it, mirroring test-migration-numbers.sh).
new_case() {
  local dir="$1"
  local bare="$dir/origin.git" seed="$dir/seed" work="$dir/work"
  git init --quiet --bare -b main "$bare"

  git init --quiet -b main "$seed"
  git -C "$seed" config user.email "test@example.com"
  git -C "$seed" config user.name "test"
  mkdir -p "$seed/internal/db/migrations"
  for n in 0001 0002 0003; do
    echo "-- migration $n" >"$seed/internal/db/migrations/${n}_seed.sql"
  done
  echo "# Changelog" >"$seed/CHANGELOG.md"
  echo >>"$seed/CHANGELOG.md"
  echo "- baseline" >>"$seed/CHANGELOG.md"
  echo "package main" >"$seed/main.go"
  git -C "$seed" add -A
  git -C "$seed" commit --quiet -m seed
  git -C "$seed" remote add origin "$bare"
  git -C "$seed" push --quiet origin main

  git clone --quiet "$bare" "$work"
  git -C "$work" config user.email "test@example.com"
  git -C "$work" config user.name "test"
  mkdir -p "$work/scripts"
  cp "$ROOT/scripts/nightly-migration-merge-check.sh" "$work/scripts/"
}

# make_pr SEED BARE NUM SETUP_FN: branches off main in SEED, runs SETUP_FN
# (which adds/edits files), commits, and pushes the result to BARE as the
# PR's head ref (refs/pull/NUM/head) — a real GitHub PR ref, built with
# plain git so nothing here needs a network.
make_pr() {
  local seed="$1" bare="$2" num="$3" setup_fn="$4"
  git -C "$seed" checkout --quiet -b "pr-$num" main
  "$setup_fn" "$seed"
  git -C "$seed" add -A
  git -C "$seed" commit --quiet -m "pr $num"
  git -C "$seed" push --quiet "$bare" "pr-$num:refs/pull/$num/head"
  git -C "$seed" checkout --quiet main
  git -C "$seed" branch --quiet -D "pr-$num"
}

# gh_prs_json WORK NUM...: writes the fake `gh pr list` JSON (every PR
# open, non-draft, based on main) and points FAKE_GH_PRS at it.
gh_prs_json() {
  local dir="$1"; shift
  local json="$dir/prs.json"
  {
    echo "["
    local first=1
    for n in "$@"; do
      [ "$first" -eq 1 ] || echo ","
      first=0
      printf '{"number":%d,"headRefName":"pr-%d","baseRefName":"main","isDraft":false}' "$n" "$n"
    done
    echo
    echo "]"
  } >"$json"
  echo "$json"
}

# run_gate WORK SUMMARY_FILE: runs the gate against WORK with
# FAKE_GH_PRS/GITHUB_STEP_SUMMARY set, capturing stdout+stderr; never lets
# `set -e` here abort the case (the caller checks $? itself).
run_gate() {
  local work="$1" summary="$2"
  : >"$summary"
  ( cd "$work" && env -u GITHUB_BASE_REF -u GITHUB_REF_NAME -u WARDYN_TEST_PG \
      GH_TOKEN=fake-token GITHUB_STEP_SUMMARY="$summary" FAKE_GH_PRS="$FAKE_GH_PRS" \
      ./scripts/nightly-migration-merge-check.sh )
}

# ── 1. a clean candidate set: no collisions, no check breakage => exit 0 ────
case1() {
  local dir="$TMP/case1"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 10 add_0004
  make_pr "$dir/seed" "$dir/origin.git" 11 add_0005
  FAKE_GH_PRS="$(gh_prs_json "$dir" 10 11)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -eq 0 ] || fail "a clean candidate set must exit 0: $out"
  grep -q "2 of 2 PR(s) merged together" "$summary" || fail "summary must say both PRs merged: $(cat "$summary")"
  echo "ok  clean candidate set exits 0, both PRs merged"
}
add_0004() { echo "-- new" >"$1/internal/db/migrations/0004_a.sql"; }
add_0005() { echo "-- new" >"$1/internal/db/migrations/0005_b.sql"; }

# ── 2. two PRs claiming the same migration prefix => exit 1, both named ────
case2() {
  local dir="$TMP/case2"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 20 add_0004_x
  make_pr "$dir/seed" "$dir/origin.git" 21 add_0004_y
  FAKE_GH_PRS="$(gh_prs_json "$dir" 20 21)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || fail "two PRs claiming the same prefix must exit non-zero"
  grep -q "migration 0004 is claimed by 2 PRs:.*#20.*#21" "$summary" \
    || fail "summary must name both claimants: $(cat "$summary")"
  echo "ok  two PRs claiming the same migration prefix fail, naming both"
}
add_0004_x() { echo "-- x" >"$1/internal/db/migrations/0004_x.sql"; }
add_0004_y() { echo "-- y" >"$1/internal/db/migrations/0004_y.sql"; }

# ── 3. a PR's migration prefix at or below origin/main's max => exit 1 ─────
case3() {
  local dir="$TMP/case3"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 30 add_0002_old
  FAKE_GH_PRS="$(gh_prs_json "$dir" 30)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || fail "a prefix at/below origin/main's max must exit non-zero"
  grep -q "PR #30 adds migration 0002, at or below origin/main's max (0003)" "$summary" \
    || fail "summary must name the offending PR and prefix: $(cat "$summary")"
  echo "ok  a migration prefix at or below origin/main's max fails, naming the PR"
}
add_0002_old() { echo "-- old" >"$1/internal/db/migrations/0002_old.sql"; }

# ── 4. binary search isolates the one bad PR out of several good ones ──────
case4() {
  local dir="$TMP/case4"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 40 add_0004
  make_pr "$dir/seed" "$dir/origin.git" 41 add_0005_broken
  make_pr "$dir/seed" "$dir/origin.git" 42 add_0006
  FAKE_GH_PRS="$(gh_prs_json "$dir" 40 41 42)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || fail "the bad PR must fail the job"
  grep -q "PR #41 breaks go vet" "$summary" || fail "summary must blame PR #41 alone: $(cat "$summary")"
  grep -q "PR #40 breaks" "$summary" && fail "PR #40 must not be blamed: $(cat "$summary")"
  grep -q "PR #42 breaks" "$summary" && fail "PR #42 must not be blamed: $(cat "$summary")"
  grep -q "2 of 3 PR(s) merged together" "$summary" \
    || fail "the other two PRs must still merge without the culprit: $(cat "$summary")"
  [ -z "$(git -C "$dir/work" for-each-ref refs/scratch)" ] \
    || fail "the gate must delete its refs/scratch refs: $(git -C "$dir/work" for-each-ref refs/scratch)"
  [ "$(git -C "$dir/work" worktree list | wc -l)" -eq 1 ] \
    || fail "the gate must remove its scratch worktree: $(git -C "$dir/work" worktree list)"
  echo "ok  binary search isolates and drops the one bad PR (#41), keeps #40/#42, cleans up"
}
add_0005_broken() { echo "-- new" >"$1/internal/db/migrations/0005_mid.sql"; : >"$1/VET_BROKEN"; }
add_0006() { echo "-- new" >"$1/internal/db/migrations/0006_c.sql"; }

# ── 5. a conflict under internal/db/migrations fails the job ───────────────
case5() {
  local dir="$TMP/case5"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 50 edit_0001_a
  make_pr "$dir/seed" "$dir/origin.git" 51 edit_0001_b
  FAKE_GH_PRS="$(gh_prs_json "$dir" 50 51)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || fail "a conflict under internal/db/migrations must fail the job"
  grep -qF '**#51** (under `internal/db/migrations`)' "$summary" \
    || fail "summary must flag PR #51's migrations-dir conflict: $(cat "$summary")"
  echo "ok  a conflict under internal/db/migrations fails the job"
}
# both 50 and 51 replace 0001_seed.sql's only line with different content —
# a real conflict under a 'merge=text' path (.sql is excluded from the
# default union attribute), since both hunks touch the same base line.
edit_0001_a() { echo "-- rewritten by 50" >"$1/internal/db/migrations/0001_seed.sql"; }
edit_0001_b() { echo "-- rewritten by 51" >"$1/internal/db/migrations/0001_seed.sql"; }

# ── 6. a union-mergeable conflict elsewhere never fails the job ────────────
case6() {
  local dir="$TMP/case6"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 60 edit_changelog_a
  make_pr "$dir/seed" "$dir/origin.git" 61 edit_changelog_b
  FAKE_GH_PRS="$(gh_prs_json "$dir" 60 61)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -eq 0 ] || fail "two PRs both editing CHANGELOG.md must still merge cleanly (union): $out"
  grep -q "2 of 2 PR(s) merged together" "$summary" \
    || fail "both PRs must merge, no conflict set-aside: $(cat "$summary")"
  echo "ok  a same-line CHANGELOG.md conflict is union-merged, never fails the job"
}
# both 60 and 61 rewrite the SAME existing CHANGELOG.md line differently —
# under a normal (non-union) merge this is exactly the shape that conflicts
# in case5; here it must not, because CHANGELOG.md falls under the default
# '* merge=union' attribute (only .go/.sql/go.mod/go.sum opt out of it).
edit_changelog_a() { sed -i 's/^- baseline$/- baseline (60)/' "$1/CHANGELOG.md"; }
edit_changelog_b() { sed -i 's/^- baseline$/- baseline (61)/' "$1/CHANGELOG.md"; }

# ── 7. a real conflict outside internal/db/migrations is listed, not fatal ─
case7() {
  local dir="$TMP/case7"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 70 edit_main_go_a
  make_pr "$dir/seed" "$dir/origin.git" 71 edit_main_go_b
  FAKE_GH_PRS="$(gh_prs_json "$dir" 70 71)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -eq 0 ] || fail "a .go conflict outside the migrations dir must not fail the job: $out"
  grep -qxF -- '- #71: main.go' "$summary" \
    || fail "summary must list PR #71's main.go conflict: $(cat "$summary")"
  grep -qF '**#' "$summary" && fail "a non-migrations conflict must not be flagged: $(cat "$summary")"
  echo "ok  a .go conflict outside internal/db/migrations is listed, never fails the job"
}
# main.go is 'merge=text', so two different rewrites of its one line conflict.
edit_main_go_a() { echo "package seventy" >"$1/main.go"; }
edit_main_go_b() { echo "package seventyone" >"$1/main.go"; }

# ── 8. a PR set aside for conflicting with a culprit is requeued ───────────
# PR 80 breaks vet and edits main.go; PR 81 conflicts only with 80 (same
# line of main.go). Round one merges 80, sets 81 aside and blames 80; round
# two must merge 81 onto origin/main, so the summary counts it, and it must
# not be listed as set aside. A check that drops conflicted PRs after a
# culprit instead of requeuing them would report "0 of 2".
case8() {
  local dir="$TMP/case8"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 80 edit_main_go_broken
  make_pr "$dir/seed" "$dir/origin.git" 81 edit_main_go_b
  FAKE_GH_PRS="$(gh_prs_json "$dir" 80 81)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || fail "the culprit must fail the job"
  grep -q "PR #80 breaks go vet" "$summary" || fail "summary must blame PR #80: $(cat "$summary")"
  grep -q "PR #81 breaks" "$summary" && fail "PR #81 must not be blamed: $(cat "$summary")"
  grep -q "1 of 2 PR(s) merged together" "$summary" \
    || fail "PR #81 conflicted only with the culprit, so it must merge on the next round: $(cat "$summary")"
  grep -qF -- '- #81' "$summary" && fail "PR #81 must not be listed as set aside: $(cat "$summary")"
  echo "ok  a PR that conflicted only with the culprit is requeued and merges the next round"
}
edit_main_go_broken() { echo "package eighty" >"$1/main.go"; : >"$1/VET_BROKEN"; }

# ── 9. two culprits in one run are both named ──────────────────────────────
# 91 breaks vet, 93 breaks the db test, 90/92/94 are good. Round one blames
# 91, round two (on top of 90) blames 93, round three passes.
case9() {
  local dir="$TMP/case9"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 90 add_0004
  make_pr "$dir/seed" "$dir/origin.git" 91 add_0005_broken
  make_pr "$dir/seed" "$dir/origin.git" 92 add_0006
  make_pr "$dir/seed" "$dir/origin.git" 93 add_0007_db_broken
  make_pr "$dir/seed" "$dir/origin.git" 94 add_0008
  FAKE_GH_PRS="$(gh_prs_json "$dir" 90 91 92 93 94)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || fail "the culprits must fail the job"
  grep -q "PR #91 breaks go vet" "$summary" || fail "summary must blame PR #91: $(cat "$summary")"
  grep -qF "PR #93 breaks go test ./internal/db -run Migrat" "$summary" \
    || fail "summary must blame PR #93 for the db test: $(cat "$summary")"
  local n
  for n in 90 92 94; do
    grep -q "PR #$n breaks" "$summary" && fail "PR #$n must not be blamed: $(cat "$summary")"
  done
  grep -q "3 of 5 PR(s) merged together" "$summary" \
    || fail "the three good PRs must still merge: $(cat "$summary")"
  echo "ok  a second culprit in the same run is named alongside the first"
}
add_0007_db_broken() { echo "-- new" >"$1/internal/db/migrations/0007_d.sql"; : >"$1/DB_TEST_BROKEN"; }
add_0008() { echo "-- new" >"$1/internal/db/migrations/0008_e.sql"; }

# ── 10. a culprit at index 0 (the lo=-1 path) is named alone ───────────────
# The broken PR is the first one merged, so nothing before it passes and the
# search must fall back to origin/main as the last good commit.
case10() {
  local dir="$TMP/case10"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 100 add_0004_broken
  make_pr "$dir/seed" "$dir/origin.git" 101 add_0005
  make_pr "$dir/seed" "$dir/origin.git" 102 add_0006
  FAKE_GH_PRS="$(gh_prs_json "$dir" 100 101 102)"
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || fail "the culprit must fail the job"
  grep -q "PR #100 breaks go vet" "$summary" || fail "summary must blame PR #100: $(cat "$summary")"
  grep -qF "On top of origin/main." "$summary" || fail "the first PR is blamed on top of bare origin/main: $(cat "$summary")"
  grep -q "PR #101 breaks" "$summary" && fail "PR #101 must not be blamed: $(cat "$summary")"
  grep -q "PR #102 breaks" "$summary" && fail "PR #102 must not be blamed: $(cat "$summary")"
  grep -q "2 of 3 PR(s) merged together" "$summary" \
    || fail "the other two PRs must merge without the culprit: $(cat "$summary")"
  echo "ok  a culprit at index 0 is named alone and the rest still merge"
}
add_0004_broken() { echo "-- new" >"$1/internal/db/migrations/0004_a.sql"; : >"$1/VET_BROKEN"; }

# ── 11. the stacked-base retarget warning (pass 1) ─────────────────────────
# 110 is stacked on 'old-stack', which is no open PR's head: warned. 111 is
# stacked on 112's head (still open), 113 is on feature/x and 114 on
# release/0.8: none warned. Only 112 targets main.
case11() {
  local dir="$TMP/case11"
  new_case "$dir"
  make_pr "$dir/seed" "$dir/origin.git" 112 add_0004
  FAKE_GH_PRS="$dir/prs.json"
  cat >"$FAKE_GH_PRS" <<'JSON'
[
{"number":110,"headRefName":"pr-110","baseRefName":"old-stack","isDraft":false},
{"number":111,"headRefName":"pr-111","baseRefName":"pr-112","isDraft":false},
{"number":112,"headRefName":"pr-112","baseRefName":"main","isDraft":false},
{"number":113,"headRefName":"pr-113","baseRefName":"feature/x","isDraft":false},
{"number":114,"headRefName":"pr-114","baseRefName":"release/0.8","isDraft":false}
]
JSON
  export FAKE_GH_PRS
  local summary="$dir/summary.md" out rc=0
  out="$(run_gate "$dir/work" "$summary" 2>&1)" || rc=$?
  [ "$rc" -eq 0 ] || fail "a stacked-base warning is informational, not a failure: $out"
  grep -qF "::warning::PR #110 is stacked on 'old-stack'" <<<"$out" || fail "PR #110 must be warned: $out"
  local n
  for n in 111 113 114; do
    grep -qF "PR #$n is stacked" <<<"$out" && fail "PR #$n must not be warned: $out"
  done
  grep -q "^1 PR(s) stacked on a merged/closed base" "$summary" \
    || fail "summary must count exactly one stacked finding: $(cat "$summary")"
  echo "ok  the stacked-base warning names only a PR whose base is no longer open"
}

case1
case2
case3
case4
case5
case6
case7
case8
case9
case10
case11

echo "test-nightly-migration-merge-check: all cases passed"
