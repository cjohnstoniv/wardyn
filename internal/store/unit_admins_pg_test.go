// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Unit admins and locks (migration governance_unit_admins) against Postgres: the grant rows, the scope
// walk up the tree, and the locked_knobs columns. Every case runs on its own throwaway database.
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

// unitAdminTree is A with children A1 and A2, A1 with child A1x, and an unrelated B.
func unitAdminTree(t *testing.T, pg store.PG) (a, a1, a2, a1x, b types.GovernanceUnit) {
	t.Helper()
	ctx := context.Background()
	put := func(name string, parent *uuid.UUID) types.GovernanceUnit {
		u := types.GovernanceUnit{ID: uuid.New(), Name: name, CreatedBy: "admin"}
		if parent != nil {
			u.ParentID, u.Overlay = parent, &types.CeilingOverlay{}
		}
		out, err := pg.UpsertGovernanceUnit(ctx, u)
		if err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
		return out
	}
	a = put("a", nil)
	a1 = put("a1", &a.ID)
	a2 = put("a2", &a.ID)
	a1x = put("a1x", &a1.ID)
	b = put("b", nil)
	return
}

func grantUnitAdmin(t *testing.T, pg store.PG, unit uuid.UUID, st types.CapabilitySubjectType, subject string) types.GovernanceUnitAdmin {
	t.Helper()
	g, err := store.GrantUnitAdminQ(context.Background(), pg.Pool, types.GovernanceUnitAdmin{
		UnitID: unit, SubjectType: st, Subject: subject, GrantedBy: "sec-1", ApprovedBy: "sec-2"})
	if err != nil {
		t.Fatalf("grant %s on %s: %v", subject, unit, err)
	}
	return g
}

