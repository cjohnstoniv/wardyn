#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# test-migration-numbers.sh — check-migration-numbers.sh actually reads
# the branch it targets on origin, not just the working tree, and the band
# rules hold: each branch numbers from its own band, a byte-for-byte copy of a
# file its own release line carries keeps its number, and the table-overlap pass
# (scripts/lib/migration-overlap.py) refuses a pair of band files that two lines
# would run in different orders.
#
# The gate's whole point is comparing THIS branch's new migration numbers
# against a remote it did not pick them from, so the fixture builds a real
# throwaway git repo with a real 'origin' remote rather than a bare directory
# of files — a bug that made the gate silently fall back to a same-repo
# comparison (always "no collision") would still pass a fixture with no
# origin to diverge from.
#
# Daemon-free, network-free: 'origin' is a local bare repo on disk.
set -euo pipefail

# Throwaway repos get no background git: a commit or fetch can leave detached
# auto-maintenance writing under .git when the EXIT trap runs rm -rf (#3328).
export GIT_CONFIG_COUNT=3 GIT_CONFIG_KEY_0=maintenance.auto GIT_CONFIG_VALUE_0=false GIT_CONFIG_KEY_1=gc.auto GIT_CONFIG_VALUE_1=0 GIT_CONFIG_KEY_2=gc.autoDetach GIT_CONFIG_VALUE_2=false

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A developer's global/system git config (commit.gpgsign, hooks paths, init
# defaults) must not reach the fixture's seed commits.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

fail() { echo "FAIL: $*" >&2; exit 1; }

MIG=internal/db/migrations
TSV=internal/db/migration_bands.tsv

# install DIR: the gate and its Python pass, as a repo carries them.
install() {
  mkdir -p "$1/scripts/lib"
  cp "$ROOT/scripts/check-migration-numbers.sh" "$1/scripts/"
  cp "$ROOT/scripts/lib/migration-overlap.py" "$1/scripts/lib/"
}

# The gate reads GITHUB_BASE_REF / GITHUB_REF_NAME, which CI sets for THIS
# repo's build (a push build of feature/** would make every case skip), so the
# fixture runs start without them and a case that needs one passes it in.
gate() { local d=$1; shift; (cd "$d" && env -u GITHUB_BASE_REF -u GITHUB_REF_NAME "$@" ./scripts/check-migration-numbers.sh); }

# expect_pass DIR [VAR=val ...] -- PATTERN...: the gate exits 0 and its output (stdout and stderr) has every PATTERN.
# expect_fail likewise for exit 1. Output is captured first and matched after, never piped into grep: under
# pipefail a pipe would report grep's early exit instead of the gate's.
expect() {
  local want=$1 d=$2 out rc=0 p
  shift 2
  local envs=()
  while [ "$#" -gt 0 ] && [ "$1" != -- ]; do
    envs+=("$1")
    shift
  done
  shift
  out="$(gate "$d" "${envs[@]+"${envs[@]}"}" 2>&1)" || rc=$?
  [ "$rc" -eq "$want" ] || fail "$d ${envs[*]-}: exit $rc, want $want: $out"
  for p in "$@"; do
    printf '%s' "$out" | grep -e "$p" >/dev/null || fail "$d ${envs[*]-}: the output must have '$p': $out"
  done
}
expect_pass() { expect 0 "$@"; }
expect_fail() { expect 1 "$@"; }

# put DIR NAME TEXT: a migration file in DIR's working tree. drop DIR NAME...: removes such files.
put() { printf '%s\n' "$3" >"${1:?}/$MIG/${2:?}"; }
drop() {
  local d=${1:?} n
  shift
  for n in "$@"; do rm -f "${d:?}/${MIG:?}/${n:?}"; done
}

