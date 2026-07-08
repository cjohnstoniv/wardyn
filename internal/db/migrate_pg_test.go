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
