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
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationExecutor is the subset of *pgxpool.Pool / *pgxpool.Conn the migration steps need. Migrate
// runs ALL of them on the SINGLE advisory-lock-holding connection, so a pool_max_conns=1 DSN can't
// self-deadlock against the held lock conn.
type migrationExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

//go:embed migrations/*.sql
var migrationFS embed.FS

// retiredMigrations is the closed set of RELEASED migration filenames that a later commit renamed
// or retired without changing what they applied — so a database migrated by the old name is not "a
// newer wardynd migrated it", it is this exact schema under a name this tree no longer ships.
//
// v0.7.12 shipped 0065_secret_envelope_v1.sql; main renumbered it to 0069_secret_envelope_v1.sql
// (byte-identical) to make room for migrations added after the 0.7 branch point, so every 0.7.12
// database carries a schema_migrations row this binary doesn't ship under that name, and
// unknownAppliedMigrations must not read that as a downgrade.
//
// Built from evidence, not memory: for every released v0.7.x tag, comparing that tag's
// migrations/ against this tree's finds exactly this one name absent, pinned by
// TestRetiredMigrationsCoverEveryReleasedName. A migration renamed again later adds a second entry
// here; it never needs one removed.
var retiredMigrations = map[string]string{
	"0065_secret_envelope_v1.sql": "0069_secret_envelope_v1.sql",
}

// DefaultPoolMaxConns is the pool size when the DSN leaves pool_max_conns
// unset, or the CPU count if that is larger. pgx's own default is the larger
// of 4 and the CPU count, but on a 4-CPU host the single-instance lock, the
// sweeper leader and the ground-truth rotator hold three connections for the
// process lifetime, so the reaper's tick lock took the fourth and waited
// forever for a fifth to prune, and every request after it hung.
const DefaultPoolMaxConns = 10

// Connect opens a pgxpool to dsn and performs a lightweight liveness check.
// Returns the pool; caller owns Close().
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if !dsnSetsPoolMaxConns(dsn) {
		cfg.MaxConns = max(cfg.MaxConns, DefaultPoolMaxConns)
	}
	if nestedAcquireGuardOn() {
		cfg.ConnConfig.Tracer = newNestedAcquireGuard()
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// dsnSetsPoolMaxConns reports whether the operator sized the pool. pgx reads
// pool_max_conns from the connection's runtime parameters and then deletes it.
func dsnSetsPoolMaxConns(dsn string) bool {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return false
	}
	_, ok := cfg.RuntimeParams["pool_max_conns"]
	return ok
}

// Migrate applies all migrations in internal/db/migrations/*.sql in lexical order. Each migration
// runs inside its own transaction; already-applied filenames are skipped. Idempotent. It REFUSES a
// database that records an applied migration this binary does not ship: see unknownAppliedMigrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrate(ctx, pool, false)
}

// MigrateAllowingUnknown is Migrate with the unknown-migration refusal turned into a WARN — the
// break-glass behind WARDYN_ALLOW_UNKNOWN_MIGRATIONS.
func MigrateAllowingUnknown(ctx context.Context, pool *pgxpool.Pool) error {
	return migrate(ctx, pool, true)
}

func migrate(ctx context.Context, pool *pgxpool.Pool, allowUnknown bool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db: acquire migration lock conn: %w", err)
	}
	defer conn.Release()
	return migrateConn(ctx, conn, allowUnknown)
}

// MigrateConn is Migrate on a connection the caller already holds, for the one caller that must
// keep every statement of the migration on the session that owns a lock it took first
// (wardynd -migrate-only). Same refusals and the same migration lock as Migrate; the caller still
// owns conn and releases it.
func MigrateConn(ctx context.Context, conn *pgxpool.Conn) error {
	return migrateConn(ctx, conn, false)
}

func migrateConn(ctx context.Context, conn *pgxpool.Conn, allowUnknown bool) error {
	// Serialize concurrent boots: a session-level advisory lock on a SINGLE dedicated pooled
	// connection (lock + unlock must hit the same session) so a second wardynd blocks here.
	// Registered BEFORE acquiring, on a background context, so the lock releases even if ctx is
	// cancelled at the instant the server grants it; unlocking a non-held lock is a harmless no-op.
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrateAdvisoryLockKey) //nolint:errcheck // best-effort release
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateAdvisoryLockKey); err != nil {
		return fmt.Errorf("db: acquire migration advisory lock: %w", err)
	}

	// Runs on `conn`, not `pool`, so a pool_max_conns=1 DSN can't self-deadlock against this conn.
	return migrateOn(ctx, conn, allowUnknown)
}

