// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for migration 0050 (per-principal secrets): a pre-0050
// `secrets` row (bare `name` primary key) must land at owned_by=” once 0050
// runs, TWO rows sharing a name under DIFFERENT owners must both be
// admissible (the whole point of widening the primary key), and the primary
// key must still refuse a genuine duplicate WITHIN one owner — the negative
// control that proves 0050 widened the key rather than dropping it.
//
// Guarded by WARDYN_TEST_PG (via throwawayDatabase, which Skips cleanly when
// unset), mirroring store_source_migration_pg_test.go's databaseBefore shape.
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const secretOwnedByMigration = "0050_secret_owned_by.sql"

// insertPreOwnedBySecret inserts a pre-0050 `secrets` row: the bare (name)
// shape 0001_init.sql created, before owned_by existed.
func insertPreOwnedBySecret(t *testing.T, pool *pgxpool.Pool, name string, ciphertext []byte) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO secrets (name, ciphertext) VALUES ($1, $2)`, name, ciphertext,
	); err != nil {
		t.Fatalf("seed pre-0050 secret %q: %v", name, err)
	}
}

// readSecretOwner reads the owned_by column for one (owned_by, name) row.
func readSecretOwner(t *testing.T, pool *pgxpool.Pool, ownedBy, name string) (found bool) {
	t.Helper()
	var got string
	err := pool.QueryRow(context.Background(),
		`SELECT owned_by FROM secrets WHERE owned_by=$1 AND name=$2`, ownedBy, name,
	).Scan(&got)
	if err != nil {
		return false
	}
	return got == ownedBy
}

// TestMigration0050_PreExistingRowsOwnedByEmpty seeds a row shaped exactly as
// every secret was before 0.7 (bare name, no owned_by column at all), applies
// 0050, and asserts the row now reads owned_by=” -- operator-owned, i.e.
// exactly today's behavior for every pre-0.7 secret.
func TestMigration0050_PreExistingRowsOwnedByEmpty(t *testing.T) {
	pool := databaseBefore(t, secretOwnedByMigration)
	insertPreOwnedBySecret(t, pool, "anthropic-api-key", []byte("pre-migration-ciphertext"))

	execMigrationFile(t, pool, secretOwnedByMigration)

	var ownedBy string
	if err := pool.QueryRow(context.Background(),
		`SELECT owned_by FROM secrets WHERE name=$1`, "anthropic-api-key",
	).Scan(&ownedBy); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if ownedBy != "" {
		t.Errorf("owned_by = %q, want \"\" (operator-owned) for a pre-0050 row", ownedBy)
	}
}

// TestMigration0050_TwoOwnersSameName_Admitted is the migration's whole point:
// once the primary key is (owned_by, name), an operator row and a member row
// sharing the SAME secret name are both admissible and independently
// readable. Keeping `name` alone as the key (the pre-0050 shape) would refuse
// the second INSERT outright -- this is the finding 0050 closes.
func TestMigration0050_TwoOwnersSameName_Admitted(t *testing.T) {
	pool := databaseBefore(t, secretOwnedByMigration)
	execMigrationFile(t, pool, secretOwnedByMigration)

	const name = "anthropic-api-key"
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO secrets (owned_by, name, ciphertext) VALUES ('', $1, $2)`, name, []byte("operator-ciphertext"),
	); err != nil {
		t.Fatalf("insert operator row: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO secrets (owned_by, name, ciphertext) VALUES ('alice', $1, $2)`, name, []byte("alice-ciphertext"),
	); err != nil {
		t.Fatalf("insert member row sharing the operator's secret name: %v (0050 should admit two owners x one name)", err)
	}

	if !readSecretOwner(t, pool, "", name) {
		t.Error("operator row is gone after the member row was inserted")
	}
	if !readSecretOwner(t, pool, "alice", name) {
		t.Error("member row is gone after insertion")
	}
}

// TestMigration0050_DuplicateOperatorRow_StillConflicts is the negative
// control for the test above: 0050 WIDENS the primary key, it does not drop
// it. A second INSERT at the SAME (owned_by, name) pair must still violate
// the primary key -- if it did not, the "admitted" test above would prove
// nothing (a store with no key at all would just as happily take both rows).
func TestMigration0050_DuplicateOperatorRow_StillConflicts(t *testing.T) {
	pool := databaseBefore(t, secretOwnedByMigration)
	execMigrationFile(t, pool, secretOwnedByMigration)

	const name = "anthropic-api-key"
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO secrets (owned_by, name, ciphertext) VALUES ('', $1, $2)`, name, []byte("first"),
	); err != nil {
		t.Fatalf("insert first operator row: %v", err)
	}
	_, err := pool.Exec(context.Background(),
		`INSERT INTO secrets (owned_by, name, ciphertext) VALUES ('', $1, $2)`, name, []byte("second"),
	)
	if err == nil {
		t.Fatal("a second INSERT at the SAME (owned_by, name) succeeded; the primary key was dropped, not widened")
	}
}