# seed_to BRANCH [START]: SEED on BRANCH (created from START, else origin/BRANCH, when it is not local yet).
seed_to() {
  git -C "$SEED" fetch --quiet origin
  if git -C "$SEED" rev-parse --verify -q "refs/heads/$1" >/dev/null; then
    git -C "$SEED" checkout --quiet "$1"
  else
    git -C "$SEED" checkout --quiet -b "$1" "${2:-origin/$1}"
  fi
}
# seed_commit MESSAGE BRANCH: commit what SEED's tree holds and push it to origin/BRANCH.
seed_commit() {
  git -C "$SEED" add -A
  git -C "$SEED" commit --quiet -m "$1"
  git -C "$SEED" push --quiet origin "$2"
}
# flip DIR: the 0.12 commit's table change in DIR: the 0400 row closed, the 0700 row open.
flip() {
  awk -F'\t' -v OFS='\t' '$1 == "0400" { $3 = "no" } $1 == "0700" { $3 = "yes" } { print }' "$1/$TSV" >"$TMP/bands.flipped"
  cp "$TMP/bands.flipped" "$1/$TSV"
}

# ── build a bare 'origin' ────────────────────────────────────────────────────
BARE="$TMP/origin.git"
git init --quiet --bare -b main "$BARE"

SEED="$TMP/seed"
git init --quiet -b main "$SEED"
git -C "$SEED" config user.email "test@example.com"
git -C "$SEED" config user.name "test"
git -C "$SEED" remote add origin "$BARE"

# main: the pre-band migrations 0001..0003 and the band table.
mkdir -p "$SEED/$MIG"
for n in 0001 0002 0003; do
  echo "-- migration $n" >"$SEED/$MIG/${n}_seed.sql"
done
cp "$ROOT/$TSV" "$SEED/$TSV"
seed_commit legacy main

# release/0.10: a late pre-band file and a band-0300 patch, released as v0.10.9.
seed_to release/0.10 main
put "$SEED" 0004_late.sql '-- shipped on 0.10 before bands'
put "$SEED" 0300_patch.sql 'ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS patch_note text;'
seed_commit patch release/0.10
git -C "$SEED" tag v0.10.9
git -C "$SEED" push --quiet origin v0.10.9

# main: a band-0400 file that creates a table and alters one.
seed_to main
put "$SEED" 0400_main.sql 'CREATE TABLE IF NOT EXISTS main_only (id int PRIMARY KEY);
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS main_note text;'
seed_commit main-band main

# WORK: a clone on main (it gets the tags and every branch). RWORK: a clone at release/0.10.
WORK="$TMP/work"
git clone --quiet "$BARE" "$WORK"
git -C "$WORK" checkout --quiet main
git -C "$WORK" config user.email "test@example.com"
git -C "$WORK" config user.name "test"
install "$WORK"
RWORK="$TMP/rwork"
git clone --quiet -b release/0.10 "$BARE" "$RWORK"
git -C "$RWORK" config user.email "test@example.com"
git -C "$RWORK" config user.name "test"
install "$RWORK"

# 1. a new migration numbered past origin/main's band max => pass.
put "$WORK" 0401_new.sql 'CREATE TABLE IF NOT EXISTS new_one (id int);'
expect_pass "$WORK" -- 'band 0400-0599'
echo "ok  new prefix past the remote max passes"
drop "$WORK" 0401_new.sql

# 2. a new migration REUSING origin/main's max prefix => fail. This is the
#    regression the gate exists for: two branches off the same base each
#    independently pick "the next number" and each looks clean in isolation.
put "$WORK" 0400_collide.sql '-- collide'
expect_fail "$WORK" -- '0400_collide'
echo "ok  prefix <= remote max fails, naming the file"
drop "$WORK" 0400_collide.sql

# 3. a pre-band number that is not a released file => fail.
put "$WORK" 0005_old.sql '-- old'
expect_fail "$WORK" -- 'below every band'
echo "ok  a new pre-band prefix fails"
drop "$WORK" 0005_old.sql

# 4. a pre-band file that is byte for byte the one at a release tag passes.
git -C "$WORK" show "v0.10.9:$MIG/0004_late.sql" >"$WORK/$MIG/0004_late.sql"
expect_pass "$WORK" -- 'v0.10.9'
echo "ok  a pre-band file copied from a release tag passes"
drop "$WORK" 0004_late.sql

