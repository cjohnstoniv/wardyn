#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# release-patch.sh: `make release-patch V=x.y.z`. One command takes a patch from
# "fixes ready" to "published and verified" (RELEASING.md, "The patch command").
#
# Phases (PHASE=prepare|wait|publish|all, default all). Each one first detects
# what already exists (merge, release commit, pushed branch, PR, nightly run,
# fast-forward, tag, published Release) and skips it, so a crash or a usage
# limit resumes instead of restarting.
#   prepare  candidate branch off origin/release/X.Y, merge MERGE, the release
#            commit, local checks, push, PR into release/X.Y, nightly dispatch
#   wait     every WAIT_INTERVAL s (60) scripts/green-by-tree.sh with
#            NEED_STAGING=1 on the candidate head; exit 0 starts the clock (T0)
#   publish  green-by-tree again, fast-forward release/X.Y, tag, watch release.yml,
#            publish the draft, scripts/verify-release.sh, the step table
#
# In: V (X.Y.Z or X.Y.Z-rc.N, required); BRANCH (existing branch holding
# origin/release/X.Y); MERGE (ref to merge, e.g. origin/main); NOTES (CHANGELOG
# paste file); BODY (release body file, default: the new CHANGELOG section);
# HIGHLIGHTS (ROADMAP row text, needed when the row is missing); ISSUES (numbers;
# one `Closes #N` per line in the PR body); PHASE; DRY_RUN=1.
#
# DRY_RUN=1 rehearses: no release commit (release-commit.sh --dry-run only warns
# when it would refuse), no PR, no push to release/*, no tag, no Release. The
# nightly dispatch and the wait phase run as usual; publish becomes
# `gh workflow run release.yml --ref <branch> -f path=promote -f dry_run=true`,
# watched, then the same table. The local checks are skipped (a rehearsal branch
# need not carry a release commit).
#
# It never reruns a failed job and never runs the forward-port: it prints them.
# It never forces a push: a rejected push means a branch moved, and it stops.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$(git rev-parse --show-toplevel)"

V="${V:-}"
BRANCH="${BRANCH:-}"
MERGE="${MERGE:-}"
NOTES="${NOTES:-}"
BODY="${BODY:-}"
HIGHLIGHTS="${HIGHLIGHTS:-}"
ISSUES="${ISSUES:-}"
PHASE="${PHASE:-all}"
DRY_RUN="${DRY_RUN:-0}"
WAIT_INTERVAL="${WAIT_INTERVAL:-60}"
RUN_POLL_INTERVAL="${RUN_POLL_INTERVAL:-5}"
BODY_LIMIT=125000 # GitHub's release body limit, in characters

say() { echo "release-patch: $*"; }
die() { echo "release-patch: REFUSE: $*" >&2; exit 1; }

# ── the step table ───────────────────────────────────────────────────────────
PRE_ROWS=(); CLK_ROWS=(); T0=""; STEP=""; STEP_AT=0
begin() { STEP="$1"; STEP_AT=$SECONDS; say "$1"; }
finish() {  # finish <result>: record the row for the step since begin()
  local row
  row="$(printf '%-52s %6ss  %s' "$STEP" "$((SECONDS - STEP_AT))" "$1")"
  if [ -n "$T0" ]; then CLK_ROWS+=("$row"); else PRE_ROWS+=("$row"); fi
}
print_table() {
  echo
  if [ "${#PRE_ROWS[@]}" -gt 0 ]; then
    echo "before the clock:"; printf '  %s\n' "${PRE_ROWS[@]}"
  fi
  if [ -n "$T0" ]; then
    echo "from T0:"
    [ "${#CLK_ROWS[@]}" -eq 0 ] || printf '  %s\n' "${CLK_ROWS[@]}"
    printf '  %-52s %6ss\n' "total from T0" "$((SECONDS - T0))"
  fi
}

