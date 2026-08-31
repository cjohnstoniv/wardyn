// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for console role mappings (migration 0051). Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
// Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleMapping builds an admin-authored row tagged with a per-test unique
// value, so the shared substrate's other rows (this table is global — there
// is no per-run scoping) can never make an assertion here pass or fail by
// accident.
func roleMapping(value, role string) types.RoleMapping {
	return types.RoleMapping{Value: value, Role: role, CreatedBy: "admin@example.com"}
}

// TestPG_RoleMappings_UpsertFlipsInPlace mirrors
// TestPG_CapabilityGrants_UpsertFlipsInPlace: the UNIQUE(value) index is the
// whole reason a re-add is safe. Adding "eng-team=member" then re-adding
// "eng-team=admin" must leave ONE row carrying the new role and the ORIGINAL
// id — a fresh id on the return would hand the console a DELETE target that
// names no row.
func TestPG_RoleMappings_UpsertFlipsInPlace(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	value := "test-value-" + uuid.NewString()

	first, err := st.UpsertRoleMapping(ctx, roleMapping(value, "member"))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteRoleMapping(ctx, first.ID) })
	if first.ID == uuid.Nil {
		t.Fatal("insert returned the nil uuid; the console would have no delete target")
	}
	if first.CreatedAt.IsZero() || first.CreatedBy != "admin@example.com" {
		t.Errorf("insert = %+v, want a server-stamped created_at and the caller's created_by", first)
	}

	flipped, err := st.UpsertRoleMapping(ctx, roleMapping(value, "admin"))
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if flipped.ID != first.ID {
		t.Errorf("re-add id = %s, want the existing row's %s", flipped.ID, first.ID)
	}
	if flipped.Role != "admin" {
		t.Errorf("re-add role = %q, want admin", flipped.Role)
	}

	all, err := st.ListRoleMappings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := 0
	for _, m := range all {
		if m.Value == value {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("list has %d rows for %q, want exactly 1", got, value)
	}
}

// TestPG_RoleMappings_DeleteMissing: DELETE on an unknown id is ErrNotFound,
// so the CRUD route can answer 404 instead of a silent 204.
func TestPG_RoleMappings_DeleteMissing(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	if err := st.DeleteRoleMapping(context.Background(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_RoleMappings_ListOldestFirst pins the ordering ListRoleMappings
// promises (ORDER BY created_at) — the console's People table and the OIDC
// login-time merge both read the whole table in one call and neither should
// have to re-sort it.
func TestPG_RoleMappings_ListOldestFirst(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	prefix := "test-order-" + uuid.NewString() + "-"

	var ids []uuid.UUID
	for i, v := range []string{"a", "b", "c"} {
		m, err := st.UpsertRoleMapping(ctx, roleMapping(prefix+v, "member"))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, m.ID)
		t.Cleanup(func() { _ = st.DeleteRoleMapping(ctx, m.ID) })
	}

	all, err := st.ListRoleMappings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var mineIdx []int
	for i, m := range all {
		for _, id := range ids {
			if m.ID == id {
				mineIdx = append(mineIdx, i)
			}
		}
	}
	if len(mineIdx) != 3 {
		t.Fatalf("found %d of 3 seeded rows in the list", len(mineIdx))
	}
	if mineIdx[0] > mineIdx[1] || mineIdx[1] > mineIdx[2] {
		t.Errorf("list order = %v indices for insert order a,b,c; want ascending (oldest first)", mineIdx)
	}
}