# 5. a band-0300 file copied byte for byte from the 0.10 tag keeps its number on main.
git -C "$WORK" show "v0.10.9:$MIG/0300_patch.sql" >"$WORK/$MIG/0300_patch.sql"
expect_pass "$WORK" -- 'v0.10.9'
echo "ok  a forward-port keeps its number"

# 6. the same file with one line appended is not a forward-port.
echo "-- edited" >>"$WORK/$MIG/0300_patch.sql"
expect_fail "$WORK" -- 'byte-for-byte'
echo "ok  an edited copy fails"
drop "$WORK" 0300_patch.sql

# 7. no tag and no release/0.10 ref in the checkout: the copy has nothing to match.
git -C "$WORK" show "v0.10.9:$MIG/0300_patch.sql" >"$WORK/$MIG/0300_patch.sql"
git -C "$WORK" tag -d v0.10.9 >/dev/null
git -C "$WORK" update-ref -d refs/remotes/origin/release/0.10
expect_fail "$WORK" -- 'fetch the tags'
git -C "$WORK" fetch --quiet --tags origin
git -C "$WORK" fetch --quiet origin release/0.10:refs/remotes/origin/release/0.10
drop "$WORK" 0300_patch.sql
echo "ok  a copy with no tag and no branch to match fails, saying to fetch the tags"

# 8. a band no line of main's own may take (0600).
put "$WORK" 0600_x.sql '-- x'
expect_fail "$WORK" -- '0600_x'
echo "ok  a 0.11-line band file fails on main"
drop "$WORK" 0600_x.sql

# 9. a band no release line owns, closed to main today (0700).
put "$WORK" 0700_x.sql '-- x'
expect_fail "$WORK" -- 'no release line owns'
echo "ok  band 0700 is closed to main today"
drop "$WORK" 0700_x.sql

# 10. an UNCHANGED tree must never be flagged — only NEW files are this gate's business.
expect_pass "$WORK" --
echo "ok  unmodified tree passes"

# 11. the scenario the gate exists for: a sibling's 0401 lands on origin/main AFTER this branch
#     forked, and this branch (fetched, never merged) adds its own 0401. The branch's own tree
#     still stops at 0400, so a gate that compared against HEAD instead of origin/main would pass it.
seed_to main
put "$SEED" 0401_sibling.sql '-- sibling'
seed_commit sibling main
git -C "$WORK" fetch --quiet origin
put "$WORK" 0401_mine.sql '-- mine'
expect_fail "$WORK" -- '0401_mine'
echo "ok  a sibling's migration landing on origin/main after the fork fails the branch's same number"
drop "$WORK" 0401_mine.sql

# 12-14. a PR build targeting release/0.10 numbers from band 0300.
put "$RWORK" 0301_fix.sql 'CREATE TABLE IF NOT EXISTS fix_one (id int);'
expect_pass "$RWORK" GITHUB_BASE_REF=release/0.10 -- 'band 0300-0399'
echo "ok  a release/0.10 patch takes band 0300"
drop "$RWORK" 0301_fix.sql
put "$RWORK" 0300_dup.sql '-- dup'
expect_fail "$RWORK" GITHUB_BASE_REF=release/0.10 -- '0300_dup'
echo "ok  a release/0.10 patch reusing 0300 fails"
drop "$RWORK" 0300_dup.sql
put "$RWORK" 0402_x.sql '-- x'
expect_fail "$RWORK" GITHUB_BASE_REF=release/0.10 -- '0402_x'
echo "ok  a release/0.10 patch in band 0400 fails"
drop "$RWORK" 0402_x.sql

# 15. a train branch of the 0.10 line takes the same band (the gate fetches the base itself).
git -C "$SEED" push --quiet origin release/0.10:refs/heads/train/0.10.4
put "$RWORK" 0301_fix.sql 'CREATE TABLE IF NOT EXISTS fix_one (id int);'
expect_pass "$RWORK" GITHUB_BASE_REF=train/0.10.4 --
echo "ok  train/0.10.* takes band 0300"
drop "$RWORK" 0301_fix.sql

# 16. a base no open band names fails closed.
git -C "$SEED" push --quiet origin main:refs/heads/feature/x
expect_fail "$WORK" GITHUB_BASE_REF=feature/x -- 'no open band'
echo "ok  an unknown base fails closed"