// migrateOn applies pending migrations using a single executor (the advisory-
// lock-holding connection). Separated so both the lock path and tests share it.
func migrateOn(ctx context.Context, db migrationExecutor, allowUnknown bool) error {
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

	// Before anything is written: check whether a newer wardynd migrated this database, or it was
	// migrated under a retired name (retiredMigrations), which is not a downgrade at all.
	shipped := append(slices.Clone(names), slices.Sorted(maps.Keys(retiredMigrations))...)
	unknown, err := unknownAppliedMigrations(ctx, db, shipped)
	if err != nil {
		return err
	}
	if len(unknown) > 0 {
		newest := unknown[len(unknown)-1]
		if !allowUnknown {
			return fmt.Errorf("db: this database records %d migration(s) this wardynd does not ship, newest %q — "+
				"it was migrated by a wardynd this binary cannot run under; restore the pre-upgrade dump or run "+
				"the newer release (break-glass: WARDYN_ALLOW_UNKNOWN_MIGRATIONS=true)", len(unknown), newest)
		}
		slog.WarnContext(ctx, "db: WARDYN_ALLOW_UNKNOWN_MIGRATIONS is set — booting an older wardynd against a schema a newer one migrated; "+
			"its conversions are one-way and this binary does not know what they mean",
			slog.String("newest_unknown", newest), slog.Any("unknown_migrations", unknown))
	}

	// Read the operator's ENABLE ALWAYS hardening BEFORE anything runs. Every migration that
	// (re)defines an audit trigger does DROP TRIGGER IF EXISTS + CREATE TRIGGER, and CREATE TRIGGER
	// always yields tgenabled='O', so the loop below silently reverts 'A' back to 'O'.
	hardened, err := auditAlwaysTriggers(ctx, db)
	if err != nil {
		return err
	}
	// DEFERRED, not called on the success path: some migrations fail loudly by design ("fix the rows
	// and re-run") after earlier ones already committed DROP+CREATE TRIGGER pairs that reverted
	// tgenabled to 'O'. Returning there would strip the hardening for GOOD, since the next boot's
	// capture would read 'O' as the shipped state with those migrations already recorded applied.
	// Deferring covers every exit with the same re-apply-or-say-so the success path has, and runs
	// LAST, after ensureAuditTriggers (whose own restore path reverts the trigger the same way).
	//
	// Runs on a context the caller cannot cancel: a deadline firing mid-loop is designed-for, and the
	// same expired ctx would make the restore's statements fail the same way. context.WithoutCancel
	// keeps the caller's values but drops cancellation; the fresh deadline keeps a wedged server from
	// turning a best-effort restore into a boot that never returns.
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreHardeningTimeout)
		defer cancel()
		restoreAlwaysTriggers(rctx, db, hardened)
	}()

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

		// Log elapsed time per migration so a slow one is VISIBLE in the boot log before the
		// caller's timeout turns it fatal, rather than the boot going silent until then.
		start := time.Now()
		if err := applyMigration(ctx, db, name, string(data)); err != nil {
			return err
		}
		slog.InfoContext(ctx, "db: applied migration", slog.String("file", name), slog.Duration("elapsed", time.Since(start)))
	}
	if err := ensureAuditTriggers(ctx, db); err != nil {
		return err
	}
	// Months ahead, at every boot and before the listener: an insert into a month with no
	// partition fails and sits in the audit spool until one exists.
	if err := ensureAuditPartitions(ctx, db); err != nil {
		return err
	}
	// The hardening restore is the deferred call registered above; it runs after this returns.
	return auditChainCanary(ctx, db)
}

