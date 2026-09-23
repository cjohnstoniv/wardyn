// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"strings"
	"testing"
)

// TestMigrateRefusesUnknownAppliedMigrations is the binary-side downgrade
// refusal: a database whose schema_migrations records files this binary does
// not ship was migrated by a NEWER wardynd, and boot must stop before writing
// anything, naming the newest unknown file. The break-glass boots anyway.
//
// A shipped migration is made pending as well, so "before writing anything"
// is observable: a refusal that came after the apply loop would re-record it.
func TestMigrateRefusesUnknownAppliedMigrations(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	names := readMigrationNames(t)
	last := names[len(names)-1]
	for _, q := range []string{
		`DELETE FROM schema_migrations WHERE filename = '` + last + `'`,
		`INSERT INTO schema_migrations (filename) VALUES ('9998_from_a_newer_wardynd.sql'), ('9999_newest_from_a_newer_wardynd.sql')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("stage a newer wardynd's schema in %s: %v", schema, err)
		}
	}
	isRecorded := func(name string) bool {
		t.Helper()
		ok, err := isMigrationApplied(ctx, pool, name)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	err := Migrate(ctx, pool)
	if err == nil {
		t.Fatal("Migrate booted over a database a newer wardynd migrated; want a refusal")
	}
	for _, want := range []string{`"9999_newest_from_a_newer_wardynd.sql"`, "2 migration(s)", "WARDYN_ALLOW_UNKNOWN_MIGRATIONS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
	if isRecorded(last) {
		t.Errorf("the refusal applied pending %s first; it must write nothing", last)
	}

	if err := MigrateAllowingUnknown(ctx, pool); err != nil {
		t.Fatalf("break-glass Migrate: %v", err)
	}
	if !isRecorded(last) {
		t.Errorf("break-glass Migrate did not apply pending %s", last)
	}
	if !isRecorded("9999_newest_from_a_newer_wardynd.sql") {
		t.Error("break-glass Migrate removed the newer wardynd's record")
	}
}