# ── inputs ───────────────────────────────────────────────────────────────────
VERSION_RE='^([0-9]+)\.([0-9]+)\.([0-9]+)(-rc\.([0-9]+))?$'
[[ "$V" =~ $VERSION_RE ]] || die "V must be X.Y.Z or X.Y.Z-rc.N (got '$V'): make release-patch V=0.8.4"
XY="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}"
RB="release/$XY"
case "$PHASE" in prepare|wait|publish|all) ;; *) die "PHASE must be prepare, wait, publish or all (got '$PHASE')" ;; esac
case "$DRY_RUN" in 0|1) ;; *) die "DRY_RUN must be 0 or 1 (got '$DRY_RUN')" ;; esac
[ -z "$ISSUES" ] || [[ "$ISSUES" =~ ^[0-9]+([ ,]+[0-9]+)*$ ]] || die "ISSUES must be issue numbers separated by spaces or commas (got '$ISSUES')"
[ -z "$NOTES" ] || [ -r "$NOTES" ] || die "NOTES file '$NOTES' is not readable"
if [ -n "$BODY" ]; then
  [ -r "$BODY" ] || die "BODY file '$BODY' is not readable"
  [ "$(wc -m <"$BODY")" -le "$BODY_LIMIT" ] || die "BODY is over GitHub's ${BODY_LIMIT}-character release limit"
fi
CAND="${BRANCH:-chore/release-$V}"
case "$CAND" in main|release/*) die "the candidate branch must not be main or release/*: '$CAND'" ;; esac
want_phase() { [ "$PHASE" = all ] || [ "$PHASE" = "$1" ]; }

# vkey <X.Y.Z[-rc.N]>: a string that sorts as the version does (an rc below its final).
vkey() {
  [[ "$1" =~ $VERSION_RE ]] || return 1
  printf '%09d.%09d.%09d.%09d' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}" "${BASH_REMATCH[5]:-999999999}"
}

# ── refusals that apply to every phase ───────────────────────────────────────
[ -z "$(git status --porcelain)" ] || die "the worktree is not clean: commit or stash first"
gh auth status >/dev/null 2>&1 || die "gh auth status fails: run gh auth login"
git fetch origin --tags --quiet || die "git fetch origin --tags failed"
git rev-parse --verify -q "origin/$RB^{commit}" >/dev/null || die "origin/$RB does not exist"
REPO="${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"
[ -n "$REPO" ] || die "cannot tell the repository (set GITHUB_REPOSITORY=owner/name)"

LATEST=""; LATEST_KEY=""
while IFS= read -r t; do
  [ "$t" = "v$V" ] && continue
  k="$(vkey "${t#v}")" || continue
  if [ -z "$LATEST_KEY" ] || [[ "$k" > "$LATEST_KEY" ]]; then LATEST="$t"; LATEST_KEY="$k"; fi
done < <(git tag -l "v$XY.*")
[ -n "$LATEST" ] || die "no v$XY.* tag to patch from"
[[ "$(vkey "$V")" > "$LATEST_KEY" ]] || die "$V must be greater than every v$XY.* tag; the newest is $LATEST"
FROM="${LATEST#v}"

# A tag vV may exist only as this candidate's own, resumed release.
if git rev-parse --verify -q "refs/tags/v$V" >/dev/null; then
  tagsha="$(git rev-parse "refs/tags/v$V^{commit}")"
  candsha="$(git rev-parse --verify -q "refs/heads/$CAND^{commit}" || git rev-parse --verify -q "refs/remotes/origin/$CAND^{commit}" || true)"
  [ -n "$candsha" ] && [ "$tagsha" = "$candsha" ] || die "tag v$V already exists, at ${tagsha:0:12}, which is not the head of $CAND: $V must be greater than every v$XY.* tag"
fi

# ── the candidate branch ─────────────────────────────────────────────────────
cur="$(git symbolic-ref -q --short HEAD || true)"
if git rev-parse --verify -q "refs/heads/$CAND" >/dev/null || git rev-parse --verify -q "refs/remotes/origin/$CAND" >/dev/null; then
  [ "$cur" = "$CAND" ] || git checkout -q "$CAND" || die "cannot check out $CAND"
elif [ -n "$BRANCH" ]; then
  die "BRANCH '$BRANCH' does not exist locally or on origin"
elif want_phase prepare; then
  git checkout -q --no-track -b "$CAND" "origin/$RB"
else
  die "no candidate branch $CAND: run PHASE=prepare first"
fi
if ! git merge-base --is-ancestor "origin/$RB" HEAD; then
  if [ "$DRY_RUN" = 1 ]; then
    say "warning: $CAND does not contain origin/$RB (a rehearsal allows it: nothing is committed or pushed to release/*)"
  else
    die "$CAND does not contain origin/$RB: merge it in, or cut the candidate from it"
  fi
fi

WATCHED="$(sed -n 's/^ *watched="\([^"]*\)".*/\1/p' .github/workflows/release.yml | head -1)"
[ -n "$WATCHED" ] || die "cannot read the watched= list from .github/workflows/release.yml"

