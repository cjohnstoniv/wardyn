// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// allRunStates is the authoritative set of run states the control plane can
// persist. It is derived from the types constants so adding a new RunState
// without widening the DB CHECK constraint fails this test.
var allRunStates = []types.RunState{
	types.RunPending,
	types.RunStarting,
	types.RunRunning,
	types.RunWaiting,
	types.RunCompleted,
	types.RunStopped,
	types.RunArchived,
	types.RunFailed,
	types.RunKilled,
}

// stateInCheckRe captures the parenthesized value list of a `state IN ( ... )`
// CHECK clause in a migration (used for both the initial inline CHECK and any
// later ALTER ... ADD CONSTRAINT ... CHECK (state IN (...))).
var stateInCheckRe = regexp.MustCompile(`(?is)state\s+IN\s*\(([^)]*)\)`)

// quotedRe extracts single-quoted tokens like 'COMPLETED'.
var quotedRe = regexp.MustCompile(`'([^']+)'`)

// targetsAgentRunsRe matches a statement whose TARGET table is agent_runs
// (CREATE TABLE agent_runs / ALTER TABLE agent_runs). This must NOT match
// statements that merely FK-reference agent_runs (e.g. approval_requests's
// `REFERENCES agent_runs(id)`), which also carry their own state CHECK.
var targetsAgentRunsRe = regexp.MustCompile(
	`(?is)(?:CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?|ALTER\s+TABLE\s+(?:ONLY\s+)?)agent_runs\b`)

// effectiveAgentRunStates returns the set of agent_runs.state values allowed
// after ALL migrations are applied in lexical order. The LAST migration that
// (re)defines a `state IN (...)` CHECK wins, mirroring how an ALTER replaces
// the prior constraint at runtime.
func effectiveAgentRunStates(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var allowed map[string]bool
	for _, name := range names {
		data, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		// Process statement-by-statement (split on ';') so a `state IN (...)`
		// CHECK is attributed to its OWNING table. The approval_requests table
		// also has a state CHECK; only statements that mention agent_runs count.
		for _, stmt := range strings.Split(string(data), ";") {
			if !targetsAgentRunsRe.MatchString(stmt) {
				continue
			}
			loc := stateInCheckRe.FindStringSubmatch(stmt)
			if loc == nil {
				continue
			}
			vals := map[string]bool{}
			for _, q := range quotedRe.FindAllStringSubmatch(loc[1], -1) {
				vals[q[1]] = true
			}
			if len(vals) > 0 {
				allowed = vals // last writer (lexically-latest migration) wins
			}
		}
	}
	return allowed
}

// TestAgentRunStateCheckCoversAllStates is the always-on regression guard for
// the COMPLETED-state cluster: the completion watcher transitions runs to
// COMPLETED, but the original CHECK omitted it, so the UPDATE was rejected by
// Postgres and runs never reached terminal (credentials never revoked). This
// test fails if any types.RunState is not permitted by the effective CHECK.
func TestAgentRunStateCheckCoversAllStates(t *testing.T) {
	allowed := effectiveAgentRunStates(t)
	if allowed == nil {
		t.Fatal("no agent_runs state CHECK found in migrations")
	}
	for _, s := range allRunStates {
		if !allowed[string(s)] {
			t.Errorf("run state %q is not allowed by the agent_runs.state CHECK constraint; "+
				"add it to a migration (a run in this state cannot be persisted)", s)
		}
	}
	// Guard the other direction too: the CHECK must not allow states the code
	// does not define (catches typos in migrations).
	known := map[string]bool{}
	for _, s := range allRunStates {
		known[string(s)] = true
	}
	for s := range allowed {
		if !known[s] {
			t.Errorf("agent_runs.state CHECK allows %q which is not a defined types.RunState", s)
		}
	}
}

// checkInRe returns a regexp matching a `<column> IN ( ... )` CHECK clause —
// the general form of stateInCheckRe, parameterized by column so the parity
// guard below can reuse the same parsing for every closed enum, not just
// agent_runs.state.
func checkInRe(column string) *regexp.Regexp {
	return regexp.MustCompile(`(?is)` + regexp.QuoteMeta(column) + `\s+IN\s*\(([^)]*)\)`)
}

// targetsTableRe returns a regexp matching a statement whose TARGET table is
// `table` (CREATE TABLE table / ALTER TABLE table) — the general form of
// targetsAgentRunsRe, parameterized by table name. Must NOT match a statement
// that merely FK-references table.
func targetsTableRe(table string) *regexp.Regexp {
	return regexp.MustCompile(
		`(?is)(?:CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?|ALTER\s+TABLE\s+(?:ONLY\s+)?)` + regexp.QuoteMeta(table) + `\b`)
}

