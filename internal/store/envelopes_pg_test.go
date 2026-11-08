// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// TestEnvelopesCRUDPG: set, read, replace, nearest-above, the two write-time conflicts and the cascade
// with the unit (governance_envelopes). The tree is A with A1 and A2, A1 with A1x, and an unrelated B.
func TestEnvelopesCRUDPG(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	a, a1, a2, a1x, b := unitAdminTree(t, pg)
	set := func(unit uuid.UUID, groups ...string) (env types.GovernanceEnvelope, err error) {
		err = pg.WriteGovernance(ctx, func(q store.Querier) (e error) {
			env, e = store.SetEnvelopeQ(ctx, q, unit, groups, "sec-1")
			return e
		})
		return env, err
	}

	got, err := set(a.ID, "plat-a", "plat-a-builders")
	if err != nil || got.UnitID != a.ID || !slices.Equal(got.ApprovedGroups, []string{"plat-a", "plat-a-builders"}) || got.SetBy != "sec-1" || got.SetAt.IsZero() {
		t.Fatalf("set = %+v, %v", got, err)
	}
	if got, err = set(a.ID, "plat-a"); err != nil || !slices.Equal(got.ApprovedGroups, []string{"plat-a"}) {
		t.Fatalf("replace = %+v, %v; want the list replaced, the unit's own group kept", got, err)
	}
	if _, err := store.GetEnvelopeQ(ctx, pg.Pool, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get on a unit with none = %v, want ErrNotFound", err)
	}

	// Nearest envelope at or above: the unit itself counts unless strict.
	for _, c := range []struct {
		unit   uuid.UUID
		strict bool
		want   bool
	}{
		{a.ID, false, true}, {a.ID, true, false}, {a1x.ID, false, true}, {a1x.ID, true, true}, {b.ID, false, false},
	} {
		env, ok, err := store.NearestEnvelopeQ(ctx, pg.Pool, c.unit, c.strict)
		if err != nil || ok != c.want || (ok && env.UnitID != a.ID) {
			t.Errorf("nearest(%s, strict=%v) = %+v %v %v, want found=%v at A", c.unit, c.strict, env.UnitID, ok, err, c.want)
		}
	}

	// No nesting: neither below nor above an existing envelope.
	if _, err := set(a1.ID, "plat-x"); !errors.Is(err, store.ErrEnvelopeNested) {
		t.Errorf("set below an envelope = %v, want ErrEnvelopeNested", err)
	}
	if _, err := set(b.ID, "plat-b"); err != nil {
		t.Fatalf("set on B: %v", err)
	}
	if err := pg.WriteGovernance(ctx, func(q store.Querier) error {
		_, err := store.DeleteEnvelopeQ(ctx, q, a.ID)
		return err
	}); err != nil {
		t.Fatalf("delete A's envelope: %v", err)
	}
	if _, err := set(a1.ID, "plat-x"); err != nil {
		t.Fatalf("set on A1: %v", err)
	}
	if _, err := set(a.ID, "plat-y"); !errors.Is(err, store.ErrEnvelopeNested) {
		t.Errorf("set above an envelope = %v, want ErrEnvelopeNested", err)
	}

	// One owner per group, named in the refusal; a unit's own list does not conflict with itself.
	var taken *store.ErrEnvelopeGroupTaken
	if _, err := set(a2.ID, "plat-x"); !errors.As(err, &taken) || taken.Group != "plat-x" || taken.UnitID != a1.ID {
		t.Errorf("a group on two envelopes = %v, want ErrEnvelopeGroupTaken naming plat-x on A1", err)
	}
	if _, err := set(a1.ID, "plat-x", "plat-z"); err != nil {
		t.Errorf("a unit re-listing its own group: %v", err)
	}

	// An unknown unit.
	if _, err := set(uuid.New(), "plat-q"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("set on an unknown unit = %v, want ErrNotFound", err)
	}

	list, err := store.ListEnvelopesQ(ctx, pg.Pool)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v; want B's and A1's", list, err)
	}
	// The envelope goes with its unit.
	if _, err := pg.Pool.Exec(ctx, `DELETE FROM governance_units WHERE id = $1`, b.ID); err != nil {
		t.Fatalf("delete unit B: %v", err)
	}
	if _, err := store.GetEnvelopeQ(ctx, pg.Pool, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an envelope outlived its unit: %v", err)
	}
	if err := pg.WriteGovernance(ctx, func(q store.Querier) error {
		_, err := store.DeleteEnvelopeQ(ctx, q, a2.ID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete of an absent envelope = %v, want ErrNotFound", err)
	}
}
