#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# ci-pick-dispatched-run.sh <head-sha> <since> <dispatch-id> — ci.yml's train-suites job
# dispatches the nightly on a train head, then reads
# `gh run list --json databaseId,headSha,createdAt,displayTitle` on stdin and needs the run
# it started. It prints the databaseId of the newest run on <head-sha> created
# at or after <since> (UTC, 2006-01-02T15:04:05Z), or nothing. A run on another
# sha, or one older than the dispatch, is never picked. Only a run whose title ends in ` [<dispatch-id>]` counts.
# nightly.yml's run-name puts the `dispatch_id` input there, so a hand dispatch or a scheduled run on the same commit
# is never picked.
set -euo pipefail
jq -r --arg sha "${1:?head sha}" --arg since "${2:?since}" --arg id "${3:?dispatch id}" '
  [.[] | select(.headSha == $sha and .createdAt >= $since and ((.displayTitle // "") | endswith(" [" + $id + "]")))]
  | sort_by(.createdAt) | last | .databaseId // empty'