# 17. a file already on a train branch of its line, with no tag yet, is a forward-port too.
seed_to train/0.10.4
put "$SEED" 0301_shared.sql 'CREATE TABLE IF NOT EXISTS shared_one (id int);'
seed_commit shared train/0.10.4
git -C "$WORK" fetch --quiet origin
git -C "$SEED" show "origin/train/0.10.4:$MIG/0301_shared.sql" >"$WORK/$MIG/0301_shared.sql"
expect_pass "$WORK" -- 'origin/train/0.10.4'
echo "ok  a copy of a train branch's file passes"
drop "$WORK" 0301_shared.sql

# 18. release/0.11 takes band 0600, and still accepts a 0.10 forward-port.
git -C "$SEED" push --quiet origin main:refs/heads/release/0.11
R11="$TMP/r11"
git clone --quiet -b release/0.11 "$BARE" "$R11"
git -C "$R11" config user.email "test@example.com"
git -C "$R11" config user.name "test"
install "$R11"
put "$R11" 0600_x.sql 'CREATE TABLE IF NOT EXISTS x11 (id int);'
expect_pass "$R11" GITHUB_BASE_REF=release/0.11 -- 'band 0600-0699'
git -C "$R11" show "v0.10.9:$MIG/0300_patch.sql" >"$R11/$MIG/0300_patch.sql"
expect_pass "$R11" GITHUB_BASE_REF=release/0.11 --
echo "ok  release/0.11 takes band 0600 and accepts a 0.10 forward-port"
drop "$R11" 0600_x.sql 0300_patch.sql

# 19. once the 0.12 commit flips the table (0400 closed, 0700 open), main takes 0700 and refuses 0402.
flip "$WORK"
put "$WORK" 0700_next.sql 'CREATE TABLE IF NOT EXISTS next_one (id int);'
expect_pass "$WORK" -- 'band 0700-9999'
put "$WORK" 0402_late.sql '-- late'
expect_fail "$WORK" -- '0402_late'
drop "$WORK" 0700_next.sql 0402_late.sql
git -C "$WORK" checkout --quiet -- "$TSV"
echo "ok  after the table flips, main takes 0700 and refuses 0402"

# 20. a release/0.10 patch may not alter a table origin/main's band-0400 file alters.
put "$RWORK" 0301_bad.sql 'ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS bad text;'
expect_fail "$RWORK" GITHUB_BASE_REF=release/0.10 -- 'workspaces' '0400_main'
echo "ok  a 0.10 patch is checked against main's newer band at its own PR"
drop "$RWORK" 0301_bad.sql

# 21. a main file may not alter a table a 0.10 patch not yet on main touches.
seed_to release/0.10
put "$SEED" 0302_pending.sql 'UPDATE agent_runs SET repo = repo WHERE false;'
seed_commit pending release/0.10
git -C "$WORK" fetch --quiet origin
put "$WORK" 0402_alt.sql 'ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS alt text;'
expect_fail "$WORK" -- 'agent_runs' 'not yet on the base'
echo "ok  a main file may not alter a table a pending 0.10 file touches"
drop "$WORK" 0402_alt.sql

# 22. once those files are on main (their forward-port), the same file passes.
seed_to main
for f in 0004_late.sql 0300_patch.sql 0302_pending.sql; do
  git -C "$SEED" show "origin/release/0.10:$MIG/$f" >"$SEED/$MIG/$f"
done
seed_commit forward-port main
git -C "$WORK" pull --quiet --ff-only
put "$WORK" 0402_alt.sql 'ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS alt text;'
expect_pass "$WORK" --
echo "ok  after the forward-port the same file passes"
drop "$WORK" 0402_alt.sql

# 28. while the 0400 row is open, a 0.11 file already on release/0.11 still conflicts: a new 0400 file can land
#     after a 0600 file exists, so 0600 does not follow 0400.
seed_to release/0.11
put "$SEED" 0600_x11.sql 'ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS k11 text;'
seed_commit x11 release/0.11
git -C "$WORK" fetch --quiet origin
put "$WORK" 0402_k.sql 'ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS k04 text;'
expect_fail "$WORK" -- 'agent_runs' '0600_x11'
echo "ok  an open 0400 band does not let 0600 follow it"
drop "$WORK" 0402_k.sql

