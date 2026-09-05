// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Postgres-backed migration integration tests. Unlike db_test.go /
// migrations_check_test.go (pure SQL-text assertions over the embedded
// migrationFS), these run Migrate() against a LIVE Postgres and assert the REAL
// catalog: that the schema applies cleanly, that Migrate() is idempotent, and
// that the constraints/triggers the migrations install are actually enforced by
// the server (the COMPLETED-state CHECK and the audit_events append-only guard,
// including the TRUNCATE gap closed by 0004).
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set
// against the lane's live substrate. Mirrors the pgHarness convention in
// internal/api/grants_confinement_test.go (db.Connect -> db.Migrate).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgPool connects to the live Postgres named by WARDYN_TEST_PG and runs Migrate()
// once, so every catalog assertion below executes against the fully-migrated
// schema. Skips (the ONLY allowed skip) when the env var is absent.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed migration test")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := migrateTolerant(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// migrateTolerant runs Migrate, retrying once on the transient unique-violation
// that can occur when two test BINARIES (this package and secretstore/pg run in
// parallel by `go test ./... ./...`) race to apply the SAME migration to a fresh
// shared DB for the first time. Production Migrate is idempotent; the loser of
// the race just needs to re-read schema_migrations and no-op. This hardens the
// test harness only — it does not change production behavior.
func migrateTolerant(ctx context.Context, pool *pgxpool.Pool) error {
	err := Migrate(ctx, pool)
	if err == nil {
		return nil
	}
	if isConcurrentMigrateRace(err) {
		return Migrate(ctx, pool) // second pass sees an already-migrated DB
	}
	return err
}

// isConcurrentMigrateRace reports whether err is the Postgres unique-violation a
// parallel first-time migration produces (duplicate schema_migrations PK or a
// duplicate catalog object such as the trigger/function type).
func isConcurrentMigrateRace(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" // unique_violation
	}
	return false
}

// embeddedMigrationCount is the number of *.sql migrations bundled in the embed
// FS; schema_migrations must track each exactly once after Migrate().
func embeddedMigrationCount(t *testing.T) int {
	t.Helper()
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
}

// TestMigrateAppliesAndIsIdempotent proves the live-DB idempotency the Migrate()
// docstring promises: after the first Migrate() (done by pgPool), every embedded
// migration is recorded in schema_migrations exactly once; a SECOND Migrate() is
// a no-op (no new rows, no duplicate filenames, no error). This is the catalog
// counterpart to the pure-text TestMigrationsAreIdempotentMarked.
func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	pool := pgPool(t) // first Migrate() already ran
	ctx := context.Background()

	want := embeddedMigrationCount(t)
	if want == 0 {
		t.Fatal("no embedded migrations; embed glob is broken")
	}

	// Every embedded migration must be tracked exactly once after the first run.
	countRecorded := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatalf("count schema_migrations: %v", err)
		}
		return n
	}
	if got := countRecorded(); got != want {
		t.Fatalf("schema_migrations rows after first Migrate = %d, want %d", got, want)
	}

	// No filename is tracked more than once (filename is the PK; a dupe would
	// mean a migration was applied twice).
	var maxDupe int
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(c),0) FROM (
			SELECT COUNT(*) AS c FROM schema_migrations GROUP BY filename
		) g`).Scan(&maxDupe); err != nil {
		t.Fatalf("dupe check: %v", err)
	}
	if maxDupe != 1 {
		t.Errorf("a migration filename is tracked %d times; Migrate is not idempotent", maxDupe)
	}

	// Re-running Migrate() must be a clean no-op: same row count, no error.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate (should be no-op): %v", err)
	}
	if got := countRecorded(); got != want {
		t.Errorf("schema_migrations rows after second Migrate = %d, want %d (re-run must not add rows)", got, want)
	}
}

// TestMigrateAdvisoryLockSerializesBoots proves N5: Migrate() takes the
// dedicated session-level advisory lock so a second, concurrent boot BLOCKS
// until the first finishes rather than racing the migration loop. We simulate an
// in-flight migration on "another boot" by holding the SAME advisory lock on a
// separate connection, then assert a concurrent Migrate() blocks (returns a
// deadline error) instead of completing. Pre-fix (no lock) Migrate() ignored the
// held lock and returned nil immediately here.
func TestMigrateAdvisoryLockSerializesBoots(t *testing.T) {
	pool := pgPool(t) // fully migrated; a lone Migrate() here is a clean no-op
	ctx := context.Background()

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder conn: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateAdvisoryLockKey); err != nil {
		t.Fatalf("hold migration advisory lock: %v", err)
	}

	// With the lock held elsewhere, a concurrent Migrate() must block on
	// pg_advisory_lock. Under a short deadline it returns a context error — proof
	// it serialized behind the holder instead of racing the loop.
	blockCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := Migrate(blockCtx, pool); err == nil {
		t.Fatal("Migrate completed while the migration advisory lock was held by another session; " +
			"it is NOT serialized — concurrent boots can race the migration loop (N5 not in effect)")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Migrate blocked but failed with %v, want context.DeadlineExceeded (blocked on pg_advisory_lock)", err)
	}

	// Release the lock; Migrate() now acquires it and no-ops cleanly.
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrateAdvisoryLockKey); err != nil {
		t.Fatalf("release migration advisory lock: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after lock release should no-op: %v", err)
	}
}

// insertAgentRun inserts a minimally-valid agent_runs row with the given state
// and a unique id, returning the id. It fills every NOT NULL column so the only
// thing under test is the state CHECK. Returns the raw error (nil on success) so
// callers can assert accept/reject.
func insertAgentRun(t *testing.T, pool *pgxpool.Pool, state string) (uuid.UUID, error) {
	t.Helper()
	id := uuid.New()
	spiffe := "spiffe://wardyn.local/run/" + id.String()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO agent_runs
			(id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, 'tester', 'claude-code', 'acme/widgets', 'CC2', $2, $3, 'docker')`,
		id, state, spiffe)
	return id, err
}

