// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_CeilingAndDriveRankOneWay pins K3's one precedence-select: the
// governance ceiling and the user drive, seeded with the SAME subject rows,
// must pick the same winner for every caller shape. Each key of the ranking
// has a case where it alone decides, and every loser's name sorts FIRST, so a
// dropped key falls through to the name tie-break and picks the wrong row.
func TestPG_CeilingAndDriveRankOneWay(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]

	sub, email := "k3sub-"+uniq, "k3-"+uniq+"@example.com"
	gLow, gHigh, gTie := "k3glow-"+uniq, "k3ghigh-"+uniq, "k3gtie-"+uniq
	utype := "k3type-" + uniq

	// role -> (subject row, name). Names sort so that the INTENDED winner is
	// never the alphabetically first candidate unless name is the deciding key.
	rows := []struct {
		role string
		st   types.CapabilitySubjectType
		subj string
		prio int
		name string
	}{
		{"sub", types.CapabilitySubjectUser, sub, 0, "zz-sub-"},
		{"email", types.CapabilitySubjectUser, email, 0, "aa-email-"},
		{"ghigh", types.CapabilitySubjectGroup, gHigh, 5, "zz-ghigh-"},
		{"glow", types.CapabilitySubjectGroup, gLow, 0, "aa-glow-"},
		{"gtie", types.CapabilitySubjectGroup, gTie, 5, "mm-gtie-"},
		{"type", types.CapabilitySubjectUserType, utype, 0, "aa-type-"},
		{"all", types.CapabilitySubjectAll, "", 0, "aa-all-"},
	}
	for _, r := range rows {
		p := seedGovernanceProfile(t, st, r.name+uniq)
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{SubjectType: r.st, Subject: r.subj, ProfileID: p.ID, Priority: r.prio})
		d := seedUserDrive(t, st, r.name+uniq)
		seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: r.st, Subject: r.subj, DriveID: d.ID, Priority: r.prio})
	}
	role := func(name string) string {
		for _, r := range rows {
			if strings.HasPrefix(name, r.name) && strings.HasSuffix(name, uniq) {
				return r.role
			}
		}
		return "foreign:" + name
	}

	cases := []struct {
		name   string
		users  []string
		groups []string
		utype  string
		want   string
		tier   types.CapabilitySubjectType
	}{
		{"user beats every group, type and all", []string{sub, email}, []string{gLow, gHigh}, utype, "sub", types.CapabilitySubjectUser},
		{"sub beats email by position, not name", []string{sub, email}, nil, "", "sub", types.CapabilitySubjectUser},
		{"the caller's order is the precedence", []string{"nobody-" + uniq, email}, nil, "", "email", types.CapabilitySubjectUser},
		{"group priority beats name", []string{"nobody-" + uniq}, []string{gLow, gHigh}, utype, "ghigh", types.CapabilitySubjectGroup},
		{"equal priority: name decides", []string{"nobody-" + uniq}, []string{gHigh, gTie}, "", "gtie", types.CapabilitySubjectGroup},
		{"group beats user_type", []string{"nobody-" + uniq}, []string{gLow}, utype, "glow", types.CapabilitySubjectGroup},
		{"user_type beats all (the stale shape: groups nil)", []string{"nobody-" + uniq}, nil, utype, "type", types.CapabilitySubjectUserType},
		{"all is the floor", []string{"nobody-" + uniq}, nil, "", "all", types.CapabilitySubjectAll},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ptier, err := st.ResolveGovernanceProfile(ctx, tc.users, tc.groups, tc.utype)
			if err != nil {
				t.Fatalf("ceiling: %v", err)
			}
			d, g, dtier, err := st.ResolveUserDrive(ctx, tc.users, tc.groups, tc.utype)
			if err != nil {
				t.Fatalf("drive: %v", err)
			}
			if got := role(p.Name); got != tc.want || ptier != tc.tier {
				t.Errorf("ceiling = (%s, %s), want (%s, %s)", got, ptier, tc.want, tc.tier)
			}
			if got := role(d.Name); got != tc.want || dtier != tc.tier || g.SubjectType != tc.tier {
				t.Errorf("drive = (%s, %s), want (%s, %s)", got, dtier, tc.want, tc.tier)
			}
		})
	}

	t.Run("group-tier gate sees both tables", func(t *testing.T) {
		for name, has := range map[string]func(context.Context) (bool, error){
			"assignments": st.HasGroupTierAssignments, "drive grants": st.HasGroupTierDriveGrants,
		} {
			if ok, err := has(ctx); err != nil || !ok {
				t.Errorf("group-tier %s = (%v, %v), want true", name, ok, err)
			}
		}
	})

	t.Run("no match is ErrNotFound on both", func(t *testing.T) {
		// Remove the all rows this test seeded, then ask for nobody.
		for _, a := range mustList(t, st.ListGovernanceAssignments) {
			if a.SubjectType == types.CapabilitySubjectAll {
				if err := st.DeleteGovernanceAssignment(ctx, a.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, g := range mustList(t, st.ListUserDriveGrants) {
			if g.SubjectType == types.CapabilitySubjectAll {
				if _, err := st.DeleteUserDriveGrant(ctx, g.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, _, err := st.ResolveGovernanceProfile(ctx, []string{"nobody-" + uniq}, nil, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ceiling err = %v, want ErrNotFound", err)
		}
		if _, _, _, err := st.ResolveUserDrive(ctx, []string{"nobody-" + uniq}, nil, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("drive err = %v, want ErrNotFound", err)
		}
	})
}

func mustList[T any](t *testing.T, list func(context.Context) ([]T, error)) []T {
	t.Helper()
	out, err := list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return out
}
