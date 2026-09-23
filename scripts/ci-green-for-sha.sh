#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# ci-green-for-sha.sh <sha> — did CI pass on this commit?
#
# publish-image.yml asks this before it pushes wardynd:latest, the tag desktop
# installs pull. It counts only `push` runs of ci.yml in this repository (a
# pull request's run tested a merge commit, and a fork's run is not ours), and
# every such run for <sha> must have finished with success. No run, a run still
# going, a red, cancelled or skipped run: all mean "not green".
#
# Exit 0 = green. Exit 1 = not green (the caller skips the publish).
# Exit 2 = could not tell (API or usage error); the caller must not publish.
#
# Needs gh (authenticated via GH_TOKEN), jq and GITHUB_REPOSITORY=owner/name.
set -uo pipefail

sha="${1:-}"
if [[ ! "${sha}" =~ ^[0-9a-f]{40}$ ]] || [ -z "${GITHUB_REPOSITORY:-}" ]; then
  echo "usage: GITHUB_REPOSITORY=owner/name $0 <40-hex sha>" >&2
  exit 2
fi

if ! runs="$(gh api -X GET "repos/${GITHUB_REPOSITORY}/actions/workflows/ci.yml/runs" \
  -f head_sha="${sha}" -f event=push -f per_page=100)"; then
  echo "ci-green-for-sha: could not list CI runs for ${sha}" >&2
  exit 2
fi

# The API filters are re-checked here: an ignored query parameter must not
# turn someone else's run into ours.
if ! verdict="$(jq -r --arg sha "${sha}" --arg repo "${GITHUB_REPOSITORY}" '
  [.workflow_runs[]
   | select(.head_sha == $sha and .event == "push"
            and .head_repository.full_name == $repo)]
  | if length == 0 then "none"
    elif all(.status == "completed" and .conclusion == "success") then "green"
    else "not-green: " + (map("run \(.id) \(.status)/\(.conclusion)") | join(", "))
    end' <<<"${runs}")"; then
  echo "ci-green-for-sha: could not read the CI run list for ${sha}" >&2
  exit 2
fi

case "${verdict}" in
  green) echo "CI is green on ${sha}"; exit 0 ;;
  none) echo "no CI push run found for ${sha}"; exit 1 ;;
  *) echo "CI is ${verdict} on ${sha}"; exit 1 ;;
esac
