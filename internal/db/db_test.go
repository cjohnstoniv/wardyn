// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// SQL-level migration regression tests. These are PURE unit tests: they parse
// the embedded migration .sql via the same migrationFS the production Migrate()
// uses -- no Postgres, no network. They complement migrations_check_test.go
// (which cross-checks the effective agent_runs.state CHECK against
// types.RunState); here we assert structural invariants of the migration set
// and pin the specific 0003 (COMPLETED) and 0004 (BEFORE TRUNCATE) fixes.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// migrationName captures the 4-digit numeric prefix and the descriptive slug of
// a migration filename like "0003_run_state_completed.sql".
var migrationNameRe = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// readMigrationNames returns the sorted list of .sql migration filenames exactly
// as Migrate() discovers them (lexical sort over migrations/*.sql).
func readMigrationNames(t *testing.T) []string {
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
	return names
}

// readMigration returns the text of a single embedded migration.
func readMigration(t *testing.T, name string) string {
	t.Helper()
	data, err := migrationFS.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	return string(data)
}

// TestMigrationsDiscoveredInLexicalOrder asserts the discovery API yields the
// migrations sorted by their numeric prefix, which is the ordering Migrate()
// depends on to apply DDL deterministically (0003 must run after 0001, etc.).
func TestMigrationsDiscoveredInLexicalOrder(t *testing.T) {
	names := readMigrationNames(t)
	if len(names) == 0 {
		t.Fatal("no migrations discovered; embed glob is broken")
	}

	// Lexical sort over zero-padded 4-digit prefixes must equal numeric sort.
	prev := -1
	for _, name := range names {
		m := migrationNameRe.FindStringSubmatch(name)
		if m == nil {
			t.Fatalf("migration %q does not match NNNN_slug.sql naming", name)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("migration %q has non-numeric prefix: %v", name, err)
		}
		if n <= prev {
			t.Errorf("migration %q (prefix %04d) is not strictly after the previous prefix %04d; "+
				"lexical discovery order would mis-apply DDL", name, n, prev)
		}
		prev = n
	}
}

// TestMigrationPrefixesNoGapsOrDupes asserts the numeric prefixes form a
// contiguous, duplicate-free sequence (1,2,3,4,...). A gap or duplicate means a
// migration was renamed/dropped or two migrations collide, which breaks the
// "filename is the immutable migration identity" assumption Migrate() relies on
// for its schema_migrations tracking.
func TestMigrationPrefixesNoGapsOrDupes(t *testing.T) {
	names := readMigrationNames(t)

	seen := map[int]string{}
	var prefixes []int
	for _, name := range names {
		m := migrationNameRe.FindStringSubmatch(name)
		if m == nil {
			t.Fatalf("migration %q does not match NNNN_slug.sql naming", name)
		}
		n, _ := strconv.Atoi(m[1])
		if other, dup := seen[n]; dup {
			t.Errorf("duplicate migration prefix %04d used by both %q and %q", n, other, name)
		}
		seen[n] = name
		prefixes = append(prefixes, n)
	}

	sort.Ints(prefixes)
	if len(prefixes) == 0 {
		t.Fatal("no migration prefixes found")
	}
	if prefixes[0] != 1 {
		t.Errorf("first migration prefix = %04d, want 0001", prefixes[0])
	}
	for i := 1; i < len(prefixes); i++ {
		if prefixes[i] != prefixes[i-1]+1 {
			t.Errorf("gap in migration prefixes between %04d and %04d; "+
				"prefixes must be contiguous", prefixes[i-1], prefixes[i])
		}
	}
}

// TestMigrationFilenamesUnique asserts there are no duplicate filenames in the
// migration set. Filenames are the PRIMARY KEY of schema_migrations, so a
// collision would cause one migration to be silently skipped as "applied".
func TestMigrationFilenamesUnique(t *testing.T) {
	names := readMigrationNames(t)
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			t.Errorf("duplicate migration filename %q (filename is the schema_migrations PK)", name)
		}
		seen[name] = true
	}
}

