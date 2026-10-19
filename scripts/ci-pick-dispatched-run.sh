#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# ci-pick-dispatched-run.sh <head-sha> <since> — ci.yml's train-suites job
# dispatches the nightly on a train head, then reads
# `gh run list --json databaseId,headSha,createdAt` on stdin and needs the run
# it started. It prints the databaseId of the newest run on <head-sha> created
# at or after <since> (UTC, 2006-01-02T15:04:05Z), or nothing. A run on another
# sha, or one older than the dispatch, is never picked.
set -euo pipefail
jq -r --arg sha "${1:?head sha}" --arg since "${2:?since}" '
  [.[] | select(.headSha == $sha and .createdAt >= $since)]
  | sort_by(.createdAt) | last | .databaseId // empty'
