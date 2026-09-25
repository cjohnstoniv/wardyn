#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-fixture-dates.sh — a literal future date in a fixture is a future date
# only until it isn't (#210). FAILS when a test/spec file holds more literal
# dates than its grandfathered count below; a file not listed may hold none.
# A date the test compares against the clock belongs in a clock-relative
# value instead: aheadByHours (ui/src/app/lib/test-clock.ts) in TS,
# time.Now().Add(d) or an injected clock in Go.
#
# What counts as a literal date:
#   - a date-time, YYYY-MM-DDTHH:MM with or without seconds, any year;
#   - a bare date opening a string literal ("2026-01-02", '…' or `…`) whose
#     year is this year or later. An earlier year is always past, so it cannot
#     expire. The cut-off moves with the calendar, so a file's count only ever
#     falls: the gate never goes red on its own on 1 January.
#   - time.Date(YYYY, ... in Go, or new Date(YYYY, ... / Date.UTC(YYYY, ... in
#     TS, whose year is this year or later. This is the usual way to write a
#     fixture time — a YYYY-MM-DD string never appears on that line, so the
#     rule above misses it.
# Never counted: Go's reference layout 2006-01-02T15:04 (a format string, not a
# point in time) and Go's zero time 0001-01-01T00:00 ("unset").
#
# The counts below are what each file held when this gate landed; the literals
# in them are display/passthrough data, never compared against now. Adding a
# literal of that kind to a listed file means raising its count in the same
# change. Lowering a count is always fine. Run via `make lint`.
set -euo pipefail
cd "$(dirname "$0")/.."

YEAR="$(date -u +%Y)"
PATTERN="[\"'\`]?[0-9]{4}-[0-9]{2}-[0-9]{2}(T[0-9]{2}:[0-9]{2})?"
CTOR_PATTERN="(time\.Date|new Date|Date\.UTC)\([0-9]{4},"

# literal_dates FILE — prints each literal date in FILE that counts, as LINE:DATE.
literal_dates() {
  {
    grep -noE "${PATTERN}" "$1" || true
    grep -noE "${CTOR_PATTERN}" "$1" || true
  } | awk -v year="${YEAR}" '{
    i = index($0, ":"); line = substr($0, 1, i - 1); tok = substr($0, i + 1)
    if (tok ~ /^(time\.Date|new Date|Date\.UTC)\(/) {
      y = tok; sub(/^[^(]*\(/, "", y); sub(/,.*/, "", y)
      if (y + 0 >= year) print line ":" tok
      next
    }
    quoted = tok ~ /^["'"'"'`]/
    if (quoted) tok = substr(tok, 2)
    if (tok ~ /T/) {
      if (tok !~ /^(2006-01-02T15:04|0001-01-01T00:00)/) print line ":" tok
    } else if (quoted && substr(tok, 1, 4) + 0 >= year) {
      print line ":" tok
    }
  }' | sort -t: -k1,1n
}

declare -A ALLOWLIST=(
  ["cmd/wardyn-aws-sso/main_test.go"]=2
  ["cmd/wardyn/commands_test.go"]=6
  ["cmd/wardyn/policyio_test.go"]=2
  ["cmd/wardyn/siteconfig_test.go"]=4
  ["cmd/wardynd/login_stamp_test.go"]=1
  ["cmd/wardynd/rekey_test.go"]=1
  ["internal/api/access_test.go"]=1
  ["internal/api/ado_entra_test.go"]=1
  ["internal/api/auth_failed_coalesce_test.go"]=1
  ["internal/api/devices_bounds_test.go"]=1
  ["internal/api/directory_search_test.go"]=1
  ["internal/api/harnesscred_supersede_test.go"]=1
  ["internal/api/modelaccess_member_redaction_test.go"]=2
  ["internal/api/modelaccess_test.go"]=1
  ["internal/api/runs_bedrock_ssoinject_test.go"]=3
  ["internal/api/runs_bedrock_test.go"]=1
  ["internal/api/setup_checks_test.go"]=1
  ["internal/api/setup_onboarding_test.go"]=8
  ["internal/api/setup_status_scope_failclosed_test.go"]=1
  ["internal/api/setup_test.go"]=2
  ["internal/db/appclock_test.go"]=3
  ["internal/egress/egress_test.go"]=1
  ["internal/egress/proxy/llm_unavailable_detail_test.go"]=3
  ["internal/egress/proxy/tool_rules_test.go"]=1
  ["internal/runner/k8s/drives_test.go"]=2
  ["internal/types/types_test.go"]=1
  ["pkg/client/client_more_test.go"]=2
  ["test/adofake/adofake_test.go"]=3
  ["ui/e2e/drives.spec.ts"]=3
  ["ui/e2e/people-access.spec.ts"]=8
  ["ui/e2e/permissions.spec.ts"]=1
  ["ui/e2e/record-loop.spec.ts"]=4
  ["ui/e2e/recording.spec.ts"]=2
  ["ui/e2e/workspace-egress-tiers.spec.ts"]=3
  # #195-b: every ui/src file above this line was converted to test-clock-relative
  # values (aheadByHours) and dropped out of this allowlist entirely (0 literal
  # dates left). These three keep a literal because it is genuinely
  # display-only passthrough (never compared to the clock — see the
  # "// passthrough, never compared to the clock" notes at each site): an
  # older daemon's opaque `action` sentence rendered verbatim, with no
  # `deadline` field for the client to parse or grade. model-access-banner.
  # test.tsx's count of 2 also includes one match that isn't data at all: a
  # code comment (line 147) that names the OLD literal in prose, explaining
  # why the test below it now uses aheadByHours instead — matched by this
  # script's regex incidentally, same as any other date-shaped text.
  ["ui/src/app/components/screens/new-run/new-run-rail.test.tsx"]=1
  ["ui/src/app/components/wardyn/model-access-banner.test.tsx"]=2
  ["ui/src/app/lib/model-access.test.ts"]=5
)

fail=0
# Without :(glob) magic a `*` in a git pathspec crosses `/`, so these reach
# every depth. (`ui/e2e/**/*.spec.ts` needs a subdirectory and skips every
# top-level spec.)
while IFS= read -r f; do
  found="$(literal_dates "$f")"
  n=0
  [[ -n "${found}" ]] && n="$(wc -l <<<"${found}")"
  allowed="${ALLOWLIST[$f]:-0}"
  if [[ "${n}" -gt "${allowed}" ]]; then
    echo "FAIL: ${f} holds ${n} literal date(s), ${allowed} allowed. Compare against the clock through aheadByHours (ui/src/app/lib/test-clock.ts) or time.Now().Add(d) in Go; a date that is only display/passthrough data may stay literal if you raise this file's count in scripts/check-fixture-dates.sh:" >&2
    sed "s#^#  ${f}:#" <<<"${found}" >&2
    fail=1
  fi
done < <(git ls-files '*_test.go' 'ui/src/*.test.ts' 'ui/src/*.test.tsx' 'ui/e2e/*.spec.ts')

if [[ "${fail}" -eq 0 ]]; then
  echo "OK: no test/spec file holds more literal dates than scripts/check-fixture-dates.sh allows"
fi
exit "${fail}"
