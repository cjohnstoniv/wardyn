// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestGovernanceChangeTreeOps: a held governance change (one per key) may carry op move or op promote, an op
// outside the set is still refused, and migrating again applies nothing.
func TestGovernanceChangeTreeOps(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	insert := func(op string) error {
		_, err := pool.Exec(ctx, `INSERT INTO governance_changes (id, target_kind, op, target_key, payload, diff, base_hash, deployment_hash, proposed_by, expires_at)
			VALUES ($1, 'governance_membership', $2, $2, '{}', '{}', 'h', 'd', 'bob', now() + interval '1 day')`, uuid.New(), op)
		return err
	}
	for _, op := range []string{"move", "promote", "upsert", "delete"} {
		if err := insert(op); err != nil {
			t.Errorf("a held change with op %s: %v", op, err)
		}
	}
	if err := insert("rename"); err == nil {
		t.Error("a held change with op rename was stored; the op CHECK no longer closes the set")
	}
	var before, after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&after); err != nil || after != before {
		t.Errorf("migrating again recorded %d migrations, was %d (%v): want none applied", after, before, err)
	}
}
