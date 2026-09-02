// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package db provides Postgres connection bootstrapping and schema migration
// for the Wardyn control plane. Postgres is the ONLY required dependency.
package db

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationExecutor is the subset of *pgxpool.Pool / *pgxpool.Conn the migration
// steps need. Migrate runs ALL of them on the SINGLE advisory-lock-holding
// connection (never re-acquiring from the pool), so a pool_max_conns=1 DSN can't
// self-deadlock — the held lock conn would otherwise starve the loop.
type migrationExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateAdvisoryLockKey is the fixed session-level advisory lock key that
// serializes concurrent Migrate() runs (N5). Idempotent DDL makes a race benign
// today, but a future non-idempotent migration could partial-apply if two
// wardynd boots ran the loop at once; the lock makes the second boot BLOCK until
// the first finishes, then see every migration applied and no-op. Any stable
// value works.
const migrateAdvisoryLockKey int64 = 0x5741524459_4D4947 // ASCII "WARDYMIG"; any stable value works

// ReaperAdvisoryLockKey makes the lifecycle reap tick single-flight across
// control planes. Unlike migrateAdvisoryLockKey (a BLOCKING lock — the second
// boot must still see the migrations applied), this one is only ever taken with
// TryAdvisoryLock: a replica that loses SKIPS the tick, because a queued second
// reap of the same runs is pure duplicate work and a duplicate run.autostop.
const ReaperAdvisoryLockKey int64 = 0x5741524459_524541 // ASCII "WARDYREA"

// GroundTruthRotatorLockKey elects the ground-truth token rotator's leader
// across control planes (S2). Unlike ReaperAdvisoryLockKey (re-tried every
// tick via TryAdvisoryLock, release()d at the end of each one), this key is
// acquired ONCE before the rotator's loop starts and held for the process
// lifetime: only the holder mints/writes the shared token file, and every
// other replica parks on a backoff and retries, taking over automatically
// when the holder's Postgres session ends.
//
// It buys AT MOST ONE STEADY-STATE leader, NOT mutual exclusion. An advisory
// lock dies with its SESSION, not with the process, and the holder never
// re-verifies it: a Postgres restart, a failover, pg_terminate_backend or an
// idle-session timeout releases it under a still-running leader, and a standby
// takes over within one backoff — two rotators, neither aware. Harmless for
// THIS workload only, because every write is an atomic rename of a stateless
// token (cmd/wardynd/gt_rotator.go). Work that needs genuine fencing must not
// reuse this key. Any stable value works, as long as it differs from every
// other key in this file.
const GroundTruthRotatorLockKey int64 = 0x5741524459_475452 // ASCII "WARDYGTR"

// SecretRekeyLockKey serializes the `wardynd -rotate-age-key` maintenance mode
// (cmd/wardynd's rotateAgeKeyMode): two concurrent rekeys of the same store
// would each re-encrypt from an old key the other has already replaced, so the
// second is refused rather than queued (TryAdvisoryLock, like
// ReaperAdvisoryLockKey).
//
// HONEST CEILING — this does NOT detect a running wardynd. No wardynd holds a
// process-lifetime lock on this key or any other unconditional one (the reaper
// takes ReaperAdvisoryLockKey per tick and releases it; GroundTruthRotatorLockKey
// is only taken when the rotator is configured), so a serving daemon is
// invisible to this check. "Stop the daemon first" is an operator procedure
// documented in docs/OPERATIONS.md, not something this lock enforces — a live
// daemon holds the OLD identity in memory and would write ciphertext under a key
// the rekey has already retired.
const SecretRekeyLockKey int64 = 0x5741524459_524B59 // ASCII "WARDYRKY"
// AuditChainLockKey serializes appends to the audit_events hash chain
// (migration 0047). Unlike every key above it is taken with the TRANSACTION
// -scoped pg_advisory_xact_lock, never the session-scoped form: it is released
// by the commit that makes the new row visible, so the next writer's head read
// cannot miss it, and no code path can leak it by forgetting a release.
// SINCE 0056_audit_chain_serialize.sql THE TRIGGER TAKES IT TOO, and that is
// what binds writers this package does not know about. 0047 could not: the
// identity default had already assigned seq by the time a BEFORE INSERT trigger
// ran, so two racing writers could take the lock there in the opposite order to
// their seq allocation and invert chain order against seq order. 0056 removes
// that objection by allocating seq inside the trigger, under this lock, so
// position and chain link are decided together.
// Both in-tree insert paths still take it BEFORE their INSERT statement, on the
// inserting transaction (store.InsertAuditEvent and the broker's
// insertAuditEventTx): advisory locks are re-entrant within a transaction, so
// the trigger's acquisition is free for them, and holding it across the whole
// statement is what it always was.
// ponytail: ONE lock for the whole chain, so audit appends are globally
// serialized. That IS the feature (a chain has exactly one head), and audit
// write volume is nowhere near a contention regime. If it ever is, the upgrade
// is per-partition chains with a key per partition, not a finer lock over one.
const AuditChainLockKey int64 = 0x5741524459_434841 // ASCII "WARDYCHA"

