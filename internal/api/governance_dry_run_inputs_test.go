// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// boundaryFlipStore keeps a boundary that turns hard_limits once flipped is set, as a boundary write
// between two reads would, and counts every boundary and site-config read. Any other call panics.
type boundaryFlipStore struct {
	store.Store
	hard                       types.Boundary
	flipped                    bool
	boundaryReads, configReads int
}

func (f *boundaryFlipStore) GetBoundary(context.Context) (types.Boundary, error) {
	f.boundaryReads++
	if f.flipped {
		return f.hard, nil
	}
	return types.Boundary{Mode: types.BoundaryNoLimits}, nil
}

func (f *boundaryFlipStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	f.flipped = true
	return f.hard, nil
}

func (f *boundaryFlipStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	f.configReads++
	return types.SiteConfig{}, nil
}

// TestTreeEffectResolvesBothSidesWithTheInputsHanded: treeEffect resolves both sides of an unchanged tree
// against the inputs it is handed, reading neither the boundary nor the depth limit itself, so a boundary
// that turns hard_limits between the two sides (the seam flips the store's) reports no person and no run.
// The walls are the handed boundary's: its confinement floor raises the unit's.
func TestTreeEffectResolvesBothSidesWithTheInputsHanded(t *testing.T) {
	tb := testBoundary(types.BoundaryHardLimits)
	st := &boundaryFlipStore{hard: types.Boundary{Mode: tb.Mode, Spec: tb.Spec, Limits: tb.Limits}}
	srv := &Server{cfg: Config{DefaultPolicy: govDeployment(), Store: st}}
	unit := govProfile("flip-b")
	snap := treeSnapshot{
		units: []types.GovernanceUnit{{ID: store.DefaultUnitID, Name: "Default"}, *unit},
		memberships: []types.GovernanceMembership{{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "sub-flip-b",
			UnitID: unit.ID, Priority: 1}},
	}
	prev := treeDryRunPause
	t.Cleanup(func() { treeDryRunPause = prev })
	treeDryRunPause = func(at string) {
		if at == "walls" {
			st.flipped = true
		}
	}
	for _, b := range []types.Boundary{{Mode: types.BoundaryNoLimits}, st.hard} {
		st.flipped = false
		report, err := srv.treeEffect(context.Background(), snap, snap, wallInputs{limit: maxUnitDepthDefault, boundary: b})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.People) != 0 || len(report.LiveRuns) != 0 {
			t.Errorf("boundary %s: people %+v, live runs %v, want none for an unchanged tree", b.Mode, report.People, report.LiveRuns)
		}
	}
	if st.boundaryReads != 0 || st.configReads != 0 {
		t.Errorf("treeEffect read the boundary %d and the site config %d times, want only the inputs handed", st.boundaryReads, st.configReads)
	}
	walls := srv.snapshotWalls(context.Background(), snap, wallInputs{limit: maxUnitDepthDefault, boundary: st.hard})
	if got := walls[unit.ID]; got.err != "" || got.spec.MinConfinementClass != types.CC3 {
		t.Errorf("walls under the handed hard_limits boundary = %+v, want the boundary's CC3 floor", got)
	}
}

// noRunsStore has no live run, for a treeEffect whose walls changed. Any other call panics (nil store).
type noRunsStore struct{ store.Store }

func (noRunsStore) ListUndispatchedRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }

func (noRunsStore) ListTerminalRunsWithSandbox(context.Context) ([]types.AgentRun, error) {
	return nil, nil
}

func (noRunsStore) ListLiveRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }

func (noRunsStore) GetRunsByID(context.Context, []uuid.UUID) ([]types.AgentRun, error) {
	return nil, nil
}

// TestTreeEffectNamesTheUnitBeforeARename: a write that renames a unit and changes its limits reports each affected
// membership with the name the unit had before (from_unit_name) and the name it has after (unit_name).
func TestTreeEffectNamesTheUnitBeforeARename(t *testing.T) {
	srv := &Server{cfg: Config{DefaultPolicy: govDeployment(), Store: noRunsStore{}}}
	old := govProfile("payments")
	renamed := *old
	renamed.Name, renamed.Limits = "payments-eu", types.GovernanceLimits{}
	def := types.GovernanceUnit{ID: store.DefaultUnitID, Name: "Default"}
	m := types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "sub-rename", UnitID: old.ID, Priority: 1}
	before := treeSnapshot{units: []types.GovernanceUnit{def, *old}, memberships: []types.GovernanceMembership{m}}
	after := treeSnapshot{units: []types.GovernanceUnit{def, renamed}, memberships: []types.GovernanceMembership{m}}
	report, err := srv.treeEffect(context.Background(), before, after, wallInputs{limit: maxUnitDepthDefault, boundary: types.Boundary{Mode: types.BoundaryNoLimits}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.People) != 1 || report.People[0].FromUnitName != "payments" || report.People[0].UnitName != "payments-eu" || len(report.People[0].Changes) == 0 {
		t.Fatalf("people = %+v; want one row from payments to payments-eu with its limit change", report.People)
	}
}
