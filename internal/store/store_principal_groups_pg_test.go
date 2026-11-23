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
	"time"

	"github.com/google/uuid"

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
	if err := st.PutPrincipalGroups(ctx, "sub-pat", "", "Pat@Corp.Example", []string{"eng", "ops"}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPrincipalGroups(ctx, "sub-pat", "")
	if err != nil || got.Email != "Pat@Corp.Example" || !slices.Equal(got.Groups, []string{"eng", "ops"}) || got.Truncated {
		t.Fatalf("first sign-in = %+v, %v; want the email as sent and both groups, complete", got, err)
	}
	if _, err := st.GetPrincipalGroups(ctx, "sub-pat", portal); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no exchange through the portal yet: %v, want ErrNotFound", err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-pat", portal, "pat@corp.example", []string{"portal-users"}, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-pat", "", "pat@corp.example", nil, true, time.Now()); err != nil {
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
	if err := st.PutPrincipalGroups(ctx, "", "", "x@corp.example", nil, false, time.Now()); err == nil {
		t.Error("a record with no principal was written")
	}
}

// Narrowing a record takes out the groups the refused token does not carry, never adds one, and marks it
// partial when the token's snapshot was; it writes no record for a person with none through that door, and
// changes nothing for a token issued before the record's or a record that is no list.
func TestPG_PrincipalGroups_NarrowingOnlyTakesGroupsAway(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	const portal = "0b7f6f5e-2a1c-4d3e-9f80-1a2b3c4d5e6f"
	at := time.Now().UTC().Truncate(time.Second)
	if err := st.PutPrincipalGroups(ctx, "sub-pat", portal, "pat@corp.example", []string{"portal-users", "eng", "ops"}, false, at); err != nil {
		t.Fatal(err)
	}
	if err := st.NarrowPrincipalGroups(ctx, "sub-pat", portal, []string{"eng"}, false, at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-pat", portal); err != nil || len(got.Groups) != 3 {
		t.Fatalf("narrowed by an older token: %+v, %v; want the record as it was", got, err)
	}
	if err := st.NarrowPrincipalGroups(ctx, "sub-pat", portal, []string{"ops", "sales"}, true, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPrincipalGroups(ctx, "sub-pat", portal)
	if err != nil || got.Email != "pat@corp.example" || !slices.Equal(got.Groups, []string{"ops"}) || !got.Truncated {
		t.Fatalf("narrowed: %+v, %v; want ops alone, partial, the email kept", got, err)
	}
	if err := st.NarrowPrincipalGroups(ctx, "sub-pat", portal, []string{"portal-users", "eng"}, false, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-pat", portal); err != nil || got.Groups == nil || len(got.Groups) != 0 {
		t.Fatalf("narrowed to nothing: %+v, %v; want no groups (not nil), none added back", got, err)
	}
	if err := st.NarrowPrincipalGroups(ctx, "sub-new", portal, []string{"eng"}, false, at); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPrincipalGroups(ctx, "sub-new", portal); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a person with no record: %v, want still none", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_groups SET groups = 'null'::jsonb WHERE principal = 'sub-pat'`); err != nil {
		t.Fatal(err)
	}
	if err := st.NarrowPrincipalGroups(ctx, "sub-pat", portal, []string{"eng"}, false, at.Add(2*time.Hour)); err != nil {
		t.Fatalf("narrowing a record that is no list: %v, want it left alone", err)
	}
}

