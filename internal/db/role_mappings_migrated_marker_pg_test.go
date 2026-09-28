// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"
	"time"
)

// migration0098Cutoff mirrors 0098_role_mappings_migrated_from_member.sql's own
// hardcoded backfill cutoff (0074's landing commit) — kept as a named constant
// here rather than a magic literal repeated across the seeds below.
var migration0098Cutoff = time.Date(2026, 9, 23, 22, 20, 9, 0, time.UTC)

// TestPG_RoleMappingsMarkMigratedFromMember (#913) applies migration 0098 over
// a database holding: a role_mappings row already rewritten by 0074's rename
// (a 'user'/'standard' row whose created_at predates the backfill cutoff —
// the only state 0074's rewrite can produce before that date, since nothing
// after 0074 applies can ever be 'member') and a row an admin saved as
// Standard user on purpose after the cutoff. Only the first should come out
// marked.
func TestPG_RoleMappingsMarkMigratedFromMember(t *testing.T) {
	const floor = "0098"
	pool, _ := partialSchemaPool(t, floor)
	ctx := context.Background()
	before, after := migration0098Cutoff.Add(-24*time.Hour), migration0098Cutoff.Add(24*time.Hour)

	// Pre-cutoff state: a 'member' row landed, then 0074 (already applied by
	// partialSchemaPool) rewrote it in place — its created_at is the ORIGINAL
	// row's, well before the cutoff.
	if _, err := pool.Exec(ctx,
		`INSERT INTO role_mappings (id, value, role, user_type, created_at)
		 VALUES (gen_random_uuid(), 'eng-team', 'user', 'standard', $1)`, before); err != nil {
		t.Fatalf("seed the pre-cutoff row: %v", err)
	}
	// A genuinely fresh Standard-user save, created after the cutoff —
	// nothing 0098 should ever touch.
	if _, err := pool.Exec(ctx,
		`INSERT INTO role_mappings (id, value, role, user_type, created_at)
		 VALUES (gen_random_uuid(), 'new-hire', 'user', 'standard', $1)`, after); err != nil {
		t.Fatalf("seed the fresh row: %v", err)
	}
	// An admin row, untouched by the rename and irrelevant to the marker.
	if _, err := pool.Exec(ctx,
		`INSERT INTO role_mappings (id, value, role, created_at) VALUES (gen_random_uuid(), 'ops-team', 'admin', $1)`,
		before); err != nil {
		t.Fatalf("seed the admin row: %v", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying %s+ : %v", floor, err)
	}

	for _, c := range []struct {
		value string
		want  bool
	}{
		{"eng-team", true},
		{"new-hire", false},
		{"ops-team", false},
	} {
		var got bool
		if err := pool.QueryRow(ctx, `SELECT migrated_from_member FROM role_mappings WHERE value = $1`, c.value).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", c.value, err)
		}
		if got != c.want {
			t.Errorf("%s migrated_from_member = %v, want %v", c.value, got, c.want)
		}
	}
}