// effectiveCheckValues returns the set of values a `table.column IN (...)`
// CHECK allows after ALL migrations are applied in lexical order — the LAST
// migration that (re)defines it wins, same rule as effectiveAgentRunStates,
// generalized past agent_runs.state to the closed enums below. Reuses
// readMigrationNames (db_test.go) and quotedRe.
func effectiveCheckValues(t *testing.T, table, column string) map[string]bool {
	t.Helper()
	targetsRe := targetsTableRe(table)
	inRe := checkInRe(column)
	var allowed map[string]bool
	for _, name := range readMigrationNames(t) {
		data, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		// Process statement-by-statement so the CHECK is attributed to its
		// OWNING table, not merely a table that mentions the column name.
		for _, stmt := range strings.Split(string(data), ";") {
			if !targetsRe.MatchString(stmt) {
				continue
			}
			loc := inRe.FindStringSubmatch(stmt)
			if loc == nil {
				continue
			}
			vals := map[string]bool{}
			for _, q := range quotedRe.FindAllStringSubmatch(loc[1], -1) {
				vals[q[1]] = true
			}
			if len(vals) > 0 {
				allowed = vals // last writer (lexically-latest migration) wins
			}
		}
	}
	return allowed
}

// stringSet builds a lookup set from literal values — closedEnumCheck.known
// for the enums below.
func stringSet(vals ...string) map[string]bool {
	out := make(map[string]bool, len(vals))
	for _, v := range vals {
		out[v] = true
	}
	return out
}

// workspaceStatusValues is shared by workspaces.status and sources.status:
// 0031 gave sources the identical status vocabulary types.WorkspaceStatus
// already defines for workspaces (pending_scan/scanning/scanned/error).
func workspaceStatusValues() map[string]bool {
	return stringSet(
		string(types.WorkspacePendingScan), string(types.WorkspaceScanning),
		string(types.WorkspaceScanned), string(types.WorkspaceError),
	)
}

// enumSet is 0054's three closed sets, derived from the exported slices in
// internal/types rather than re-listed here: a fifth backend (or template, or
// reclaim intent) added to the Go side without widening the migration's CHECK
// fails this test, which is the whole point of the pin.
func enumSet[T ~string](vals []T) map[string]bool {
	out := make(map[string]bool, len(vals))
	for _, v := range vals {
		out[string(v)] = true
	}
	return out
}

// closedEnumCheck is one Go-const-vs-DB-CHECK parity case beyond
// agent_runs.state: a table.column whose CHECK the DB enforces against a
// small closed set the Go side also defines. Adding a value on one side
// without the other reproduces the COMPLETED-state incident
// TestAgentRunStateCheckCoversAllStates exists to catch.
type closedEnumCheck struct {
	table, column string
	known         map[string]bool
}

