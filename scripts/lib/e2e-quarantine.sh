#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Flaky-test quarantine for scripts/run-ui-e2e.sh (#1461 R3). Sourced, not run.
# The list lives in ui/e2e/quarantine.txt; its format and rule are stated there.
#
#   quarantine_validate <file> <today YYYY-MM-DD>
#       Prints "FAIL: <file>:<line>: <reason>" per bad entry and returns 1 if
#       any entry is malformed, expired, or expires more than 14 days out.
#       A missing file is a failure too: a list that cannot be read must not
#       pass as an empty one.
#   quarantine_classify <results.json> <spec basename> <file>
#       For each test Playwright's JSON report marks `flaky`, prints
#       "spec<TAB>title<TAB>quarantined|new". The title is the describe titles
#       and the test title joined with " › ", as the list reporter prints them.

quarantine_validate() {
  local file="$1" today="$2" max n=0 rc=0 line spec title issue owner expiry
  if [[ ! -r "${file}" ]]; then
    echo "FAIL: ${file}: cannot read the quarantine file"
    return 1
  fi
  max="$(date -u -d "${today} + 14 days" +%F)" || return 1
  while IFS= read -r line || [[ -n "${line}" ]]; do
    n=$((n + 1))
    line="${line%$'\r'}"
    [[ "${line}" =~ ^[[:space:]]*(#|$) ]] && continue
    local -a f
    mapfile -t f < <(awk -F' [|] ' '{ for (i = 1; i <= NF; i++) { gsub(/^[ \t]+|[ \t]+$/, "", $i); print $i } }' <<<"${line}")
    if [[ ${#f[@]} -ne 5 ]]; then
      echo "FAIL: ${file}:${n}: expected 5 fields (spec | title | #issue | @owner | YYYY-MM-DD), got ${#f[@]}"
      rc=1
      continue
    fi
    spec="${f[0]}" title="${f[1]}" issue="${f[2]}" owner="${f[3]}" expiry="${f[4]}"
    if [[ -z "${spec}" || -z "${title}" ]]; then
      echo "FAIL: ${file}:${n}: spec and title must not be empty"; rc=1; continue
    fi
    if [[ ! "${issue}" =~ ^#[0-9]+$ ]]; then
      echo "FAIL: ${file}:${n}: issue must be #<digits>, got '${issue}'"; rc=1; continue
    fi
    if [[ ! "${owner}" =~ ^@[A-Za-z0-9][A-Za-z0-9_-]*$ ]]; then
      echo "FAIL: ${file}:${n}: owner must be @<login>, got '${owner}'"; rc=1; continue
    fi
    if [[ ! "${expiry}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || [[ "$(date -u -d "${expiry}" +%F 2>/dev/null)" != "${expiry}" ]]; then
      echo "FAIL: ${file}:${n}: invalid date '${expiry}', want YYYY-MM-DD"; rc=1; continue
    fi
    if [[ "${expiry}" < "${today}" ]]; then
      echo "FAIL: ${file}:${n}: expired on ${expiry} (${issue}, ${owner}): fix the test and delete the line, or renew it in a reviewed PR"
      rc=1
    elif [[ "${expiry}" > "${max}" ]]; then
      echo "FAIL: ${file}:${n}: expires ${expiry}, at most 14 days from ${today} (${max}) — a quarantine is short"
      rc=1
    fi
  done < "${file}"
  return ${rc}
}

quarantine_classify() {
  local json="$1" spec="$2" file="$3" title verdict
  # The file-level suite's own title (the spec file name) is skipped: recursion
  # starts at each top-level suite's specs with an empty title path, and only
  # nested suites (describes) contribute their titles.
  while IFS= read -r title; do
    verdict=new
    if Q_SPEC="${spec}" Q_TITLE="${title}" awk -F' [|] ' '
        $0 !~ /^[ \t]*#/ && NF == 5 {
          gsub(/^[ \t]+|[ \t]+$/, "", $1); gsub(/^[ \t]+|[ \t]+$/, "", $2)
          if (($1 "") == ENVIRON["Q_SPEC"] "" && ($2 "") == ENVIRON["Q_TITLE"] "") found = 1
        }
        END { exit !found }' "${file}" 2>/dev/null; then
      verdict=quarantined
    fi
    printf '%s\t%s\t%s\n' "${spec}" "${title}" "${verdict}"
  done < <(jq -r '
    def flaky($t):
      (.specs[]? | . as $s | select(any($s.tests[]?; .status == "flaky")) | ($t + [$s.title]) | join(" › ")),
      (.suites[]? | flaky($t + [.title]));
    .suites[]? | flaky([])' "${json}" 2>/dev/null)
}
