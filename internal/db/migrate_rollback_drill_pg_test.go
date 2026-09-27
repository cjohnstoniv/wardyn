// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// The #1002 rollback drill: 0.8 -> 0.7.13 (release/0.7's own version line).
// unknownAppliedMigrations and migrateOn's refusal (db.go) already have a
// unit-shaped pin — TestMigrateRefusesUnknownAppliedMigrations — but it
// stages two PLACEHOLDER filenames ("9998_...", "9999_..."). This file proves
// the same refusal against a REAL 0.8 install: three additive migrations
// copied verbatim from main's internal/db/migrations (0066, 0067, 0068 —
// devices/federation, the user_drives object_scheme column, and the AWS SSO
// spent-token table), fixed under testdata/rollback_drill_08_migrations so
// this branch never has to track main's numbering, PLUS the rename that makes
// this a genuine 0.8 install rather than "0.7.13 plus three extra files":
// main's own secret-envelope migration is 0069_secret_envelope_v1.sql, and it
// is byte-identical to this branch's own 0065_secret_envelope_v1.sql (see
// a84f21681's commit message — main renumbered, it did not rewrite). A real
// database an 0.8 wardynd migrated therefore has NO "0065_secret_envelope_v1.sql"
// row at all; it has "0069_…" instead. That is reproduced here by renaming the
// row probeSchemaPool's Migrate() call already wrote, which leaves this
// branch's OWN 0065 file looking UNAPPLIED to this branch's Migrate() — the
// condition that makes "the refusal writes nothing" a real assertion rather
// than a vacuous one (a schema with every one of this branch's own migrations
// already recorded has nothing left for a write-before-refuse bug to write).
//
// A full binary-level drill (build wardynd, point it at a throwaway
// postgres:17, exercise the documented pg_dump/restore commands with the same
// fixtures) lives in scripts/rollback-drill.sh — including the actual restore
// step, which this package cannot do (no docker access from a `go test`
// binary). TestPG_RollbackDrill_FreshPreUpgradeSchemaCarriesNo08Objects below
// checks only what it says: a from-scratch 0.7.13 schema has none of the
// 0.8-only objects. It is a sanity guard for the fixtures, NOT a restore test.

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

// modelRealV08Install turns a complete, freshly-migrated 0.7.13 schema
// (probeSchemaPool) into what a REAL 0.8 install left behind: it renames the
// recorded 0065_secret_envelope_v1.sql row to 0069_secret_envelope_v1.sql (the
// same migration, main's filename — byte-identical content, confirmed by
// `cmp` against main's copy), then applies the three 0.8-only fixtures for
// real and records them under their real filenames. It never touches
// migrationFS: production Migrate() must not gain any way to see these files.
func modelRealV08Install(t *testing.T, pool migrationExecutor) {
	t.Helper()
	ctx := context.Background()
	if tag, err := pool.Exec(ctx,
		`UPDATE schema_migrations SET filename = '0069_secret_envelope_v1.sql' WHERE filename = '0065_secret_envelope_v1.sql'`); err != nil {
		t.Fatalf("rename 0065_secret_envelope_v1.sql to 0069_ (modeling main's renumbering): %v", err)
	} else if tag.RowsAffected() != 1 {
		t.Fatalf("expected exactly one 0065_secret_envelope_v1.sql row to rename, affected %d — probeSchemaPool's schema shape changed?", tag.RowsAffected())
	}
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
// (#1002): 0.7.13's Migrate() against a database a real 0.8 install left
// behind refuses, names the newest unknown migration, and leaves
// schema_migrations byte-for-byte unchanged — including NOT applying its own
// 0065_secret_envelope_v1.sql, which this database genuinely has pending
// (see modelRealV08Install).
func TestPG_RollbackDrill_RefusesRealV08MigrationsWithoutWriting(t *testing.T) {
	pool, _ := probeSchemaPool(t) // a complete, fresh 0.7.13 schema
	ctx := context.Background()

	modelRealV08Install(t, pool)
	before := schemaMigrationsSnapshot(t, pool)

	err := Migrate(ctx, pool)
	if err == nil {
		t.Fatal("Migrate booted over a database a real 0.8 install left behind (renamed secret-envelope row, devices/federation, user_drives object_scheme, aws_sso_spent_tokens); want a refusal")
	}
	for _, want := range []string{
		`"0069_secret_envelope_v1.sql"`, // newest unknown (COLLATE "C": 0069 > 0068), named
		"4 migration(s)",                // 0066, 0067, 0068, and the renamed secret-envelope row
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
		t.Fatalf("the refusal changed schema_migrations: before %v, after %v — it must write nothing, including not applying "+
			"its own 0065_secret_envelope_v1.sql even though this database has no row recorded under that name", before, after)
	}
}

// TestPG_RollbackDrill_FreshPreUpgradeSchemaCarriesNo08Objects is a sanity
// guard on the fixtures, NOT a restore test: it checks that a from-scratch
// 0.7.13 schema (what "restore the pre-upgrade dump" should get an operator
// back to) has none of the 0.8-only objects the fixtures add. The actual
// pg_dump/restore round-trip this proves is meant to model runs only in
// scripts/rollback-drill.sh's step 4, against a real postgres:17 and a real
// built binary — a `go test` in this package has no docker access to do that
// itself.
func TestPG_RollbackDrill_FreshPreUpgradeSchemaCarriesNo08Objects(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	for _, table := range []string{"devices", "device_enrolment_tokens", "org_federation", "aws_sso_spent_tokens"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relname = $2)`, schema, table).Scan(&exists); err != nil {
			t.Fatalf("look up %s: %v", table, err)
		}
		if exists {
			t.Errorf("a fresh 0.7.13 schema already has 0.8-only table %s — the fixture set is not additive-only over this branch", table)
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
		t.Error("a fresh 0.7.13 schema already has 0.8-only user_drives.object_scheme — the fixture set is not additive-only over this branch")
	}
}
