#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-nightly-migration-merge-check.sh — pins the riskiest logic in
# scripts/nightly-migration-merge-check.sh: the migration-prefix claim
# checks, the binary search that isolates a single bad PR out of several
# good ones, and the conflict handling that only fails the job on a
# conflict under internal/db/migrations (a union-mergeable conflict
# elsewhere must not).
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
  ( cd "$work" && env -u GITHUB_BASE_REF -u GITHUB_REF_NAME \
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
  echo "ok  binary search isolates and drops the one bad PR (#41), keeps #40/#42"
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
  grep -q "under \`internal/db/migrations\`" "$summary" \
    || fail "summary must name the migrations-dir conflict: $(cat "$summary")"
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

case1
case2
case3
case4
case5
case6

echo "test-nightly-migration-merge-check: all cases passed"
