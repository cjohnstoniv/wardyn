// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The split-DSN boot migrates through WARDYN_PG_MIGRATE_DSN and serves through
// WARDYN_PG_DSN. Only the migrate pool met the refusal of a schema a newer
// wardynd migrated, so an app DSN whose search_path reached such a schema, or
// one missing a migration, was served unchecked. Two fully migrated schemas
// stand in for the two DSNs; the single-DSN boot is pinned unchanged beside
// them. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

func TestConnectAndMigrate_SplitDSNRefusesAnAppSchemaOnOtherMigrations(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the split-DSN app schema test")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot derive two schema-scoped DSNs from it")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	schemaDSN := func(schema string) string {
		v := *u
		q := v.Query()
		q.Set("search_path", schema)
		v.RawQuery = q.Encode()
		return v.String()
	}
	stamp := time.Now().UnixNano() % 1_000_000_000
	migrateSchema := fmt.Sprintf("wardyn_as_m_%d", stamp)
	appSchema := fmt.Sprintf("wardyn_as_a_%d", stamp)
	for _, s := range []string{migrateSchema, appSchema} {
		if _, err := admin.Exec(ctx, `CREATE SCHEMA `+s); err != nil {
			t.Fatalf("create schema %s: %v", s, err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+s+` CASCADE`); err != nil {
				t.Logf("cleanup drop schema %s: %v", s, err)
			}
		})
		p, err := connectAndMigrate(t.Context(), schemaDSN(s), "", 30*time.Second, 2*time.Minute, false)
		if err != nil {
			t.Fatalf("migrate %s: %v", s, err)
		}
		p.Close()
	}
	app, migrator := schemaDSN(appSchema), schemaDSN(migrateSchema)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	refused := func(migrateDSN, name string, want ...string) {
		t.Helper()
		pool, err := connectAndMigrate(t.Context(), app, migrateDSN, 30*time.Second, 2*time.Minute, false)
		if pool != nil {
			pool.Close()
		}
		if err == nil {
			t.Fatalf("a boot (migrate DSN %q) came up serving %s, whose applied migrations differ by %s", migrateDSN, appSchema, name)
		}
		for _, w := range append(want, name) {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error = %q, want it to name %q so the operator knows which DSN to fix", err, w)
			}
		}
	}

	// The app DSN reaches a schema a newer wardynd migrated: refused, naming
	// the app pool. Single-DSN over that schema keeps the refusal it had.
	const unknown = "9999_from_a_newer_wardynd_split_dsn_test.sql"
	exec(`INSERT INTO `+appSchema+`.schema_migrations (filename) VALUES ($1)`, unknown)
	refused(migrator, unknown, "WARDYN_PG_DSN")
	refused("", unknown, "WARDYN_ALLOW_UNKNOWN_MIGRATIONS")
	exec(`DELETE FROM `+appSchema+`.schema_migrations WHERE filename = $1`, unknown)

	// The app DSN reaches a schema missing a migration this binary ships.
	newest, err := db.NewestMigration()
	if err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM `+appSchema+`.schema_migrations WHERE filename = $1`, newest)
	refused(migrator, newest, "WARDYN_PG_DSN")

	// SCOPED: both DSNs naming one schema, and single-DSN on a healthy one,
	// still come up.
	for _, migrateDSN := range []string{migrator, ""} {
		ok, err := connectAndMigrate(t.Context(), migrator, migrateDSN, 30*time.Second, 2*time.Minute, false)
		if err != nil {
			t.Fatalf("a boot on one healthy schema (migrate DSN %q): %v, want it to come up", migrateDSN, err)
		}
		ok.Close()
	}
}
