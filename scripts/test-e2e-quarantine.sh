#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Daemon-free test for scripts/lib/e2e-quarantine.sh (#1461 R3): the flaky-test
# quarantine that run-ui-e2e.sh consults. Canned Playwright JSON drives
# quarantine_classify; canned quarantine files drive quarantine_validate; and
# the checked-in ui/e2e/quarantine.txt is validated against today's date, which
# is what turns every PR red the day an entry expires.
#
# Usage: scripts/test-e2e-quarantine.sh   (exit 0 = PASS, non-zero = FAIL)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "${REPO_ROOT}/scripts/lib/e2e-quarantine.sh"

fail() { echo "test-e2e-quarantine: FAIL: $*" >&2; exit 1; }
ok() { echo "  [pass] $*"; }

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
today=2026-09-30
tab=$'\t'
sep=' › '

# One file suite (its own title is the file name) with a top-level test, and a
# describe nested in a describe holding one flaky, one expected and one
# unexpected test.
cat > "${tmp}/results.json" <<'JSON'
{
  "suites": [{
    "title": "runs.spec.ts",
    "file": "runs.spec.ts",
    "specs": [
      {"title": "top-level ok", "tests": [{"projectName": "chromium", "status": "expected"}]}
    ],
    "suites": [{
      "title": "Runs page",
      "specs": [],
      "suites": [{
        "title": "filters",
        "specs": [
          {"title": "flaky one", "tests": [{"projectName": "chromium", "status": "flaky"}]},
          {"title": "steady one", "tests": [{"projectName": "chromium", "status": "expected"}]},
          {"title": "broken one", "tests": [{"projectName": "chromium", "status": "unexpected"}]}
        ]
      }]
    }]
  }]
}
JSON
flaky_title="Runs page${sep}filters${sep}flaky one"

# --- classify ---------------------------------------------------------------
printf '# empty\n' > "${tmp}/none.txt"
got="$(quarantine_classify "${tmp}/results.json" runs.spec.ts "${tmp}/none.txt")"
want="runs.spec.ts${tab}${flaky_title}${tab}new"
[[ "${got}" == "${want}" ]] || fail "no entry: got ${got@Q}, want ${want@Q}"
ok "classify: a flaky test with no entry is new (expected and unexpected tests are not reported)"

printf 'runs.spec.ts | %s | #12 | @octo | 2026-10-05\n' "${flaky_title}" > "${tmp}/q.txt"
got="$(quarantine_classify "${tmp}/results.json" runs.spec.ts "${tmp}/q.txt")"
want="runs.spec.ts${tab}${flaky_title}${tab}quarantined"
[[ "${got}" == "${want}" ]] || fail "with entry: got ${got@Q}, want ${want@Q}"
ok "classify: a flaky test with an entry is quarantined"

got="$(quarantine_classify "${tmp}/results.json" other.spec.ts "${tmp}/q.txt")"
[[ "${got}" == "other.spec.ts${tab}${flaky_title}${tab}new" ]] || fail "entry for another spec must not match: got ${got@Q}"
ok "classify: an entry for a different spec file does not quarantine"

# --- validate ---------------------------------------------------------------
good="${tmp}/good.txt"
printf '# comment\n\nruns.spec.ts | A › b | #12 | @octo | 2026-10-05\nruns.spec.ts | C | #13 | @o-ctO_9 | 2026-10-14\n' > "${good}"
out="$(quarantine_validate "${good}" "${today}")" || fail "good file rejected: ${out}"
ok "validate: a good file passes"

expect_bad() { # <label> <line> <message fragment>
  local label="$1" line="$2" frag="$3" out rc
  printf '%s\n' "${line}" > "${tmp}/bad.txt"
  out="$(quarantine_validate "${tmp}/bad.txt" "${today}")"; rc=$?
  [[ ${rc} -eq 1 ]] || fail "${label}: want exit 1, got ${rc}"
  [[ "${out}" == FAIL:* ]] || fail "${label}: output must start with FAIL:, got ${out@Q}"
  [[ "${out}" == *"${frag}"* ]] || fail "${label}: want ${frag@Q} in ${out@Q}"
  ok "validate: ${label}"
}
expect_bad "expired entry" 'runs.spec.ts | t | #7 | @octo | 2026-09-29' \
  'expired on 2026-09-29 (#7, @octo): fix the test and delete the line, or renew it in a reviewed PR'
expect_bad "more than 14 days out" 'runs.spec.ts | t | #7 | @octo | 2026-10-15' 'at most 14 days'
expect_bad "bad issue" 'runs.spec.ts | t | 7 | @octo | 2026-10-05' 'issue'
expect_bad "bad owner" 'runs.spec.ts | t | #7 | octo | 2026-10-05' 'owner'
expect_bad "four fields" 'runs.spec.ts | t | #7 | 2026-10-05' '5 fields'
expect_bad "impossible date" 'runs.spec.ts | t | #7 | @octo | 2026-02-31' 'date'

out="$(quarantine_validate "${tmp}/does-not-exist.txt" "${today}")" && fail "a missing file must fail"
[[ "${out}" == FAIL:* ]] || fail "missing file: want FAIL:, got ${out@Q}"
ok "validate: a missing file fails"

out="$(quarantine_validate "${good}" "${today}" ; printf x)"
[[ "${out}" == x ]] || fail "a passing validate must print nothing, got ${out@Q}"
ok "validate: a passing run is silent"

# The real file, against the real clock.
out="$(quarantine_validate "${REPO_ROOT}/ui/e2e/quarantine.txt" "$(date -u +%F)")" \
  || fail "ui/e2e/quarantine.txt: ${out}"
ok "validate: checked-in ui/e2e/quarantine.txt passes against today"

echo "test-e2e-quarantine: PASS"