// TestEveryMigrationIsWellFormedDDL asserts each embedded migration is non-empty
// and parses as balanced, statement-bearing SQL. This is a cheap structural
// gate (not a real SQL parser) that catches truncated files, unbalanced
// parens/dollar-quotes, and "comment-only" migrations that would no-op.
func TestEveryMigrationIsWellFormedDDL(t *testing.T) {
	names := readMigrationNames(t)
	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			sql := readMigration(t, name)

			if strings.TrimSpace(sql) == "" {
				t.Fatalf("migration %q is empty", name)
			}

			// Strip -- line comments so comment-only files are detected and so
			// parens inside prose don't skew the balance check.
			var code strings.Builder
			for _, line := range strings.Split(sql, "\n") {
				if i := strings.Index(line, "--"); i >= 0 {
					line = line[:i]
				}
				code.WriteString(line)
				code.WriteString("\n")
			}
			body := code.String()

			if strings.TrimSpace(body) == "" {
				t.Fatalf("migration %q has no SQL statements (comment-only)", name)
			}

			// Must contain at least one terminated statement.
			if !strings.Contains(body, ";") {
				t.Errorf("migration %q contains no terminated statement (missing ';')", name)
			}

			// Parentheses must balance across the statement body.
			if got := strings.Count(body, "(") - strings.Count(body, ")"); got != 0 {
				t.Errorf("migration %q has unbalanced parentheses (open-close=%d)", name, got)
			}

			// plpgsql bodies use $$ delimiters; they must come in pairs.
			if n := strings.Count(body, "$$"); n%2 != 0 {
				t.Errorf("migration %q has an odd number of $$ dollar-quote delimiters (%d)", name, n)
			}

			// Must start (first keyword) with a DDL/known verb, not garbage.
			first := strings.Fields(strings.ToUpper(strings.TrimSpace(body)))
			if len(first) == 0 {
				t.Fatalf("migration %q has no leading keyword", name)
			}
			switch first[0] {
			case "CREATE", "ALTER", "DROP", "INSERT", "UPDATE", "COMMENT", "GRANT", "REVOKE", "SET":
				// ok
			default:
				t.Errorf("migration %q starts with unexpected keyword %q", name, first[0])
			}
		})
	}
}

