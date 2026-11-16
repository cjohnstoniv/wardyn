// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for what a SCIM group change does to the record of each person's last sign-ins.
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A SCIM removal takes the group out of every record of the principal the identity row binds, its
// sign-in's and each portal's, matched as the token step matches it (case and spaces aside; a blank id
// takes every group), and leaves the rest of the record, a person with no record, an identity no sign-in
// bound (whatever its email) and anyone else's record as they were. A SCIM add never puts a group in a
// record.
func TestPG_PrincipalGroups_ScimRemovalNarrowsTheRecord(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now()
	identity := func(principal any, email string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ($1, 'https://idp.example', $2) RETURNING id`,
			principal, email).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	const portal = "0b7f6f5e-2a1c-4d3e-9f80-1a2b3c4d5e6f"
	record := func(principal, door string) []string {
		t.Helper()
		got, err := st.GetPrincipalGroups(ctx, principal, door)
		if err != nil {
			t.Fatalf("%s: %v", principal, err)
		}
		return got.Groups
	}
	ann, ben, dee, cy, eve := identity("sub-ann", "ann@corp.example"), identity("sub-ben", "ben@corp.example"),
		identity("sub-dee", "dee@corp.example"), identity(nil, "cy@corp.example"), identity("sub-eve", "eve@corp.example")
	for p, groups := range map[string][]string{"sub-ann": {"eng", "ops"}, "sub-ben": {"eng"}, "sub-cy": {"eng"}, "sub-eve": {"eng", "ops"}} {
		if err := st.PutPrincipalGroups(ctx, p, "", strings.TrimPrefix(p, "sub-")+"@corp.example", groups, p == "sub-ann"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutPrincipalGroups(ctx, "sub-ann", portal, "ann@corp.example", []string{"eng", "sales"}, false); err != nil {
		t.Fatal(err)
	}
	eng, err := st.CreateScimGroup(ctx, " ENG ", "Engineering", now)
	if err != nil {
		t.Fatal(err)
	}
	admins, err := st.CreateScimGroup(ctx, "admins", "Admins", now)
	if err != nil {
		t.Fatal(err)
	}
	blank, err := st.CreateScimGroup(ctx, "  ", "No id", now)
	if err != nil {
		t.Fatal(err)
	}
	// CreateScimGroup trims the id; a row written another way keeps its spaces, which the match trims.
	if _, err := pool.Exec(ctx, `UPDATE scim_groups SET external_id = ' ENG ' WHERE id = $1`, eng.ID); err != nil {
		t.Fatal(err)
	}
	for _, g := range []uuid.UUID{eng.ID, admins.ID, blank.ID} {
		if err := st.AddScimGroupMembers(ctx, g, []uuid.UUID{ann, ben, dee, cy, eve}, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := record("sub-ann", ""); !slices.Equal(got, []string{"eng", "ops"}) {
		t.Fatalf("after SCIM adds: %v, want the sign-in's groups alone", got)
	}
	if _, err := st.GetPrincipalGroups(ctx, "sub-dee", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a SCIM add made a record: %v", err)
	}
	for range 2 {
		for _, id := range []uuid.UUID{ann, dee, cy} {
			if err := st.StartGroupRemoval(ctx, eng.ID, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := st.GetPrincipalGroups(ctx, "sub-ann", "")
	if err != nil || !slices.Equal(got.Groups, []string{"ops"}) || !got.Truncated {
		t.Fatalf("removed from eng: %+v, %v; want ops alone, still partial", got, err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-ann", portal); err != nil || !slices.Equal(got.Groups, []string{"sales"}) || got.Truncated {
		t.Errorf("removed from eng, the portal's record: %+v, %v; want sales alone, still complete", got, err)
	}
	if _, err := st.GetPrincipalGroups(ctx, "sub-dee", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a person with no record: %v, want still none", err)
	}
	if got := record("sub-cy", ""); !slices.Equal(got, []string{"eng"}) {
		t.Errorf("an identity no sign-in bound narrowed sub-cy: %v", got)
	}
	if got := record("sub-ben", ""); !slices.Equal(got, []string{"eng"}) {
		t.Errorf("a member not removed: %v, want untouched", got)
	}
	if err := st.StartGroupRemoval(ctx, blank.ID, eve); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-eve", ""); err != nil || got.Groups == nil || len(got.Groups) != 0 || !got.Truncated {
		t.Errorf("removed from a group with a blank id: %+v, %v; want no groups (not nil), marked partial", got, err)
	}
}

