// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the user_type subject (migration
// 0079_user_type_subject). Guarded by WARDYN_TEST_PG; skipped cleanly when
// unset.
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

// seedSubjectUserType creates one custom type and removes it (and every row
// the test wrote against it) afterwards.
func seedSubjectUserType(t *testing.T, st store.PG, prefix string) string {
	t.Helper()
	ctx := context.Background()
	id := prefix + "-" + uuid.NewString()[:8]
	if _, err := st.CreateUserType(ctx, types.UserType{ID: id, Name: "Type " + id}); err != nil {
		t.Fatalf("create user type %s: %v", id, err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM capability_grants WHERE subject_type = 'user_type' AND subject = $1`,
			`DELETE FROM governance_assignments WHERE subject_type = 'user_type' AND subject = $1`,
			`DELETE FROM user_drive_grants WHERE subject_type = 'user_type' AND subject = $1`,
			`DELETE FROM user_types WHERE id = $1`,
		} {
			_, _ = st.Pool.Exec(ctx, q, id)
		}
	})
	return id
}

// TestPG_UserTypeTier_Governance pins the ceiling's tier order with the type
// between group and all: user > group > user_type > all, one type per caller.
func TestPG_UserTypeTier_Governance(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	uniq := uuid.NewString()
	pm := seedSubjectUserType(t, st, "pm")
	dev := seedSubjectUserType(t, st, "dev")

	// Alphabetical order is the OPPOSITE of the tier order, so name ASC can
	// never be why a case passes.
	pUser := seedGovernanceProfile(t, st, "zzz-user-"+uniq)
	pGroup := seedGovernanceProfile(t, st, "yyy-group-"+uniq)
	pType := seedGovernanceProfile(t, st, "xxx-type-"+uniq)
	pAll := seedGovernanceProfile(t, st, "aaa-all-"+uniq)
	sub, group := "sub-"+uniq, "group-"+uniq

	seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectAll, ProfileID: pAll.ID})
	seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUserType, Subject: pm, ProfileID: pType.ID})
	seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectGroup, Subject: group, ProfileID: pGroup.ID, Priority: -1000})
	seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUser, Subject: sub, ProfileID: pUser.ID})

	for _, tc := range []struct {
		name          string
		users, groups []string
		userType      string
		want          string
		wantTier      types.CapabilitySubjectType
	}{
		{"user beats the type", []string{sub}, []string{group}, pm, pUser.Name, types.CapabilitySubjectUser},
		{"group beats the type at any priority", []string{"nobody-" + uniq}, []string{group}, pm, pGroup.Name, types.CapabilitySubjectGroup},
		{"the type beats all", []string{"nobody-" + uniq}, nil, pm, pType.Name, types.CapabilitySubjectUserType},
		{"another type's row never matches", []string{"nobody-" + uniq}, nil, dev, pAll.Name, types.CapabilitySubjectAll},
		{"no type matches no type row", []string{"nobody-" + uniq}, nil, "", pAll.Name, types.CapabilitySubjectAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, tier, err := st.ResolveGovernanceProfile(ctx, tc.users, tc.groups, tc.userType)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if p.Name != tc.want || tier != tc.wantTier {
				t.Fatalf("resolve = %q at tier %q, want %q at %q", p.Name, tier, tc.want, tc.wantTier)
			}
		})
	}
}

// TestPG_UserTypeTier_Drives is the same tier order on the drive resolver.
func TestPG_UserTypeTier_Drives(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	uniq := uuid.NewString()
	pm := seedSubjectUserType(t, st, "pm")
	dev := seedSubjectUserType(t, st, "dev")

	dGroup := seedUserDrive(t, st, "yyy-group-"+uniq)
	dType := seedUserDrive(t, st, "xxx-type-"+uniq)
	dAll := seedUserDrive(t, st, "aaa-all-"+uniq)
	group := "group-" + uniq

	seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectAll, DriveID: dAll.ID})
	seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUserType, Subject: pm, DriveID: dType.ID})
	seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: group, DriveID: dGroup.ID, Priority: -1000})

	for _, tc := range []struct {
		name     string
		groups   []string
		userType string
		want     string
	}{
		{"group beats the type at any priority", []string{group}, pm, dGroup.Name},
		{"the type beats all", nil, pm, dType.Name},
		{"another type's row never matches", nil, dev, dAll.Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, g, tier, err := st.ResolveUserDrive(ctx, []string{"nobody-" + uniq}, tc.groups, tc.userType)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if d.Name != tc.want || tier != g.SubjectType {
				t.Fatalf("resolve = %q at tier %q (grant %s), want %q", d.Name, tier, g.SubjectType, tc.want)
			}
		})
	}
}

// TestPG_ListCapabilityGrantsFor_UserType: the caller's own type's rows come
// back beside their user and group rows; another type's never do.
func TestPG_ListCapabilityGrantsFor_UserType(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	pm := seedSubjectUserType(t, st, "pm")
	dev := seedSubjectUserType(t, st, "dev")
	for _, subj := range []string{pm, dev} {
		if _, err := st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
			ID: uuid.New(), SubjectType: types.CapabilitySubjectUserType, Subject: subj,
			Capability: "agent", Value: "agent-" + subj, Effect: types.CapabilityDeny,
		}); err != nil {
			t.Fatalf("grant %s: %v", subj, err)
		}
	}
	got, err := st.ListCapabilityGrantsFor(ctx, []string{"nobody"}, nil, pm)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var mine []string
	for _, g := range got {
		if g.SubjectType == types.CapabilitySubjectUserType {
			mine = append(mine, g.Subject)
		}
	}
	if !slices.Equal(mine, []string{pm}) {
		t.Fatalf("user_type rows returned = %v, want only %s's", mine, pm)
	}
	none, err := st.ListCapabilityGrantsFor(ctx, []string{"nobody"}, nil, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if slices.ContainsFunc(none, func(g types.CapabilityGrant) bool { return g.SubjectType == types.CapabilitySubjectUserType }) {
		t.Fatal("an empty user type matched a user_type row")
	}
}

// TestPG_DeleteUserType_RefusedWhileASubjectRowNamesIt: the delete guard now
// has real rows to count in each of the three tables.
func TestPG_DeleteUserType_RefusedWhileASubjectRowNamesIt(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	uniq := uuid.NewString()
	for _, table := range []string{"capability_grants", "governance_assignments", "user_drive_grants"} {
		t.Run(table, func(t *testing.T) {
			id := seedSubjectUserType(t, st, "del")
			switch table {
			case "capability_grants":
				if _, err := st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
					ID: uuid.New(), SubjectType: types.CapabilitySubjectUserType, Subject: id,
					Capability: "agent", Value: "x", Effect: types.CapabilityAllow,
				}); err != nil {
					t.Fatal(err)
				}
			case "governance_assignments":
				p := seedGovernanceProfile(t, st, "del-"+id+"-"+uniq)
				seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUserType, Subject: id, ProfileID: p.ID})
			case "user_drive_grants":
				d := seedUserDrive(t, st, "del-"+id+"-"+uniq)
				seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUserType, Subject: id, DriveID: d.ID})
			}
			if n, err := st.UserTypeReferences(ctx, id); err != nil || n != 1 {
				t.Fatalf("references = %d, %v; want 1", n, err)
			}
			if err := st.DeleteUserType(ctx, id); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("delete while named = %v, want ErrConflict", err)
			}
		})
	}
}

// TestPG_CreateUserType_RefusedWhileAnOrphanedSubjectRowNamesTheID pins the
// close for userTypeSubjectExists' disclosed check-then-insert race: a
// subject row can be written just after a delete's reference check passed,
// outliving the type it names (migration 0079 has no FK by design). Recreating
// that id must stay refused, or the orphan would silently rebind to whatever
// type is created next with the same id.
func TestPG_CreateUserType_RefusedWhileAnOrphanedSubjectRowNamesTheID(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	id := seedSubjectUserType(t, st, "orphan")
	if _, err := st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUserType, Subject: id,
		Capability: "agent", Value: "x", Effect: types.CapabilityAllow,
	}); err != nil {
		t.Fatal(err)
	}
	// Force the race DeleteUserType's own reference check normally prevents:
	// the user_types row gone while the grant still names its id.
	if _, err := st.Pool.Exec(ctx, `DELETE FROM user_types WHERE id = $1`, id); err != nil {
		t.Fatalf("force-delete: %v", err)
	}
	if _, err := st.CreateUserType(ctx, types.UserType{ID: id, Name: "Reborn " + id}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("recreate orphaned id = %v, want ErrConflict", err)
	}
}
