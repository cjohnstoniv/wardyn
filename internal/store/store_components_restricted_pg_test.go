// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
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

// TestPG_Components_DeleteRestrictedLeavesTheIDClosed: the delete removes the org row and the
// grants naming it and leaves the id restricted, even where the restriction had been lifted. An id
// that is no org row's changes nothing, and a grant naming another id, or none, is kept.
func TestPG_Components_DeleteRestrictedLeavesTheIDClosed(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	const kind = "component"

	id, other, personal := uuid.New(), uuid.New(), uuid.New()
	subject := "c8-" + uuid.NewString()
	var grantIDs []uuid.UUID
	grant := func(value string, effect types.CapabilityEffect) {
		t.Helper()
		g, err := st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: subject, Capability: kind, Value: value, Effect: effect,
		})
		if err != nil {
			t.Fatalf("grant %s: %v", value, err)
		}
		grantIDs = append(grantIDs, g.ID)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = st.Pool.Exec(bg, `DELETE FROM components WHERE id = ANY($1)`, []uuid.UUID{id, personal})
		_, _ = st.Pool.Exec(bg, `DELETE FROM capability_grants WHERE id = ANY($1)`, grantIDs)
		for _, v := range []uuid.UUID{id, other, personal} {
			_ = st.SetCapabilityRestriction(bg, kind, v.String(), false, "test")
		}
	})
	name := "Org-" + uuid.NewString()
	if _, err := st.CreateRestrictedComponent(ctx, types.Component{ID: id, Name: name}, kind, "admin-1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A name that differs only by case is the same name, for a create and for a rename.
	if _, err := st.CreateRestrictedComponent(ctx, types.Component{ID: other, Name: strings.ToUpper(name)}, kind, "admin-1"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a case variant of a taken name: err = %v, want ErrConflict", err)
	}
	if restrictedOf(t, st, kind, other) {
		t.Fatal("the refused case-variant create left its restriction behind")
	}
	if _, err := st.CreateComponent(ctx, types.Component{ID: personal, Owner: subject, Name: strings.ToLower(name)}); err != nil {
		t.Fatalf("another owner's same name: %v", err)
	}

	grant(id.String(), types.CapabilityAllow)
	grant(other.String(), types.CapabilityAllow)
	grant("*", types.CapabilityDeny)
	if err := st.SetCapabilityRestriction(ctx, kind, id.String(), false, "sec-1"); err != nil {
		t.Fatalf("lift: %v", err)
	}

	for _, absent := range []uuid.UUID{other, personal} {
		if _, _, err := st.DeleteRestrictedComponent(ctx, absent, kind, "admin-1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("an id that is no org row's: err = %v, want ErrNotFound", err)
		}
		if restrictedOf(t, st, kind, absent) {
			t.Fatal("a delete that found no org row restricted its id")
		}
	}
	if _, err := st.GetComponent(ctx, personal, subject); err != nil {
		t.Fatalf("a person's row after the org delete of its id: %v", err)
	}

	deleted, n, err := st.DeleteRestrictedComponent(ctx, id, kind, "admin-1")
	if err != nil || deleted.ID != id || deleted.Name != name || n != 1 {
		t.Fatalf("delete = %+v, %d grants, %v; want the row and its one grant", deleted, n, err)
	}
	if !restrictedOf(t, st, kind, id) {
		t.Fatal("the deleted id is not restricted")
	}
	if _, err := st.GetComponent(ctx, id, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the row after the delete: %v", err)
	}
	left, err := st.ListCapabilityGrantsFor(ctx, []string{subject}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, g := range left {
		if g.Subject == subject {
			values = append(values, g.Value)
		}
	}
	slices.Sort(values)
	if want := []string{"*", other.String()}; !slices.Equal(values, want) {
		t.Fatalf("grants left for the subject = %v, want the wildcard and the other id's: %v", values, want)
	}
}