remote_tip() { git ls-remote origin "refs/heads/$1" | awk '{print $1}'; }

# nightly_state <tree>: active (a dispatched run is queued or running on it), green
# (the newest finished, non-cancelled dispatched run has every watched job green
# and the multi-arch build green), or none.
nightly_state() {
  local tree="$1" runs id rows
  runs="$(gh api -X GET "repos/$REPO/actions/workflows/nightly.yml/runs" -f event=workflow_dispatch -f per_page=100)" \
    || { echo "release-patch: cannot list the nightly runs" >&2; return 1; }
  if [ "$(jq --arg t "$tree" '[.workflow_runs[] | select(.head_commit.tree_id == $t and .event == "workflow_dispatch" and .status != "completed")] | length' <<<"$runs")" -gt 0 ]; then
    echo active; return 0
  fi
  id="$(jq -r --arg t "$tree" '[.workflow_runs[] | select(.head_commit.tree_id == $t and .event == "workflow_dispatch" and .status == "completed" and .conclusion != "cancelled")] | sort_by(.created_at) | reverse | .[0].id // empty' <<<"$runs")"
  if [ -z "$id" ]; then echo none; return 0; fi
  rows="$(gh api -X GET --paginate "repos/$REPO/actions/runs/$id/jobs" -f per_page=100 --jq '.jobs[] | [.name, .conclusion] | @tsv')" \
    || { echo "release-patch: cannot read the jobs of nightly run $id" >&2; return 1; }
  if awk -F'\t' -v watched="$WATCHED" '
    BEGIN { n = split(watched, w, " ") }
    { seen[$1] = 1; if ($2 != "success") bad[$1] = 1
      if (index($1, "multi-arch build (") == 1) { m = 1; if ($2 != "success") mbad = 1 } }
    END { for (i = 1; i <= n; i++) if (!(w[i] in seen) || (w[i] in bad)) exit 1; if (!m || mbad) exit 1 }' <<<"$rows"; then
    echo green
  else
    echo none
  fi
}

