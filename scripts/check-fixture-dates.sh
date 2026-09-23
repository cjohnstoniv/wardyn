#!/usr/bin/env bash
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# check-fixture-dates.sh — a literal future date in a fixture is a future date
# only until it isn't (#210): 114 of them shipped in one release, 12 already
# expired or expiring within weeks. FAILS when a NON-allowlisted test/spec
# file gains a new literal RFC3339-shaped date-time, pointing the author at
# internal/testclock.FutureRFC3339 (Go) or ui/src/app/lib/test-clock.ts's
# aheadByHours (TS) instead.
#
# Two shapes are excluded everywhere, allowlisted or not, because neither is a
# wall-clock fixture that can go stale:
#   - Go's reference-time layout constant, 2006-01-02T15:04:05 (a FORMAT
#     STRING every time.Parse/time.Format call in this repo that touches
#     RFC3339 without a named layout constant spells out literally — not a
#     point in time).
#   - The Go zero time in RFC3339, 0001-01-01T00:00:00 — "unset", not "a
#     future date"; there is no relative offset that means "never set".
#
# Companion to check-file-size.sh's ALLOWLIST pattern: pre-existing matches
# are grandfathered by file (most are static display/passthrough data a
# reader's clock can never invalidate — see the file comments this script's
# own PR added), and the allowlist is where a fresh contributor's literal date
# will land if the fixture stays intentionally static. Migrating a listed file
# off the allowlist is always fine; adding to it for a fixture that computes
# against the wall clock is not — use the helper instead. Run via `make lint`.
set -euo pipefail
cd "$(dirname "$0")/.."

# RFC3339-shaped date-time, year 19xx/20xx (matches what the fixtures in this
# repo actually use; not a full RFC3339 validator).
PATTERN='[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'
LAYOUT_CONST='2006-01-02T15:04:05'
ZERO_TIME='0001-01-01T00:00:00'

