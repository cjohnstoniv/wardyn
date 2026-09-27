// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// The #1002 rollback drill: 0.8 -> 0.7.13 -> 0.7.11 (release/0.7's own version
// line). unknownAppliedMigrations and migrateOn's refusal (db.go) already have
// a unit-shaped pin — TestMigrateRefusesUnknownAppliedMigrations — but it
// stages two PLACEHOLDER filenames ("9998_...", "9999_..."). This file proves
// the same refusal against REAL 0.8 migration content: three additive
// migrations copied verbatim from main's internal/db/migrations (0066, 0067,
// 0068 — devices/federation, the user_drives object_scheme column, and the AWS
// SSO spent-token table), fixed under testdata/rollback_drill_08_migrations so
// this branch never has to track main's numbering. They are applied here with
// raw SQL, never through this branch's own migrationFS, exactly the way an 0.8
// wardynd would have recorded them on a database this 0.7.13 line never
// upgrades to on its own.
//
// The drill has two halves:
//
//  1. DOWN: an 0.7.13 Migrate() against a database those three migrations were
//     applied to must refuse, name the newest one, and write nothing.
//  2. ROLLBACK: docs/OPERATIONS.md's only supported recovery — restore the
//     pre-upgrade dump — modeled here as a FRESH schema carrying only what
//     0.7.13 itself ships (probeSchemaPool, same as every other test in this
//     package). Migrate() against that must succeed cleanly and the 0.8-only
//     tables must be absent, so the "restore the pre-upgrade dump" remedy in
//     the refusal message actually leads back to a database this build boots.
//
// A full binary-level drill (build wardynd, point it at a throwaway
// postgres:17, exercise the documented pg_dump/restore commands with the same
// three fixtures) lives in scripts/rollback-drill.sh — this test proves the
// SAME refusal and restore behavior at the db.Migrate boundary, on every `go
// test` run, without a docker dependency.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// rollbackDrill08Migrations lists the fixture files in apply order (they are
// independent of each other's content, but 0066 < 0067 < 0068 mirrors the
// order an 0.8 wardynd actually applied them in).
var rollbackDrill08Migrations = []string{
	"0066_devices_and_federation.sql",
	"0067_user_drives_object_scheme.sql",
	"0068_aws_sso_spent_tokens.sql",
}

// applyRealV08Migrations records schema_migrations rows for each fixture and
// runs its SQL, in a schema already carrying a complete 0.7.13 install
// (probeSchemaPool). It never touches migrationFS: production Migrate() must
// not gain any way to see these files.
func applyRealV08Migrations(t *testing.T, pool migrationExecutor) {
	t.Helper()
	ctx := context.Background()
	for _, name := range rollbackDrill08Migrations {
		data, err := os.ReadFile(filepath.Join("testdata", "rollback_drill_08_migrations", name))
		if err != nil {
			t.Fatalf("read 0.8 fixture %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatalf("apply 0.8 fixture %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
			t.Fatalf("record 0.8 fixture %s applied: %v", name, err)
		}
	}
}

// schemaMigrationsSnapshot returns every recorded filename, sorted, so the
// refusal path can be proven to have written nothing (same row set before and
// after) rather than just asserting the returned error.
func schemaMigrationsSnapshot(t *testing.T, pool migrationExecutor) []string {
	t.Helper()
	ctx := context.Background()
	var names []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(filename ORDER BY filename), ARRAY[]::text[]) FROM schema_migrations`).Scan(&names); err != nil {
		t.Fatalf("snapshot schema_migrations: %v", err)
	}
	sort.Strings(names)
	return names
}

// TestPG_RollbackDrill_RefusesRealV08MigrationsWithoutWriting is the DOWN half
// (#1002): 0.7.13's Migrate() against a database carrying real 0.8-only
// migrations refuses, names the newest one, and leaves schema_migrations
// byte-for-byte unchanged.
func TestPG_RollbackDrill_RefusesRealV08MigrationsWithoutWriting(t *testing.T) {
	pool, _ := probeSchemaPool(t) // a complete, fresh 0.7.13 schema
	ctx := context.Background()

	applyRealV08Migrations(t, pool)
	before := schemaMigrationsSnapshot(t, pool)

	err := Migrate(ctx, pool)
	if err == nil {
		t.Fatal("Migrate booted over a database carrying real 0.8 migrations (devices/federation, user_drives object_scheme, aws_sso_spent_tokens); want a refusal")
	}
	for _, want := range []string{
		`"0068_aws_sso_spent_tokens.sql"`, // newest unknown, named
		"3 migration(s)",                  // exactly the three fixtures, nothing else miscounted
		"downgrade is unsupported",
		"restore the pre-upgrade dump",
		"WARDYN_ALLOW_UNKNOWN_MIGRATIONS",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}

	after := schemaMigrationsSnapshot(t, pool)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("the refusal changed schema_migrations: before %v, after %v — it must write nothing", before, after)
	}
}

// TestPG_RollbackDrill_RestoredPreUpgradeSchemaBootsClean is the ROLLBACK half
// (#1002): the refusal message's own remedy — "restore the pre-upgrade dump"
// — modeled as a fresh schema carrying only what 0.7.13 ships. Migrate() must
// succeed, and none of the three 0.8-only tables/columns may exist: a restore
// that left 0.8 artifacts behind would not be the rollback docs/OPERATIONS.md
// documents.
func TestPG_RollbackDrill_RestoredPreUpgradeSchemaBootsClean(t *testing.T) {
	pool, schema := probeSchemaPool(t) // models "the pre-upgrade dump, restored"
	ctx := context.Background()

	// probeSchemaPool already ran Migrate() once to build the schema; running
	// it again is the boot every restore is followed by, and must be a clean
	// no-op — the same idempotency every other Migrate() caller in this
	// package relies on, just spelled out here because this test's whole point
	// is "the restored database still boots".
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate on the restored pre-upgrade schema: %v", err)
	}

	for _, table := range []string{"devices", "device_enrolment_tokens", "org_federation", "aws_sso_spent_tokens"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relname = $2)`, schema, table).Scan(&exists); err != nil {
			t.Fatalf("look up %s: %v", table, err)
		}
		if exists {
			t.Errorf("restored pre-upgrade schema has 0.8-only table %s — the restore is not clean", table)
		}
	}
	var hasObjectScheme bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_schema = $1 AND table_name = 'user_drives' AND column_name = 'object_scheme')`,
		schema).Scan(&hasObjectScheme); err != nil {
		t.Fatalf("look up user_drives.object_scheme: %v", err)
	}
	if hasObjectScheme {
		t.Error("restored pre-upgrade schema has 0.8-only user_drives.object_scheme — the restore is not clean")
	}
}
