// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// TestConnectAndMigrate_RecordsTheRelease: a boot records the release it runs in deployment_versions
// (migration 0190), which setup status reads to name the defaults an upgrade changed.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestConnectAndMigrate_RecordsTheRelease(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed boot record test")
	}
	pool, err := connectAndMigrate(t.Context(), dsn, "", 30*time.Second, 5*time.Minute, false)
	if err != nil {
		t.Fatalf("connectAndMigrate: %v", err)
	}
	t.Cleanup(pool.Close)
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM deployment_versions WHERE version = $1`, version.Version).Scan(&n); err != nil || n != 1 {
		t.Fatalf("deployment_versions rows for %s = %d (%v), want 1", version.Version, n, err)
	}
}

// TestMigrateOnly_RecordsTheReleaseBeforeTheBoot: a stopped-writer upgrade from 0.9.1 migrates in a
// -migrate-only Job, and the serving boot after it finds nothing pending. The Job records the release
// with the tip the database held before (0.9.1's 0179), and the boot keeps it, so setup status places
// the deployment as 0.9.1 and keeps 0.9.2's internal_hosts entry.
func TestMigrateOnly_RecordsTheReleaseBeforeTheBoot(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("../../internal/db/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	sort.Strings(files)
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	const tip091 = "0179_runner_arch.sql"
	for _, f := range files {
		name := filepath.Base(f)
		if name > tip091 {
			break
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
			t.Fatal(err)
		}
	}
	pool.Close()

	if err := migrateOnlyMode(migrateOnlyFlags(dsn)); err != nil {
		t.Fatalf("migrate-only: %v", err)
	}
	served, err := connectAndMigrate(ctx, dsn, "", 30*time.Second, 5*time.Minute, false)
	if err != nil {
		t.Fatalf("boot after the Job: %v", err)
	}
	t.Cleanup(served.Close)
	o, err := store.NewPG(served).UpgradeOrigin(ctx, version.Version)
	if err != nil || !o.Recorded || o.PriorMigration != tip091 {
		t.Fatalf("origin = %+v, %v; want this release recorded with prior migration %s", o, err, tip091)
	}
}
