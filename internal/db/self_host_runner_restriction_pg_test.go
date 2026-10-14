// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"
)

const selfHostRunnerMigration = "0178"

// TestMigration0178_RestrictsSelfHostRunner: the migration writes the capability_restrictions row that
// keeps self_host_runner off until a grant names it, beside the 0167 row, over a database that already
// holds data; and it leaves alone a row an admin already wrote.
func TestMigration0178_RestrictsSelfHostRunner(t *testing.T) {
	pool, _ := partialSchemaPool(t, selfHostRunnerMigration)
	const q = `SELECT count(*)::int FROM capability_restrictions WHERE capability = 'feature' AND value = 'self_host_runner'`
	if n := pgScalar[int](t, pool, q); n != 0 {
		t.Fatalf("the restriction exists before the migration: %d rows", n)
	}
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate applying 0178: %v", err)
	}
	if n := pgScalar[int](t, pool, q); n != 1 {
		t.Errorf("capability_restrictions holds %d feature/self_host_runner rows after the migration, want 1", n)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM capability_restrictions`); n != 2 {
		t.Errorf("capability_restrictions holds %d rows, want service_attach and self_host_runner only", n)
	}
}

// TestMigration0178_KeepsAnExistingRow: a row that is already there keeps its author.
func TestMigration0178_KeepsAnExistingRow(t *testing.T) {
	pool, _ := partialSchemaPool(t, selfHostRunnerMigration)
	pgExec(t, pool, `INSERT INTO capability_restrictions (capability, value, created_by) VALUES ('feature', 'self_host_runner', 'admin')`)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate applying 0178: %v", err)
	}
	if by := pgScalar[string](t, pool, `SELECT created_by FROM capability_restrictions WHERE capability = 'feature' AND value = 'self_host_runner'`); by != "admin" {
		t.Errorf("created_by = %q after the migration, want the admin's row untouched", by)
	}
}
