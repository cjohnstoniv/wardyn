// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the record of each person's last sign-in through each door. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A sign-in replaces the record before it through the same door and leaves another door's, a sign-in
// with no groups is recorded as none rather than as no snapshot, and a person with no record through a
// door is ErrNotFound there.
func TestPG_PrincipalGroups_ASignInReplacesTheRecord(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	const portal = "0b7f6f5e-2a1c-4d3e-9f80-1a2b3c4d5e6f"
	if _, err := st.GetPrincipalGroups(ctx, "sub-pat", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no sign-in yet: %v, want ErrNotFound", err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-pat", "", "Pat@Corp.Example", []string{"eng", "ops"}, false); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPrincipalGroups(ctx, "sub-pat", "")
	if err != nil || got.Email != "Pat@Corp.Example" || !slices.Equal(got.Groups, []string{"eng", "ops"}) || got.Truncated {
		t.Fatalf("first sign-in = %+v, %v; want the email as sent and both groups, complete", got, err)
	}
	if _, err := st.GetPrincipalGroups(ctx, "sub-pat", portal); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no exchange through the portal yet: %v, want ErrNotFound", err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-pat", portal, "pat@corp.example", []string{"portal-users"}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-pat", "", "pat@corp.example", nil, true); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetPrincipalGroups(ctx, "sub-pat", "")
	if err != nil || got.Email != "pat@corp.example" || got.Groups == nil || len(got.Groups) != 0 || !got.Truncated {
		t.Fatalf("second sign-in = %+v, %v; want it alone: no groups (not nil) and partial", got, err)
	}
	got, err = st.GetPrincipalGroups(ctx, "sub-pat", portal)
	if err != nil || !slices.Equal(got.Groups, []string{"portal-users"}) || got.Truncated {
		t.Fatalf("the portal's record = %+v, %v; want its own groups, untouched by the sign-in", got, err)
	}
	if err := st.PutPrincipalGroups(ctx, "", "", "x@corp.example", nil, false); err == nil {
		t.Error("a record with no principal was written")
	}
}

// Erasing a person's group snapshots takes every door's record of theirs with them and leaves another
// person's.
func TestPG_PrincipalGroups_ErasureDeletesTheRecord(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	const portal = "0b7f6f5e-2a1c-4d3e-9f80-1a2b3c4d5e6f"
	for _, p := range []string{"sub-ann", "sub-ben"} {
		for _, door := range []string{"", portal} {
			if err := st.PutPrincipalGroups(ctx, p, door, p+"@corp.example", []string{"eng"}, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := st.DeleteLoginGroups(ctx, []string{"sub-ann"}); err != nil {
		t.Fatal(err)
	}
	for _, door := range []string{"", portal} {
		if _, err := st.GetPrincipalGroups(ctx, "sub-ann", door); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("erased person's record through %q: %v, want ErrNotFound", door, err)
		}
		if got, err := st.GetPrincipalGroups(ctx, "sub-ben", door); err != nil || !slices.Equal(got.Groups, []string{"eng"}) {
			t.Errorf("the other person's record through %q = %+v, %v; want it untouched", door, got, err)
		}
	}
}
