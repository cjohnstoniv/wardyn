// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for migration 0080_user_view_type. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_PrincipalPref_UpsertIsPerPrincipal: a preference reads back for its
// own principal only, and a second write replaces the first.
func TestPG_PrincipalPref_UpsertIsPerPrincipal(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()

	if _, err := st.GetPrincipalPref(ctx, alice, "user_view.type"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get before any put: err = %v, want ErrNotFound", err)
	}
	for _, v := range []string{`"portfolio-manager"`, `"developer"`} {
		if err := st.PutPrincipalPref(ctx, alice, "user_view.type", json.RawMessage(v)); err != nil {
			t.Fatalf("put %s: %v", v, err)
		}
	}
	got, err := st.GetPrincipalPref(ctx, alice, "user_view.type")
	if err != nil || string(got) != `"developer"` {
		t.Fatalf("alice = %s, %v; want the second write", got, err)
	}
	if _, err := st.GetPrincipalPref(ctx, bob, "user_view.type"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob: err = %v, want ErrNotFound (a pref is its principal's alone)", err)
	}
}

// TestPG_RunUserTypeRoundTrips: agent_runs.user_type is written by CreateRun
// and read back by scanRun, and a run with none reads "".
func TestPG_RunUserTypeRoundTrips(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	typed := newRun(types.RunRunning)
	typed.UserType = "portfolio-manager"
	if got := persistRun(t, ctx, pool, typed); got.UserType != "portfolio-manager" {
		t.Fatalf("CreateRun returned user_type %q, want portfolio-manager", got.UserType)
	}
	got, err := store.NewPG(pool).GetRun(ctx, typed.ID)
	if err != nil || got.UserType != "portfolio-manager" || got.AutonomyLevel != "" {
		t.Fatalf("GetRun = %q/%q, %v; want portfolio-manager and no autonomy level", got.UserType, got.AutonomyLevel, err)
	}
	if got := persistRun(t, ctx, pool, newRun(types.RunRunning)); got.UserType != "" {
		t.Fatalf("untyped run read back user_type %q, want empty", got.UserType)
	}
}
