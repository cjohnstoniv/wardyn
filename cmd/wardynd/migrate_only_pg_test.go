// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Postgres-backed tests for `wardynd -migrate-only`: it migrates under the single-instance lock, and
// refuses while that lock is held or while any other client is connected to the database. Each test
// gets its own throwaway CREATE DATABASE because the client check counts every backend on the
// database, so a sibling test's connection would be read as a live writer. Guarded by WARDYN_TEST_PG;
// skipped cleanly when unset.

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// emptyDatabase creates an unmigrated database on the WARDYN_TEST_PG server and returns its DSN.
func emptyDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the Postgres-backed -migrate-only test")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_migonly_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		t.Fatalf("create throwaway database %s: %v", name, err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = admin.Exec(cctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(cctx, `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WARDYN_TEST_PG: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func migrateOnlyFlags(dsn string) *bootFlags {
	empty, timeout := "", 2*time.Minute
	return &bootFlags{dsn: &dsn, migrateDSN: &empty, migrateTimeout: &timeout}
}

func requireExit(t *testing.T, err error, want int, mentions ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want exit code %d, got success", want)
	}
	if got := exitCodeOf(err); got != want {
		t.Fatalf("exit code = %d, want %d (err: %v)", got, want, err)
	}
	for _, m := range mentions {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("error never says %q: %v", m, err)
		}
	}
}

func appliedCount(t *testing.T, dsn string) int {
	t.Helper()
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		return 0 // no tracking table yet: nothing applied
	}
	return n
}

func TestMigrateOnly_MigratesThenExitsZeroAndReleasesTheLock(t *testing.T) {
	dsn := emptyDatabase(t)
	if err := migrateOnlyMode(migrateOnlyFlags(dsn)); err != nil {
		t.Fatalf("migrate-only on an idle database: %v", err)
	}
	if n := appliedCount(t, dsn); n == 0 {
		t.Fatal("exit 0 but schema_migrations holds nothing")
	}

	// The lock is free again afterwards: a normal boot's claim succeeds.
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	release, err := claimSingleInstance(ctx, pool, false)
	if err != nil {
		t.Fatalf("lock still held after -migrate-only returned: %v", err)
	}
	release()
}

func TestMigrateOnly_RefusesWhileTheSingleInstanceLockIsHeld(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx := context.Background()
	holderPool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer holderPool.Close()
	release, err := claimSingleInstance(ctx, holderPool, false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer release()

	err = migrateOnlyMode(migrateOnlyFlags(dsn))
	requireExit(t, err, exitMigrateRefused, "single-instance lock is held by", "backend pid")
	if n := appliedCount(t, dsn); n != 0 {
		t.Fatalf("a refused -migrate-only still applied %d migration(s)", n)
	}
}

func TestMigrateOnly_RefusesWhileASecondConnectionIsOpenAndNoLockIsHeld(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx := context.Background()
	// A replica run with WARDYN_HA holds a connection and no lock.
	other, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer other.Close()

	err = migrateOnlyMode(migrateOnlyFlags(dsn))
	requireExit(t, err, exitMigrateRefused, "other client connection", "WARDYN_HA")
	if n := appliedCount(t, dsn); n != 0 {
		t.Fatalf("a refused -migrate-only still applied %d migration(s)", n)
	}
	// The refusal released its own lock: nothing is left holding the database.
	release, err := claimSingleInstance(ctx, other, false)
	if err != nil {
		t.Fatalf("refused -migrate-only left the lock held: %v", err)
	}
	release()
}

// A migrator that is not the app role sees the app role's sessions with backend_type NULL, so the
// client check must not depend on that column alone (the WARDYN_PG_MIGRATE_DSN split is the normal
// hardened deployment).
func TestMigrateOnly_SeesAnotherRolesSessionFromAnUnprivilegedMigrator(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	role := "wardyn_migonly_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'pw'`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP ROLE IF EXISTS `+role); err != nil {
			t.Logf("drop role %s: %v", role, err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(role, "pw")

	// admin's connection is the live writer, a different role from the migrator.
	_, err = acquireMigrateOnly(ctx, u.String(), 30*time.Second)
	requireExit(t, err, exitMigrateRefused, "other client connection")
}

func TestMigrateOnly_HoldsTheLockWhileItRuns(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx := context.Background()

	s, err := acquireMigrateOnly(ctx, dsn, 30*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer s.close()

	// A wardynd booting now fails claimSingleInstance. Its pool is a second connection, which is what
	// a real boot is; the lock is what refuses it.
	boot, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect boot pool: %v", err)
	}
	defer boot.Close()
	if _, err := claimSingleInstance(ctx, boot, false); err == nil {
		t.Fatal("a wardynd booted while -migrate-only holds the single-instance lock")
	}

	// A second -migrate-only is refused with the refusal code, naming the holder.
	_, err = acquireMigrateOnly(ctx, dsn, 30*time.Second)
	requireExit(t, err, exitMigrateRefused, "single-instance lock is held by")

	// The migration itself runs on the lock-holding connection.
	if err := db.MigrateConn(ctx, s.conn); err != nil {
		t.Fatalf("migrate on the held connection: %v", err)
	}
}

func TestMigrateOnly_AFailedMigrationExitsWithTheFailureCode(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// A database a newer wardynd migrated: Migrate refuses it, after the lock and client checks pass.
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
		INSERT INTO schema_migrations (filename) VALUES ('9999_from_the_future.sql')`); err != nil {
		t.Fatalf("stage a newer schema: %v", err)
	}
	pool.Close()

	// The staging pool's backend takes a moment to leave pg_stat_activity, and until it does the
	// client check rightly reads it as a live writer: retry only that refusal.
	var merr error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if merr = migrateOnlyMode(migrateOnlyFlags(dsn)); exitCodeOf(merr) != exitMigrateRefused {
			break
		}
	}
	requireExit(t, merr, exitMigrateFailed, "9999_from_the_future.sql")
}

func TestMigrateOnly_FailureAndConfigurationExitCodes(t *testing.T) {
	empty, timeout := "", time.Minute
	err := migrateOnlyMode(&bootFlags{dsn: &empty, migrateDSN: &empty, migrateTimeout: &timeout})
	if err == nil || exitCodeOf(err) != exitMigrateFailed {
		t.Fatalf("no database configured: exit %d, err %v; want %d", exitCodeOf(err), err, exitMigrateFailed)
	}
	if got := exitCodeOf(errors.New("plain")); got != 1 {
		t.Fatalf("an error with no code exits %d, want 1", got)
	}
	if exitMigrateRefused == exitMigrateFailed || exitMigrateRefused == 0 || exitMigrateRefused == 2 {
		t.Fatalf("refused code %d must be distinct from success, failure and the flag package's usage code", exitMigrateRefused)
	}
}
