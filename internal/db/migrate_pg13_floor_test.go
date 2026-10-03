// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// The Postgres 13 floor: one migration, found by its _pg13_floor.sql suffix, that refuses an older
// server before any later migration is applied or recorded. The ordering half needs no server; the
// catalog half runs against WARDYN_TEST_PG, on a throwaway database, and branches on the server's
// version: PG12 must refuse and record nothing past the last 0.8.5 file, PG13+ must apply everything.
// On PG12 run ONLY this test: every other migrating test fails there by design.

import (
	"context"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// lastBeforePG13Floor is the final v0.8.5 migration. The gate must be the very next file, so it sorts
// before every migration a later 0.8.6 lane adds.
const lastBeforePG13Floor = "0106_attach_ticket_authority.sql"

func pg13FloorGate(t *testing.T) string {
	t.Helper()
	var gate []string
	for _, name := range readMigrationNames(t) {
		if strings.HasSuffix(name, "_pg13_floor.sql") {
			gate = append(gate, name)
		}
	}
	if len(gate) != 1 {
		t.Fatalf("want exactly one *_pg13_floor.sql migration, found %v", gate)
	}
	return gate[0]
}

func TestMigratePG13Floor(t *testing.T) {
	gate := pg13FloorGate(t)

	names := readMigrationNames(t)
	sort.Strings(names)
	at := sort.SearchStrings(names, lastBeforePG13Floor)
	if at+1 >= len(names) || names[at] != lastBeforePG13Floor {
		t.Fatalf("%s is not a shipped migration; the floor's anchor moved", lastBeforePG13Floor)
	}
	if names[at+1] != gate {
		t.Fatalf("the migration after %s is %s, not the Postgres 13 gate %s: a migration that sorts "+
			"between them runs on a Postgres 12 server before the gate can refuse it",
			lastBeforePG13Floor, names[at+1], gate)
	}

	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the Postgres-backed floor test")
	}
	ctx := context.Background()
	admin, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "wardyn_pg13_floor_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	var version int
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil {
		t.Fatalf("read server_version_num: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse WARDYN_TEST_PG: %v", err)
	}
	u.Path = "/" + name
	pool, err := Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to throwaway database %s: %v", name, err)
	}
	t.Cleanup(pool.Close)

	if version < 130000 {
		// An earlier migration uses gen_random_uuid(), which a Postgres 12 server lacks, so a fresh
		// database would stop there and never reach the gate. The case that matters is the one the
		// gate exists for: a database 0.8.5 already migrated, now booted by 0.8.6. Record the
		// 0.8.5 files as applied, as that database has them.
		if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (
			filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
			t.Fatalf("create schema_migrations: %v", err)
		}
		for _, n := range names {
			if n > lastBeforePG13Floor {
				break
			}
			if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (filename) VALUES ($1)`, n); err != nil {
				t.Fatalf("record %s as applied: %v", n, err)
			}
		}
	}

	migrateErr := Migrate(ctx, pool)
	var past int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE filename > $1`, lastBeforePG13Floor).Scan(&past); err != nil {
		t.Fatalf("count migrations recorded past %s: %v", lastBeforePG13Floor, err)
	}

	if version < 130000 {
		if migrateErr == nil {
			t.Fatalf("Migrate succeeded on server_version_num %d: the Postgres 13 floor did not refuse", version)
		}
		if !strings.Contains(migrateErr.Error(), "PostgreSQL 13 or newer") {
			t.Errorf("Migrate refused, but not with the floor message: %v", migrateErr)
		}
		if past != 0 {
			t.Errorf("%d migration(s) past %s are recorded after a refused Migrate; the gate and everything after it must leave no row",
				past, lastBeforePG13Floor)
		}
		return
	}
	if migrateErr != nil {
		t.Fatalf("Migrate on server_version_num %d: %v", version, migrateErr)
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)`, gate).Scan(&recorded); err != nil {
		t.Fatalf("look up the gate's row: %v", err)
	}
	if !recorded {
		t.Errorf("Migrate succeeded but %s has no row; a later boot would run it again", gate)
	}
}