// TestAgentRunStateCheckEnforcedLive proves the COMPLETED critical at the DB
// level: the live agent_runs.state CHECK ACCEPTS 'COMPLETED' (the terminal
// state 0003 added — without it the completion watcher's UPDATE was rejected and
// the revoke cascade never fired) and REJECTS an unknown state. The pure-text
// TestAgentRunStateCheckCoversAllStates asserts the migration source; this
// asserts Postgres actually enforces it.
func TestAgentRunStateCheckEnforcedLive(t *testing.T) {
	pool := pgPool(t)

	// ACCEPT: COMPLETED must be insertable (regression for the COMPLETED fix).
	if _, err := insertAgentRun(t, pool, "COMPLETED"); err != nil {
		t.Fatalf("INSERT agent_runs state=COMPLETED rejected by live CHECK: %v; "+
			"the 0003 COMPLETED fix is not in effect on this DB", err)
	}

	// REJECT: an unknown state must violate the CHECK constraint.
	if _, err := insertAgentRun(t, pool, "BOGUS_STATE"); err == nil {
		t.Fatal("INSERT agent_runs state=BOGUS_STATE was ACCEPTED; the state CHECK is not enforced")
	} else if !strings.Contains(strings.ToLower(err.Error()), "check") &&
		!strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Errorf("unknown state rejected, but not by a CHECK/constraint violation: %v", err)
	}
}