// auditChainCanary appends ONE synthetic audit row inside a transaction it always rolls back, and
// asserts it came out chained: row_hash set, prev_hash equal to the head read under the same lock.
// It is the FUNCTIONAL half of the boot check — catalog shape (trigger present, enabled, shipped
// function) is not enough: 0057 shipped a trigger that was all three and did not work (a SECURITY
// DEFINER pinned to the wrong search_path), so Migrate reported success while every audit insert
// then failed or silently linked to the wrong table's head. Never committed, so there is no
// synthetic row in anybody's audit log — cost is one burned seq per boot, which the sweep tolerates.
//
// A chain that demonstrably does not chain refuses the boot, since serving over it writes a log the
// verify sweep will report broken for as long as it runs. A canary that could not be RUN (chain lock
// busy, statement cancelled) reports at ERROR and lets the boot continue — those are bounded,
// transient and self-clearing.
//
// AuditChainCanary runs the same check against an arbitrary pool, for the caller that needs it on a
// pool Migrate never touched: with a split migrate/app DSN, real audit writes go through a different
// role with different search_path/privileges, so a canary that only ever ran as the migrator would
// prove nothing about the pool that actually writes audit rows. Same function, not a second copy, so
// the two boot paths can't disagree about what a working chain is.
func AuditChainCanary(ctx context.Context, pool *pgxpool.Pool) error {
	return auditChainCanary(ctx, pool)
}

func auditChainCanary(ctx context.Context, db migrationExecutor) error {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil
	}
	tx, err := beginReadCommitted(ctx, db)
	if err != nil {
		return fmt.Errorf("db: begin audit chain canary: %w", err)
	}
	// Background context: a cancelled ctx must still roll the canary row back.
	defer tx.Rollback(context.Background()) //nolint:errcheck // the canary is never committed

	// Same bound and same lock the real writers take, so the canary queues behind a busy chain
	// instead of waiting for a boot timeout.
	if _, err := tx.Exec(ctx, AuditChainLockTimeoutSQL()); err != nil {
		return auditCanaryTransient(ctx, "bound the canary's chain lock wait", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, AuditChainLockKey); err != nil {
		return auditCanaryTransient(ctx, "take the chain lock for the canary", err)
	}
	// The head the trigger links to since 0130: the recorded high-water hash, else the newest hashed
	// row. Read the same way, so a row removed from the tail is not mistaken for a mis-bound trigger.
	// The meta table is looked up first because the canary also runs on pre-0111 fixtures, and a
	// query that names a missing table fails at parse time whatever its WHERE says.
	var hasMeta bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('audit_partition_meta') IS NOT NULL`).Scan(&hasMeta); err != nil {
		return auditCanaryTransient(ctx, "look up audit_partition_meta for the canary", err)
	}
	headSQL := `SELECT COALESCE((SELECT row_hash FROM audit_events
		WHERE row_hash IS NOT NULL ORDER BY seq DESC LIMIT 1), '')`
	if hasMeta {
		headSQL = `SELECT COALESCE((SELECT hw_row_hash FROM audit_partition_meta),
			(SELECT row_hash FROM audit_events WHERE row_hash IS NOT NULL ORDER BY seq DESC LIMIT 1), '')`
	}
	var head string
	if err := tx.QueryRow(ctx, headSQL).Scan(&head); err != nil {
		return auditCanaryTransient(ctx, "read the chain head for the canary", err)
	}
	// Through audit_append, the only way a row enters the log since 0111, so the canary on the
	// serving pool proves the path real writes take (EXECUTE on the function included).
	var rowHash, prevHash string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(row_hash, ''), COALESCE(prev_hash, '')
		  FROM audit_append(gen_random_uuid(), now(), NULL, 'system', 'wardynd', 'audit.chain.canary', 'audit_events', 'success', '', NULL)`).Scan(&rowHash, &prevHash); err != nil {
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

// AuditPartitionMonthsAhead is how many months past the current one audit_ensure_partitions keeps
// created. A constant, not a setting: the function itself refuses anything outside 1..24, and the
// boot and the daily sweeper both pass this.
const AuditPartitionMonthsAhead = 12

// ensureAuditPartitions runs audit_ensure_partitions on the migration connection. It continues from
// the highest existing upper bound, so a deployment that sat offline for months catches up in one call.
func ensureAuditPartitions(ctx context.Context, db migrationExecutor) error {
	if _, err := db.Exec(ctx, `SELECT audit_ensure_partitions($1)`, AuditPartitionMonthsAhead); err != nil {
		return fmt.Errorf("db: create the next %d months of audit_events partitions: %w", AuditPartitionMonthsAhead, err)
	}
	return nil
}

// beginReadCommitted starts a transaction and PINS it to READ COMMITTED, so it does not inherit
// default_transaction_isolation — a USERSET GUC any role can set per-role or per-database. A
// transaction that touches the audit chain must not have its snapshot semantics decided by that
// setting: the canary reads the chain head and then INSERTs, and the trigger reads the head again
// inside that INSERT; at REPEATABLE READ both reads would answer from a stale snapshot, and at
// SERIALIZABLE the transaction can abort with a failure this function would wrongly report as a
// broken chain and refuse the boot over.
//
// SET TRANSACTION rather than a BeginTx on the interface: migrationExecutor is deliberately the
// small subset of pgxpool.Pool/Conn the migration steps need, and `SET TRANSACTION ISOLATION LEVEL`
// is exactly equivalent as the first statement of the transaction. A failed SET leaves no
// transaction behind for the caller to clean up.
func beginReadCommitted(ctx context.Context, db migrationExecutor) (pgx.Tx, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		tx.Rollback(context.Background()) //nolint:errcheck // best-effort on the failure path
		return nil, fmt.Errorf("pin read committed: %w", err)
	}
	return tx, nil
}

