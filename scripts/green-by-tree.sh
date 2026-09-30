#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# green-by-tree.sh <sha> — did CI and the nightly pass on this commit's TREE?
#
# release.yml's preflight-green and the release command both ask this. Evidence
# is keyed by the git tree of <sha>, not by the commit, so a run on any commit
# with the identical tree counts: a pull request's run, a push run on main, a
# nightly dispatched on the candidate branch. The tree must be IDENTICAL, never
# an ancestor's: an older tree is a different set of files.
#
# CI evidence. ci.yml uploads an artifact named ci-full-tree-<tree> holding the
# tree it checked out, in every run that runs everything. A run found through
# that artifact counts only if it is in this repository (no forks), is a run of
# .github/workflows/ci.yml, completed with conclusion success, has a head commit
# whose tree_id is the tree (a pull request that edited ci.yml to upload a
# marker for someone else's tree fails here), and every required status check of
# main's branch protection is a success job in it. The marker uploads before the
# tests run, so a cancelled or red run carries it too: the conclusion decides.
# head_commit.tree_id is the tree of the PR HEAD, while the marker holds the
# tree of GitHub's merge commit, so a pull request counts only when the two are
# equal. That fails closed: a PR whose merge moved the tree is never accepted.
#
# Nightly evidence. A completed workflow_dispatch run of nightly.yml in this
# repository whose head commit has the tree, where every name in WATCHED is a
# success job and there is at least one `multi-arch build (` job, all success.
# A scheduled run never counts: it builds nothing to stage, and GitHub may
# report its skipped matrix as one row named `multi-arch build (${{ matrix.name
# }})` with conclusion skipped. NEED_STAGING says the caller will promote
# staged images, which only a dispatched run pushes; today every qualifying run
# is a dispatched one, so it changes nothing but is validated so callers can say
# what they need.
#
# In: GITHUB_REPOSITORY=owner/name, GH_TOKEN, WATCHED="job job ..." (required),
# NEED_STAGING=0|1 (default 0). Needs gh and jq.
# Out: on green, stdout is only tree=, ci_run=, nightly_run= and nightly_sha=
# (fit for $GITHUB_OUTPUT); every reason goes to stderr.
# Exit 0 = green. Exit 1 = not green. Exit 2 = usage or API error (could not
# tell); the caller must not release.
set -uo pipefail

sha="${1:-}"
repo="${GITHUB_REPOSITORY:-}"
watched="${WATCHED:-}"
need_staging="${NEED_STAGING:-0}"
if [[ ! "${sha}" =~ ^[0-9a-f]{40}$ ]] || [ -z "${repo}" ] || [ -z "${watched}" ] \
  || { [ "${need_staging}" != 0 ] && [ "${need_staging}" != 1 ]; }; then
  echo "usage: GITHUB_REPOSITORY=owner/name WATCHED='job job' [NEED_STAGING=0|1] $0 <40-hex sha>" >&2
  exit 2
fi

say() { echo "green-by-tree: $*" >&2; }

# api <endpoint> [gh api args...]: a GET whose body lands in $OUT. -X GET is
# load-bearing: gh api turns into a POST the moment a -f field is present.
# Any failure ends the script with 2: nothing here falls back to other evidence.
api() {
  OUT="$(gh api -X GET "$@")" || { say "gh api $1 failed"; exit 2; }
}

# jq_out <jq args...>: runs jq over $OUT into $JQ; a bad filter or body is an API error.
jq_out() {
  JQ="$(jq "$@" <<<"${OUT}")" || { say "could not read the API response"; exit 2; }
}

# status_of <job name> <name<TAB>conclusion lines>: success, the first bad
# conclusion, or "not reported". Every job carrying the name must succeed.
status_of() {
  awk -F'\t' -v n="$1" '
    $1 == n { seen = 1; if ($2 != "success" && bad == "") bad = $2 }
    END { if (!seen) print "not reported"; else if (bad != "") print bad; else print "success" }' <<<"$2"
}

api "repos/${repo}/git/commits/${sha}"
jq_out -r '.tree.sha // empty'
tree="${JQ}"
[[ "${tree}" =~ ^[0-9a-f]{40}$ ]] || { say "no tree found for ${sha}"; exit 2; }

api "repos/${repo}"
jq_out -r '.id // empty'
repo_id="${JQ}"
[[ "${repo_id}" =~ ^[0-9]+$ ]] || { say "no repository id found for ${repo}"; exit 2; }

# The live required contexts, never a hand-typed copy. "Get a branch" needs only
# Contents: read; branches/main/protection would need Administration: read.
api "repos/${repo}/branches/main"
jq_out -r '.protection.required_status_checks.contexts[]?'
contexts="${JQ}"
[ -n "${contexts}" ] || { say "branch protection reported no required_status_checks.contexts: nothing to check against"; exit 2; }