# ── prepare ──────────────────────────────────────────────────────────────────
phase_prepare() {
  local sha tree rtip n body state

  if [ -n "$MERGE" ]; then
    begin "merge $MERGE"
    git rev-parse --verify -q "$MERGE^{commit}" >/dev/null || die "MERGE '$MERGE' is not a commit"
    if git merge-base --is-ancestor "$MERGE" HEAD; then finish "skipped (already in)"
    else
      git -c rerere.enabled=false merge --no-ff --no-edit --signoff "$MERGE" \
        || die "merging $MERGE conflicted. Resolve it, commit, and re-run: it resumes here"
      finish ok
    fi
  fi

  begin "release commit $V"
  if git grep -qF -e "## [$V]" HEAD -- CHANGELOG.md; then finish "skipped (CHANGELOG already has $V)"
  else
    local args=(--tree . --from "$FROM" --to "$V")
    [ -z "$NOTES" ] || args+=(--notes "$NOTES")
    [ -z "$HIGHLIGHTS" ] || args+=(--highlights "$HIGHLIGHTS")
    if [ "$DRY_RUN" = 1 ]; then
      "$HERE/release-commit.sh" --dry-run "${args[@]}" \
        || say "warning: the release commit would be refused on this branch. The rehearsal goes on; a missing ROADMAP Shipped row needs HIGHLIGHTS='...'"
      finish "dry run, no commit"
    else
      "$HERE/release-commit.sh" --apply "${args[@]}" \
        || die "release-commit.sh refused (exit $?). A missing ROADMAP Shipped row needs HIGHLIGHTS='...'; a repeated cut exits 4"
      finish ok
    fi
  fi

  sha="$(git rev-parse HEAD)"
  rtip="$(remote_tip "$CAND")"

  begin "local checks"
  if [ "$DRY_RUN" = 1 ]; then finish "skipped (DRY_RUN)"
  elif [ "$rtip" = "$sha" ]; then finish "skipped (already pushed)"
  else
    make dco DCO_RANGE="HEAD ^origin/main ^origin/$RB" || die "make dco failed"
    go test -count=1 ./cmd/wardyn/ -run 'TestVersionMatchesChangelog|TestShippedVersionStringsAgree' || die "the version tests failed"
    ./scripts/test-claims-match-code.sh || die "test-claims-match-code.sh failed"
    finish ok
  fi

  begin "push $CAND"
  if [ "$rtip" = "$sha" ]; then finish "skipped (already pushed)"
  else
    git push origin "HEAD:refs/heads/$CAND" || die "pushing $CAND was rejected: it moved on origin. Merge or rebase it; nothing is forced"
    finish ok
  fi

  begin "pull request into $RB"
  if [ "$DRY_RUN" = 1 ]; then finish "skipped (DRY_RUN)"
  else
    n="$(gh pr list --head "$CAND" --base "$RB" --state open --json number --jq '.[0].number // empty')" || die "cannot list pull requests"
    if [ -n "$n" ]; then finish "skipped (PR #$n exists)"
    else
      body="$(mktemp)"
      {
        echo "Release $V."
        echo
        echo "Candidate for $RB: the fixes, then the release commit. scripts/green-by-tree.sh matches CI and the nightly to this tree."
        for n in ${ISSUES//,/ }; do echo; echo "Closes #$n"; done
      } >"$body"
      gh pr create --base "$RB" --head "$CAND" --title "release: $V" --body-file "$body" >/dev/null || die "gh pr create failed"
      rm -f "$body"
      finish ok
    fi
  fi

  begin "nightly on $CAND"
  tree="$(git rev-parse 'HEAD^{tree}')"
  state="$(nightly_state "$tree")" || exit 1
  case "$state" in
    active) finish "skipped (a nightly is queued or running on this tree)" ;;
    green) finish "skipped (a nightly is already green on this tree)" ;;
    *)
      gh workflow run nightly.yml --ref "$CAND" || die "gh workflow run nightly.yml failed"
      finish ok ;;
  esac
}

# ── wait ─────────────────────────────────────────────────────────────────────
active_runs() {  # active_runs <sha>: how many ci.yml and nightly.yml runs on it are unfinished
  local wf n total=0
  for wf in ci.yml nightly.yml; do
    n="$(gh run list --workflow "$wf" --commit "$1" --limit 100 --json status --jq '[.[] | select(.status != "completed")] | length')" \
      || die "cannot list the $wf runs"
    total=$((total + ${n:-0}))
  done
  echo "$total"
}

report_red() {  # report_red <sha>: each red run with its red jobs and the rerun command (printed only)
  local wf id names found=0
  for wf in ci.yml nightly.yml; do
    while IFS= read -r id; do
      [ -n "$id" ] || continue
      found=1
      names="$(gh run view "$id" --json jobs --jq '.jobs[] | select(.conclusion == "failure" or .conclusion == "timed_out") | .name' 2>/dev/null)" || names=""
      echo "  $wf run $id is red"
      [ -z "$names" ] || sed 's/^/    red job: /' <<<"$names"
      echo "    next: gh run rerun $id --failed"
    done < <(gh run list --workflow "$wf" --commit "$1" --limit 100 --json databaseId,status,conclusion \
      --jq '.[] | select(.status == "completed" and (.conclusion == "failure" or .conclusion == "timed_out")) | .databaseId')
  done
  [ "$found" = 1 ] || echo "  no red ci.yml or nightly.yml run on $1. Push the branch, or: gh workflow run nightly.yml --ref $CAND"
}