// auditCanaryTransient decides the one direction this check must not get wrong: a lock wait timeout
// (55P03) or cancelled statement (57014) says nothing about whether the chain works, so it is
// reported and the boot continues. Anything else is returned and refuses.
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

// restoreAlwaysTriggers re-applies ENABLE ALWAYS to each trigger in want that is no longer 'A'.
// docs/OPERATIONS.md promises a hardened trigger is left "exactly as it is"; this keeps that promise
// for the whole of Migrate — without it an already-hardened deployment would lose the hardening the
// moment it upgraded, with nothing logged, and the next boot would read the reverted 'O' as normal.
//
// Idempotent and deliberately narrow: re-reads the catalog and issues the ALTER only for a trigger
// that WAS 'A' and is not any more, so a trigger nobody hardened is never touched.
//
// A failure is logged, not returned: refusing the boot would not save the hardening, since the
// migrations are already applied and re-recorded, so the next boot's capture has nothing left to
// restore. An ERROR naming the exact statement to re-run is the honest outcome instead.
func restoreAlwaysTriggers(ctx context.Context, db migrationExecutor, want []string) {
	if len(want) == 0 {
		return
	}
	still, err := auditAlwaysTriggers(ctx, db)
	if err != nil {
		// Naming the statements, not just the trigger names: this is the last chance the operator
		// gets to be told what to run when the catalog can no longer be read (e.g. an expired
		// boot deadline mid-loop).
		slog.ErrorContext(ctx, "db: cannot tell whether the migration loop reverted an ENABLE ALWAYS audit trigger; re-check pg_trigger.tgenabled and re-apply by hand if it is not 'A'",
			slog.Any("error", err), slog.Any("hardened_before", want),
			slog.Any("statements", alwaysTriggerStatements(want)))
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
		stmt := alwaysTriggerStatement(name)
		if _, err := db.Exec(ctx, stmt); err != nil {
			slog.ErrorContext(ctx, "db: a migration reverted this trigger's ENABLE ALWAYS hardening and it could NOT be re-applied; it now fires only for ordinary writes, not under session_replication_role = replica — re-apply it by hand",
				slog.String("trigger", name), slog.String("statement", stmt), slog.Any("error", err))
			continue
		}
		slog.WarnContext(ctx, "db: re-applied the ENABLE ALWAYS hardening a migration reverted on an audit_events trigger",
			slog.String("trigger", name))
	}
	restorePartitionAlwaysTriggers(ctx, db, want)
}