func TestUnitScopeWalksUpTheTree(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	a, a1, a2, a1x, b := unitAdminTree(t, pg)
	grantUnitAdmin(t, pg, a.ID, types.CapabilitySubjectUser, "alice")
	grantUnitAdmin(t, pg, a1.ID, types.CapabilitySubjectGroup, "eng")

	for _, tc := range []struct {
		name          string
		unit          uuid.UUID
		users, groups []string
		strict, want  bool
	}{
		{"own unit", a.ID, []string{"alice"}, nil, false, true},
		{"own unit, strict", a.ID, []string{"alice"}, nil, true, false},
		{"child", a1.ID, []string{"alice"}, nil, true, true},
		{"grandchild", a1x.ID, []string{"alice"}, nil, true, true},
		{"sibling subtree", a2.ID, []string{"alice"}, nil, false, true},
		{"unrelated unit", b.ID, []string{"alice"}, nil, false, false},
		{"group grant on its unit", a1.ID, nil, []string{"eng"}, false, true},
		{"group grant, strict", a1.ID, nil, []string{"eng"}, true, false},
		{"group grant below", a1x.ID, nil, []string{"eng"}, true, true},
		{"group grant beside", a2.ID, nil, []string{"eng"}, false, false},
		{"group grant above", a.ID, nil, []string{"eng"}, false, false},
		{"a user name is not a group", a1.ID, []string{"eng"}, nil, false, false},
		{"no subject", a.ID, nil, nil, false, false},
		{"unknown unit", uuid.New(), []string{"alice"}, nil, false, false},
		{"the Default unit", store.DefaultUnitID, []string{"alice"}, nil, false, false},
	} {
		got, err := pg.UnitScope(ctx, tc.unit, tc.users, tc.groups, tc.strict)
		if err != nil || got != tc.want {
			t.Errorf("%s: UnitScope = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

func TestUnitAdminGrantRevokeAndLists(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	a, a1, _, _, b := unitAdminTree(t, pg)
	ga := grantUnitAdmin(t, pg, a.ID, types.CapabilitySubjectUser, "alice")
	if ga.GrantedBy != "sec-1" || ga.ApprovedBy != "sec-2" || ga.ID == uuid.Nil || ga.CreatedAt.IsZero() {
		t.Errorf("grant row = %+v; want granted_by, approved_by, an id and a time", ga)
	}
	grantUnitAdmin(t, pg, b.ID, types.CapabilitySubjectGroup, "eng")
	grantUnitAdmin(t, pg, a1.ID, types.CapabilitySubjectUser, "bob")

	if _, err := store.GrantUnitAdminQ(ctx, pg.Pool, types.GovernanceUnitAdmin{UnitID: a.ID,
		SubjectType: types.CapabilitySubjectUser, Subject: "alice", GrantedBy: "sec-1"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a second grant to the same subject = %v; want ErrConflict", err)
	}
	if _, err := store.GrantUnitAdminQ(ctx, pg.Pool, types.GovernanceUnitAdmin{UnitID: uuid.New(),
		SubjectType: types.CapabilitySubjectUser, Subject: "alice", GrantedBy: "sec-1"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a grant on an unknown unit = %v; want ErrNotFound", err)
	}
	if _, err := store.GrantUnitAdminQ(ctx, pg.Pool, types.GovernanceUnitAdmin{UnitID: a.ID,
		SubjectType: types.CapabilitySubjectAll, Subject: "", GrantedBy: "sec-1"}); err == nil {
		t.Error("a grant to everyone was stored; the subject_type CHECK allows only user and group")
	}

	all, err := pg.ListUnitAdmins(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("ListUnitAdmins = %d rows, %v; want 3", len(all), err)
	}
	of, err := pg.ListUnitAdminOf(ctx, []string{"alice"}, []string{"eng"})
	if err != nil || len(of) != 2 {
		t.Fatalf("ListUnitAdminOf(alice, eng) = %+v, %v; want the a and b grants", of, err)
	}
	units := []uuid.UUID{of[0].UnitID, of[1].UnitID}
	if !slices.Contains(units, a.ID) || !slices.Contains(units, b.ID) {
		t.Errorf("ListUnitAdminOf units = %v; want a and b", units)
	}
	if of, err := pg.ListUnitAdminOf(ctx, nil, nil); err != nil || len(of) != 0 {
		t.Errorf("ListUnitAdminOf(no subject) = %+v, %v; want none", of, err)
	}

	if _, err := pg.GetUnitAdmin(ctx, b.ID, ga.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetUnitAdmin on the wrong unit = %v; want ErrNotFound", err)
	}
	if _, err := store.RevokeUnitAdminQ(ctx, pg.Pool, b.ID, ga.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RevokeUnitAdmin on the wrong unit = %v; want ErrNotFound", err)
	}
	gone, err := store.RevokeUnitAdminQ(ctx, pg.Pool, a.ID, ga.ID)
	if err != nil || gone.Subject != "alice" {
		t.Fatalf("RevokeUnitAdmin = %+v, %v; want alice's row", gone, err)
	}
	if in, err := pg.UnitScope(ctx, a1.ID, []string{"alice"}, nil, true); err != nil || in {
		t.Errorf("after revoke UnitScope = %v, %v; want false", in, err)
	}
	if _, err := store.RevokeUnitAdminQ(ctx, pg.Pool, a.ID, ga.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a second revoke = %v; want ErrNotFound", err)
	}
}

func TestUnitAdminGrantsGoWithTheirUnit(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	_, _, _, a1x, _ := unitAdminTree(t, pg)
	grantUnitAdmin(t, pg, a1x.ID, types.CapabilitySubjectUser, "carol")
	if err := pg.DeleteGovernanceUnit(ctx, a1x.ID); err != nil {
		t.Fatalf("delete a1x: %v", err)
	}
	if of, err := pg.ListUnitAdminOf(ctx, []string{"carol"}, nil); err != nil || len(of) != 0 {
		t.Errorf("grants on a deleted unit = %+v, %v; want none", of, err)
	}
}

func TestUnitLockedKnobsRoundTrip(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	a, a1, _, _, _ := unitAdminTree(t, pg)
	for _, u := range []types.GovernanceUnit{a, a1} {
		u.LockedKnobs = []string{"min_confinement_class", "allowed_domains"}
		if _, err := pg.UpsertGovernanceUnit(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	units, err := pg.ListGovernanceUnits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range units {
		if u.ID != a.ID && u.ID != a1.ID {
			continue
		}
		if !slices.Equal(u.LockedKnobs, []string{"min_confinement_class", "allowed_domains"}) {
			t.Errorf("%s locked_knobs = %v; want the two stored", u.Name, u.LockedKnobs)
		}
	}
	a.LockedKnobs = nil
	if out, err := pg.UpsertGovernanceUnit(ctx, a); err != nil || len(out.LockedKnobs) != 0 {
		t.Errorf("a write with no locks = %v, %v; want the locks cleared", out.LockedKnobs, err)
	}

	locks := []string{"recording"}
	if got, err := pg.PutBoundary(ctx, types.BoundaryWrite{Mode: types.BoundaryNoLimits, LockedKnobs: &locks}, "sec"); err != nil ||
		!slices.Equal(got.LockedKnobs, locks) {
		t.Fatalf("PutBoundary with locks = %v, %v; want %v", got.LockedKnobs, err, locks)
	}
	if got, err := pg.PutBoundary(ctx, types.BoundaryWrite{Mode: types.BoundaryNoLimits}, "sec"); err != nil ||
		!slices.Equal(got.LockedKnobs, locks) {
		t.Errorf("PutBoundary with no locked_knobs = %v, %v; want the stored locks kept", got.LockedKnobs, err)
	}
	none := []string{}
	if got, err := pg.PutBoundary(ctx, types.BoundaryWrite{Mode: types.BoundaryNoLimits, LockedKnobs: &none}, "sec"); err != nil ||
		len(got.LockedKnobs) != 0 {
		t.Errorf("PutBoundary with an empty list = %v, %v; want the locks cleared", got.LockedKnobs, err)
	}
}
