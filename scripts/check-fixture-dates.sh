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
  ["internal/api/uigateway_record_test.go"]=1 # injected clock only: srv.cfg.Now starts here and steps a second per call, never compared to the wall clock
  ["internal/recording/rfb_test.go"]=3 # injected clock only: the recorder's clock starts here and the test advances it, never compared to the wall clock
  ["ui/e2e/recording-level.spec.ts"]=4 # passthrough only: stubbed audit rows and a recording's started_at, rendered as given, never compared to the clock
  ["ui/src/app/components/screens/run-detail/recording-tab.level.test.tsx"]=3 # passthrough only: audit rows' time and a gap's start/end, never compared to the clock
  ["cmd/wardyn-aws-sso/main_test.go"]=2
  ["cmd/wardyn/commands_test.go"]=6
  ["cmd/wardyn/runner_limits_test.go"]=2 # display only: runner list prints LastSeenAt as given, never compared to the clock
  ["cmd/wardyn/drive_test.go"]=1 # passthrough only: the member drive list's created_at, never compared to the clock
  ["cmd/wardyn/people_test.go"]=3 # display only: personDay formats LastSignedInAt, never compared to the clock
  ["cmd/wardyn/policyio_test.go"]=2
  ["cmd/wardyn/siteconfig_test.go"]=4
  ["cmd/wardynd/login_stamp_test.go"]=1
  ["cmd/wardynd/rekey_test.go"]=1
  # ar-l1.2's partition tests: the 2200-01-01 sentinel is a far-future bound passed through as data,
  # never compared against the clock.
  ["internal/api/audit_partition_export_test.go"]=1
  ["internal/db/audit_partition_digest_pg_test.go"]=1
  ["internal/store/auditpartition_pg_test.go"]=1
  # ar-l1.6's retention tab test: the partition bounds and the pending date are display data the
  # table renders in the packet's own wording ("3 Oct 2026", "Oct 2026"), asserted as literal
  # strings so the canon wording is pinned. Nothing compares them against the clock. +2: the
  # Not-started row's bounds; its state comes from the fixture, not the date.
  ["ui/src/app/components/screens/audit-retention.test.tsx"]=15
  ["ui/src/app/components/screens/runners/remote-runner-drawer.test.tsx"]=2 # passthrough only: the runner fixture's created_at and updated_at, never compared to the clock
  ["internal/api/access_test.go"]=1
  ["internal/api/ado_entra_test.go"]=1
  ["internal/api/auth_failed_coalesce_test.go"]=1
  ["internal/api/devices_bounds_test.go"]=1
  ["internal/api/directory_search_test.go"]=1
  ["internal/api/fixtures_awssso_test.go"]=1
  ["internal/api/harnesscred_supersede_test.go"]=1
  ["internal/api/modelaccess_member_redaction_test.go"]=2
  ["internal/api/modelaccess_test.go"]=1
  ["internal/api/runs_bedrock_ssoinject_test.go"]=3
  ["internal/api/setup_checks_test.go"]=1
  ["internal/api/setup_onboarding_test.go"]=8
  ["internal/api/setup_status_scope_failclosed_test.go"]=1
  ["internal/api/setup_test.go"]=2
  ["internal/db/appclock_test.go"]=3
  ["internal/egress/egress_test.go"]=1
  ["internal/egress/proxy/llm_unavailable_detail_test.go"]=3
  ["internal/egress/proxy/tool_rules_test.go"]=1
  ["internal/notify/render_test.go"]=1 # display only: the golden body prints RequestedAt, never compared to the clock
  ["internal/runner/k8s/drives_test.go"]=2
  ["internal/types/types_test.go"]=1
  ["pkg/client/client_more_test.go"]=2
  ["test/adofake/adofake_test.go"]=3
  ["ui/e2e/drives.spec.ts"]=3
  ["ui/e2e/people-access.spec.ts"]=10
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
  ["ui/src/app/components/wardyn/model-access-banner.test.tsx"]=2
  ["ui/src/app/lib/model-access.test.ts"]=5
  # #1425: the run policy view fixture's created_at/updated_at and a restart
  # change's `at` are display data rendered into the tab (S-21 prints the
  # restart date); nothing compares them against the clock.
  ["ui/src/app/components/screens/run-detail/policy-tab.test.tsx"]=3
  # 0.8.6 W4: a run output's captured_at is printed as a clock time ("Captured
  # 14:02") or decoded and passed through (the CLI's --json), and a key-domain
  # assignment's set_at is passed through and never rendered; nothing compares
  # either against the clock.
  ["cmd/wardyn/run_output_test.go"]=1
  ["ui/e2e/run-output.spec.ts"]=1
  ["ui/src/app/components/screens/run-detail/output-tab.test.tsx"]=1
  ["ui/src/app/components/screens/credentials-key-domains.test.tsx"]=2
  # #1649: the governance profile fixture's created_at/updated_at are passthrough
  # data on the Runner credentials tab; nothing renders or compares them.
  ["ui/src/app/components/screens/governance/governance-delivery-tab.test.tsx"]=2
  # #1508 C4: migration 0167's keep-an-existing-row test seeds a fixed created_at and checks it is kept;
  # nothing compares it against the clock.
  ["internal/db/service_attach_restriction_pg_test.go"]=1
  # Q-c: a queued run's queued_at is a fixed instant the CLI prints back (and its deadline from it);
  # nothing compares it against the clock.
  ["cmd/wardyn/run_inspect_test.go"]=1
  ["cmd/wardyn/run_queue_wait_test.go"]=1
  # The approved runner design's own command-line frame: `use-policy get` prints the policy's time verbatim
  # ("revision 9, updated 2026-10-09T14:02:11Z"), and both tests hold the CLI to that line. Never compared to the clock.
  ["cmd/wardyn/runner_design_lines_test.go"]=1
  ["cmd/wardyn/runner_pool_use_policy_test.go"]=2
  # New Run Launched: the wait/elapsed words are computed from a fixed reference instant passed in as now;
  # nothing reads the real clock.
  ["ui/src/app/components/screens/new-run-flow/launch/launched-model.test.ts"]=1
  # Invitations and service identities: a fixed instant that is wire passthrough (CLI table rows, the account
  # lists) or the frozen clock the token dialog and the copy formatters are tested against; nothing reads the real clock.
  ["cmd/wardyn/service_identity_test.go"]=1
  ["ui/src/app/components/screens/settings/token-dialog.test.tsx"]=1
  ["ui/src/app/components/screens/settings/your-account-screen.test.tsx"]=2
  ["ui/src/app/components/wardyn/copy/service-tokens.test.ts"]=4
  # #541 fix review: connectionRowCopy's C5 (expiring) test pins a LITERAL
  # expected string for absoluteTime's clock-time rendering, deliberately not
  # a recomputation through absoluteTime itself (that would let the
  # implementation and the test drift in lockstep and still pass) — so the
  # fixed deadline is genuinely display/passthrough data, never compared
  # against the clock (connectionRowCopy does no expiry math of its own).
  ["ui/src/app/lib/model-connections.test.ts"]=1
  # credentials.test.tsx's aliceRow fixture (added_at/last_used_at): passthrough
  # display data only, rendered through relativeTime/absoluteTime — no
  # assertion compares the rendered string against the clock, so the literal
  # dates themselves never go stale.
  ["ui/src/app/components/screens/credentials.test.tsx"]=2
  # 0.8.6 W2a: the seed of an injected fake clock (sweep health, substrate
  # health), never compared against the wall clock; the audit-partition tests'
  # '2200-01-01' high-water mark, which only has to sit after every partition
  # bound; and a mocked workspace's created_at/updated_at, rendered, never graded.
  ["cmd/wardynd/sweep_health_test.go"]=5
  ["internal/api/substrate_health_test.go"]=2
  ["internal/sweephealth/sweephealth_test.go"]=2
  ["internal/api/audit_partition_export_test.go"]=1
  ["internal/db/audit_partition_digest_pg_test.go"]=1
  ["internal/store/auditpartition_pg_test.go"]=1
  ["ui/e2e/governed-admin.spec.ts"]=2
  # 0.8.6 W3: the directory fake stamps CreatedAt and a listing row's
  # LastSignInAt with a fixed instant that is only echoed into the JSON body;
  # the hourly test's t0 seeds an injected fake clock, never the wall clock.
  ["internal/api/people_keydomains_fake_test.go"]=2
  ["internal/notify/worker_unit_test.go"]=1
  ["internal/store/workspaces_order_pg_test.go"]=1 # passthrough: one shared created_at so only the id tie-break orders the list
  ["ui/src/app/components/screens/new-run-flow/steps/shape/seed-loaders.test.ts"]=1 # passthrough: a launch intent's created_at, never compared to the clock
  ["ui/src/app/components/screens/templates/rename-template-dialog.test.tsx"]=1 # display only: a template's updated_at
  ["ui/src/app/components/screens/templates/templates-screen.test.tsx"]=1 # display only: a template's updated_at
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