// restorePartitionAlwaysTriggers carries an operator's ENABLE ALWAYS on a parent trigger down to the
// copies Postgres does NOT clone. ALTER TABLE ... ENABLE ALWAYS TRIGGER on the partitioned parent
// recurses to the row-level clones, but a statement-level trigger (the TRUNCATE guard) exists once
// per partition under the same name and is not reached from the parent; a partition created or
// re-armed since would otherwise fire it only for ordinary writes. Narrow like its caller: only a
// name the operator had hardened, only a non-clone copy that is not 'A'.
func restorePartitionAlwaysTriggers(ctx context.Context, db migrationExecutor, want []string) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return
	}
	var rels, names []string
	if err := db.QueryRow(ctx, auditTreeCTE+`
		SELECT COALESCE(array_agg(t.tgrelid::regclass::text ORDER BY t.tgrelid::regclass::text, t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(t.tgname::text            ORDER BY t.tgrelid::regclass::text, t.tgname), ARRAY[]::text[])
		FROM pg_trigger t JOIN audit_tree ON audit_tree.oid = t.tgrelid
		WHERE t.tgrelid <> 'audit_events'::regclass AND NOT t.tgisinternal AND t.tgparentid = 0
		  AND t.tgenabled <> 'A' AND t.tgname = ANY($1)`, want).Scan(&rels, &names); err != nil {
		slog.ErrorContext(ctx, "db: cannot read the partition-level audit triggers to re-apply ENABLE ALWAYS to; re-check pg_trigger.tgenabled on every audit_events partition",
			slog.Any("error", err), slog.Any("hardened_before", want))
		return
	}
	for i := range rels {
		if i >= len(names) {
			break
		}
		stmt := `ALTER TABLE ` + rels[i] + ` ENABLE ALWAYS TRIGGER ` + pgx.Identifier{names[i]}.Sanitize()
		if _, err := db.Exec(ctx, stmt); err != nil {
			slog.ErrorContext(ctx, "db: a partition's audit trigger lost its ENABLE ALWAYS hardening and it could NOT be re-applied; re-apply it by hand",
				slog.String("statement", stmt), slog.Any("error", err))
		}
	}
}

// alwaysTriggerStatement is the one statement that re-applies an operator's
// hardening to a single trigger. Kept in one place so the ERROR lines quote
// exactly what restoreAlwaysTriggers itself would have run.
func alwaysTriggerStatement(name string) string {
	return `ALTER TABLE audit_events ENABLE ALWAYS TRIGGER ` + pgx.Identifier{name}.Sanitize()
}

// alwaysTriggerStatements is the same for a whole capture, for the branch that
// could not even read the catalog back and so cannot say which of them are
// still needed.
func alwaysTriggerStatements(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, alwaysTriggerStatement(n))
	}
	return out
}

// restoreHardeningTimeout bounds the detached ENABLE ALWAYS restore registered
// by migrateOn. It runs on a context the caller cannot cancel (that is the
// point), so it needs a deadline of its own: a best-effort re-apply must never
// be the reason a boot fails to return.
const restoreHardeningTimeout = 10 * time.Second

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

// ensureAuditTriggers is the boot-time answer to "the migration ran once, years of restarts ago".
// schema_migrations records a FILENAME, so an owner or superuser who DROPs (or DISABLEs) an
// audit_events trigger leaves a database every later Migrate happily reports as fully migrated,
// while every row written after that is unchained — exactly what the verify sweep names as a break.
//
// The chain trigger is RESTORED rather than refused: it is defined by idempotent
// DROP-IF-EXISTS/CREATE migrations that can simply be replayed, and refusing to boot would leave the
// deployment with no audit log at all. The append-only triggers are only CHECKED: they are defined
// by the whole initial schema, and replaying that at boot to fix one trigger is a far bigger blast
// radius than refusing. Either way the process does not continue silently.
//
// A missing audit_events table is not this function's business (an empty database mid-bootstrap has
// none yet); it reports protected-by-absence and leaves the rest of the boot to say so.
func ensureAuditTriggers(ctx context.Context, db migrationExecutor) error {
	present, err := auditTriggerNames(ctx, db)
	if err != nil {
		return err
	}
	if present == nil { // no audit_events table at all
		return nil
	}
	// A name is not an identity, and every check above this line was keyed on one: a trigger WEARING
	// a shipped name over a body nobody shipped (DROP + CREATE TRIGGER ... EXECUTE FUNCTION
	// somebody_elses_function()) passes every catalog check while the trigger the whole audit design
	// rests on is the forger's. The append-only arm is the same hole with a worse ending — a function
	// that returns NEW puts UPDATE/DELETE back on the audit log while this check reports it enforced.
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
	// The shipped guards being armed is not the same as nothing ELSE being armed beside them.
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
		return fmt.Errorf("db: row-level BEFORE INSERT trigger(s) on audit_events that Wardyn does not ship (%s); "+
			"such a trigger sees NEW and its changes are what Postgres stores, so it can rewrite or drop any "+
			"audit row on the way in while every shipped guard stays armed and the verify sweep still reports "+
			"clean — refusing to start", strings.Join(tamperCapable, ", "))
	}
	return nil
}

