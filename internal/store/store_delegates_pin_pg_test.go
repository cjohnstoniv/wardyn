// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A portal's pin round-trips through every read, and the unit it names is a bare id: no unit need
// exist, so a deleted unit leaves a pin behind.
func TestPG_DelegateRunInUnderRoundTrip(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	unit := uuid.New()
	pinned, err := st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "pinned", IdPClientID: "c-" + uuid.NewString()[:8],
		Group: "g", RunInUnder: &unit}, "wdp_"+uuid.NewString())
	if err != nil || pinned.RunInUnder == nil || *pinned.RunInUnder != unit {
		t.Fatalf("create pinned: %+v, %v", pinned, err)
	}
	plain := newCutPortal(t, st)
	if plain.RunInUnder != nil {
		t.Errorf("an unpinned portal reads back pin %v", plain.RunInUnder)
	}
	if got, err := st.GetDelegateByID(ctx, pinned.ID); err != nil || got.RunInUnder == nil || *got.RunInUnder != unit {
		t.Errorf("GetDelegateByID: %+v, %v, want the pin", got, err)
	}
	list, err := st.ListDelegates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list {
		if want := d.ID == pinned.ID; (d.RunInUnder != nil) != want {
			t.Errorf("ListDelegates: portal %s pin %v, want pinned=%v", d.ID, d.RunInUnder, want)
		}
	}
}
