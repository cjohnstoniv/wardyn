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

# literal_dates FILE — prints each literal date in FILE that counts, as LINE:DATE.
literal_dates() {
  { grep -noE "${PATTERN}" "$1" || true; } | awk -v year="${YEAR}" '{
    i = index($0, ":"); line = substr($0, 1, i - 1); tok = substr($0, i + 1)
    quoted = tok ~ /^["'"'"'`]/
    if (quoted) tok = substr(tok, 2)
    if (tok ~ /T/) {
      if (tok !~ /^(2006-01-02T15:04|0001-01-01T00:00)/) print line ":" tok
    } else if (quoted && substr(tok, 1, 4) + 0 >= year) {
      print line ":" tok
    }
  }'
}

declare -A ALLOWLIST=(
  ["cmd/wardyn-aws-sso/main_test.go"]=6
  ["cmd/wardyn/commands_test.go"]=4
  ["cmd/wardyn/policyio_test.go"]=2
  ["cmd/wardyn/siteconfig_test.go"]=3
  ["cmd/wardynd/rekey_test.go"]=1
  ["internal/api/devices_bounds_test.go"]=1
  ["internal/api/harnesscred_disconnect_scope_test.go"]=1
  ["internal/api/modelaccess_member_redaction_test.go"]=2
  ["internal/api/modelaccess_test.go"]=1
  ["internal/api/runs_bedrock_ssoinject_test.go"]=2
  ["internal/api/setup_checks_test.go"]=1
  ["internal/api/setup_onboarding_test.go"]=3
  ["internal/api/setup_status_scope_failclosed_test.go"]=1
  ["internal/api/setup_test.go"]=2
  ["internal/api/ssotoken_binding_test.go"]=6
  ["internal/api/ssotoken_test.go"]=7
  ["internal/broker/github_lazy_test.go"]=1
  ["internal/broker/github_revoke_test.go"]=1
  ["internal/broker/github_rotation_test.go"]=4
  ["internal/broker/ruleset_test.go"]=2
  ["internal/egress/proxy/llm_unavailable_detail_test.go"]=3
  ["test/adofake/adofake_test.go"]=3
  ["ui/e2e/drives.spec.ts"]=3
  ["ui/e2e/people-access.spec.ts"]=8
  ["ui/e2e/permissions.spec.ts"]=1
  ["ui/e2e/record-loop.spec.ts"]=4
  ["ui/e2e/recording.spec.ts"]=2
  ["ui/e2e/workspace-egress-tiers.spec.ts"]=3
  ["ui/src/app/components/attach-terminal-session.test.tsx"]=1
  ["ui/src/app/components/attach-terminal.test.tsx"]=1
  ["ui/src/app/components/screens/approvals-credential-kind.test.tsx"]=1
  ["ui/src/app/components/screens/approvals.test.tsx"]=1
  ["ui/src/app/components/screens/audit.test.tsx"]=3
  ["ui/src/app/components/screens/drives/allocations.test.tsx"]=5
  ["ui/src/app/components/screens/drives/drives-screen.test.tsx"]=3
  ["ui/src/app/components/screens/governance/governance-screen.test.tsx"]=4
  ["ui/src/app/components/screens/governance/profile-editor.test.tsx"]=2
  ["ui/src/app/components/screens/new-run/new-run-rail.test.tsx"]=1
  ["ui/src/app/components/screens/permissions.test.tsx"]=1
  ["ui/src/app/components/screens/policies.test.tsx"]=2
  ["ui/src/app/components/screens/recording.test.tsx"]=2
  ["ui/src/app/components/screens/run-detail-ssh.test.tsx"]=3
  ["ui/src/app/components/screens/run-detail.test.tsx"]=1
  ["ui/src/app/components/screens/run-detail/failure-block.test.tsx"]=3
  ["ui/src/app/components/screens/run-detail/focus-mode.test.tsx"]=6
  ["ui/src/app/components/screens/run-detail/widgets/effective-policy.test.tsx"]=1
  ["ui/src/app/components/screens/run-detail/widgets/widgets.test.tsx"]=2
  ["ui/src/app/components/screens/runs/run-card.test.tsx"]=1
  ["ui/src/app/components/screens/setup/access-panel.test.tsx"]=8
  ["ui/src/app/components/screens/setup/step-bodies.test.tsx"]=2
  ["ui/src/app/components/screens/setup/steps.test.ts"]=2
  ["ui/src/app/components/screens/ssh-keys.test.tsx"]=5
  ["ui/src/app/components/screens/workspace-detail/session-helpers.test.ts"]=12
  ["ui/src/app/components/wardyn/audit-decision.test.tsx"]=1
  ["ui/src/app/components/wardyn/live-approvals.test.tsx"]=2
  ["ui/src/app/components/wardyn/model-access-banner.test.tsx"]=2
  ["ui/src/app/lib/api/approvals.wire.test.ts"]=1
  ["ui/src/app/lib/api/audit.test.ts"]=1
  ["ui/src/app/lib/api/health.test.ts"]=1
  ["ui/src/app/lib/api/integrations.test.ts"]=2
  ["ui/src/app/lib/api/runs.test.ts"]=2
  ["ui/src/app/lib/api/ssh-keys.test.ts"]=2
  ["ui/src/app/lib/api/workspaces.test.ts"]=2
  ["ui/src/app/lib/capabilities.test.ts"]=1
  ["ui/src/app/lib/model-access.test.ts"]=5
  ["ui/src/app/lib/types/approvals.test.ts"]=2
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
