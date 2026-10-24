#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# k6-rerun-rate.sh [N] — how often each k6-kind-conformance leg fails (#3538). A failed leg costs a
# full rerun, about an hour, so a leg that keeps failing should show as a number. It reads the last
# N (default 30) completed scheduled runs of nightly.yml through gh, and every attempt of each (a
# rerun is a second attempt, and `gh run view` alone shows only the last), and prints a markdown
# table: per leg, the attempts that ran, the ones that failed, and the failure rate. A skipped or
# cancelled leg is not an attempt. Needs gh authenticated with actions: read.
#
# k6-rerun-rate.sh --table reads the attempts as JSON on stdin and prints only the table:
#   [{"run":1,"attempt":1,"jobs":[{"name":"k6-kind-conformance (v1.31.4, org)","conclusion":"success"}]}]
set -euo pipefail

table() {
  jq -r '
    [.[].jobs[] | select(.name | startswith("k6-kind-conformance")) | select(.conclusion == "success" or .conclusion == "failure")]
    | group_by(.name)
    | map({name: .[0].name, runs: length, failed: (map(select(.conclusion == "failure")) | length)})
    | ["| leg | attempts | failed | failure rate |", "|---|---|---|---|"]
      + (map("| \(.name) | \(.runs) | \(.failed) | \(.failed * 1000 / .runs | round / 10)% |"))
    | .[]'
}

if [ "${1:-}" = "--table" ]; then
  table
  exit
fi

n="${1:-30}"
runs="$(gh run list -w nightly.yml --event schedule --status completed --limit "$n" --json databaseId,attempt)"
jq -r '.[] | [.databaseId, .attempt] | @tsv' <<<"$runs" | while IFS=$'\t' read -r id attempts; do
  for ((a = 1; a <= attempts; a++)); do
    gh run view "$id" --attempt "$a" --json jobs |
      jq -c --argjson run "$id" --argjson attempt "$a" '{run: $run, attempt: $attempt, jobs: [.jobs[] | {name, conclusion}]}'
  done
done | jq -s . | table
