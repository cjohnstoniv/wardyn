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

// AuditChainLockTimeout bounds how long ANY writer waits for AuditChainLockKey
// before giving up. Since 0056 the trigger takes that lock on every insert into
// audit_events, including inserts from outside this repo, so one transaction
// that inserted an audit row and stayed open holds up every audit write in the
// process - and that is reachable with no Wardyn bug at all: an operator's psql
// session, a seed script, a paused migration tool. AuditSpool.Drain already
// bounds its side of this at 15s per pass ("one idle psql transaction must not
// become a process-wide stall"); the SYNCHRONOUS side had no bound of any kind.
// A request-path audit write took the lock on the raw request context, against a
// server with lock_timeout = 0 and statement_timeout = 0 and an http.Server that
// deliberately sets no WriteTimeout, so it waited forever - pinning a request
// goroutine and a pool connection each time. With pool_max_conns at the
// documented minimum of 3, a handful of stuck audit writes exhausts the pool and
// every other query in the process starts blocking behind them.
//
// BOTH DIRECTIONS OF THE CHOICE, because a bound on a synchronous path can fail
// either way. Too short and a healthy-but-loaded deployment refuses audit writes
// it could have completed; too long and the request path stalls exactly when the
// database is already in trouble. 5s is chosen against measured shapes rather
// than taste: a legitimate wait here is other audit writers queueing, each
// holding the lock for one nextval, one indexed head read, one sha256 and one
// insert - low single-digit milliseconds - so 5s absorbs a queue in the
// thousands before it ever refuses a write that would have completed. It is also
// deliberately well UNDER the drain's 15s pass bound, so the request path yields
// before the background drain does, which is the right order: the drain is the
// thing built to absorb a backlog.
//
// WHAT HAPPENS TO THE WRITE THAT LOSES THE RACE decides whether this is a fix or
// a relocation of the failure, so it is stated here. On the request path, the
// error travels back through spoolingRecorder, which fsyncs the event to the
// local spool and logs AUDIT WRITE FAILED; the drain replays it once the lock
// clears. The event is not dropped - it takes exactly the degraded path C1 built
// for a failed durable write. On the broker's mint transaction the audit insert
// is in the same tx as the credential, so a timeout refuses the MINT: no
// credential is issued that could not be audited, which is the fail-closed
// direction a governance tool wants, and is what that path already did for every
// other audit failure.
//
// lock_timeout rather than a context deadline, verified rather than assumed: it
// fires ONLY on a lock wait, never on a slow-but-progressing statement (so the
// "too short" direction cannot abort work that was making progress), it is
// enforced server-side, SET LOCAL scopes it to the transaction so nothing leaks
// onto a pooled connection, and it reports the distinguishable SQLSTATE 55P03.
// Measured on Postgres 17: it bounds the explicit pg_advisory_xact_lock AND the
// trigger's own acquisition during an ordinary INSERT, so it also covers a
// writer that never takes the lock explicitly.
var AuditChainLockTimeout = 5 * time.Second

