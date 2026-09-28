// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"
)

// TestPG_RoleMappingsMarkMigratedFromMember (#913) applies 0074's rename and
// migration 0098 in the SAME Migrate() call — the shape every real upgrade
// takes, since 0074 ships in no release tag. Floored at 0073 (below 0074), a
// 'member' row (any created_at: the marker no longer looks at the column)
// lands, 0074 rewrites it to role='user'/user_type='standard', and 0098 must
// mark that exact row. A row saved fresh AFTER Migrate() has already run —
// the only state 0098 is not present to mark FALSE by construction, since it
// never runs a second time — must NOT carry the marker.
func TestPG_RoleMappingsMarkMigratedFromMember(t *testing.T) {
	const floor = "0074"
	pool, _ := partialSchemaPool(t, floor)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO role_mappings (id, value, role) VALUES (gen_random_uuid(), 'eng-team', 'member')`); err != nil {
		t.Fatalf("seed the member row: %v", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying %s+ : %v", floor, err)
	}

	var role, userType string
	var migrated bool
	if err := pool.QueryRow(ctx,
		`SELECT role, user_type, migrated_from_member FROM role_mappings WHERE value = 'eng-team'`).Scan(&role, &userType, &migrated); err != nil {
		t.Fatalf("read the rewritten row: %v", err)
	}
	if role != "user" || userType != "standard" || !migrated {
		t.Errorf("eng-team = role %q user_type %q migrated_from_member %v, want user/standard/true", role, userType, migrated)
	}

	// A genuinely fresh Standard-user save, made AFTER 0098 already ran —
	// nothing here should ever retroactively mark it.
	if _, err := pool.Exec(ctx,
		`INSERT INTO role_mappings (id, value, role, user_type) VALUES (gen_random_uuid(), 'new-hire', 'user', 'standard')`); err != nil {
		t.Fatalf("seed the post-migrate row: %v", err)
	}
	var freshMigrated bool
	if err := pool.QueryRow(ctx,
		`SELECT migrated_from_member FROM role_mappings WHERE value = 'new-hire'`).Scan(&freshMigrated); err != nil {
		t.Fatalf("read the fresh row: %v", err)
	}
	if freshMigrated {
		t.Error("new-hire migrated_from_member = true, want false — it was saved after 0098 already ran")
	}
}