# 29. a redefined function is an altered object.
put "$SEED" 0601_fn.sql 'CREATE OR REPLACE FUNCTION f11() RETURNS int AS $$ SELECT 1 $$ LANGUAGE sql;'
seed_commit fn release/0.11
git -C "$WORK" fetch --quiet origin
put "$WORK" 0402_fn.sql 'CREATE OR REPLACE FUNCTION f11() RETURNS int AS $$ SELECT 2 $$ LANGUAGE sql;'
expect_fail "$WORK" -- 'fn:f11' '0601_fn'
echo "ok  a redefined function counts as an altered object"
drop "$WORK" 0402_fn.sql

# 30. after the flip, a 04xx file that landed on main after release/0.11 was cut is still compared with a new
#     06xx file: release/0.11 never carries it, so a 0.11.x database runs it after its 06xx.
flip "$R11"
seed_to main
put "$SEED" 0402_after_cut.sql 'UPDATE agent_runs SET repo = repo WHERE false;'
seed_commit after-cut main
git -C "$R11" pull --quiet --ff-only
put "$R11" 0602_k.sql 'ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS k06 text;'
expect_fail "$R11" GITHUB_BASE_REF=release/0.11 -- 'agent_runs' '0402_after_cut'
echo "ok  a closed 0400 band's late files still meet a new 0600 file"
drop "$R11" 0602_k.sql
git -C "$R11" checkout --quiet -- "$TSV"

# 31. an edit to a migration the base already has fails: Migrate never re-runs a recorded name.
echo "-- edited" >>"$WORK/$MIG/0001_seed.sql"
expect_fail "$WORK" -- 'is never edited'
git -C "$WORK" checkout --quiet -- "$MIG/0001_seed.sql"
echo "ok  an edited migration the base already has fails"

# 23. a push build of a branch other than main has no base to compare against and skips.
expect_pass "$WORK" GITHUB_REF_NAME=release/0.10 -- 'skipping'
echo "ok  push build of a non-main branch skips"

# 24. a checkout with no default fetch refspec (and so no refs/remotes/origin/*
#     after a plain fetch) still compares against the real origin/main.
git -C "$WORK" config --unset-all remote.origin.fetch
git -C "$WORK" update-ref -d refs/remotes/origin/main
put "$WORK" 0401_mine.sql '-- mine'
expect_fail "$WORK" -- '0401_mine'
echo "ok  no default fetch refspec still fetches origin/main"
drop "$WORK" 0401_mine.sql

# 25. no 'origin' remote at all => skip (exit 0), never a hard failure — this
#     gate degrades gracefully rather than blocking `make lint` offline.
NOORIGIN="$TMP/noorigin"
git init --quiet -b main "$NOORIGIN"
git -C "$NOORIGIN" config user.email "test@example.com"
git -C "$NOORIGIN" config user.name "test"
mkdir -p "$NOORIGIN/$MIG"
cp "$ROOT/$TSV" "$NOORIGIN/$TSV"
install "$NOORIGIN"
echo "-- x" >"$NOORIGIN/$MIG/0001_x.sql"
git -C "$NOORIGIN" add -A
git -C "$NOORIGIN" commit --quiet -m seed
expect_pass "$NOORIGIN" -- 'no .origin. remote'
echo "ok  no origin remote skips instead of failing"

# 26. a missing band table fails closed.
mv "$WORK/$TSV" "$TMP/bands.moved"
expect_fail "$WORK" -- 'is missing'
mv "$TMP/bands.moved" "$WORK/$TSV"
echo "ok  a missing band table fails"

# 27. the overlap pass's own selftest.
python3 -I "$ROOT/scripts/lib/migration-overlap.py" --selftest >/dev/null || fail "migration-overlap.py --selftest failed"
echo "ok  the overlap pass's selftest passes"

echo "test-migration-numbers: all cases passed"
