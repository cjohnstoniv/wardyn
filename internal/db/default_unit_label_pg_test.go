// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultUnitID is the reserved Default unit (store.DefaultUnitID), inserted by 0208.
const defaultUnitID = "00000000-0000-0000-0000-000000000001"

// unitNames returns every unit's id -> name.
func unitNames(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text, name FROM governance_units`)
	if err != nil {
		t.Fatalf("read units: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scan unit: %v", err)
		}
		out[id] = name
	}
	return out
}

// migrateWithUnits seeds units with these names under 0208, runs the real Migrate(),
// and checks the reserved Default unit took want and every seeded unit kept its id and name.
func migrateWithUnits(t *testing.T, seeded []string, want string) (*pgxpool.Pool, map[string]string) {
	t.Helper()
	pool, _ := partialSchemaPool(t, "0208") // 0207 is the last applied: units exist, no tree yet
	ctx := context.Background()
	before := map[string]string{}
	for _, name := range seeded {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO governance_units (id, name, ceiling, limits)
			VALUES (gen_random_uuid(), $1, '{}', '{}') RETURNING id::text`, name).Scan(&id); err != nil {
			t.Fatalf("seed unit %q: %v", name, err)
		}
		before[id] = name
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate with units %q: %v", seeded, err)
	}
	after := unitNames(t, ctx, pool)
	if got := after[defaultUnitID]; got != want {
		t.Errorf("reserved Default unit is named %q, want %q (units: %v)", got, want, after)
	}
	for id, name := range before {
		if after[id] != name {
			t.Errorf("seeded unit %s is %q after Migrate, want %q", id, after[id], name)
		}
	}
	if len(after) != len(before)+1 {
		t.Errorf("%d units after Migrate, want %d", len(after), len(before)+1)
	}
	return pool, after
}

func TestDefaultUnitLabel_DefaultAndDeploymentTaken(t *testing.T) {
	migrateWithUnits(t, []string{"Default", "Default (deployment)"}, "Default (deployment 2)")
}

func TestDefaultUnitLabel_ThreeNamesTaken(t *testing.T) {
	migrateWithUnits(t, []string{"Default", "Default (deployment)", "Default (deployment 2)"}, "Default (deployment 3)")
}

func TestDefaultUnitLabel_OnlyDefaultTaken(t *testing.T) {
	migrateWithUnits(t, []string{"Default"}, "Default (deployment)")
}

func TestDefaultUnitLabel_NoneTaken(t *testing.T) {
	migrateWithUnits(t, nil, "Default")
}

func TestDefaultUnitLabel_ReentryChangesNothing(t *testing.T) {
	pool, after := migrateWithUnits(t, []string{"Default", "Default (deployment)"}, "Default (deployment 2)")
	ctx := context.Background()
	// Migrate() again is a no-op over the recorded set; re-applying 0208 itself must be one too.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, readMigration(t, "0208_governance_unit_tree.sql")); err != nil {
		t.Fatalf("re-apply 0208: %v", err)
	}
	again := unitNames(t, ctx, pool)
	if len(again) != len(after) {
		t.Errorf("%d units after re-entry, want %d", len(again), len(after))
	}
	for id, name := range after {
		if again[id] != name {
			t.Errorf("unit %s is %q after re-entry, want %q", id, again[id], name)
		}
	}
}