phase_wait() {
  local sha out rc err reason
  sha="$(git rev-parse HEAD)"
  [ "$(remote_tip "$CAND")" = "$sha" ] || die "$CAND is not pushed at ${sha:0:12}: run PHASE=prepare first"
  begin "wait for CI and the nightly on ${sha:0:12}"
  err="$(mktemp)"
  while :; do
    if out="$(GITHUB_REPOSITORY="$REPO" WATCHED="$WATCHED" NEED_STAGING=1 "$HERE/green-by-tree.sh" "$sha" 2>"$err")"; then rc=0; else rc=$?; fi
    reason="$(tail -n 3 "$err" | tr '\n' ' ')"
    case "$rc" in
      0)
        rm -f "$err"
        sed 's/^/  /' <<<"$out"
        finish "green: T0 starts"
        T0=$SECONDS
        return 0 ;;
      2)
        rm -f "$err"
        die "green-by-tree could not tell (exit 2): $reason" ;;
      *)
        if [ "$(active_runs "$sha")" -gt 0 ]; then
          say "waiting ${WAIT_INTERVAL}s: $reason"
          sleep "$WAIT_INTERVAL"
          continue
        fi
        rm -f "$err"
        {
          echo "release-patch: STOP: not green, and no ci.yml or nightly.yml run is active on ${sha:0:12}: $reason"
          report_red "$sha"
          echo "release-patch: it never reruns on its own. Rerun the failed jobs, then make release-patch V=$V again: it resumes."
        } >&2
        exit 1 ;;
    esac
  done
}

