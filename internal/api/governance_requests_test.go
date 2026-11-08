// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGovernanceRequestKindsTable: every request kind maps to a held-change target the table decides, and
// back; a target that is not a request is not one.
func TestGovernanceRequestKindsTable(t *testing.T) {
	for name, k := range governanceRequestKinds {
		got, ok := requestKindOf(k.target)
		if !ok || got != name {
			t.Errorf("requestKindOf(%q) = %q, %v; want %q", k.target, got, ok, name)
		}
		if _, held := governanceChangeKinds[k.target]; !held {
			t.Errorf("request kind %s's target %s is not in the held-change table", name, k.target)
		}
	}
	if _, ok := requestKindOf(types.GovernanceTargetMembership); ok {
		t.Error("a membership change reads as a request")
	}
	if governanceChangeKinds[types.GovernanceTargetElevation].rejectAction != "governance.elevation.reject" {
		t.Error("an elevation's reject writes no governance.elevation.reject")
	}
}

// TestCapAtElevation: a run launched under an elevation is capped at its end when that is earlier.
func TestCapAtElevation(t *testing.T) {
	now := time.Now().UTC()
	early, late := now.Add(time.Hour), now.Add(3*time.Hour)
	run := types.AgentRun{LifetimeCapAt: &late, LifetimeCapSource: types.LimitSourcePool}
	capAtElevation(&run, nil)
	if run.LifetimeCapSource != types.LimitSourcePool {
		t.Errorf("no elevation changed the cap to %q", run.LifetimeCapSource)
	}
	capAtElevation(&run, &early)
	if !run.LifetimeCapAt.Equal(early) || run.LifetimeCapSource != types.LimitSourceElevation {
		t.Errorf("an earlier elevation end = %v %q, want %v elevation", run.LifetimeCapAt, run.LifetimeCapSource, early)
	}
	capAtElevation(&run, &late)
	if !run.LifetimeCapAt.Equal(early) {
		t.Errorf("a later elevation end moved the cap to %v", run.LifetimeCapAt)
	}
	none := types.AgentRun{}
	capAtElevation(&none, &late)
	if none.LifetimeCapAt == nil || !none.LifetimeCapAt.Equal(late) || none.LifetimeCapSource != types.LimitSourceElevation {
		t.Errorf("an uncapped run = %v %q, want the elevation's end", none.LifetimeCapAt, none.LifetimeCapSource)
	}
}

// TestPickContextDropsElevations: an expiring row is a context only when RunIn names its unit, and a
// permanent row in the same unit wins over it.
func TestPickContextDropsElevations(t *testing.T) {
	s := &Server{}
	end := time.Now().Add(time.Hour)
	home := types.GovernanceUnit{ID: uuid.New(), Name: "home"}
	elevated := types.GovernanceUnit{ID: uuid.New(), Name: "elevated"}
	row := func(u types.GovernanceUnit, st types.CapabilitySubjectType, expires *time.Time) store.MembershipMatch {
		return store.MembershipMatch{Unit: u, Membership: types.GovernanceMembership{ID: uuid.New(), SubjectType: st, Subject: "x", UnitID: u.ID, ExpiresAt: expires}}
	}
	ctx := context.Background()
	only := []store.MembershipMatch{row(elevated, types.CapabilitySubjectUser, &end)}
	if p, _, err := s.pickContext(ctx, only, contextChoice{}); err != nil || !p.isDefault() {
		t.Errorf("an elevation alone, no run_in = %+v, %v; want Default", p.unit, err)
	}
	both := []store.MembershipMatch{row(elevated, types.CapabilitySubjectUser, &end), row(home, types.CapabilitySubjectUser, nil)}
	if p, _, err := s.pickContext(ctx, both, contextChoice{}); err != nil || p.unit.ID != home.ID {
		t.Errorf("no run_in = %s, %v; want home", p.unit.Name, err)
	}
	if p, _, err := s.pickContext(ctx, both, contextChoice{RunIn: elevated.ID}); err != nil || p.unit.ID != elevated.ID || p.row.Membership.ExpiresAt == nil {
		t.Errorf("run_in elevated = %s, %v; want the elevation", p.unit.Name, err)
	}
	same := []store.MembershipMatch{row(elevated, types.CapabilitySubjectUser, &end), row(elevated, types.CapabilitySubjectGroup, nil)}
	if p, _, err := s.pickContext(ctx, same, contextChoice{RunIn: elevated.ID}); err != nil || p.row.Membership.ExpiresAt != nil {
		t.Errorf("run_in with a permanent row in the unit = %+v, %v; want the permanent row", p.row, err)
	}
}
