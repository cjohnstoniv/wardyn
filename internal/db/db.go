// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package db provides Postgres connection bootstrapping and schema migration
// for the Wardyn control plane. Postgres is the ONLY required dependency.
package db

import (
	"context"
	"embed"
	"errors"
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

// SingleInstanceLockKey is the RUNTIME half of the one-replica safety control
// (the Helm chart's `replicas > 1` render refusal is the other half). wardynd
// takes it ONCE at boot, with TryAdvisoryLock, and holds it for the process
// lifetime: a second instance against the same database refuses to start
// instead of quietly serving.
//
// WHY IT IS A SAFETY CONTROL, not modesty: wardynd keeps state per-process that
// a second instance cannot see, the sharpest being the secret-masking registry
// (internal/secretmask) — an in-memory, process-local map that FAILS OPEN.
// Secrets are registered on whichever instance served the run's proxy
// injection; a session recording uploaded to any other instance finds an empty
// snapshot and is persisted VERBATIM, live credentials in cleartext, with a
// `success` audit event. The chart's refusal is render-time only, so
// `kubectl scale`, an HPA, or a non-Helm replica edit defeated it silently.
//
// HONEST CEILING — AT MOST ONE STEADY-STATE INSTANCE, NOT MUTUAL EXCLUSION,
// exactly as GroundTruthRotatorLockKey documents for the same mechanism. An
// advisory lock dies with its SESSION, not with the process, and the holder
// never re-verifies it: a Postgres restart, a failover, pg_terminate_backend or
// an idle-session timeout releases it under a still-running daemon, and the
// next instance to boot takes it — two daemons, neither aware. It closes the
// silent-scale hole; it is not a fence. Any stable value works, as long as it
// differs from every other key in this file.
const SingleInstanceLockKey int64 = 0x5741524459_494E53 // ASCII "WARDYINS"

// SecretRekeyLockKey serializes the `wardynd -rotate-age-key` maintenance mode
// (cmd/wardynd's rotateAgeKeyMode): two concurrent rekeys of the same store
// would each re-encrypt from an old key the other has already replaced, so the
// second is refused rather than queued (TryAdvisoryLock, like
// ReaperAdvisoryLockKey).
//
// HONEST CEILING — this does NOT detect a running wardynd. No wardynd holds a
// process-lifetime lock on THIS key: a serving daemon holds
// SingleInstanceLockKey (a different key), the reaper takes
// ReaperAdvisoryLockKey per tick and releases it, and GroundTruthRotatorLockKey
// is only taken when the rotator is configured — so a serving daemon is
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
// UNABLE to bypass the audit_events append-only triggers — i.e. it is neither a
// superuser nor a MEMBER of the table's owner role (membership, not just direct
// ownership: a role GRANTed the owner role inherits DROP TRIGGER /
// ALTER ... DISABLE TRIGGER rights), it does not hold the TRIGGER privilege on
// the table, AND it cannot SET session_replication_role, which silences every
// simply-enabled trigger without touching DDL at all (the fourth leg, below). The N4 role-separation only protects the append-only guarantee
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
// ALL THREE ROLE LEGS TEST MEMBERSHIP, not a role attribute. The superuser leg asks
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
	if canBypass {
		return false, nil // already answered; the fourth leg cannot make it more false
	}
	// THE FOURTH LEG, AND IT IS NOT A DDL ONE. The three above ask who can DROP
	// or DISABLE a trigger. `SET session_replication_role = 'replica'` needs no
	// DDL at all: it makes every SIMPLY-ENABLED ('O') trigger stop firing for the
	// session, so a role holding nothing but INSERT appends rows past all three
	// audit guards — unchained, unrewritten by the chain trigger, and the boot
	// meanwhile logged the deployment as DDL-protected. Executed in the probe
	// beside this function, not assumed: row_hash came back NULL. It is also the
	// reason ENABLE ALWAYS is the documented hardening — an 'A' trigger fires
	// regardless of replication role — and why auditForeignTriggers now reads
	// tgenabled 'R' as armed.
	//
	// GRANTABLE SINCE POSTGRESQL 15, which is what makes it a leg rather than a
	// restatement of the superuser one. GRANT SET ON PARAMETER
	// session_replication_role TO app is exactly the narrow grant a DBA hands an
	// application role for a bulk load, and it survives as a standing capability.
	//
	// SEPARATE QUERY, AND VERSION-GUARDED, deliberately. has_parameter_privilege
	// does not exist before PostgreSQL 15, and a missing function is a PARSE
	// error — it would fail even inside an untaken CASE branch — so folding this
	// into the query above would turn every pre-15 split-role boot into a hard
	// refusal (cmd/wardynd treats an error here as fatal). Skipping the leg there
	// is not a gap but the correct answer: parameter-level GRANT did not exist
	// before 15 either, so on those servers only a superuser can set the GUC, and
	// the first leg already covers that.
	var granular bool
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int >= 150000`).Scan(&granular); err != nil {
		return false, fmt.Errorf("db: read server version for the session_replication_role check: %w", err)
	}
	if !granular {
		return true, nil
	}
	var canSilenceTriggers bool
	if err := pool.QueryRow(ctx,
		`SELECT has_parameter_privilege(current_user, 'session_replication_role', 'SET')`,
	).Scan(&canSilenceTriggers); err != nil {
		return false, fmt.Errorf("db: check session_replication_role privilege: %w", err)
	}
	return !canSilenceTriggers, nil
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
	// DEFERRED, not called on the success path, and the difference is the whole
	// promise. A migration that FAILS is not an exotic state here: 0059 and 0060
	// both fail loudly by design and nominate "fix the rows and re-run" as the
	// supported response, and 0056-0058 sit BEFORE them in apply order — so by
	// the time the loop returns an error, three committed DROP TRIGGER + CREATE
	// TRIGGER pairs have already put tgenabled back to 'O'. Returning there left
	// the hardening stripped with nothing logged, and stripped FOR GOOD: the
	// remediated boot's capture reads that 'O' as the shipped state and finds
	// 0056-0058 already recorded applied, so there is nothing left to restore
	// and nothing left to notice. Deferring it here covers every exit — a loop
	// error, an ensureAuditTriggers refusal, a cancelled ctx — with the same
	// re-apply-or-say-so the success path always had. It still runs LAST, after
	// ensureAuditTriggers, which is what the tail call was careful about: that
	// function's restore path replays the trigger-defining migrations and
	// re-creates the trigger as plain 'O' for the same reason the loop does.
	defer restoreAlwaysTriggers(ctx, db, hardened)

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
	// The hardening restore is the deferred call registered above, so it runs
	// after this returns however it returns.
	return auditChainCanary(ctx, db)
}

// auditChainCanary appends ONE synthetic audit row inside a transaction it
// always rolls back, and asserts the row came out chained: row_hash set, and
// prev_hash equal to the head read under the same lock. It is the FUNCTIONAL
// half of the boot check.
//
// WHY CATALOG SHAPE IS NOT ENOUGH, and the release has the receipt. Everything
// above this asks the catalog: is the trigger there, is it enabled, is it the
// function we ship. 0057 shipped a trigger that was all three and did not work
// — a SECURITY DEFINER pinned to `pg_catalog, public` while its body named
// audit_events unqualified, so on a deployment whose objects live in another
// schema Migrate reported success and then EVERY audit insert failed from inside
// the trigger, or (quieter and worse, where public held a second Wardyn schema)
// linked the chain to the wrong table's head and never linked at all. 0058
// repaired that particular body; nothing made the failure CLASS visible at boot.
// A single round trip does, and it converts the class from post-hoc to
// boot-time.
//
// NEVER COMMITTED, so there is no synthetic row in anybody's audit log and no
// question about what an operator is looking at. The cost is one burned seq per
// boot — which the sweep already tolerates by design: auditChainWalk's own
// doc records that seq gaps below the chain come from rolled-back inserts and
// that seq is not hashed.
//
// WHAT REFUSES AND WHAT ONLY REPORTS. A chain that demonstrably does not chain
// — the insert succeeded and the row came back with no row_hash, or with a
// prev_hash that is not the head — refuses the boot: that is the 0057 state, and
// a wardynd serving over it writes an audit log the verify sweep will report as
// broken for as long as it runs. A canary that could not be RUN because the
// chain lock was busy or the statement was cancelled reports at ERROR and lets
// the boot continue: those are bounded, transient and self-clearing (the whole
// subject of AuditChainLockTimeout), and bricking a boot over one is a failure
// this check would cause rather than one it would find.
func auditChainCanary(ctx context.Context, db migrationExecutor) error {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin audit chain canary: %w", err)
	}
	// Background context, and it is the ONE thing this function must not fail to
	// do: a cancelled ctx must still roll the canary row back.
	defer tx.Rollback(context.Background()) //nolint:errcheck — the canary is never committed

	// Same bound and same lock the real writers take, so the canary queues
	// behind a busy chain instead of waiting for a boot timeout.
	if _, err := tx.Exec(ctx, AuditChainLockTimeoutSQL()); err != nil {
		return auditCanaryTransient(ctx, "bound the canary's chain lock wait", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, AuditChainLockKey); err != nil {
		return auditCanaryTransient(ctx, "take the chain lock for the canary", err)
	}
	var head string
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT row_hash FROM audit_events
		WHERE row_hash IS NOT NULL ORDER BY seq DESC LIMIT 1), '')`).Scan(&head); err != nil {
		return auditCanaryTransient(ctx, "read the chain head for the canary", err)
	}
	var rowHash, prevHash string
	if err := tx.QueryRow(ctx, `
		INSERT INTO audit_events (id, actor_type, actor, action, target, outcome)
		VALUES (gen_random_uuid(), 'system', 'wardynd', 'audit.chain.canary', 'audit_events', 'success')
		RETURNING COALESCE(row_hash, ''), COALESCE(prev_hash, '')`).Scan(&rowHash, &prevHash); err != nil {
		return fmt.Errorf("db: the audit chain canary could not append a row, so every audit write this process makes "+
			"will fail the same way — the chain trigger is catalogued and enabled but not working: %w", err)
	}
	if rowHash == "" {
		return fmt.Errorf("db: the audit chain canary appended a row with NO row_hash: the %s trigger is present, "+
			"enabled and correctly named, and it is not chaining — every row written by this process would be "+
			"unchained, which the verify sweep reports as a break; refusing to start", auditChainTrigger)
	}
	if prevHash != head {
		return fmt.Errorf("db: the audit chain canary linked to %q but the chain head is %q: the %s trigger's head read "+
			"is resolving somewhere other than the table it is attached to, so the chain would never link; "+
			"refusing to start", prevHash, head, auditChainTrigger)
	}
	return nil
}