// AuditChainLockTimeoutSQL is the statement that applies AuditChainLockTimeout
// to the current transaction. SET takes no bind parameters, so the value is
// formatted in - it is an integer from the variable above, never caller input.
// SET LOCAL, so it reverts at commit or rollback and the pooled connection is
// handed back exactly as it was found.
func AuditChainLockTimeoutSQL() string {
	return fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", AuditChainLockTimeout.Milliseconds())
}

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
// something quieter: CREATE its own row-level BEFORE INSERT trigger, which is
// handed NEW and whose changes are what Postgres stores — so it can rewrite any
// field, choose prev_hash/row_hash, or drop the row entirely, minting records
// that say whatever it wants while every shipped guard stays armed and every
// catalog check still finds them. Name order is NOT what makes that work: a
// trigger sorting after audit_events_chain (same-event row triggers fire in name
// order) runs last and can overwrite the hashes directly, but one sorting BEFORE
// it is easier still — it rewrites NEW and the shipped chain trigger then hashes
// the forgery for it. ensureAuditTriggers refuses the boot over either, keyed on
// the trigger's SHAPE rather than its name; this function is the PREVENTIVE half
// and only asks whether the privilege to create one is held.
// 0007_audit_least_privilege.sql revokes
// TRIGGER from PUBLIC precisely because of that, but a deploy is free to grant
// it back, so the claim has to be checked and not inferred.
//
// ALL THREE LEGS TEST MEMBERSHIP, not a role attribute. The superuser leg asks
// whether current_user is a member of ANY role with rolsuper — not whether
// current_user itself has rolsuper. Reading the attribute off the current_user
// row missed the ordinary managed-Postgres shape (GRANT some admin role TO the
// app role): that role has rolsuper = false, is not a member of the table's
// owner, and holds no TRIGGER privilege, so it was reported PROTECTED while it
// could SET ROLE to a superuser and ALTER TABLE ... DISABLE TRIGGER. pg_has_role
// with 'MEMBER' is what makes this honest: 'MEMBER' is the right to SET ROLE, so
// it follows the grant chain to any depth AND ignores INHERIT — a NOINHERIT role
// that can still SET ROLE is caught. A role is a member of itself, so a directly
// superuser role is reported exactly as it was before.
//
// WHAT THIS DELIBERATELY DOES NOT MODEL, stated so the next reader does not
// widen it by guesswork. Membership in pg_write_all_data is NOT a bypass and is
// NOT tested for: it confers INSERT/UPDATE/DELETE rights, but the append-only
// triggers still fire and raise — measured in the probe beside this function,
// not assumed. Roles that own the HOST rather than the guard —
// pg_execute_server_program, pg_write_server_files — can escalate to superuser
// by documented PostgreSQL behaviour and are still reported protected here. The
// predicate stays a closed, catalog-derived test (rolsuper) rather than a list
// of role names that rots with every Postgres release: once the database host is
// compromised, no claim Wardyn makes about that database survives anyway.
func AuditDDLProtected(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var canBypass bool
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(
			bool_or(EXISTS (SELECT 1 FROM pg_roles s
			                 WHERE s.rolsuper
			                   AND pg_has_role(current_user, s.oid, 'MEMBER'))
			        OR pg_has_role(current_user, c.relowner, 'MEMBER')
			        OR has_table_privilege(current_user, c.oid, 'TRIGGER')),
			true)
		FROM pg_class c
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

	// Read the operator's ENABLE ALWAYS hardening BEFORE anything runs. Every
	// migration that (re)defines an audit trigger does so with DROP TRIGGER IF
	// EXISTS + CREATE TRIGGER, and CREATE TRIGGER always yields tgenabled='O' —
	// so the loop below, and the trigger-restore replay inside
	// ensureAuditTriggers, both silently revert 'A' back to 'O'. This is the
	// WRITE side of the invariant auditTriggerNames states on the READ side.
	hardened, err := auditAlwaysTriggers(ctx, db)
	if err != nil {
		return err
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
	if err := ensureAuditTriggers(ctx, db); err != nil {
		return err
	}
	// AFTER ensureAuditTriggers, not just after the loop: its restore path
	// replays the trigger-defining migrations, which re-creates the trigger as
	// plain 'O' for exactly the same reason the loop does.
	restoreAlwaysTriggers(ctx, db, hardened)
	return nil
}

// auditAlwaysTriggers returns the audit_events triggers an operator has hardened
// with ALTER TABLE ... ENABLE ALWAYS TRIGGER (pg_trigger.tgenabled = 'A'), or
// nil when the table does not exist yet (a database mid-bootstrap has none).
func auditAlwaysTriggers(ctx context.Context, db migrationExecutor) ([]string, error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil, nil
	}
	var names []string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(tgname::text ORDER BY tgname), ARRAY[]::text[])
		FROM pg_trigger
		WHERE tgrelid = 'audit_events'::regclass AND NOT tgisinternal AND tgenabled = 'A'`,
	).Scan(&names); err != nil {
		return nil, fmt.Errorf("db: read hardened audit_events triggers: %w", err)
	}
	return names, nil
}

// restoreAlwaysTriggers re-applies ENABLE ALWAYS to each trigger in want that is
// no longer 'A'. docs/OPERATIONS.md promises a hardened trigger is left "exactly
// as it is"; auditTriggerNames keeps that promise on the READ side by counting
// 'A' as firing, and this keeps it on the WRITE side, for the whole of Migrate.
// Without it the promise held only for a database with nothing left to apply:
// a 0.6.x deployment that had hardened the chain trigger lost the hardening the
// moment it upgraded, with nothing logged, and the next boot then read the
// resulting 'O' as the normal shipped state.
//
// Idempotent, and deliberately narrow: it re-reads the catalog and issues the
// ALTER only for a trigger that WAS 'A' and is not any more, so a run with
// nothing pending — or on a deployment that never hardened anything — touches
// nothing at all. A trigger nobody hardened is never promoted to 'A' by this.
//
// A failure is logged, not returned. Refusing the boot would not save the
// hardening: the migrations have already been applied and re-recorded, so the
// NEXT boot's capture reads the reverted 'O' and has nothing left to restore.
// An ERROR line naming the exact statement to re-run is the honest outcome, and
// it keeps the "never continue silently" property that ensureAuditTriggers has.
func restoreAlwaysTriggers(ctx context.Context, db migrationExecutor, want []string) {
	if len(want) == 0 {
		return
	}
	still, err := auditAlwaysTriggers(ctx, db)
	if err != nil {
		slog.ErrorContext(ctx, "db: cannot tell whether the migration loop reverted an ENABLE ALWAYS audit trigger; re-check pg_trigger.tgenabled by hand",
			slog.Any("error", err), slog.Any("hardened_before", want))
		return
	}
	always := make(map[string]bool, len(still))
	for _, n := range still {
		always[n] = true
	}
	for _, name := range want {
		if always[name] {
			continue // untouched by this run; nothing to re-apply
		}
		stmt := `ALTER TABLE audit_events ENABLE ALWAYS TRIGGER ` + pgx.Identifier{name}.Sanitize()
		if _, err := db.Exec(ctx, stmt); err != nil {
			slog.ErrorContext(ctx, "db: a migration reverted this trigger's ENABLE ALWAYS hardening and it could NOT be re-applied; it now fires only for ordinary writes, not under session_replication_role = replica — re-apply it by hand",
				slog.String("trigger", name), slog.String("statement", stmt), slog.Any("error", err))
			continue
		}
		slog.WarnContext(ctx, "db: re-applied the ENABLE ALWAYS hardening a migration reverted on an audit_events trigger",
			slog.String("trigger", name))
	}
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
	// The shipped guards being armed is not the same as nothing ELSE being
	// armed beside them. auditTriggerNames already read the complete list.
	tamperCapable, other, err := auditForeignTriggers(ctx, db)
	if err != nil {
		return err
	}
	if len(other) > 0 {
		slog.ErrorContext(ctx, "db: trigger(s) on audit_events that Wardyn does not ship; they cannot rewrite a row on the way in (only a row-level BEFORE INSERT trigger can) but nothing else in the system reports them — confirm they are yours",
			slog.Any("triggers", other))
	}
	if len(tamperCapable) > 0 {
		return fmt.Errorf("db: row-level BEFORE INSERT trigger(s) on audit_events that Wardyn does not ship (%s); such a trigger sees NEW and its changes are what Postgres stores, so it can rewrite or drop any audit row on the way in while every shipped guard stays armed and the verify sweep still reports clean — refusing to start", strings.Join(tamperCapable, ", "))
	}
	return nil
}

// auditForeignTriggers returns the FIRING, non-internal triggers on audit_events
// that Wardyn does not ship, split into the two classes that matter. Returns
// nothing when the table does not exist.
//
// tamperCapable is the class that defeats the whole audit design: a ROW-level
// BEFORE INSERT trigger. It is handed NEW and whatever it returns is what
// Postgres stores, so it can rewrite any field, choose prev_hash/row_hash, or
// RETURN NULL to make the event vanish — and the row it leaves behind is
// internally consistent, so store.VerifyAuditChain reports the log clean. This
// was measured, not assumed: with such a trigger installed, an event submitted
// through store.InsertAuditEvent as actor=X outcome=denied was stored as
// actor=Y outcome=success, InsertAuditEvent returned nil, this boot check
// returned nil, and the sweep returned ok=true.
//
// NAME ORDER IS NOT THE TEST, and reasoning that it is would have left the
// easier attack open. AuditDDLProtected's doc comment and docs/OPERATIONS.md
// both describe this bypass as a trigger sorting AFTER audit_events_chain
// (same-event row triggers fire in name order, so it runs last and overwrites
// the hashes). That is one way to do it. A trigger sorting BEFORE the chain
// trigger is strictly easier: it rewrites NEW and the SHIPPED chain trigger
// then hashes the forgery for it — no name trick, no hash call. Both were
// executed; both left the sweep reporting ok=true. So every foreign row-level
// BEFORE INSERT trigger is refused, whatever it is called.
//
// other is every remaining foreign trigger — AFTER, statement-level, or bound
// to another event. None of them can alter the stored row, so they are reported
// rather than refused: a deployment may legitimately hang a replication or
// notify trigger off this table, and bricking that boot would be a worse
// failure than naming it.
func auditForeignTriggers(ctx context.Context, db migrationExecutor) (tamperCapable, other []string, err error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, nil, fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil, nil, nil
	}
	shipped := append([]string{auditChainTrigger}, auditAppendOnlyTriggers...)
	// pg_trigger.tgtype is the bitmask from Postgres's own trigger.h:
	// 1 = FOR EACH ROW, 2 = BEFORE, 4 = INSERT. So (tgtype & 3) = 3 is a
	// row-level BEFORE trigger and (tgtype & 4) <> 0 means it fires on INSERT.
	const rowBeforeInsert = `(tgtype & 3) = 3 AND (tgtype & 4) <> 0`
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(tgname::text ORDER BY tgname) FILTER (WHERE `+rowBeforeInsert+`), ARRAY[]::text[]),
		       COALESCE(array_agg(tgname::text ORDER BY tgname) FILTER (WHERE NOT (`+rowBeforeInsert+`)), ARRAY[]::text[])
		FROM pg_trigger
		WHERE tgrelid = 'audit_events'::regclass
		  AND NOT tgisinternal
		  AND tgenabled IN ('O', 'A')
		  AND tgname <> ALL($1)`, shipped,
	).Scan(&tamperCapable, &other); err != nil {
		return nil, nil, fmt.Errorf("db: read foreign audit_events triggers: %w", err)
	}
	return tamperCapable, other, nil
}