// insertAuditEvent inserts a minimally-valid audit_events row (every NOT NULL
// column filled) with a unique id and returns that id. Used to seed a row the
// append-only trigger tests then try (and must fail) to mutate.
func insertAuditEvent(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES ($1, 'system', 'tester', 'test.seed', 'success')`,
		id); err != nil {
		t.Fatalf("seed audit_events row: %v", err)
	}
	return id
}

// TestAuditEventsAppendOnlyEnforcedLive proves the append-only guarantee at the
// DB level for ALL three mutation paths, including the TRUNCATE gap 0004 closes:
//   - UPDATE is rejected (0001 row trigger),
//   - DELETE is rejected (0001 row trigger),
//   - TRUNCATE is rejected (0004 statement trigger — a real TRUNCATE is issued
//     and must error; without 0004 it would silently wipe the audit log).
//
// Each subtest seeds its OWN unique row so they are isolated within the shared
// DB and never depend on an empty table.
func TestAuditEventsAppendOnlyEnforcedLive(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	const wantMsg = "append-only" // the trigger raises 'audit_events is append-only'

	t.Run("UPDATE_rejected", func(t *testing.T) {
		id := insertAuditEvent(t, pool)
		_, err := pool.Exec(ctx,
			`UPDATE audit_events SET actor='tamper' WHERE id=$1`, id)
		if err == nil {
			t.Fatal("UPDATE on audit_events was ACCEPTED; append-only row trigger not enforced")
		}
		if !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("UPDATE rejected with %q, want it to mention %q", err.Error(), wantMsg)
		}
	})

	t.Run("DELETE_rejected", func(t *testing.T) {
		id := insertAuditEvent(t, pool)
		_, err := pool.Exec(ctx,
			`DELETE FROM audit_events WHERE id=$1`, id)
		if err == nil {
			t.Fatal("DELETE on audit_events was ACCEPTED; append-only row trigger not enforced")
		}
		if !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("DELETE rejected with %q, want it to mention %q", err.Error(), wantMsg)
		}
	})

	t.Run("TRUNCATE_rejected", func(t *testing.T) {
		// Seed a row first so we can also confirm the table survives the attempt.
		id := insertAuditEvent(t, pool)

		// Issue a REAL TRUNCATE. The 0004 statement-level BEFORE TRUNCATE trigger
		// must raise (TRUNCATE bypasses the 0001 row trigger). This is the direct
		// regression for the TRUNCATE append-only gap.
		_, err := pool.Exec(ctx, `TRUNCATE TABLE audit_events`)
		if err == nil {
			t.Fatal("TRUNCATE audit_events was ACCEPTED; the 0004 BEFORE TRUNCATE guard is missing — " +
				"the audit log could be silently wiped")
		}
		if !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("TRUNCATE rejected with %q, want it to mention %q", err.Error(), wantMsg)
		}

		// The row we seeded must still be present (TRUNCATE was blocked, not
		// partially applied).
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM audit_events WHERE id=$1`, id).Scan(&n); err != nil {
			t.Fatalf("post-TRUNCATE survivor check: %v", err)
		}
		if n != 1 {
			t.Errorf("seeded audit row survivor count = %d, want 1 (TRUNCATE should have been fully blocked)", n)
		}
	})
}