// TryAdvisoryLock takes session-level advisory lock key on a connection borrowed
// from pool WITHOUT waiting, reporting ok=false when another session already
// holds it. Call the returned release (deferred) to unlock and hand the
// connection back — skipping it strands a pooled conn for the life of the
// process. Only for work of BOUNDED duration: the borrowed conn is unavailable
// to everyone else until release — which also means the caller's own queries
// need a SECOND conn, so this requires pool_max_conns >= 2 (a 1-conn pool would
// self-deadlock: the lock holds the only conn while the guarded work blocks on
// Acquire; the reaper's per-tick deadline turns that into a failed tick, not a
// hang, but the lock is still wasted).
func TryAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key int64) (release func(), ok bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("db: acquire advisory lock conn: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("db: try advisory lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// Unlock on a background context: ctx is typically cancelled at shutdown,
		// exactly when releasing matters most. Best-effort — the lock also dies
		// with the session when the conn is finally closed.
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key) //nolint:errcheck — best-effort release
		conn.Release()
	}, true, nil
}

// AuditDDLProtected reports whether the given (application) pool's role is
// UNABLE to bypass the audit_events append-only triggers via DDL — i.e. it is
// neither a superuser nor a MEMBER of the table's owner role (membership, not
// just direct ownership: a role GRANTed the owner role inherits DROP TRIGGER /
// ALTER ... DISABLE TRIGGER rights) AND it does not hold the TRIGGER privilege
// on the table. The N4 role-separation only protects the append-only guarantee
// when this is true, so the two-DSN deploy must be VERIFIED here rather than
// assumed (honesty: never log a protection claim stronger than the enforcing
// role setup). Fails safe: any ambiguity (missing table, error) reports NOT
// protected.
//
// THE TRIGGER PRIVILEGE IS PART OF THE CLAIM, and it is the least obvious third
// of it. A role that is neither owner nor superuser but holds
// GRANT TRIGGER ON audit_events cannot drop the shipped triggers — it can do
// something quieter: CREATE its own BEFORE INSERT trigger. Postgres fires
// same-event row triggers in NAME order, so one named after audit_events_chain
// runs last and overwrites NEW.prev_hash/NEW.row_hash on the way in, minting
// rows that hash to whatever it says while every shipped guard stays armed and
// every catalog check still finds them. 0007_audit_least_privilege.sql revokes
// TRIGGER from PUBLIC precisely because of that, but a deploy is free to grant
// it back, so the claim has to be checked and not inferred.
func AuditDDLProtected(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var canBypass bool
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(
			bool_or(r.rolsuper
			        OR pg_has_role(current_user, c.relowner, 'MEMBER')
			        OR has_table_privilege(current_user, c.oid, 'TRIGGER')),
			true)
		FROM pg_class c
		JOIN pg_roles r ON r.rolname = current_user
		WHERE c.relname = 'audit_events' AND c.relkind = 'r'`,
	).Scan(&canBypass)
	if err != nil {
		return false, fmt.Errorf("db: check audit ddl protection: %w", err)
	}
	return !canBypass, nil
}

// Connect opens a pgxpool to dsn and performs a lightweight liveness check.
// Returns the pool; caller owns Close().
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies all migrations in internal/db/migrations/*.sql in lexical
// order. Each migration runs inside its own transaction; already-applied
// filenames (tracked in schema_migrations) are skipped. Idempotent.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// N5: serialize concurrent boots. Take a session-level advisory lock on a
	// SINGLE dedicated pooled connection (lock + unlock must hit the same
	// session) so a second wardynd blocks here until the first finishes the loop.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: acquire migration lock conn: %w", err)
	}
	defer conn.Release()
	// Register the best-effort unlock BEFORE acquiring, on a background context,
	// so the lock is released even if ctx is cancelled at the instant the server
	// grants it (pgx can return the ctx error after the grant); unlocking a
	// non-held lock is a harmless no-op.
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrateAdvisoryLockKey) //nolint:errcheck — best-effort release
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateAdvisoryLockKey); err != nil {
		return fmt.Errorf("db: acquire migration advisory lock: %w", err)
	}

	// Every statement below runs on `conn` (NOT `pool`) so the migration needs
	// exactly ONE connection — a pool_max_conns=1 DSN must not self-deadlock
	// against the lock-holding conn.
	return migrateOn(ctx, conn)
}

// migrateOn applies pending migrations using a single executor (the advisory-
// lock-holding connection). Separated so both the lock path and tests share it.
func migrateOn(ctx context.Context, db migrationExecutor) error {
	// Ensure the tracking table exists first (outside any migration tx).
	if _, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("db: ensure schema_migrations: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("db: read migrations dir: %w", err)
	}

	// Sort lexically (0001_init.sql < 0002_... etc.).
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		applied, err := isMigrationApplied(ctx, db, name)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		data, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("db: read migration %s: %w", name, err)
		}

		// W28-S1-4: log elapsed time per applied migration so a slow one (e.g. an
		// index build on an unbounded table) is VISIBLE in the boot log before its
		// caller's timeout turns it fatal, rather than the boot just going silent
		// for however long the timeout allows.
		start := time.Now()
		if err := applyMigration(ctx, db, name, string(data)); err != nil {
			return err
		}
		slog.InfoContext(ctx, "db: applied migration", slog.String("file", name), slog.Duration("elapsed", time.Since(start)))
	}
	return ensureAuditTriggers(ctx, db)
}

// auditChainTrigger is the BEFORE INSERT trigger that hash-chains audit_events
// (0047, redefined by 0056). auditAppendOnlyTriggers are the two that make the
// table append-only (0001, 0004).
const auditChainTrigger = "audit_events_chain"

var auditAppendOnlyTriggers = []string{"audit_events_no_update", "audit_events_no_truncate"}

// ensureAuditTriggers is the boot-time answer to "the migration ran once, years
// of restarts ago". schema_migrations records a FILENAME, so an owner or
// superuser who DROPs (or DISABLEs) one of the audit_events triggers leaves a
// database that every later Migrate happily reports as fully migrated: the
// catalog no longer matches the schema the migrations describe, and nothing
// looked. Every row written after that is unchained — and an unchained row is
// exactly what the verify sweep now names as a break, so the two halves of this
// hole close together.
//
// The chain trigger is RESTORED rather than refused: it is defined by
// idempotent DROP-IF-EXISTS/CREATE migrations that can simply be replayed, and a
// wardynd that refuses to boot leaves the deployment with no audit log at all —
// worse than one that puts the trigger back and says so loudly. The append-only
// triggers are only CHECKED: they are defined by 0001, the whole initial schema,
// and replaying that at boot to fix one trigger is a far bigger blast radius
// than refusing. Either way the process does not continue silently, which is the
// property that was missing.
//
// A missing audit_events table is not this function's business (an empty
// database mid-bootstrap has none yet); it reports protected-by-absence and
// leaves the rest of the boot to say so.
func ensureAuditTriggers(ctx context.Context, db migrationExecutor) error {
	present, err := auditTriggerNames(ctx, db)
	if err != nil {
		return err
	}
	if present == nil { // no audit_events table at all
		return nil
	}
	if !present[auditChainTrigger] {
		slog.ErrorContext(ctx, "db: the audit_events hash-chain trigger is missing or disabled; restoring it — rows written since it went away are UNCHAINED and the verify sweep will report them",
			slog.String("trigger", auditChainTrigger))
		if err := replayTriggerMigrations(ctx, db, auditChainTrigger); err != nil {
			return err
		}
		if present, err = auditTriggerNames(ctx, db); err != nil {
			return err
		}
		if !present[auditChainTrigger] {
			return fmt.Errorf("db: %s trigger is missing and could not be restored; refusing to run with an unchained audit log", auditChainTrigger)
		}
		slog.WarnContext(ctx, "db: audit_events hash-chain trigger restored", slog.String("trigger", auditChainTrigger))
	}
	for _, name := range auditAppendOnlyTriggers {
		if !present[name] {
			return fmt.Errorf("db: %s trigger is missing or disabled on audit_events; the append-only guarantee is not in force, and restoring it means replaying the initial schema — refusing to start", name)
		}
	}
	return nil
}

// auditTriggerNames returns the ENABLED ("O") row/statement triggers on
// audit_events, or nil when the table does not exist. Disabled is treated as
// absent on purpose: ALTER TABLE ... DISABLE TRIGGER leaves the catalog row in
// place, so a check that only asked whether the trigger EXISTS would pass on a
// table where it never fires.
func auditTriggerNames(ctx context.Context, db migrationExecutor) (map[string]bool, error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil, nil
	}
	var names []string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(tgname::text), ARRAY[]::text[])
		FROM pg_trigger
		WHERE tgrelid = 'audit_events'::regclass AND NOT tgisinternal AND tgenabled = 'O'`,
	).Scan(&names); err != nil {
		return nil, fmt.Errorf("db: read audit_events triggers: %w", err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

// replayTriggerMigrations re-executes every embedded migration that defines
// trigger, in filename order, WITHOUT touching schema_migrations: those rows
// still describe what was applied and when, and a restore is not a new
// migration. Discovered by content rather than listed, so a later migration
// that redefines the trigger is replayed too — replaying only the original
// would reinstate a superseded definition (0047's unserialized chain function,
// which 0056 replaced).
func replayTriggerMigrations(ctx context.Context, db migrationExecutor, trigger string) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("db: read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return fmt.Errorf("db: read migration %s: %w", e.Name(), err)
		}
		if strings.Contains(string(body), "TRIGGER "+trigger) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("db: read migration %s: %w", name, err)
		}
		if _, err := db.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("db: replay %s to restore trigger %s: %w", name, trigger, err)
		}
		slog.InfoContext(ctx, "db: replayed migration to restore an audit trigger",
			slog.String("file", name), slog.String("trigger", trigger))
	}
	return nil
}

func isMigrationApplied(ctx context.Context, db migrationExecutor, filename string) (bool, error) {
	var count int
	err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE filename = $1`, filename,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("db: check migration %s: %w", filename, err)
	}
	return count > 0, nil
}

func applyMigration(ctx context.Context, db migrationExecutor, filename, sql string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx for migration %s: %w", filename, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — best-effort on failure path

	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("db: apply migration %s: %w", filename, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (filename) VALUES ($1)`, filename,
	); err != nil {
		return fmt.Errorf("db: record migration %s: %w", filename, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit migration %s: %w", filename, err)
	}
	return nil
}