// auditTriggerNames returns the FIRING row/statement triggers on audit_events,
// or nil when the table does not exist. Disabled is treated as absent on
// purpose: ALTER TABLE ... DISABLE TRIGGER leaves the catalog row in place, so a
// check that only asked whether the trigger EXISTS would pass on a table where
// it never fires.
//
// Firing is tgenabled 'O' (origin, the shipped state) OR 'A' (ALWAYS). 'A' is a
// HARDENING, not a deviation: an ALWAYS trigger fires even under
// session_replication_role = replica, which is exactly the bypass the sweep's
// rule 3 exists to catch after the fact (store.auditChainWalk) — an operator who
// applies it is closing that hole at the source. Reading 'A' as absent would
// have made the next boot log the chain trigger as missing, DROP and re-create
// it as plain 'O' (silently reverting the hardening), and an ALWAYS append-only
// trigger would have made Migrate refuse the boot outright — bricking the
// upgrade of the most careful deployments. 'D' (disabled) and 'R' (replica-only,
// which does NOT fire for ordinary writes) stay absent, correctly.
//
// This is only the READ half of what 'A' means. Reading it as firing is not
// enough on its own: every migration that (re)defines an audit trigger ends in
// CREATE TRIGGER, which always yields 'O', so the migration loop reverted the
// hardening this function is careful not to punish. restoreAlwaysTriggers is the
// WRITE half, and the two must keep agreeing — 'A' is a hardening to be
// preserved, never a deviation to be normalised.
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
		WHERE tgrelid = 'audit_events'::regclass AND NOT tgisinternal AND tgenabled IN ('O', 'A')`,
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
	names, err := triggerMigrationFiles(trigger)
	if err != nil {
		return err
	}
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

// triggerMigrationFiles returns, in apply order, the embedded migrations whose
// text defines trigger — the REPLAY SET that replayTriggerMigrations re-executes
// verbatim, against a database where all of them are already applied and none is
// re-recorded in schema_migrations.
//
// Exists as its own function so the guard that keeps those files idempotent is
// derived from the SAME predicate the replay uses instead of restating it. A
// test that re-implemented the rule would be right until the day the rule
// changed, and the failure that day is a boot refusing on exactly the database
// whose audit trigger already went missing — the case the replay exists to
// rescue. Content-derived rather than listed for the same reason
// replayTriggerMigrations was: a later migration that redefines the trigger
// joins the set on its own, and 0058 did.
func triggerMigrationFiles(trigger string) ([]string, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("db: read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("db: read migration %s: %w", e.Name(), err)
		}
		if strings.Contains(string(body), "TRIGGER "+trigger) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
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
