// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
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

const selfHostGrant = `SELECT count(*)::int FROM capability_grants WHERE subject_type = 'all' AND subject = '' AND capability = 'feature' AND value = 'self_host_runner' AND effect = 'allow' AND created_by = 'migration:0178'`

// seedRunnerRow stores a runner owned by sub-person whose registration token minter minted.
func seedRunnerRow(t *testing.T, pool *pgxpool.Pool, state, minter string) {
	t.Helper()
	pgExec(t, pool, `INSERT INTO runners (id, owner, public_key, key_fingerprint, state, max_runs, minted_by) VALUES (gen_random_uuid(), 'sub-person', decode(repeat('ab', 32), 'hex'), md5(random()::text), $1, 2, $2)`, state, minter)
}

// TestMigration0178_KeepsSelfHostingWhereASelfMintedRunnerIsClaimed: upgrading an estate where someone
// claimed a runner whose token they minted themself adds the all-allow, since 0.9.0 could not stage it.
func TestMigration0178_KeepsSelfHostingWhereASelfMintedRunnerIsClaimed(t *testing.T) {
	pool, _ := partialSchemaPool(t, selfHostRunnerMigration)
	seedRunnerRow(t, pool, "revoked", "sub-person")
	seedRunnerRow(t, pool, "claimed", "sub-person")
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate applying 0178: %v", err)
	}
	if n := pgScalar[int](t, pool, selfHostGrant); n != 1 {
		t.Errorf("capability_grants holds %d migration:0178 self_host_runner allows, want 1", n)
	}
}

// TestMigration0178_StartsClosedWithoutASelfMintedClaim: only revoked runners, an unclaimed self-minted
// one, or a claimed one an admin minted for the person (minted_by is not the owner) add no grant.
func TestMigration0178_StartsClosedWithoutASelfMintedClaim(t *testing.T) {
	pool, _ := partialSchemaPool(t, selfHostRunnerMigration)
	seedRunnerRow(t, pool, "revoked", "sub-person")
	seedRunnerRow(t, pool, "unclaimed", "sub-person")
	seedRunnerRow(t, pool, "claimed", "sub-admin")
	seedRunnerRow(t, pool, "claimed", "")
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate applying 0178: %v", err)
	}
	if n := pgScalar[int](t, pool, `SELECT count(*)::int FROM capability_grants WHERE value = 'self_host_runner'`); n != 0 {
		t.Errorf("capability_grants holds %d self_host_runner rows for an estate with no self-minted claim, want 0", n)
	}
}

// TestMigration0178_KeepsAnAdminsGrantRow: a grant row already on the key keeps its effect and author.
func TestMigration0178_KeepsAnAdminsGrantRow(t *testing.T) {
	pool, _ := partialSchemaPool(t, selfHostRunnerMigration)
	seedRunnerRow(t, pool, "claimed", "sub-person")
	pgExec(t, pool, `INSERT INTO capability_grants (id, subject_type, subject, capability, value, effect, created_by) VALUES (gen_random_uuid(), 'all', '', 'feature', 'self_host_runner', 'deny', 'admin')`)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("Migrate applying 0178: %v", err)
	}
	if got := pgScalar[string](t, pool, `SELECT effect || '/' || created_by FROM capability_grants WHERE value = 'self_host_runner'`); got != "deny/admin" {
		t.Errorf("grant row = %q after the migration, want deny/admin untouched", got)
	}
}