# ci_run_green <run id>: 0 when this run is acceptable CI evidence for the tree.
ci_run_green() {
  local id="$1" ctx status good jobs
  api "repos/${repo}/actions/runs/${id}"
  jq_out -r --arg t "${tree}" '
    (.path == ".github/workflows/ci.yml" and .status == "completed" and .conclusion == "success"
      and .head_commit.tree_id == $t) | tostring'
  good="${JQ}"
  if [ "${good}" != true ]; then
    jq_out -r '"path \(.path), \(.status)/\(.conclusion), head tree \(.head_commit.tree_id)"'
    say "CI run ${id} does not count: ${JQ}"
    return 1
  fi
  api "repos/${repo}/actions/runs/${id}/jobs" --paginate -f per_page=100
  jq_out -r '.jobs[] | "\(.name)\t\(.conclusion)"'
  jobs="${JQ}"
  good=0
  while IFS= read -r ctx; do
    [ -n "${ctx}" ] || continue
    status="$(status_of "${ctx}" "${jobs}")"
    if [ "${status}" != success ]; then
      say "CI run ${id}: required context '${ctx}' is not green (saw: ${status})"
      good=1
    fi
  done <<<"${contexts}"
  return "${good}"
}

ci_run=""
api "repos/${repo}/actions/artifacts" -f "name=ci-full-tree-${tree}" -f per_page=100
# The name, expiry and repository filters are re-checked: an ignored query
# parameter must not turn someone else's artifact into ours.
jq_out -r --arg n "ci-full-tree-${tree}" --argjson rid "${repo_id}" '
  [.artifacts[]
   | select(.name == $n and .expired == false and .workflow_run.head_repository_id == $rid)]
  | sort_by(.created_at) | reverse | .[] | .workflow_run.id'
candidates="$(awk '!seen[$0]++' <<<"${JQ}")" || { say "could not list the marker runs"; exit 2; }
if [ -z "${candidates}" ]; then
  say "no unexpired ci-full-tree-${tree} marker from this repository: CI never ran everything on this tree"
fi
for id in ${candidates}; do
  if ci_run_green "${id}"; then ci_run="${id}"; break; fi
done
[ -n "${ci_run}" ] || say "no CI run is green on tree ${tree}"

nightly_run=""
nightly_sha=""
api "repos/${repo}/actions/workflows/nightly.yml/runs" -f status=completed -f per_page=100 -f event=workflow_dispatch
jq_out -r --arg r "${repo}" --arg t "${tree}" '
  [.workflow_runs[]
   | select(.head_repository.full_name == $r and .head_commit.tree_id == $t and .status == "completed")]
  | sort_by(.created_at) | reverse | .[] | [.id, .head_sha, .event] | @tsv'
nightlies="${JQ}"
read -ra watched_jobs <<<"${watched}"
while IFS=$'\t' read -r id head event; do
  [ -n "${id}" ] || continue
  if [ "${event}" != workflow_dispatch ]; then
    say "nightly run ${id} does not count: a ${event} run never qualifies a tree, only a dispatched one"
    continue
  fi
  api "repos/${repo}/actions/runs/${id}/jobs" --paginate -f per_page=100
  jq_out -r '.jobs[] | "\(.name)\t\(.conclusion)"'
  jobs="${JQ}"
  good=0
  for job in "${watched_jobs[@]}"; do
    status="$(status_of "${job}" "${jobs}")"
    if [ "${status}" != success ]; then
      say "nightly run ${id}: watched job '${job}' is not green (saw: ${status})"
      good=1
    fi
  done
  rows="$(awk -F'\t' 'index($1, "multi-arch build (") == 1' <<<"${jobs}")"
  if [ -z "${rows}" ]; then
    say "nightly run ${id}: no 'multi-arch build (' job was reported"
    good=1
  else
    while IFS=$'\t' read -r name status; do
      if [ "${status}" != success ]; then
        say "nightly run ${id}: '${name}' is not green (saw: ${status})"
        good=1
      fi
    done <<<"${rows}"
  fi
  if [ "${good}" = 0 ]; then nightly_run="${id}"; nightly_sha="${head}"; break; fi
done <<<"${nightlies}"
[ -n "${nightly_run}" ] || say "no dispatched nightly.yml run is green on tree ${tree}: dispatch it on the candidate branch, wait for it, then retry"

if [ -z "${ci_run}" ] || [ -z "${nightly_run}" ]; then
  exit 1
fi
printf 'tree=%s\nci_run=%s\nnightly_run=%s\nnightly_sha=%s\n' "${tree}" "${ci_run}" "${nightly_run}" "${nightly_sha}"