// A record from a token issued before the one on record changes nothing, so an older token still valid
// cannot put back a group a newer one dropped; a token issued at or after it replaces it, and a record
// with no issue time (one the migration filled) is replaced by any.
func TestPG_PrincipalGroups_AnOlderTokenKeepsTheNewerRecord(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	const portal = "0b7f6f5e-2a1c-4d3e-9f80-1a2b3c4d5e6f"
	newer := time.Now().UTC().Truncate(time.Second)
	if err := st.PutPrincipalGroups(ctx, "sub-pat", portal, "pat@corp.example", []string{"portal-users"}, false, newer); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-pat", portal, "pat@corp.example", []string{"portal-users", "eng"}, false, newer.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-pat", portal); err != nil || !slices.Equal(got.Groups, []string{"portal-users"}) {
		t.Fatalf("after an older token: %+v, %v; want the newer token's groups kept", got, err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-pat", portal, "pat@corp.example", []string{"ops"}, false, newer); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-pat", portal); err != nil || !slices.Equal(got.Groups, []string{"ops"}) {
		t.Fatalf("after a token issued at the same time: %+v, %v; want its groups", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE principal_groups SET issued_at = NULL WHERE principal = 'sub-pat'`); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-pat", portal, "pat@corp.example", []string{"eng"}, false, newer.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-pat", portal); err != nil || !slices.Equal(got.Groups, []string{"eng"}) {
		t.Fatalf("over a record with no issue time: %+v, %v; want it replaced", got, err)
	}
}

// A sign-in with no issue time is stamped with the database's time, so each later sign-in replaces it
// whichever server's clock is behind.
func TestPG_PrincipalGroups_ASignInIsStampedByTheDatabase(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	if err := st.PutPrincipalGroups(ctx, "sub-pat", "", "pat@corp.example", []string{"eng"}, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var stamped, dbNow time.Time
	if err := pool.QueryRow(ctx, `SELECT issued_at, now() FROM principal_groups WHERE principal = 'sub-pat' AND portal = ''`).Scan(&stamped, &dbNow); err != nil {
		t.Fatal(err)
	}
	if stamped.IsZero() || stamped.After(dbNow) {
		t.Fatalf("issued_at = %v at database time %v; want the database's time", stamped, dbNow)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-pat", "", "pat@corp.example", []string{"ops"}, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetPrincipalGroups(ctx, "sub-pat", ""); err != nil || !slices.Equal(got.Groups, []string{"ops"}) {
		t.Fatalf("after a later sign-in: %+v, %v; want its groups", got, err)
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
			if err := st.PutPrincipalGroups(ctx, p, door, p+"@corp.example", []string{"eng"}, false, time.Now()); err != nil {
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

// A sign-in (zero issuedAt) replaces the record even when the stored row's issued_at is later than the
// database's clock now, as after a failover to a standby behind the primary: the guard never skips it.
func TestPG_PrincipalGroups_ASignInReplacesARecordStampedAhead(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	if err := st.PutPrincipalGroups(ctx, "sub-ahead", "", "a@corp.example", []string{"old"}, false, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-ahead", "", "a@corp.example", []string{"new"}, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPrincipalGroups(ctx, "sub-ahead", "")
	if err != nil || !slices.Equal(got.Groups, []string{"new"}) {
		t.Fatalf("record = %+v, %v; want the sign-in's groups [new]", got, err)
	}
}

// A sign-in records the roles claim beside the groups; a write that carries no roles (the portal door, or a
// groups-only write) leaves it; a SCIM group removal clears it, so no role is left that the narrowed groups no
// longer show, and reads as NULL (no roles recorded) until the person's next sign-in.
func TestPG_PrincipalSignIn_ScimRemovalClearsTheRoles(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ('sub-roles', 'https://idp.example', 'roles@corp.example') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPrincipalSignIn(ctx, "sub-roles", "roles@corp.example", []string{"eng"}, []string{"wardyn-admins"}, false); err != nil {
		t.Fatal(err)
	}
	roles := func() (r []string, null bool) {
		t.Helper()
		var raw *[]string
		if err := pool.QueryRow(ctx, `SELECT roles FROM principal_groups WHERE principal = 'sub-roles' AND portal = ''`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if raw == nil {
			return nil, true
		}
		return *raw, false
	}
	if r, null := roles(); null || !slices.Equal(r, []string{"wardyn-admins"}) {
		t.Fatalf("roles after sign-in = %v (null %v), want the claim", r, null)
	}
	if err := st.PutPrincipalGroups(ctx, "sub-roles", "", "roles@corp.example", []string{"eng"}, false, now); err != nil {
		t.Fatal(err)
	}
	if r, null := roles(); null || len(r) != 1 {
		t.Fatalf("roles after a write with none = %v (null %v), want them kept", r, null)
	}
	eng, err := st.CreateScimGroup(ctx, "eng", "Engineering", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddScimGroupMembers(ctx, eng.ID, []uuid.UUID{id}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.StartGroupRemoval(ctx, eng.ID, id); err != nil {
		t.Fatal(err)
	}
	if _, null := roles(); !null {
		t.Fatal("a SCIM group removal left the roles claim in place, want NULL")
	}
	got, err := st.GetPrincipalGroups(ctx, "sub-roles", "")
	if err != nil || len(got.Groups) != 0 {
		t.Fatalf("record after the removal = %+v, %v; want the group gone", got, err)
	}
}