// reportTransactionIsolation names a default_transaction_isolation that is not READ COMMITTED, at
// ERROR, with the statement to fix it — the same "never continue silently" treatment a missing
// guard gets.
//
// Why the chain cares: the head read that decides prev_hash runs INSIDE the trigger, inside the
// inserting transaction. Under REPEATABLE READ that read uses a snapshot taken before the advisory
// lock was granted, so a writer that queued behind the lock reads a stale head and chains to it —
// two rows claim one predecessor and the verify sweep reports a break. This GUC is USERSET: any role
// can set it, per role or per database, with no superuser involved.
//
// Report, not refuse: Wardyn's OWN writers no longer depend on it (they pin ReadCommitted on their
// own Begin, which overrides the GUC), so this is a deployment posture to report for writers this
// package knows nothing about, not a defect to refuse over. Also read on whichever connection
// Migrate holds — in a split-DSN deploy that's the MIGRATE role, which the app role may not match —
// so a clean line here is not a promise about the serving pool.
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
	slog.ErrorContext(ctx, "db: default_transaction_isolation is not read committed; "+
		"Wardyn's own audit writers pin READ COMMITTED per transaction and are unaffected, but any OTHER writer to audit_events inheriting this default can chain to a stale head and the verify sweep will report the result as a break - fix with ALTER DATABASE ... "+
		"SET default_transaction_isolation = 'read committed' (or the matching ALTER ROLE)",
		slog.String("default_transaction_isolation", iso),
		slog.String("statement", "ALTER DATABASE <db> SET default_transaction_isolation = 'read committed'"))
}

// replayTriggerMigrations re-executes every embedded migration that defines trigger, in filename
// order, WITHOUT touching schema_migrations: those rows still describe what was applied and when,
// and a restore is not a new migration. Discovered by content rather than listed, so a later
// migration that redefines the trigger is replayed too, instead of reinstating a superseded body.
func replayTriggerMigrations(ctx context.Context, db migrationExecutor, trigger string) error {
	names, err := triggerMigrationFiles(trigger)
	if err != nil {
		return err
	}
	// One transaction around the whole set: it is ordered, and each file REPLACES the previous
	// function body, so a failure partway would leave the database bound to a definition NOBODY
	// SHIPPED AS FINAL. This never self-heals like the forward path does: an intermediate body still
	// satisfies "trigger present, not an impostor", so the next boot's canary chains fine and nothing
	// replays or reports — until two concurrent inserters read one head, the chain forks, and the
	// verify sweep reports a tamper verdict no operator can clear. A context expiring mid-loop is a
	// designed-for exit; none of these files uses CREATE INDEX CONCURRENTLY, the one thing a
	// transaction here would refuse.
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin tx to restore trigger %s: %w", trigger, err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // best-effort on the failure path; ctx may already be dead
	for _, name := range names {
		data, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("db: read migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("db: replay %s to restore trigger %s: %w", name, trigger, err)
		}
		slog.InfoContext(ctx, "db: replayed migration to restore an audit trigger",
			slog.String("file", name), slog.String("trigger", trigger))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit the replay that restores trigger %s: %w", trigger, err)
	}
	return nil
}

// triggerMigrationFiles returns, in apply order, the embedded migrations whose text defines trigger
// — the REPLAY SET replayTriggerMigrations re-executes verbatim, against a database where all of
// them are already applied and none is re-recorded in schema_migrations.
//
// Its own function so the idempotency guard derives from the SAME predicate the replay uses,
// instead of a hand-written list that could drift and then fail on exactly the database whose
// trigger already went missing — the case the replay exists to rescue.
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

// unknownAppliedMigrations returns, oldest first, the filenames schema_migrations records that are
// not in shipped — the migrations a NEWER wardynd applied. This is the binary-side downgrade
// refusal: without it an older wardynd boots over a schema whose one-way conversions it cannot read
// (a CHECK it will violate, a re-encoded secret it cannot decrypt). COLLATE "C" so "newest" is Go's
// byte order, not the database's locale.
func unknownAppliedMigrations(ctx context.Context, db migrationExecutor, shipped []string) ([]string, error) {
	var unknown []string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(filename ORDER BY filename COLLATE "C"), ARRAY[]::text[])
		FROM schema_migrations WHERE NOT (filename = ANY($1::text[]))`, shipped).Scan(&unknown); err != nil {
		return nil, fmt.Errorf("db: list applied migrations: %w", err)
	}
	return unknown, nil
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
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on failure path

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