// TestClosedEnumChecksMatchConstants extends the always-on parity guard past
// agent_runs.state to the closed enums 0029/0031 shipped: workspaces.status,
// sources.status, sources.kind and base_images.kind had no equivalent guard,
// so any of them could silently drift the way agent_runs.state once did.
func TestClosedEnumChecksMatchConstants(t *testing.T) {
	cases := []closedEnumCheck{
		{"workspaces", "status", workspaceStatusValues()},
		{"sources", "status", workspaceStatusValues()},
		{"sources", "kind", stringSet(string(types.SourceLocalDir), string(types.SourceRepo))},
		{"approvals", "decision_scope", stringSet(
			string(types.ScopeOnce), string(types.ScopeRun), string(types.ScopeUntil), string(types.ScopeAlways),
		)},
		// 0042's two closed enums. capability_grants.capability is deliberately
		// NOT here: it carries no CHECK at all (the kind set is one Go slice in
		// internal/api, validated at the write boundary) — see the migration.
		{"capability_grants", "subject_type", stringSet(
			string(types.CapabilitySubjectUser), string(types.CapabilitySubjectGroup),
			string(types.CapabilitySubjectAll),
		)},
		{"capability_grants", "effect", stringSet(
			string(types.CapabilityAllow), string(types.CapabilityDeny),
		)},
		// base_images.kind has no typed Go enum (types.BaseImageEntry.Kind is a
		// plain string — internal/types/workspace_contract.go) and is validated
		// ad hoc in internal/api/base_images.go, so this literal list IS the
		// closed set, not a derived one.
		{"base_images", "kind", stringSet("registry", "custom", "byo")},
		// 0051's role — a small, complete set with real Go constants (unlike
		// capability_grants.capability above), so it gets a CHECK and this pin.
		// Pinned to the CONSTANTS oidc.ValidRole itself accepts, never a
		// literal list: 0.7 added the third tier (RoleSecurityAdmin) and
		// widened ValidRole for it, which left POST /access/mappings
		// accepting a role 0051's CHECK still refused — a write that passed
		// API validation and then 500'd at the database. 0053 widens the
		// CHECK; naming the constants here is what makes a FOURTH role
		// impossible to land on one side only — which it now is, in BOTH
		// directions, because the set is DERIVED from oidc.Roles rather than
		// typed out here a third time. Hand-enumerating it made this case blind
		// in the Go-widens-first direction: oidc.ValidRole could accept a fourth
		// role the CHECK refused and this test stayed green, which is exactly
		// the incident 0053 documents (security_admin passed the /access write
		// boundary and then 500'd at the database). Only the opposite direction
		// was caught, by the "allows X which is not a defined Go constant"
		// branch. The user_drives cases below already derived; this one did not.
		{"role_mappings", "role", stringSet(oidc.Roles...)},
		// 0060's api_tokens.role, derived from the SAME slice for the same
		// reason. 0045 shipped this column with no CHECK and said why: "the two
		// values are Go constants ... any other value is inert (fail closed),
		// never a privilege." 0.7 made the column carry the caller's verbatim
		// session role instead of a two-valued re-derivation, so security_admin
		// is written here and is NOT inert -- isSecurityOperator gates the whole
		// securityOps route group on it. The argument's content was "the
		// privileged set is a singleton", which is now a pair and grows with
		// every tier, so it cannot be restated in a form that survives a fourth
		// role. 0060 adds the CHECK; this case is what keeps it and oidc.Roles
		// moving together, and is what makes ADDING the CHECK safe -- without it
		// the constraint would re-import 0053's incident onto token minting.
		{"api_tokens", "role", stringSet(oidc.Roles...)},
		// 0052's governance_assignments.subject_type — the SAME closed enum
		// capability_grants.subject_type above carries, reused rather than
		// re-enumerated (types.CapabilitySubjectType is the one Go definition
		// for "who is this row written against"). Pinned here so a fourth
		// subject type cannot land on one table's CHECK and not the other's,
		// which is precisely the drift this test exists to catch — and note
		// there is NO governance_profiles.limits pin: that column carries no
		// CHECK at all, on the same "closed Go enum validated at the write
		// boundary" doctrine 0042's capability column follows.
		{"governance_assignments", "subject_type", stringSet(
			string(types.CapabilitySubjectUser), string(types.CapabilitySubjectGroup),
			string(types.CapabilitySubjectAll),
		)},
		// 0054's three user_drives enums, each pinned to the Go set the write
		// boundary validates against (types.DriveBackend.Valid,
		// HomeTemplate.Valid, DriveReclaim.Valid) rather than to a literal
		// list. Every one of the three decides something a drifted CHECK would
		// turn into a 500 on an admin surface the console offers — the exact
		// half-open window 0053 closed for role_mappings.role.
		//
		// backend is the sharpest of them: it decides which RUNNER can mount a
		// drive and whether Wardyn ALLOCATES the object or refuses because one
		// is missing, so a fifth backend landing in Go without the CHECK is a
		// write that passes validation and is refused by Postgres immediately
		// after.
		{"user_drives", "backend", enumSet(types.DriveBackends)},
		{"user_drives", "home_template", enumSet(types.HomeTemplates)},
		{"user_drives", "reclaim", enumSet(types.DriveReclaims)},
		// user_drive_grants.subject_type is the SAME closed enum the two tables
		// above carry, reused rather than re-enumerated — one Go definition for
		// "who is this row written against". Pinned here so a fourth subject
		// type cannot land on two tables' CHECKs and not the third's.
		{"user_drive_grants", "subject_type", stringSet(
			string(types.CapabilitySubjectUser), string(types.CapabilitySubjectGroup),
			string(types.CapabilitySubjectAll),
		)},
	}
	for _, c := range cases {
		t.Run(c.table+"."+c.column, func(t *testing.T) {
			allowed := effectiveCheckValues(t, c.table, c.column)
			if allowed == nil {
				t.Fatalf("no %s.%s CHECK found in migrations", c.table, c.column)
			}
			for v := range c.known {
				if !allowed[v] {
					t.Errorf("%s.%s CHECK does not allow %q, which the Go side defines", c.table, c.column, v)
				}
			}
			for v := range allowed {
				if !c.known[v] {
					t.Errorf("%s.%s CHECK allows %q, which is not a defined Go constant", c.table, c.column, v)
				}
			}
		})
	}
}