# ── publish ──────────────────────────────────────────────────────────────────
# find_run <gh run list args for release.yml...> <jq filter that prints an id>:
# polls up to RUN_POLL_INTERVAL x 30 for the run to show up.
find_run() {
  local i id
  for ((i = 0; i < 30; i++)); do
    id="$(gh run list --workflow release.yml "${@:1:$#-1}" --limit 10 --json databaseId,headBranch,event --jq "${*: -1}")" \
      || { echo "release-patch: cannot list the release.yml runs" >&2; return 1; }
    [ -z "$id" ] || { echo "$id"; return 0; }
    sleep "$RUN_POLL_INTERVAL"
  done
  return 1
}

release_body() {  # prints the path of the release body file
  local f
  if [ -n "$BODY" ]; then echo "$BODY"; return 0; fi
  f="$(mktemp)"
  awk -v h="## [$V]" 'index($0, h) == 1 { p = 1; next } p && /^## \[/ { exit } p' CHANGELOG.md | sed '/./,$!d' >"$f"
  [ -s "$f" ] || { echo "release-patch: CHANGELOG.md has no body under ## [$V]" >&2; return 1; }
  [ "$(wc -m <"$f")" -le "$BODY_LIMIT" ] || { echo "release-patch: the CHANGELOG section for $V is over the ${BODY_LIMIT}-character limit; pass BODY=file" >&2; return 1; }
  echo "$f"
}

print_forward_port() {
  local rel
  rel="$(git log --format=%H --grep="^release: $V\$" -1 "$1")"
  cat <<EOF

forward-port (printed, never run):
  git fetch origin
  git checkout -b chore/forward-port-$V origin/main
  git cherry-pick -x --signoff ${rel:-<the "release: $V" commit>}
  # CHANGELOG: move the shipped entries out of [Unreleased] into the dated [$V] section
  git push origin chore/forward-port-$V   # then open a PR into main
EOF
}

phase_publish() {
  local sha rtip rc id queued state body vout err prev
  sha="$(git rev-parse HEAD)"
  [ "$(remote_tip "$CAND")" = "$sha" ] || die "$CAND is not pushed at ${sha:0:12}: run PHASE=prepare first"
  [ -n "$T0" ] || T0=$SECONDS

  begin "green-by-tree"
  err="$(mktemp)"
  if GITHUB_REPOSITORY="$REPO" WATCHED="$WATCHED" NEED_STAGING=1 "$HERE/green-by-tree.sh" "$sha" >/dev/null 2>"$err"; then rc=0; else rc=$?; fi
  if [ "$rc" != 0 ]; then
    echo "release-patch: green-by-tree said: $(tail -n 3 "$err" | tr '\n' ' ')" >&2
    rm -f "$err"
    die "publish refused: the tree of ${sha:0:12} is not green (green-by-tree exit $rc). PHASE=wait shows why"
  fi
  rm -f "$err"
  finish ok

  begin "queued runs in the repo"
  queued="$(gh run list --status queued --limit 100 --json databaseId --jq length)" || queued=0
  if [ "${queued:-0}" -gt 0 ]; then
    say "warning: $queued run(s) are queued in the repo and may delay release.yml's jobs"
    finish "warning: $queued queued"
  else
    finish ok
  fi

  if [ "$DRY_RUN" = 1 ]; then
    begin "release.yml promote dry run on $CAND"
    prev="$(gh run list --workflow release.yml --branch "$CAND" --event workflow_dispatch --limit 1 --json databaseId --jq '.[0].databaseId // 0')" \
      || die "cannot list the release.yml runs"
    gh workflow run release.yml --ref "$CAND" -f path=promote -f dry_run=true || die "gh workflow run release.yml failed"
    id="$(find_run --branch "$CAND" --event workflow_dispatch "[.[] | select(.databaseId > ${prev:-0})] | max_by(.databaseId) | .databaseId // empty")" \
      || die "the promote dry run did not appear"
    gh run watch "$id" --exit-status || die "the promote dry run $id failed: gh run view $id --log-failed"
    finish "ok (run $id)"
    print_table
    say "dry run finished: no commit, PR, release/* push, tag or Release was made"
    return 0
  fi

  begin "fast-forward origin/$RB"
  git fetch origin --quiet || die "git fetch origin failed"
  rtip="$(remote_tip "$RB")"
  if [ "$rtip" = "$sha" ]; then finish "skipped (already there)"
  else
    git merge-base --is-ancestor "origin/$RB" "$sha" || die "$RB moved: origin/$RB is not an ancestor of ${sha:0:12}. Prepare the candidate again"
    git push origin "$sha:refs/heads/$RB" || die "the push to $RB was rejected: it moved. Nothing is forced"
    finish ok
  fi

  begin "tag v$V"
  rtip="$(git ls-remote origin "refs/tags/v$V^{}" | awk '{print $1}')"
  if [ "$rtip" = "$sha" ]; then finish "skipped (already pushed)"
  elif [ -n "$rtip" ]; then die "tag v$V already exists on origin at ${rtip:0:12}, not ${sha:0:12}"
  else
    git rev-parse --verify -q "refs/tags/v$V" >/dev/null || git tag -a "v$V" -m "Wardyn v$V" "$sha"
    git push origin "v$V" || die "pushing v$V failed"
    finish ok
  fi

  begin "release.yml run for v$V"
  id="$(find_run --branch "v$V" "[.[] | select(.headBranch == \"v$V\")] | max_by(.databaseId) | .databaseId // empty")" \
    || die "no release.yml run for v$V appeared"
  gh run watch "$id" --exit-status || die "release.yml run $id failed: gh run view $id --log-failed. Rerun its failed jobs, then run this command again"
  finish "ok (run $id)"

  begin "publish the Release"
  state="$(gh release view "v$V" --json isDraft --jq .isDraft)" || state=""
  if [ "$state" = false ]; then finish "skipped (already published)"
  else
    body=""
    [ "$state" != true ] || body="$(release_body)" || die "no release body"
    if [ "$state" != true ] || ! gh release edit "v$V" --draft=false --prerelease --title "Wardyn v$V" --notes-file "$body"; then
      echo "release-patch: no draft for v$V, which release-assets should have made. The releases:" >&2
      gh api -X GET "repos/$REPO/releases" --jq '.[] | [.tag_name, .draft, .prerelease, .name] | @tsv' >&2 || echo "release-patch: the listing failed too" >&2
      die "gh release edit v$V could not publish the draft"
    fi
    finish ok
  fi

  begin "verify-release"
  vout="$(mktemp)"
  set +e
  V="$V" "$HERE/verify-release.sh" 2>&1 | tee "$vout"
  rc="${PIPESTATUS[0]}"
  set -e
  grep -qx 'fails=0' "$vout" || rc=1
  rm -f "$vout"
  [ "$rc" = 0 ] || die "verify-release.sh did not end fails=0 (exit $rc): the Release is published but not verified"
  finish "ok (fails=0)"

  print_table
  print_forward_port "$sha"
}

# ── run ──────────────────────────────────────────────────────────────────────
if want_phase prepare; then phase_prepare; fi
if want_phase wait; then phase_wait; fi
if want_phase publish; then phase_publish; fi
[ "$PHASE" = all ] || [ "$PHASE" = publish ] || print_table
exit 0