# Pre-existing literal dates, grandfathered file-by-file (#210). Every one of
# these was audited when this check was introduced: each remaining literal is
# either static passthrough/display data (formatted or echoed verbatim, never
# compared against `now`) or an intentionally-always-past "already expired"
# sentinel — see the file's own comments. A NEW file is never added here for a
# fixture that means "N hours/days from now"; that one uses the helper.
declare -A ALLOWLIST=(
  ["cmd/wardyn-aws-sso/main_test.go"]=1
  ["cmd/wardyn/commands_test.go"]=1
  ["cmd/wardyn/policyio_test.go"]=1
  ["cmd/wardyn/siteconfig_test.go"]=1
  ["cmd/wardynd/rekey_test.go"]=1
  ["internal/api/access_test.go"]=1
  ["internal/api/awsssofake_e2e_test.go"]=1
  ["internal/api/devices_bounds_test.go"]=1
  ["internal/api/harnesscred_disconnect_scope_test.go"]=1
  ["internal/api/llm_lanes_modelrun_test.go"]=1
  ["internal/api/modelaccess_member_redaction_test.go"]=1
  ["internal/api/modelaccess_test.go"]=1
  ["internal/api/runs_bedrock_ssoinject_test.go"]=1
  ["internal/api/setup_checks_test.go"]=1
  ["internal/api/setup_onboarding_test.go"]=1
  ["internal/api/setup_status_scope_failclosed_test.go"]=1
  ["internal/api/setup_test.go"]=1
  ["internal/api/ssotoken_binding_test.go"]=1
  ["internal/api/ssotoken_test.go"]=1
  ["internal/broker/github_lazy_test.go"]=1
  ["internal/broker/github_revoke_test.go"]=1
  ["internal/broker/github_rotation_test.go"]=1
  ["internal/broker/ruleset_test.go"]=1
  ["internal/egress/proxy/llm_unavailable_detail_test.go"]=1
  ["test/adofake/adofake_test.go"]=1
  ["ui/e2e/drives.spec.ts"]=1
  ["ui/e2e/people-access.spec.ts"]=1
  ["ui/e2e/permissions.spec.ts"]=1
  ["ui/e2e/record-loop.spec.ts"]=1
  ["ui/e2e/recording.spec.ts"]=1
  ["ui/e2e/workspace-egress-tiers.spec.ts"]=1
  ["ui/src/app/components/attach-terminal-session.test.tsx"]=1
  ["ui/src/app/components/attach-terminal.test.tsx"]=1
  ["ui/src/app/components/screens/approvals-credential-kind.test.tsx"]=1
  ["ui/src/app/components/screens/approvals.test.tsx"]=1
  ["ui/src/app/components/screens/audit.test.tsx"]=1
  ["ui/src/app/components/screens/drives/allocations.test.tsx"]=1
  ["ui/src/app/components/screens/drives/drives-screen.test.tsx"]=1
  ["ui/src/app/components/screens/governance/governance-screen.test.tsx"]=1
  ["ui/src/app/components/screens/governance/profile-editor.test.tsx"]=1
  ["ui/src/app/components/screens/new-run/new-run-rail.test.tsx"]=1
  ["ui/src/app/components/screens/permissions.test.tsx"]=1
  ["ui/src/app/components/screens/policies.test.tsx"]=1
  ["ui/src/app/components/screens/recording.test.tsx"]=1
  ["ui/src/app/components/screens/run-detail-ssh.test.tsx"]=1
  ["ui/src/app/components/screens/run-detail.test.tsx"]=1
  ["ui/src/app/components/screens/run-detail/failure-block.test.tsx"]=1
  ["ui/src/app/components/screens/run-detail/focus-mode.test.tsx"]=1
  ["ui/src/app/components/screens/run-detail/widgets/effective-policy.test.tsx"]=1
  ["ui/src/app/components/screens/run-detail/widgets/widgets.test.tsx"]=1
  ["ui/src/app/components/screens/runs/run-card.test.tsx"]=1
  ["ui/src/app/components/screens/setup/access-panel.test.tsx"]=1
  ["ui/src/app/components/screens/setup/step-bodies.test.tsx"]=1
  ["ui/src/app/components/screens/setup/steps.test.ts"]=1
  ["ui/src/app/components/screens/ssh-keys.test.tsx"]=1
  ["ui/src/app/components/screens/workspace-detail/session-helpers.test.ts"]=1
  ["ui/src/app/components/wardyn/audit-decision.test.tsx"]=1
  ["ui/src/app/components/wardyn/model-access-banner.test.tsx"]=1
  ["ui/src/app/lib/api/approvals.wire.test.ts"]=1
  ["ui/src/app/lib/api/audit.test.ts"]=1
  ["ui/src/app/lib/api/health.test.ts"]=1
  ["ui/src/app/lib/api/integrations.test.ts"]=1
  ["ui/src/app/lib/api/runs.test.ts"]=1
  ["ui/src/app/lib/api/ssh-keys.test.ts"]=1
  ["ui/src/app/lib/api/workspaces.test.ts"]=1
  ["ui/src/app/lib/capabilities.test.ts"]=1
  ["ui/src/app/lib/model-access.test.ts"]=1
  ["ui/src/app/lib/types/approvals.test.ts"]=1
)

fail=0
# Tracked test/spec files only (git ls-files, mirroring check-file-size.sh —
# gitignored trees never false-red, untracked-but-added ones are still seen).
while IFS= read -r f; do
  [[ -n "${ALLOWLIST[$f]:-}" ]] && continue
  matches="$(grep -nE "${PATTERN}" "$f" 2>/dev/null | grep -vF "${LAYOUT_CONST}" | grep -vF "${ZERO_TIME}" || true)"
  if [[ -n "${matches}" ]]; then
    echo "FAIL: ${f} has a literal date-time fixture — use internal/testclock.FutureRFC3339 (Go) or aheadByHours (ui/src/app/lib/test-clock.ts) instead:" >&2
    echo "${matches}" | sed "s#^#  ${f}:#" >&2
    fail=1
  fi
done < <(git ls-files '*_test.go' 'ui/src/**/*.test.ts' 'ui/src/**/*.test.tsx' 'ui/e2e/**/*.spec.ts')

if [[ "${fail}" -eq 0 ]]; then
  echo "OK: no new literal date-time fixtures outside the #210 allowlist"
fi
exit "${fail}"