// auditCanaryTransient decides the one direction this check must not get wrong:
// a lock wait that timed out (SQLSTATE 55P03) or a statement that was cancelled
// (57014) says nothing about whether the chain works, so it is reported and the
// boot continues. Anything else is returned and refuses.
func auditCanaryTransient(ctx context.Context, what string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "57014") {
		slog.ErrorContext(ctx, "db: could not run the boot audit-chain canary; the chain trigger's catalog shape was verified but its BEHAVIOUR was not - re-run the verify sweep once the database is quiet",
			slog.String("step", what), slog.Any("error", err))
		return nil
	}
	return fmt.Errorf("db: audit chain canary: %s: %w", what, err)
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

// auditAppendOnlyFunc is the function 0001 and 0004 attach to BOTH append-only
// triggers. The chain trigger's function is named after the trigger itself
// (0047/0056/0057/0058), so auditChainTrigger doubles as its proname.
const auditAppendOnlyFunc = "audit_events_append_only"

// auditShippedTriggerFuncs maps each shipped trigger to the function the
// migrations bind it to. Derived from the same constants the checks use, so a
// renamed trigger cannot leave a stale expectation behind here.
func auditShippedTriggerFuncs() map[string]string {
	m := map[string]string{auditChainTrigger: auditChainTrigger}
	for _, n := range auditAppendOnlyTriggers {
		m[n] = auditAppendOnlyFunc
	}
	return m
}

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
	// A NAME IS NOT AN IDENTITY, and every check above this line was keyed on
	// one. auditTriggerNames reports a trigger PRESENT when the catalog holds
	// its name; auditForeignTriggers excludes the three shipped names from the
	// foreign set by that same name. So the one shape neither can see is a
	// trigger WEARING a shipped name over a body nobody shipped —
	// DROP TRIGGER audit_events_chain, then CREATE TRIGGER audit_events_chain
	// ... EXECUTE FUNCTION somebody_elses_function(). That is the quiet bypass
	// in its strongest form: the boot log is clean, every catalog check passes,
	// and the trigger the whole audit design rests on is the forger's. The
	// append-only arm is the same hole with a worse ending — a function that
	// returns NEW puts UPDATE and DELETE back on the audit log while this check
	// reports the guarantee in force.
	impostors, err := auditImpostorTriggers(ctx, db)
	if err != nil {
		return err
	}
	if fn := impostors[auditChainTrigger]; fn != "" {
		slog.ErrorContext(ctx, "db: the audit_events hash-chain trigger executes a function Wardyn does not ship; restoring it — whatever it wrote is what that function decided, so treat rows written since as unverified even where the chain verifies",
			slog.String("trigger", auditChainTrigger), slog.String("function", fn))
		if err := replayTriggerMigrations(ctx, db, auditChainTrigger); err != nil {
			return err
		}
		if impostors, err = auditImpostorTriggers(ctx, db); err != nil {
			return err
		}
		if fn := impostors[auditChainTrigger]; fn != "" {
			return fmt.Errorf("db: %s trigger executes %s rather than the function Wardyn ships and could not be restored; "+
				"refusing to run with an audit log a foreign trigger writes", auditChainTrigger, fn)
		}
		if present, err = auditTriggerNames(ctx, db); err != nil {
			return err
		}
		slog.WarnContext(ctx, "db: audit_events hash-chain trigger restored over a foreign function",
			slog.String("trigger", auditChainTrigger))
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
		if fn := impostors[name]; fn != "" {
			return fmt.Errorf("db: %s trigger on audit_events executes %s rather than the %s function Wardyn ships, so the "+
				"append-only guarantee is not in force; restoring it means replaying the initial schema — refusing to start",
				name, fn, auditAppendOnlyFunc)
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
	reportTransactionIsolation(ctx, db)
	if len(tamperCapable) > 0 {
		// Wrapped only to satisfy lll; the sentence is the operator's whole
		// explanation of why a boot refusal is the proportionate response, so
		// it is split rather than shortened.
		return fmt.Errorf("db: row-level BEFORE INSERT trigger(s) on audit_events that Wardyn does not ship (%s); "+
			"such a trigger sees NEW and its changes are what Postgres stores, so it can rewrite or drop any "+
			"audit row on the way in while every shipped guard stays armed and the verify sweep still reports "+
			"clean — refusing to start", strings.Join(tamperCapable, ", "))
	}
	return nil
}

// reportTransactionIsolation names a default_transaction_isolation that is not
// READ COMMITTED, at ERROR, with the statement to fix it — the same "never
// continue silently" treatment a missing guard gets.
//
// WHY THE CHAIN CARES AT ALL. Since 0056 the head read that decides prev_hash
// runs INSIDE the trigger, i.e. inside the inserting transaction. Under
// REPEATABLE READ that read uses the transaction's snapshot, which the advisory
// -lock statement takes BEFORE the lock is granted — so a writer that queued
// behind the lock reads a head from before the winner committed and chains to
// it. Two rows claim one predecessor and the verify sweep reports a break.
// default_transaction_isolation is a USERSET GUC: any role can set it, per role
// or per database, with no superuser involved.
//
// REPORT, NOT REFUSE, and the two halves are deliberate. Wardyn's OWN writers no
// longer depend on it — store.InsertAuditEvent and the broker's mint transaction
// pin pgx.ReadCommitted on their Begin, and a transaction-level isolation level
// overrides the GUC — so this is about writers this package knows nothing about,
// which is a deployment posture to report rather than a defect to refuse over.
// It is also read on whichever connection Migrate holds: in a split-DSN deploy
// that is the MIGRATE role, and the app role may carry a different ALTER ROLE
// setting, so a clean line here is not a promise about the serving pool. Said
// plainly so nobody reads it as one.
func reportTransactionIsolation(ctx context.Context, db migrationExecutor) {
	var iso string
	if err := db.QueryRow(ctx, `SELECT current_setting('default_transaction_isolation')`).Scan(&iso); err != nil {
		slog.ErrorContext(ctx, "db: cannot read default_transaction_isolation; a level other than read committed forks the audit chain for writers that do not pin their own",
			slog.Any("error", err))
		return
	}
	if strings.EqualFold(strings.TrimSpace(iso), "read committed") {
		return
	}
	slog.ErrorContext(ctx, "db: default_transaction_isolation is not read committed; Wardyn's own audit writers pin READ COMMITTED per transaction and are unaffected, but any OTHER writer to audit_events inheriting this default can chain to a stale head and the verify sweep will report the result as a break - fix with ALTER DATABASE ... SET default_transaction_isolation = 'read committed' (or the matching ALTER ROLE)",
		slog.String("default_transaction_isolation", iso),
		slog.String("statement", "ALTER DATABASE <db> SET default_transaction_isolation = 'read committed'"))
}

// auditImpostorTriggers returns the SHIPPED-NAMED triggers on audit_events that
// are bound to something other than the function the migrations bind them to,
// keyed by trigger name with the offending function as `<schema>.<name>` so the
// operator can find it. Empty when every shipped trigger is the one Wardyn
// created, and when the table does not exist.
//
// THE SCHEMA IS PART OF THE COMPARISON, not decoration in the message. Matching
// on proname alone would accept a forger's own `audit_events_chain()` created in
// a schema earlier on the search_path — the same shadowing 0058 exists to stop
// on the resolution side, arriving here through the catalog instead. The
// shipped function always lives in the schema the table does (0001 creates it
// unqualified alongside audit_events; 0058 creates it as `<schema>.
// audit_events_chain` from the table's own namespace), so that is the identity
// tested.
//
// WHAT THIS DOES NOT CATCH, stated so nobody reads it as more than it is: the
// shipped function's BODY, replaced in place with CREATE OR REPLACE FUNCTION.
// The name and the schema still match and the catalog looks identical. That is a
// behavioural question, not a catalog one, and it is what the boot canary answers
// — a rolled-back synthetic append asserting the row actually chains.
func auditImpostorTriggers(ctx context.Context, db migrationExecutor) (map[string]string, error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil, nil
	}
	want := auditShippedTriggerFuncs()
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)

	// One read, decided in Go: the catalog answers "what does each shipped
	// trigger execute, and where does that function live"; the expectation is
	// the map above, so the two cannot be restated differently in SQL and Go.
	var tgnames, funcs, funcSchemas, tableSchemas []string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(t.tgname::text   ORDER BY t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(p.proname::text  ORDER BY t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(fn.nspname::text ORDER BY t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(tn.nspname::text ORDER BY t.tgname), ARRAY[]::text[])
		FROM pg_trigger t
		JOIN pg_proc p       ON p.oid  = t.tgfoid
		JOIN pg_namespace fn ON fn.oid = p.pronamespace
		JOIN pg_class c      ON c.oid  = t.tgrelid
		JOIN pg_namespace tn ON tn.oid = c.relnamespace
		WHERE t.tgrelid = 'audit_events'::regclass
		  AND NOT t.tgisinternal
		  AND t.tgname = ANY($1)`, names,
	).Scan(&tgnames, &funcs, &funcSchemas, &tableSchemas); err != nil {
		return nil, fmt.Errorf("db: read audit_events trigger functions: %w", err)
	}
	out := make(map[string]string)
	for i, n := range tgnames {
		if i >= len(funcs) || i >= len(funcSchemas) || i >= len(tableSchemas) {
			break
		}
		if funcs[i] == want[n] && funcSchemas[i] == tableSchemas[i] {
			continue
		}
		out[n] = funcSchemas[i] + "." + funcs[i]
	}
	return out, nil
}

// auditForeignTriggers returns the non-internal triggers on audit_events that
// Wardyn does not ship and that are not DISABLED, split into the two classes
// that matter. Returns nothing when the table does not exist.
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
//
// TGENABLED IS NOT auditTriggerNames' FILTER, and reusing that one left the
// state that matters most invisible. auditTriggerNames asks whether one of
// WARDYN'S OWN triggers is firing for ORDINARY writes, so it reads 'O' and 'A'
// and correctly treats 'R' (replica-only) as absent. Asking the same question of
// a FOREIGN trigger inverts the answer: 'R' means dormant for ordinary traffic
// and ARMED for exactly the `session_replication_role = replica` session the
// whole audit design names as the bypass window (docs/OPERATIONS.md,
// AuditDDLProtected's doc comment, the sweep's rule 3 in store.auditChainWalk).
// A forging row-level BEFORE INSERT trigger parked at 'R' therefore passed this
// refusal outright — and with the shipped chain trigger hardened to ENABLE
// ALWAYS, the hardening this file goes out of its way to preserve and which
// fires under replica too, the forged row was hash-chained on the way in: actor
// and outcome the forger's, row_hash present and valid, VerifyAuditChain
// reporting the log clean. So the predicate is stated in ITS OWN terms rather
// than borrowed: any state except 'D'.
//
// 'D' STAYS OUT, deliberately and narrowly. A disabled trigger fires for
// nothing at all, so a catalog row parked at 'D' cannot rewrite a row; arming it
// is an ALTER TABLE ... ENABLE, which needs the very TRIGGER privilege
// AuditDDLProtected exists to report on, and a boot that refused over a trigger
// somebody had neutralised the supported way would fail in the wrong direction.
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
		  AND tgenabled <> 'D'
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