// auditTriggerEnabled reports whether one audit_events trigger exists AND fires
// — tgenabled 'O' (origin) or 'A' (ALWAYS), the two states ensureAuditTriggers
// accepts. It MIRRORS that predicate, so it has to move with it.
func auditTriggerEnabled(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_trigger
		 WHERE tgname = $1 AND tgrelid = 'audit_events'::regclass AND tgenabled IN ('O', 'A')`, name).Scan(&n); err != nil {
		t.Fatalf("read pg_trigger %s: %v", name, err)
	}
	return n == 1
}

// TestMigrateRestoresADisabledChainTrigger covers the half of F11 H2 that the
// DROP probe does not: ALTER TABLE ... DISABLE TRIGGER leaves the catalog row in
// place, so a check that only asked "does it exist" would pass on a table where
// the chain never fires — the quietest version of the same hole, and the one an
// owner reaches for because it looks reversible.
func TestMigrateRestoresADisabledChainTrigger(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	if !auditTriggerEnabled(t, pool, auditChainTrigger) {
		t.Fatal("precondition: the chain trigger is not enabled before the test ran")
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER `+auditChainTrigger); err != nil {
		t.Skipf("cannot DISABLE TRIGGER as this role (%v); the test needs table ownership", err)
	}
	t.Cleanup(func() {
		// Belt and braces: Migrate below is what should have re-enabled it, but
		// a failure here must not leave the shared table writing unchained rows
		// for every later test in the run.
		if !auditTriggerEnabled(t, pool, auditChainTrigger) {
			if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER `+auditChainTrigger); err != nil {
				t.Errorf("re-enable %s: %v", auditChainTrigger, err)
			}
		}
	})
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate with a disabled chain trigger: %v", err)
	}
	if !auditTriggerEnabled(t, pool, auditChainTrigger) {
		t.Error("a DISABLED chain trigger survived Migrate; every row written from now on is unchained and the sweep will report it")
	}
}

// TestMigrateRefusesWithoutTheAppendOnlyTrigger pins the other arm of
// ensureAuditTriggers. The chain trigger is restored because its migrations are
// replayable; the append-only trigger is defined by 0001 (the whole initial
// schema), so replaying it at boot to fix one trigger is a bigger blast radius
// than refusing — but the process must NOT continue silently either, which is
// what it used to do.
func TestMigrateRefusesWithoutTheAppendOnlyTrigger(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	const name = "audit_events_no_update"
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER `+name); err != nil {
		t.Skipf("cannot DISABLE TRIGGER as this role (%v); the test needs table ownership", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER `+name); err != nil {
			t.Errorf("re-enable %s: %v — the shared audit_events table is left writable", name, err)
		}
	})
	err := Migrate(ctx, pool)
	if err == nil {
		t.Fatalf("Migrate returned nil with %s disabled; the append-only guarantee is not in force and the boot said nothing", name)
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("Migrate error = %q; it must name the missing trigger so an operator knows what to restore", err)
	}
}

// auditTriggerState returns one audit_events trigger's raw tgenabled letter.
func auditTriggerState(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `
		SELECT tgenabled FROM pg_trigger
		 WHERE tgname = $1 AND tgrelid = 'audit_events'::regclass`, name).Scan(&state); err != nil {
		t.Fatalf("read tgenabled %s: %v", name, err)
	}
	return state
}

// TestMigrateLeavesAnAlwaysTriggerAlone pins the hardening case that the
// existence check must not punish. ALTER TABLE ... ENABLE ALWAYS TRIGGER sets
// tgenabled='A', which makes the trigger fire even under
// session_replication_role = replica — precisely the bypass the verify sweep's
// rule 3 catches only after the fact, closed here at the source. A boot check
// that read 'A' as "missing or disabled" would DROP and re-create the trigger as
// plain 'O', silently reverting the operator's hardening; the same reading on an
// append-only trigger would refuse the boot and brick the upgrade of the most
// careful deployment on the fleet.
//
// Both trigger classes are covered, because they take different arms of
// ensureAuditTriggers: the chain trigger's arm restores, the append-only arm
// refuses.
func TestMigrateLeavesAnAlwaysTriggerAlone(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	for _, name := range append([]string{auditChainTrigger}, auditAppendOnlyTriggers...) {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE ALWAYS TRIGGER `+name); err != nil {
				t.Skipf("cannot ENABLE ALWAYS as this role (%v); the test needs table ownership", err)
			}
			t.Cleanup(func() {
				// Back to the shipped state for every later test in the run.
				if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER `+name); err != nil {
					t.Errorf("restore %s to 'O': %v", name, err)
				}
			})
			if got := auditTriggerState(t, pool, name); got != "A" {
				t.Fatalf("precondition: tgenabled = %q after ENABLE ALWAYS, want 'A'", got)
			}
			if err := Migrate(ctx, pool); err != nil {
				t.Fatalf("Migrate with %s set to ALWAYS: %v — a hardened trigger must not refuse the boot", name, err)
			}
			if got := auditTriggerState(t, pool, name); got != "A" {
				t.Errorf("tgenabled = %q after Migrate, want 'A' — the boot check reverted an operator's ENABLE ALWAYS hardening", got)
			}
		})
	}
}

// chainTriggerMigrations returns the migration filenames that define the chain
// trigger. It calls the PRODUCTION helper rather than restating its predicate,
// so this test and the boot-time replay can never disagree about which files
// the set contains.
func chainTriggerMigrations(t *testing.T) []string {
	t.Helper()
	names, err := triggerMigrationFiles(auditChainTrigger)
	if err != nil {
		t.Fatalf("triggerMigrationFiles: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no migration defines the chain trigger")
	}
	return names
}

// unapplyMigrations deletes the given filenames from schema_migrations so the
// next Migrate re-runs them, exactly as a 0.6.x database that never saw them
// would. The rows are put back by the Migrate under test (it re-records what it
// applies), and the cleanup restores any the test did not reach.
func unapplyMigrations(t *testing.T, pool *pgxpool.Pool, names []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE filename = ANY($1)`, names); err != nil {
		t.Fatalf("un-apply %v: %v", names, err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO schema_migrations (filename) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING`, names); err != nil {
			t.Errorf("re-record %v in schema_migrations: %v", names, err)
		}
	})
}

// TestMigrateKeepsAnAlwaysTriggerAcrossAnUpgrade is the arm
// TestMigrateLeavesAnAlwaysTriggerAlone structurally cannot reach: that test
// calls Migrate on an ALREADY fully-migrated database, where every migration is
// skipped and only ensureAuditTriggers runs — so it exercises the half that
// honours 'A' and never the migration loop that reverted it.
//
// The real upgrade path is a 0.6.x deployment that had hardened the chain
// trigger and then applies the 0.7 migrations that redefine it. Each of those
// ends in DROP TRIGGER IF EXISTS + CREATE TRIGGER, and CREATE TRIGGER always
// yields tgenabled='O', so the operator's hardening was removed with nothing
// logged — and the boot check that follows read the resulting 'O' as the normal
// shipped state. docs/OPERATIONS.md promises this never happens.
func TestMigrateKeepsAnAlwaysTriggerAcrossAnUpgrade(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	pending := chainTriggerMigrations(t)

	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE ALWAYS TRIGGER `+auditChainTrigger); err != nil {
		t.Skipf("cannot ENABLE ALWAYS as this role (%v); the test needs table ownership", err)
	}
	t.Cleanup(func() {
		// Back to the shipped state for every later test in the run.
		if _, err := pool.Exec(context.Background(), `ALTER TABLE audit_events ENABLE TRIGGER `+auditChainTrigger); err != nil {
			t.Errorf("restore %s to 'O': %v", auditChainTrigger, err)
		}
	})
	if got := auditTriggerState(t, pool, auditChainTrigger); got != "A" {
		t.Fatalf("precondition: tgenabled = %q after ENABLE ALWAYS, want 'A'", got)
	}

	unapplyMigrations(t, pool, pending) // simulate the 0.6.x -> 0.7 upgrade
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying %v over a hardened trigger: %v", pending, err)
	}
	if got := auditTriggerState(t, pool, auditChainTrigger); got != "A" {
		t.Fatalf("tgenabled = %q after applying %v, want 'A' — the upgrade silently reverted the operator's "+
			"ENABLE ALWAYS hardening on the chain trigger, which docs/OPERATIONS.md promises is left exactly as it is",
			got, pending)
	}

	// Idempotent: a second Migrate with nothing pending must leave 'A' alone
	// (and must not thrash the catalog re-applying an ALTER that is not needed).
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if got := auditTriggerState(t, pool, auditChainTrigger); got != "A" {
		t.Errorf("tgenabled = %q after a no-op Migrate, want 'A'", got)
	}
}

// TestMigrateDoesNotHardenATriggerNobodyHardened is the other direction of the
// same guard: the restore must fire ONLY for a trigger the operator had set to
// 'A' before the loop ran. A deployment that never hardened anything must come
// out of the same upgrade with plain 'O' — promoting a shipped trigger to ALWAYS
// on its owner's behalf would be its own surprise (an ALWAYS trigger fires under
// session_replication_role = replica, which is exactly what a restore/replication
// tool sets to load rows).
func TestMigrateDoesNotHardenATriggerNobodyHardened(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	pending := chainTriggerMigrations(t)

	for _, name := range append([]string{auditChainTrigger}, auditAppendOnlyTriggers...) {
		if got := auditTriggerState(t, pool, name); got != "O" {
			t.Fatalf("precondition: %s is %q, want the shipped 'O' — another test left the table hardened", name, got)
		}
	}

	unapplyMigrations(t, pool, pending)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying %v: %v", pending, err)
	}
	for _, name := range append([]string{auditChainTrigger}, auditAppendOnlyTriggers...) {
		if got := auditTriggerState(t, pool, name); got != "O" {
			t.Errorf("%s is %q after Migrate, want 'O' — the hardening restore fired on a trigger nobody hardened", name, got)
		}
	}
}

// ─── the 0048-0054 upgrade set, applied over NON-EMPTY data ──────────────────

// partialSchemaPool migrates a throwaway schema up to (but NOT including)
// upTo, and returns a pool pointed at it. It is probeSchemaPool's other half:
// that helper gives a FULLY migrated schema, which is exactly the state an
// upgrade test cannot start from.
//
// It calls the PRODUCTION applyMigration rather than executing the files by
// hand, so a partially-migrated database here is recorded in schema_migrations
// the same way a real one is — and the Migrate() under test then picks up
// precisely the pending set a 0.6.x deployment would.
func partialSchemaPool(t *testing.T, upTo string) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot point a connection at another schema")
	}
	ctx := context.Background()
	base := pgPool(t)
	schema := fmt.Sprintf("wardyn_up_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := base.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create upgrade schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect with search_path=%s: %v", schema, err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatalf("ensure schema_migrations in %s: %v", schema, err)
	}
	applied := 0
	for _, name := range readMigrationNames(t) {
		if name >= upTo {
			break // lexical order IS apply order
		}
		if err := applyMigration(ctx, pool, name, readMigration(t, name)); err != nil {
			t.Fatalf("apply %s into %s: %v", name, schema, err)
		}
		applied++
	}
	if applied == 0 {
		t.Fatalf("no migration sorts before %q; the helper applied nothing", upTo)
	}
	return pool, schema
}

