// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package db provides Postgres connection bootstrapping and schema migration
// for the Wardyn control plane. Postgres is the ONLY required dependency.
package db

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

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

// AuditDDLProtected reports whether the given (application) pool's role is
// UNABLE to bypass the audit_events append-only triggers via DDL — i.e. it is
// neither a superuser nor a MEMBER of the table's owner role (membership, not
// just direct ownership: a role GRANTed the owner role inherits DROP TRIGGER /
// ALTER ... DISABLE TRIGGER rights). The N4 role-separation only protects the
// append-only guarantee when this is true, so the two-DSN deploy must be
// VERIFIED here rather than assumed (honesty: never log a protection claim
// stronger than the enforcing role setup). Fails safe: any ambiguity (missing
// table, error) reports NOT protected.
func AuditDDLProtected(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var canBypass bool
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(
			bool_or(r.rolsuper OR pg_has_role(current_user, c.relowner, 'MEMBER')),
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

		if err := applyMigration(ctx, db, name, string(data)); err != nil {
			return err
		}
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
