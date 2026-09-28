// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// #1050 follow-up (item 2): the downgrade refusal itself is pinned at the
// internal/db level (TestMigrateRefusesUnknownAppliedMigrations calls
// db.Migrate/db.MigrateAllowingUnknown directly), but nothing exercised the
// WIRING that connects WARDYN_ALLOW_UNKNOWN_MIGRATIONS to that behavior:
// connectAndMigrate (boot_deps.go) picks db.Migrate or db.MigrateAllowingUnknown
// based on its allowUnknownMigrations argument. Mutating that selection away —
// e.g. always using db.Migrate — survives every internal/db unit test, since
// those call the two exported functions directly and never go through this
// selection at all; only a binary-level (or this) test would catch it.
//
// This stages a schema whose schema_migrations already records a migration
// this binary does not ship (as if a newer wardynd had migrated it), then
// proves connectAndMigrate refuses it with allowUnknownMigrations=false and
// boots successfully with it =true — through the SAME wiring main() uses.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
// Run: WARDYN_TEST_PG="postgres://wardyn:wardyn@localhost:55432/wardyn?sslmode=disable" \
//        go test ./cmd/wardynd/... -run TestConnectAndMigrate_AllowUnknownMigrations

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

func TestConnectAndMigrate_AllowUnknownMigrationsWiring(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the allow-unknown-migrations wiring test")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot point a connection at another schema")
	}
	ctx := context.Background()

	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	schema := fmt.Sprintf("wardyn_um_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
	})

	schemaDSN := func() string {
		v := *u
		q := v.Query()
		q.Set("search_path", schema)
		v.RawQuery = q.Encode()
		return v.String()
	}()

	// Stage the schema as if a newer wardynd had already migrated it: the
	// tracking table plus one filename this binary does not ship. Mirrors
	// migrateOn's own CREATE TABLE IF NOT EXISTS in internal/db/db.go.
	const unknownFile = "9999_from_a_newer_wardynd_wiring_test.sql"
	if _, err := admin.Exec(ctx, `CREATE TABLE `+schema+`.schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("stage schema_migrations in %s: %v", schema, err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.schema_migrations (filename) VALUES ($1)`, unknownFile); err != nil {
		t.Fatalf("stage unknown migration row in %s: %v", schema, err)
	}

	// Without the break-glass, connectAndMigrate must refuse — proving the
	// staged row really is "unknown" to this binary, not a no-op fixture.
	if pool, err := connectAndMigrate(t.Context(), schemaDSN, "", 30*time.Second, 2*time.Minute, false); err == nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatal("connectAndMigrate(allowUnknownMigrations=false) booted over a schema recording an unknown migration; want a refusal")
	} else if !strings.Contains(err.Error(), unknownFile) || !strings.Contains(err.Error(), "WARDYN_ALLOW_UNKNOWN_MIGRATIONS") {
		t.Fatalf("refusal %q does not name the unknown file and the break-glass var", err)
	}

	// With it, the SAME call through the SAME wiring must boot: this is the
	// line that pins boot_deps.go's `if allowUnknownMigrations { migrate =
	// db.MigrateAllowingUnknown }` selection, not just db.MigrateAllowingUnknown
	// itself.
	pool, err := connectAndMigrate(t.Context(), schemaDSN, "", 30*time.Second, 2*time.Minute, true)
	if err != nil {
		t.Fatalf("connectAndMigrate(allowUnknownMigrations=true): %v", err)
	}
	defer pool.Close()

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations in %s: %v", schema, err)
	}
	if count <= 1 {
		t.Errorf("schema_migrations has %d row(s); want the staged unknown row PLUS this binary's own shipped migrations applied", count)
	}
	var stillThere bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+schema+`.schema_migrations WHERE filename = $1)`, unknownFile).Scan(&stillThere); err != nil {
		t.Fatalf("check unknown row survived in %s: %v", schema, err)
	}
	if !stillThere {
		t.Error("break-glass boot removed the newer wardynd's migration record; it must only ever add rows")
	}
}