// TestPG_MigrateAppliesTheUpgradeSetOverNonEmptyData covers the gap the
// audit-chain trio already had and 0048-0054 did not: every committed test that
// applies those files runs against an EMPTY database, so the upgrade path a real
// deployment takes — the same DDL over tables that already hold rows — was
// exercised nowhere, and the CI PG lane inherited the gap.
//
// 0050 is the sharp one and the reason this is not a formality: it DROPs
// secrets' primary key and rebuilds it as (owned_by, name) on a table every
// deployment holds rows in. Nothing committed had ever run that statement over a
// row. The other six are additive, and this asserts they stay additive — a
// column added NOT NULL without a default would fail here and only here.
func TestPG_MigrateAppliesTheUpgradeSetOverNonEmptyData(t *testing.T) {
	const upgradeFloor = "0048"
	pool, schema := partialSchemaPool(t, upgradeFloor)
	ctx := context.Background()

	// Pre-upgrade rows, in the three tables the upgrade set touches columns on.
	wsID, tokenID := uuid.New(), uuid.New()
	secretName := "test-upgrade-secret-" + uuid.NewString()[:8]
	for _, s := range []struct {
		what, q string
		args    []any
	}{
		// attachments is the one column with no default (0031 set it NOT NULL
		// after backfilling); everything else the pre-0048 shape requires has one.
		{"workspace", `INSERT INTO workspaces (id, name, attachments) VALUES ($1, $2, '[]'::jsonb)`,
			[]any{wsID, "test-upgrade-ws-" + uuid.NewString()[:8]}},
		{"secret", `INSERT INTO secrets (name, ciphertext) VALUES ($1, '\x00'::bytea)`,
			[]any{secretName}},
		{"api token", `INSERT INTO api_tokens (id, principal, token_sha256) VALUES ($1, 'upgrade@example.com', $2)`,
			[]any{tokenID, uuid.NewString()}},
		{"audit row", `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
			VALUES (gen_random_uuid(), 'system', 'upgrade-probe', 'test.upgrade.seed', 'success')`, nil},
	} {
		if _, err := pool.Exec(ctx, s.q, s.args...); err != nil {
			t.Fatalf("seed %s in %s: %v", s.what, schema, err)
		}
	}

	// Precondition: the upgrade set really IS pending, or this test would be a
	// second copy of the fully-migrated one.
	var pending int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE filename >= $1`, upgradeFloor).Scan(&pending); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if pending != 0 {
		t.Fatalf("precondition: %d migration(s) at or above %s are already recorded applied", pending, upgradeFloor)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying the %s+ upgrade set over non-empty workspaces/secrets/api_tokens/audit_events: %v",
			upgradeFloor, err)
	}

	// THE ROWS SURVIVED, with the new columns taking their defaults.
	var wsOwner, secretOwner, tokenRole string
	if err := pool.QueryRow(ctx, `SELECT owned_by FROM workspaces WHERE id = $1`, wsID).Scan(&wsOwner); err != nil {
		t.Fatalf("0048 over an existing workspace row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT owned_by FROM secrets WHERE name = $1`, secretName).Scan(&secretOwner); err != nil {
		t.Fatalf("0050 over an existing secrets row: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT role FROM api_tokens WHERE id = $1`, tokenID).Scan(&tokenRole); err != nil {
		t.Fatalf("0060's CHECK over an existing api_tokens row: %v", err)
	}
	if wsOwner != "" || secretOwner != "" {
		t.Errorf("pre-upgrade rows did not take the '' owner default: workspace=%q secret=%q", wsOwner, secretOwner)
	}
	if tokenRole != "member" {
		t.Errorf("pre-upgrade api_tokens.role = %q, want the column default 'member'", tokenRole)
	}

	// 0050 REBUILT THE PRIMARY KEY on a table holding a row. Asserted from the
	// catalog rather than inferred from the migration text.
	var pkCols []string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(array_agg(a.attname::text ORDER BY k.ord), ARRAY[]::text[])
		FROM pg_constraint c
		JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.conrelid = 'secrets'::regclass AND c.contype = 'p'`).Scan(&pkCols); err != nil {
		t.Fatalf("read the secrets primary key: %v", err)
	}
	if len(pkCols) != 2 || pkCols[0] != "owned_by" || pkCols[1] != "name" {
		t.Errorf("secrets primary key = %v, want [owned_by name] — 0050 rebuilds it over rows every deployment holds", pkCols)
	}

	// And the upgrade is idempotent over the same non-empty data.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate over the upgraded, non-empty schema: %v", err)
	}
}
