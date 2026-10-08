// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func restrictedOf(t *testing.T, st store.PG, capability string, id uuid.UUID) bool {
	t.Helper()
	got, err := st.ListCapabilityRestrictions(context.Background())
	if err != nil {
		t.Fatalf("list restrictions: %v", err)
	}
	return got[capability][id.String()]
}

// TestPG_Components_CreateRestrictedIsOneTransaction: the row and its restriction
// commit together, and a create that fails takes the restriction with it.
func TestPG_Components_CreateRestrictedIsOneTransaction(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	const kind = "component"

	id := uuid.New()
	t.Cleanup(func() {
		_, _ = st.Pool.Exec(context.Background(), `DELETE FROM components WHERE id = $1`, id)
		_ = st.SetCapabilityRestriction(context.Background(), kind, id.String(), false, "test")
	})
	name := "org-" + uuid.NewString()
	c, err := st.CreateRestrictedComponent(ctx, types.Component{ID: id, Name: name, Definition: testComponentDefinition()}, kind, "admin-1")
	if err != nil {
		t.Fatalf("create restricted: %v", err)
	}
	if c.Version != 1 || c.Owner != "" || c.Name != name {
		t.Fatalf("row = %+v, want an org row at version 1", c)
	}
	if !restrictedOf(t, st, kind, id) {
		t.Fatal("the row exists but its restriction does not")
	}

	// A refused create leaves nothing restricted: same name under the org owner, new id.
	loser := uuid.New()
	t.Cleanup(func() { _ = st.SetCapabilityRestriction(context.Background(), kind, loser.String(), false, "test") })
	if _, err := st.CreateRestrictedComponent(ctx, types.Component{ID: loser, Name: name}, kind, "admin-1"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name: err = %v, want ErrConflict", err)
	}
	if restrictedOf(t, st, kind, loser) {
		t.Fatal("a create the store refused left its restriction behind")
	}
	if _, err := st.GetComponent(ctx, loser, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused create left a row: %v", err)
	}

	if _, err := st.CreateRestrictedComponent(ctx, types.Component{ID: uuid.New(), Owner: "alice", Name: "x"}, kind, "admin-1"); err == nil {
		t.Fatal("a person's row was created through the org path")
	}
}
