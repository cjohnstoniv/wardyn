// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the run-cockpit widget-layout store (migration 0037).
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
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

func TestPG_RunLayout_GetPutRoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	// A fresh uuid per run, not a t.Name()-derived string: this table has no
	// delete path exposed (no console route calls one — see
	// store_ui_layout.go's doc), so an unqualified rerun against the SAME
	// database would otherwise find the previous run's leftover row and fail
	// the "no saved layout yet" assertion below. Matches the rest of this
	// package's PG integration tests (store_runs_pg_test.go's newRun, etc.),
	// which reach for uniqueness over cleanup.
	principal := "alice-" + uuid.NewString()

	// No saved layout yet: ErrNotFound, exactly like GetSSHKeyByFingerprint on
	// an unregistered key — the api layer is what turns this into a 200
	// empty/default shape, not the store.
	if _, err := st.GetRunLayout(ctx, principal, "live"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get before any save: err = %v, want ErrNotFound", err)
	}

	layout := []types.RunLayoutWidget{
		{Widget: "terminal", X: 0, Y: 0, W: 8, H: 6},
		{Widget: "timeline", X: 8, Y: 0, W: 4, H: 6},
	}
	saved, err := st.PutRunLayout(ctx, principal, "live", layout)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if saved.Preset != "live" || len(saved.Layout) != 2 || saved.UpdatedAt.IsZero() {
		t.Fatalf("saved = %+v, want preset=live, 2 widgets, a non-zero updated_at", saved)
	}

	got, err := st.GetRunLayout(ctx, principal, "live")
	if err != nil {
		t.Fatalf("get after put: %v", err)
	}
	if len(got.Layout) != 2 || got.Layout[0].Widget != "terminal" || got.Layout[1].Widget != "timeline" {
		t.Errorf("got = %+v, want the two widgets just saved, in order", got)
	}

	// A different preset for the SAME principal is an independent row — the
	// primary key is (principal, preset), not principal alone.
	if _, err := st.GetRunLayout(ctx, principal, "finished"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("finished preset before any save: err = %v, want ErrNotFound", err)
	}

	// PutRunLayout again for the SAME (principal, preset) is an upsert, not a
	// conflict — ON CONFLICT DO UPDATE, not a second row.
	replaced, err := st.PutRunLayout(ctx, principal, "live", []types.RunLayoutWidget{
		{Widget: "audit", X: 0, Y: 0, W: 12, H: 4},
	})
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if len(replaced.Layout) != 1 || replaced.Layout[0].Widget != "audit" {
		t.Errorf("replaced = %+v, want exactly the one new widget (full replace, not merge)", replaced)
	}
	got, err = st.GetRunLayout(ctx, principal, "live")
	if err != nil {
		t.Fatalf("get after second put: %v", err)
	}
	if len(got.Layout) != 1 || got.Layout[0].Widget != "audit" {
		t.Errorf("get after upsert = %+v, want the replaced single-widget layout", got)
	}

	// A nil layout upserts as an EMPTY array, never a stored JSON null and
	// never a nil slice on read-back — "reset to default" is PutRunLayout
	// with an empty layout, and the round trip must stay [], not null.
	cleared, err := st.PutRunLayout(ctx, principal, "live", nil)
	if err != nil {
		t.Fatalf("put nil layout: %v", err)
	}
	if cleared.Layout == nil || len(cleared.Layout) != 0 {
		t.Errorf("cleared.Layout = %#v, want a non-nil empty slice", cleared.Layout)
	}

	// Scoped to principal: a different principal's (never-saved) "live" preset
	// is untouched by anything done above.
	if _, err := st.GetRunLayout(ctx, "mallory-"+uuid.NewString(), "live"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("other principal's layout: err = %v, want ErrNotFound", err)
	}
}
