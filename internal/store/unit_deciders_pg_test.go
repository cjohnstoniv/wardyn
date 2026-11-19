// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Unit deciders (migration governance_unit_deciders) against Postgres: the one row per unit, the
// clear on empty rules, the cascade with the unit and a row a newer build wrote. Every case runs on
// its own throwaway database.
package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestUnitDeciders(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	a, a1, _, a1x, _ := unitAdminTree(t, pg)

	if _, err := store.GetUnitDecidersQ(ctx, pool, a.ID, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a unit with no row: err = %v, want ErrNotFound", err)
	}
	rules := []types.DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"owner", "group:eng"}, MaxScope: types.ScopeRun}}
	saved, err := store.SetUnitDecidersQ(ctx, pool, a.ID, rules, "sec-1")
	if err != nil || saved.UnitID != a.ID || saved.UpdatedBy != "sec-1" || saved.UpdatedAt.IsZero() || len(saved.Rules) != 1 {
		t.Fatalf("set = %+v, %v", saved, err)
	}
	// A second set replaces the row; it does not add one.
	rules2 := []types.DeciderRule{{Kinds: []string{"mcp_call"}, Who: []string{"unit_admins"}, SecondHuman: true}}
	if _, err := store.SetUnitDecidersQ(ctx, pool, a.ID, rules2, "sec-2"); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetUnitDecidersQ(ctx, pool, a.ID, true)
	if err != nil || got.UpdatedBy != "sec-2" || len(got.Rules) != 1 || !slices.Equal(got.Rules[0].Kinds, []string{"mcp_call"}) || !got.Rules[0].SecondHuman {
		t.Fatalf("after replace = %+v, %v", got, err)
	}
	if _, err := store.SetUnitDecidersQ(ctx, pool, a1.ID, rules, "sec-1"); err != nil {
		t.Fatal(err)
	}
	all, err := store.ListUnitDecidersQ(ctx, pool)
	if err != nil || len(all) != 2 {
		t.Fatalf("list = %d rows, %v, want 2", len(all), err)
	}
	if listed, err := pg.ListUnitDeciders(ctx); err != nil || len(listed) != 2 {
		t.Errorf("ListUnitDeciders = %d, %v, want 2", len(listed), err)
	}

	// Empty rules clear the row.
	if _, err := store.SetUnitDecidersQ(ctx, pool, a.ID, nil, "sec-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetUnitDecidersQ(ctx, pool, a.ID, false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after clearing: err = %v, want ErrNotFound", err)
	}
	// An unknown unit is ErrNotFound, not a foreign-key error.
	if _, err := store.SetUnitDecidersQ(ctx, pool, uuid.New(), rules, "sec-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown unit: err = %v, want ErrNotFound", err)
	}
	// The row goes with its unit. A unit with a child cannot be deleted, so a1's child goes first.
	for _, id := range []uuid.UUID{a1x.ID, a1.ID} {
		if _, err := pool.Exec(ctx, `DELETE FROM governance_units WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := store.ListUnitDecidersQ(ctx, pool); len(left) != 0 {
		t.Errorf("rows left after the unit was deleted: %v", left)
	}
}

// A row a newer build wrote reads back: an unknown kind, type and field survive the decode, so the
// resolver can match none of it rather than failing the read.
func TestUnitDecidersReadsUnknownStoredValues(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	a, _, _, _, _ := unitAdminTree(t, store.NewPG(pool))
	if _, err := pool.Exec(ctx, `INSERT INTO governance_unit_deciders (unit_id, rules, updated_by)
		VALUES ($1, '[{"kinds":["future_kind"],"who":["manager","owner"],"quorum":2}]', 'newer')`, a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetUnitDecidersQ(ctx, pool, a.ID, false)
	if err != nil || len(got.Rules) != 1 || got.Rules[0].Kinds[0] != "future_kind" || !slices.Equal(got.Rules[0].Who, []string{"manager", "owner"}) {
		t.Fatalf("read = %+v, %v, want the stored values untouched", got, err)
	}
}
