// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// One Postgres-backed test per refusal of the rehearsal's target chain (recovery-set.md §5.1), each
// of which fails with its check removed, plus the probe's own properties: a transaction-scoped lock
// that leaves nothing on live, and an unconfirmed hold that refuses. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// migratedDatabase is emptyDatabase after every migration, so it has a schema_migrations table.
func migratedDatabase(t *testing.T) string {
	t.Helper()
	dsn := emptyDatabase(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return dsn
}

// withHost returns dsn pointed at host (keeping the port), the same database reached by another name.
func withHost(t *testing.T, dsn, host string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = host + ":" + u.Port()
	return u.String()
}

func identityOf(t *testing.T, dsn string) dsnIdentity {
	t.Helper()
	id, _, err := parseDSNIdentity(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// chainFor builds a chain that passes every check but the one a test breaks: live and target are two
// databases on one server, the manifest says the target's migration, and the set is a fresh directory.
func chainFor(t *testing.T, live, target string) targetChecks {
	t.Helper()
	newest, err := db.NewestMigration()
	if err != nil {
		t.Fatal(err)
	}
	return targetChecks{
		liveDSN: live, targetDSN: target,
		source:    dsnIdentity{Host: "source.invalid", Port: 5432, Database: "wardyn"},
		migration: newest,
		setDir:    t.TempDir(),
	}
}

func runChain(t *testing.T, c targetChecks) (*rehearseSession, error) {
	t.Helper()
	s, err := c.run(context.Background())
	if s != nil {
		t.Cleanup(s.close)
	}
	return s, err
}

func TestRehearseTarget_ScratchDatabaseOnTheLiveServerPassesEveryCheck(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	s, err := runChain(t, chainFor(t, live, target))
	if err != nil {
		t.Fatalf("a scratch database on the live server must pass T0 to T7: %v", err)
	}
	var one int
	if err := s.conn().QueryRow(context.Background(), `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("the target backend is unusable after the chain: %v", err)
	}
}

func TestRehearseTarget_T0RefusesMoreThanOneHost(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	multi := withHost(t, target, "127.0.0.1:"+mustPort(t, target)+",localhost") // whatever host the DSN names
	_, err := runChain(t, chainFor(t, live, multi))
	requireExit(t, err, exitMigrateRefused, "T0")
}

func mustPort(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func TestRehearseTarget_T1RefusesLiveAndTheSourceByName(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	_, err := runChain(t, chainFor(t, live, live))
	requireExit(t, err, exitMigrateRefused, "T1")

	c := chainFor(t, live, target)
	c.source = identityOf(t, target)
	_, err = runChain(t, c)
	requireExit(t, err, exitMigrateRefused, "T1")
}

func TestRehearseTarget_T2RefusesWhenLiveIsUnreachable(t *testing.T) {
	_, target := migratedDatabase(t), migratedDatabase(t)
	_, err := runChain(t, chainFor(t, "postgres://u:p@127.0.0.1:1/wardyn?sslmode=disable&connect_timeout=2", target))
	requireExit(t, err, exitMigrateRefused, "T2")
}

func TestRehearseTarget_T5RefusesATargetWithAnotherClient(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	other, err := db.Connect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_, err = runChain(t, chainFor(t, live, target))
	requireExit(t, err, exitMigrateRefused, "T5", "other client")
}

func TestRehearseTarget_T3RefusesTheLiveDatabaseUnderAnotherName(t *testing.T) {
	live := migratedDatabase(t)
	// localhost for live, 127.0.0.1 for the target: T1 sees two names, only the lock sees one database.
	_, err := runChain(t, chainFor(t, withHost(t, live, "localhost"), withHost(t, live, "127.0.0.1")))
	requireExit(t, err, exitMigrateRefused, "T3", "live database under another name")
}

// recordingHold runs the real probe and, once the probe's transaction has ended, asks live from a
// separate connection whether the key is still held. Live's pool is still open at that point, so a
// session-scoped lock would still be there.
func recordingHold(t *testing.T, leftover *[]bool) holdLiveFunc {
	return func(ctx context.Context, pool *pgxpool.Pool, key int64) (bool, func(), error) {
		confirmed, release, err := holdLiveLock(ctx, pool, key)
		return confirmed, func() {
			release()
			var held bool
			err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND classid=$1 AND objid=$2 AND objsubid=1)`,
				uint32(uint64(key)>>32), uint32(key)).Scan(&held)
			if err != nil {
				t.Errorf("check live for a leftover lock: %v", err)
			}
			*leftover = append(*leftover, held)
		}, err
	}
}

func TestRehearseTarget_T3LeavesNoLockOnLive(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	var leftover []bool
	c := chainFor(t, live, target)
	c.holdLive = recordingHold(t, &leftover)
	if _, err := runChain(t, c); err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 1 || leftover[0] {
		t.Fatalf("after the probe, live still holds the probe key (leftover = %v): the lock must be transaction-scoped", leftover)
	}
}

func TestRehearseTarget_T3AnUnconfirmedHoldRefuses(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	c := chainFor(t, live, target)
	c.holdLive = func(context.Context, *pgxpool.Pool, int64) (bool, func(), error) {
		return false, func() {}, nil
	}
	_, err := runChain(t, c)
	requireExit(t, err, exitMigrateRefused, "T3", "did not confirm")
}

func TestRehearseTarget_T4RefusesAStandby(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	ctx := context.Background()
	// A function in public shadows pg_is_in_recovery once the connection puts public ahead of
	// pg_catalog: the answer a standby gives, without a second server.
	admin, err := db.Connect(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE FUNCTION public.pg_is_in_recovery() RETURNS boolean LANGUAGE sql AS 'SELECT true'`); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	_, err = runChain(t, chainFor(t, live, target+"&options=-csearch_path%3Dpublic,pg_catalog"))
	requireExit(t, err, exitMigrateRefused, "T4")
}

func TestRehearseTarget_T4RefusesARealStandby(t *testing.T) {
	standby := os.Getenv("WARDYN_TEST_PG_STANDBY")
	if standby == "" {
		t.Skip("WARDYN_TEST_PG_STANDBY not set; the shadowed-function T4 test above runs everywhere")
	}
	live := migratedDatabase(t)
	_, err := runChain(t, chainFor(t, live, standby))
	requireExit(t, err, exitMigrateRefused, "T4")
}

func TestRehearseTarget_T6RefusesASetOverlappingALiveDirectory(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	root := t.TempDir()
	spoolDir := filepath.Join(root, "spool")
	if err := os.MkdirAll(filepath.Join(spoolDir, "set"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(spoolDir, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	for name, set := range map[string]string{
		"inside the spool's directory": filepath.Join(spoolDir, "set"),
		"the spool's directory itself": spoolDir,
		"through a symlink alias":      filepath.Join(root, "alias", "set"),
		"containing the spool":         root,
	} {
		c := chainFor(t, live, target)
		c.setDir, c.protected = set, []string{spoolDir}
		_, err := runChain(t, c)
		if err == nil || exitCodeOf(err) != exitMigrateRefused || !strings.Contains(err.Error(), "T6") {
			t.Errorf("set %s: want a T6 refusal, got %v", name, err)
		}
	}
	c := chainFor(t, live, target)
	c.protected = []string{spoolDir, "", filepath.Join(root, "does-not-exist")}
	if _, err := runChain(t, c); err != nil {
		t.Errorf("a set beside the live directories must pass: %v", err)
	}
}

func TestRehearseTarget_T7RefusesAVersionSkew(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	c := chainFor(t, live, target)
	c.migration = "0001_older.sql"
	_, err := runChain(t, c)
	requireExit(t, err, exitMigrateRefused, "T7")

	ctx := context.Background()
	admin, err := db.Connect(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `DELETE FROM schema_migrations WHERE filename = (SELECT max(filename COLLATE "C") FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	_, err = runChain(t, chainFor(t, live, target))
	requireExit(t, err, exitMigrateRefused, "T7")
}

func TestRehearseTarget_TheBackendIsReadOnly(t *testing.T) {
	live, target := migratedDatabase(t), migratedDatabase(t)
	s, err := runChain(t, chainFor(t, live, target))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, stmt := range []string{`CREATE TABLE rehearsal_probe (x int)`, `DELETE FROM schema_migrations`} {
		if _, err := s.conn().Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "read-only transaction") {
			t.Errorf("%q through the target backend: want a read-only refusal, got %v", stmt, err)
		}
	}
	tx, err := s.readTx(ctx, pgx.RepeatableRead)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations`); err == nil {
		t.Error("a write inside readTx succeeded")
	}
}
