// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// Exit codes of `wardynd -migrate-only`. 0 is success. A refusal is its own code so a Job or script
// can tell "nothing was attempted, a writer may still be live" from "the migration ran and failed".
// 2 is skipped: the flag package exits 2 on a usage error.
const (
	exitMigrateFailed  = 1
	exitMigrateRefused = 3
)

// exitCodeError is an error that carries the process exit code main should use instead of 1.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

// exitCodeOf is the exit code for err: its own when it is an exitCodeError, 1 otherwise.
func exitCodeOf(err error) int {
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code
	}
	return 1
}

// refusedTo is a refusal of a quiet-database maintenance mode, naming what it refused to do ("migrate",
// "split the legacy audit partition"). It carries exitMigrateRefused.
func refusedTo(verb, format string, args ...any) error {
	return &exitCodeError{code: exitMigrateRefused, err: fmt.Errorf("refusing to "+verb+": "+format, args...)}
}

// migrateOnlyMode is `wardynd -migrate-only`: run the schema migration alone, then exit, serving
// nothing. It exists for an upgrade that must convert data under stopped writers (a one-off Job after
// the deployment is scaled to zero), where a normal boot would start serving on the converted schema.
//
// The migration runs on the ONE connection that holds db.SingleInstanceLockKey, so a wardynd booting
// meanwhile fails its claimSingleInstance and a second -migrate-only is refused. That lock alone is
// not proof the database is quiet: a replica started with -allow-multi-instance never takes it, so
// the lock cannot show it. The mode therefore also refuses while any other client backend is
// connected to the database. Both checks are made before the first migration statement.
//
// It connects with WARDYN_PG_MIGRATE_DSN when set (the owner/migrator role), else WARDYN_PG_DSN, and
// is bounded by WARDYN_MIGRATE_TIMEOUT. Like the other maintenance modes it has no WARDYN_* env pair.
func migrateOnlyMode(f *bootFlags) error {
	dsn := strings.TrimSpace(*f.migrateDSN)
	if dsn == "" {
		dsn = strings.TrimSpace(*f.dsn)
	}
	if dsn == "" {
		return errors.New("-migrate-only needs a database; set WARDYN_PG_DSN (or WARDYN_PG_MIGRATE_DSN)")
	}
	ctx := context.Background()
	s, err := acquireMigrateOnly(ctx, dsn, 30*time.Second)
	if err != nil {
		return err
	}
	defer s.close()

	migrateCtx, cancel := context.WithTimeout(ctx, *f.migrateTimeout)
	defer cancel()
	if err := db.MigrateConn(migrateCtx, s.conn); err != nil {
		return &exitCodeError{code: exitMigrateFailed, err: fmt.Errorf("migrate: %w", err)}
	}
	slog.Info("wardynd: -migrate-only finished; every pending migration is applied and nothing was served")
	return nil
}

// migrateOnlySession is a connection that holds the single-instance lock and has seen no other
// client on the database. Close releases the lock and the connection.
type migrateOnlySession struct {
	pool    *pgxpool.Pool
	conn    *pgxpool.Conn
	release func()
}

func (s *migrateOnlySession) close() {
	s.release()
	s.pool.Close()
}

// acquireMigrateOnly connects, takes db.SingleInstanceLockKey and verifies no other client backend is
// on the database, in that order: the lock first, so a booting wardynd is already shut out while the
// client check runs. Any refusal returns an error with exit code exitMigrateRefused.
func acquireMigrateOnly(ctx context.Context, dsn string, connectTimeout time.Duration) (*migrateOnlySession, error) {
	return acquireQuiet(ctx, dsn, connectTimeout, "migrate")
}

// acquireQuiet is acquireMigrateOnly for any maintenance mode that needs the database to itself; verb
// names what a refusal refused to do.
func acquireQuiet(ctx context.Context, dsn string, connectTimeout time.Duration, verb string) (*migrateOnlySession, error) {
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	pool, err := db.Connect(connectCtx, dsn)
	if err != nil {
		return nil, &exitCodeError{code: exitMigrateFailed, err: fmt.Errorf("connect db: %w", err)}
	}
	conn, release, ok, err := db.TryAdvisoryLockConn(connectCtx, pool, db.SingleInstanceLockKey)
	if err != nil {
		pool.Close()
		return nil, &exitCodeError{code: exitMigrateFailed, err: err}
	}
	if !ok {
		holder := singleInstanceHolder(connectCtx, pool)
		pool.Close()
		return nil, refusedTo(verb, "the single-instance lock is held by %s: a wardynd, or another maintenance mode, is running against this database. "+
			"Stop it first (docs/OPERATIONS.md, \"Stopped-writer upgrade\")", holder)
	}
	s := &migrateOnlySession{pool: pool, conn: conn, release: release}
	others, err := otherClients(connectCtx, conn)
	if err != nil {
		s.close()
		return nil, &exitCodeError{code: exitMigrateFailed, err: err}
	}
	if len(others) > 0 {
		s.close()
		return nil, refusedTo(verb, "%d other client connection(s) are open on this database (%s). The single-instance lock does not stop a replica started "+
			"with -allow-multi-instance, or an older wardynd, so a conversion could run under a live writer. "+
			"Scale every wardynd to zero first (docs/OPERATIONS.md, \"Stopped-writer upgrade\")", len(others), strings.Join(others, "; "))
	}
	return s, nil
}

// otherClients describes each client backend other than conn's own on conn's database.
//
// A role without pg_read_all_stats sees another role's session with backend_type NULL (measured on
// Postgres 17), so matching 'client backend' alone would let a live writer running as the app role go
// unseen by a migrator running as a different role, which is exactly the WARDYN_PG_MIGRATE_DSN split.
// A masked session keeps its usename, and the server's own workers (autovacuum, replication) have none
// or no datname, so "backend_type NULL with a usename" is a masked client.
func otherClients(ctx context.Context, conn *pgxpool.Conn) ([]string, error) {
	rows, err := conn.Query(ctx, `
		SELECT pid, COALESCE(usename::text, ''), COALESCE(application_name, '')
		FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND (backend_type = 'client backend' OR (backend_type IS NULL AND usename IS NOT NULL))
		ORDER BY pid`)
	if err != nil {
		return nil, fmt.Errorf("list other connections: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pid int32
		var user, app string
		if err := rows.Scan(&pid, &user, &app); err != nil {
			return nil, fmt.Errorf("list other connections: %w", err)
		}
		out = append(out, fmt.Sprintf("pid %d user %q application %q", pid, user, app))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list other connections: %w", err)
	}
	return out, nil
}

// singleInstanceHolder names the session holding the single-instance lock, best effort: the answer
// only shapes an error message, so a failed lookup says so rather than failing the refusal.
func singleInstanceHolder(ctx context.Context, pool *pgxpool.Pool) string {
	var pid int32
	var user, app, addr string
	err := pool.QueryRow(ctx, `
		SELECT l.pid, COALESCE(a.usename::text, ''), COALESCE(a.application_name, ''), COALESCE(a.client_addr::text, '')
		FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.objsubid = 1 AND l.granted
		  AND ((l.classid::bigint << 32) | l.objid::bigint) = $1
		LIMIT 1`, db.SingleInstanceLockKey).Scan(&pid, &user, &app, &addr)
	if err != nil {
		return "a session that could not be identified"
	}
	return fmt.Sprintf("backend pid %d (user %q, application %q, address %q)", pid, user, app, addr)
}