// TestMigration0003AddsCompletedState is the SQL-level regression for the
// COMPLETED critical. migrations_check_test.go proves the *effective* CHECK
// covers all RunStates; here we pin the fix to its OWNING migration: 0003 must
// drop the old constraint and re-add one that explicitly lists COMPLETED. If a
// future edit removes COMPLETED from 0003 (or moves the fix out of 0003 without
// keeping it), this fails even if some other migration happens to compensate.
func TestMigration0003AddsCompletedState(t *testing.T) {
	const fname = "0003_run_state_completed.sql"
	names := readMigrationNames(t)
	found := false
	for _, n := range names {
		if n == fname {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected migration %q to exist (the COMPLETED-state fix)", fname)
	}

	sql := strings.ToUpper(readMigration(t, fname))

	// It must re-define the agent_runs state CHECK and include COMPLETED.
	if !strings.Contains(sql, "ADD CONSTRAINT") || !strings.Contains(sql, "STATE IN") {
		t.Errorf("%s does not (re)add a `state IN (...)` CHECK constraint on agent_runs", fname)
	}
	if !strings.Contains(sql, "'COMPLETED'") {
		t.Errorf("%s does not add 'COMPLETED' to the agent_runs.state CHECK; "+
			"runs would never reach terminal and the revoke cascade would not fire", fname)
	}

	// The original (0001) CHECK must NOT have had COMPLETED -- this is what makes
	// 0003 a genuine fix rather than a no-op. (Guards against someone "fixing"
	// the bug by editing 0001, which would not migrate already-deployed DBs.)
	init := strings.ToUpper(readMigration(t, "0001_init.sql"))
	initState := stateInCheckRe.FindString(init)
	if strings.Contains(strings.ToUpper(initState), "COMPLETED") {
		t.Errorf("0001_init.sql already lists COMPLETED in its inline CHECK; the COMPLETED fix " +
			"must live in 0003 so deployed databases get the ALTER, not be back-edited into 0001")
	}
}

// TestMigration0004AddsBeforeTruncateTrigger is the regression for the
// append-only TRUNCATE gap. The 0001 trigger fires BEFORE UPDATE OR DELETE
// FOR EACH ROW, which does NOT block `TRUNCATE audit_events` (a statement-level
// DDL that bypasses row triggers). 0004 must add a statement-level
// BEFORE TRUNCATE trigger on audit_events.
func TestMigration0004AddsBeforeTruncateTrigger(t *testing.T) {
	const fname = "0004_audit_truncate_guard.sql"
	names := readMigrationNames(t)
	found := false
	for _, n := range names {
		if n == fname {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected migration %q to exist (the TRUNCATE append-only fix)", fname)
	}

	sql := strings.ToUpper(readMigration(t, fname))

	if !strings.Contains(sql, "CREATE TRIGGER") {
		t.Fatalf("%s does not CREATE a trigger", fname)
	}
	if !strings.Contains(sql, "BEFORE TRUNCATE") {
		t.Errorf("%s trigger is not BEFORE TRUNCATE; a TRUNCATE could silently wipe the audit log", fname)
	}
	if !strings.Contains(sql, "ON AUDIT_EVENTS") {
		t.Errorf("%s trigger is not attached to audit_events", fname)
	}
	if !strings.Contains(sql, "FOR EACH STATEMENT") {
		t.Errorf("%s TRUNCATE trigger must be statement-level (FOR EACH STATEMENT); "+
			"TRUNCATE does not fire row-level triggers", fname)
	}
	// It must reuse the raising function that enforces append-only.
	if !strings.Contains(sql, "EXECUTE FUNCTION AUDIT_EVENTS_APPEND_ONLY") {
		t.Errorf("%s does not execute the audit_events_append_only() guard function", fname)
	}
}

// TestNoOtherMigrationGuardsTruncate documents that the TRUNCATE guard lives
// only in 0004 (the original 0001 trigger covers only UPDATE/DELETE). This is
// the gap 0004 closes; if 0001 ever grows a TRUNCATE clause this test should be
// revisited rather than silently passing the wrong way.
func TestNoOtherMigrationGuardsTruncate(t *testing.T) {
	init := strings.ToUpper(readMigration(t, "0001_init.sql"))
	if strings.Contains(init, "BEFORE TRUNCATE") {
		t.Errorf("0001_init.sql unexpectedly contains a BEFORE TRUNCATE clause; the TRUNCATE " +
			"guard was supposed to be the 0004 fix -- update tests if the design changed")
	}
	// And 0001 DOES still carry the original row-level UPDATE/DELETE guard, so
	// the two trigger concerns are not conflated.
	if !strings.Contains(init, "BEFORE UPDATE OR DELETE") {
		t.Errorf("0001_init.sql lost its original BEFORE UPDATE OR DELETE row trigger on audit_events")
	}
}

// TestMigration0007RevokesAuditDDLPrivileges is the SQL-level regression for N4:
// migration 0007 must establish the least-privilege posture by REVOKEing the
// mutation/DDL privileges on audit_events from PUBLIC. This is defense-in-depth
// (the guard only bites when wardynd connects as a NON-owner role — the honest
// residual documented in the migration and in cmd/wardynd/main.go). Red-first:
// before 0007 exists this test fails (file missing).
func TestMigration0007RevokesAuditDDLPrivileges(t *testing.T) {
	const fname = "0007_audit_least_privilege.sql"
	names := readMigrationNames(t)
	found := false
	for _, n := range names {
		if n == fname {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected migration %q to exist (the audit_events least-privilege fix)", fname)
	}

	sql := strings.ToUpper(readMigration(t, fname))
	if !strings.Contains(sql, "REVOKE") {
		t.Fatalf("%s does not REVOKE any privilege", fname)
	}
	if !strings.Contains(sql, "ON AUDIT_EVENTS") {
		t.Errorf("%s REVOKE is not scoped to audit_events", fname)
	}
	if !strings.Contains(sql, "FROM PUBLIC") {
		t.Errorf("%s must REVOKE ... FROM PUBLIC (the least-privilege default)", fname)
	}
	// The DDL-bypass privileges the append-only guard needs stripped from PUBLIC.
	for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE", "TRIGGER"} {
		if !strings.Contains(sql, priv) {
			t.Errorf("%s does not REVOKE %s on audit_events; that privilege could bypass the append-only guard", fname, priv)
		}
	}
}

// TestMigrationsAreIdempotentMarked asserts the schema-creating migrations use
// idempotent markers so a re-run (or partial prior apply) does not error. The
// Migrate() docstring promises idempotency; while schema_migrations tracking
// gives the first line of defense, the DDL itself uses IF [NOT] EXISTS guards.
func TestMigrationsAreIdempotentMarked(t *testing.T) {
	cases := []struct {
		name   string
		needle string // an idempotency marker the migration must contain
	}{
		// 0001 creates tables/indexes/triggers with IF NOT EXISTS / DROP ... IF EXISTS.
		{"0001_init.sql", "IF NOT EXISTS"},
		// 0002 creates a unique index idempotently.
		{"0002_approval_uniqueness.sql", "IF NOT EXISTS"},
		// 0003 drops-if-exists before re-adding the constraint.
		{"0003_run_state_completed.sql", "DROP CONSTRAINT IF EXISTS"},
		// 0004 drops-if-exists before re-creating the trigger.
		{"0004_audit_truncate_guard.sql", "DROP TRIGGER IF EXISTS"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			sql := strings.ToUpper(readMigration(t, tc.name))
			if !strings.Contains(sql, strings.ToUpper(tc.needle)) {
				t.Errorf("migration %q lacks idempotency marker %q; re-running Migrate() "+
					"or replaying after a partial apply could error", tc.name, tc.needle)
			}
		})
	}
}

// TestTrackingTableAssumptionsHold pins the assumptions Migrate() makes about
// its schema_migrations bookkeeping so a migration cannot accidentally redefine
// or collide with the tracking table. No migration should manage
// schema_migrations itself (Migrate() owns it, created outside any migration tx).
func TestTrackingTableAssumptionsHold(t *testing.T) {
	names := readMigrationNames(t)
	for _, name := range names {
		sql := strings.ToUpper(readMigration(t, name))
		if strings.Contains(sql, "SCHEMA_MIGRATIONS") {
			t.Errorf("migration %q references schema_migrations; that table is owned and managed "+
				"exclusively by Migrate() (created outside any migration tx) and must not be "+
				"created/altered by a migration", name)
		}
	}
}
