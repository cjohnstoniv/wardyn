// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"
)

const serviceAttachMigration = "0167"

// TestMigration0167_RestrictsServiceAttach: the migration writes the capability_restrictions row that keeps
// service_attach off until a grant names it, over a database that already holds data; and it leaves alone a row
// an admin already wrote.
func TestMigration0167_RestrictsServiceAttach(t *testing.T) {
	pool, _ := partialSchemaPool(t, serviceAttachMigration)
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM capability_restrictions WHERE capability = 'feature' AND value = 'service_attach'`); n != 0 {
		t.Fatalf("the restriction exists before the migration: %d rows", n)
	}
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate applying 0167: %v", err)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM capability_restrictions WHERE capability = 'feature' AND value = 'service_attach'`); n != 1 {
		t.Errorf("capability_restrictions holds %d feature/service_attach rows after the migration, want 1", n)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM capability_restrictions`); n != 1 {
		t.Errorf("the migration wrote %d restriction rows, want only service_attach", n)
	}
}

// TestMigration0167_KeepsAnExistingRow: a row that is already there keeps its author and its timestamp.
func TestMigration0167_KeepsAnExistingRow(t *testing.T) {
	pool, _ := partialSchemaPool(t, serviceAttachMigration)
	pgExec(t, pool, `INSERT INTO capability_restrictions (capability, value, created_by, created_at)
		VALUES ('feature', 'service_attach', 'admin', '2026-01-01T00:00:00Z')`)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate applying 0167: %v", err)
	}
	if by := pgScalar[string](t, pool, `SELECT created_by FROM capability_restrictions WHERE capability = 'feature' AND value = 'service_attach'`); by != "admin" {
		t.Errorf("created_by = %q after the migration, want the admin's row untouched", by)
	}
}
