// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The verified re-fold against Postgres: each test seeds a unit tree, ages every layer's updated_at, and takes
// the database's clock as the run's created_at, so every layer predates the run by more than the margin until
// the test writes one again. No run row is needed: wallsRefold reads only the run it is given.

const refoldOwner = "sub-refold-owner"

// refoldTree is a standalone root that allows four hosts, a unit on it that removes unit-drops.example, and an
// own-walls leaf below that, the run's unit.
type refoldTree struct{ root, mid, leaf types.GovernanceUnit }

// refoldUnitStrike is the entry the tree's middle unit removed.
var refoldUnitStrike = wallsStrike{Field: fieldAllowed, Entry: "unit-drops.example", Cause: causeLimits}

// refoldPut stores one unit as given.
func refoldPut(t *testing.T, e *govEnv, u types.GovernanceUnit) types.GovernanceUnit {
	t.Helper()
	u.CreatedBy = "seed"
	got, err := e.pg.UpsertGovernanceUnit(context.Background(), u)
	if err != nil {
		t.Fatalf("seed unit %s: %v", u.Name, err)
	}
	return got
}

// refoldSeedTree seeds the tree on a fresh database. boundary is the boundary the tree resolves under.
func refoldSeedTree(t *testing.T, boundary types.BoundaryWrite) (*govEnv, refoldTree) {
	t.Helper()
	e := newGovEnvOn(t, throwawayPGPool(t))
	root := refoldPut(t, e, types.GovernanceUnit{ID: uuid.New(), Name: "refold-root", Ceiling: types.RunPolicySpec{
		AllowedDomains: []string{"keep.example", "unit-drops.example", "restriction-drops.example", "boundary-drops.example"}}})
	mid := refoldPut(t, e, types.GovernanceUnit{ID: uuid.New(), Name: "refold-mid", ParentID: &root.ID, Overlay: &types.CeilingOverlay{
		AllowedDomains: &[]string{"keep.example", "restriction-drops.example", "boundary-drops.example"}}})
	leaf := refoldPut(t, e, types.GovernanceUnit{ID: uuid.New(), Name: "refold-leaf", ParentID: &mid.ID,
		Overlay: &types.CeilingOverlay{DeniedDomains: &[]string{"leaf-denies.example"}}})
	if _, err := e.pg.PutBoundary(context.Background(), boundary, "seed"); err != nil {
		t.Fatalf("put boundary: %v", err)
	}
	return e, refoldTree{root: root, mid: mid, leaf: leaf}
}

// refoldNoBoundary is no boundary at all: a fresh schema starts with a seeded one in hard_limits.
var refoldNoBoundary = types.BoundaryWrite{Mode: types.BoundaryNoLimits}

// refoldOlderBoundary allows two of the hosts the tree leaves, so it removes boundary-drops.example.
func refoldOlderBoundary() types.BoundaryWrite {
	return types.BoundaryWrite{Mode: types.BoundaryHardLimits,
		Spec: types.BoundarySpec{AllowedDomains: &[]string{"keep.example", "restriction-drops.example"}}}
}

// refoldAge moves every unit, restriction and boundary write ten minutes back and returns the database's clock:
// a run created at it is newer than every layer by more than the margin.
func refoldAge(t *testing.T, e *govEnv) time.Time {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"governance_units", "governance_restrictions", "governance_boundary"} {
		if _, err := e.pool.Exec(ctx, `UPDATE `+table+` SET updated_at = updated_at - interval '10 minutes'`); err != nil {
			t.Fatalf("age %s: %v", table, err)
		}
	}
	var now time.Time
	if err := e.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// refoldRun is the owner's run in unit, created at created, with the restrictions it captured.
func refoldRun(unit types.GovernanceUnit, created time.Time, restrictions ...uuid.UUID) types.AgentRun {
	id := unit.ID
	return types.AgentRun{ID: uuid.New(), UnitID: &id, CreatedAt: created, CreatedBy: refoldOwner, RestrictionIDs: restrictions}
}

// refoldExpect asserts the one removal wallsRefold gives st.
func refoldExpect(t *testing.T, e *govEnv, run types.AgentRun, prov knobProvenance, setter policyref.Provenance, st wallsStrike, want wallsRemoval) {
	t.Helper()
	got := e.srv.wallsRefold(context.Background(), run, prov, setter, []wallsStrike{st})
	if len(got) != 1 || got[0] != want {
		t.Errorf("wallsRefold(%s) = %+v, want %+v", st.Entry, got, want)
	}
}

// refoldByMid is the tree's middle unit as the verified remover.
var refoldByMid = wallsRemoval{Layer: policyref.LayerUnit, Name: "refold-mid", Verified: true}

// refoldLeafProv is a member's create-time provenance in the tree: the leaf set allowed_domains.
func refoldLeafProv(tree refoldTree) (knobProvenance, policyref.Provenance) {
	leaf := unitRec(tree.leaf.Name)
	return knobProvenance{"allowed_domains": leaf}, leaf
}

// TestWallsRefoldVerifiedAbsentBoundary: a member's run created with no hard-limits boundary, still none, has a
// limits strike that verifies; a run whose provenance records the boundary on a knob, the boundary now absent,
// does not.
func TestWallsRefoldVerifiedAbsentBoundary(t *testing.T) {
	e, tree := refoldSeedTree(t, refoldNoBoundary)
	run := refoldRun(tree.leaf, refoldAge(t, e))
	prov, setter := refoldLeafProv(tree)
	refoldExpect(t, e, run, prov, setter, refoldUnitStrike, refoldByMid)

	bounded := knobProvenance{"allowed_domains": setter, "min_confinement_class": boundaryProvenance}
	refoldExpect(t, e, run, bounded, setter, refoldUnitStrike, wallsRemoval{})
	refoldExpect(t, e, run, nil, setter, refoldUnitStrike, wallsRemoval{})
}

// TestWallsRefoldVerifiedOlderBoundaryRemover: a boundary older than the run by more than the margin removes an
// entry, and the strike verifies with the boundary as its remover.
func TestWallsRefoldVerifiedOlderBoundaryRemover(t *testing.T) {
	e, tree := refoldSeedTree(t, refoldOlderBoundary())
	run := refoldRun(tree.leaf, refoldAge(t, e))
	prov := knobProvenance{"allowed_domains": boundaryProvenance}
	strike := wallsStrike{Field: fieldAllowed, Entry: "boundary-drops.example", Cause: causeLimits}
	refoldExpect(t, e, run, prov, boundaryProvenance, strike, wallsRemoval{Layer: policyref.LayerBoundary, Verified: true})
	// A unit's removal comes before the boundary, the recorded setter, so it is still the unit's.
	refoldExpect(t, e, run, prov, boundaryProvenance, refoldUnitStrike, refoldByMid)
}

// TestWallsRefoldVerifiedWallsResetAfterClamp: the boundary removed an entry at create and the person's walls or
// wall preset re-set allowed_domains after the clamp, so the provenance is theirs, not the boundary's. While the
// boundary stands it is the remover; once it is deleted, the strike only it caused is unverified.
func TestWallsRefoldVerifiedWallsResetAfterClamp(t *testing.T) {
	e, tree := refoldSeedTree(t, refoldOlderBoundary())
	run := refoldRun(tree.leaf, refoldAge(t, e))
	strike := wallsStrike{Field: fieldAllowed, Entry: "boundary-drops.example", Cause: causeLimits}
	setters := []policyref.Provenance{personalProvenance, {Layer: policyref.LayerWallPreset}}
	for _, setter := range setters {
		refoldExpect(t, e, run, knobProvenance{"allowed_domains": setter}, setter, strike,
			wallsRemoval{Layer: policyref.LayerBoundary, Verified: true})
	}
	if _, err := e.pg.PutBoundary(context.Background(), refoldNoBoundary, "seed"); err != nil {
		t.Fatal(err)
	}
	for _, setter := range setters {
		refoldExpect(t, e, run, knobProvenance{"allowed_domains": setter}, setter, strike, wallsRemoval{})
		// The unit's removal does not depend on the boundary and still verifies.
		refoldExpect(t, e, run, knobProvenance{"allowed_domains": setter}, setter, refoldUnitStrike, refoldByMid)
	}
}

// TestWallsRefoldVerifiedEveryChainUnit: a chain row that is no step still counts. A standalone root edited
// after create, and a leaf switched to pass-through after create, each leave the strike unverified though
// neither is its remover.
func TestWallsRefoldVerifiedEveryChainUnit(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, e *govEnv, tree refoldTree)
	}{
		{"standalone root edited", func(t *testing.T, e *govEnv, tree refoldTree) { refoldPut(t, e, tree.root) }},
		{"leaf switched to pass-through", func(t *testing.T, e *govEnv, tree refoldTree) {
			leaf := tree.leaf
			leaf.Mode, leaf.Overlay = types.UnitModePassThrough, &types.CeilingOverlay{}
			refoldPut(t, e, leaf)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, tree := refoldSeedTree(t, refoldNoBoundary)
			run := refoldRun(tree.leaf, refoldAge(t, e))
			prov, setter := refoldLeafProv(tree)
			refoldExpect(t, e, run, prov, setter, refoldUnitStrike, refoldByMid)
			tc.edit(t, e, tree)
			refoldExpect(t, e, run, prov, setter, refoldUnitStrike, wallsRemoval{})
		})
	}
}

// TestWallsRefoldVerifiedEditMargin: the remover edited 90 s before create verifies; 30 s before does not, since
// that edit may have been in flight when the run read its ceiling.
func TestWallsRefoldVerifiedEditMargin(t *testing.T) {
	e, tree := refoldSeedTree(t, refoldNoBoundary)
	created := refoldAge(t, e)
	run := refoldRun(tree.leaf, created)
	prov, setter := refoldLeafProv(tree)
	for _, tc := range []struct {
		before time.Duration
		want   wallsRemoval
	}{{90 * time.Second, refoldByMid}, {30 * time.Second, wallsRemoval{}}} {
		if _, err := e.pool.Exec(context.Background(), `UPDATE governance_units SET updated_at = $2 WHERE id = $1`,
			tree.mid.ID, created.Add(-tc.before)); err != nil {
			t.Fatal(err)
		}
		refoldExpect(t, e, run, prov, setter, refoldUnitStrike, tc.want)
	}
}

// TestWallsRefoldVerifiedDeletedUnit: the run's unit deleted since create leaves nothing to re-fold.
func TestWallsRefoldVerifiedDeletedUnit(t *testing.T) {
	e, tree := refoldSeedTree(t, refoldNoBoundary)
	run := refoldRun(tree.leaf, refoldAge(t, e))
	prov, setter := refoldLeafProv(tree)
	refoldExpect(t, e, run, prov, setter, refoldUnitStrike, refoldByMid)
	if err := e.pg.DeleteGovernanceUnit(context.Background(), tree.leaf.ID); err != nil {
		t.Fatal(err)
	}
	refoldExpect(t, e, run, prov, setter, refoldUnitStrike, wallsRemoval{})
}

// TestWallsRefoldVerifiedReparent: moving the remover under another root with the same walls changes no value,
// and still bumps its updated_at, so the strike is unverified.
func TestWallsRefoldVerifiedReparent(t *testing.T) {
	e, tree := refoldSeedTree(t, refoldNoBoundary)
	other := refoldPut(t, e, types.GovernanceUnit{ID: uuid.New(), Name: "refold-other-root", Ceiling: tree.root.Ceiling})
	run := refoldRun(tree.leaf, refoldAge(t, e))
	prov, setter := refoldLeafProv(tree)
	refoldExpect(t, e, run, prov, setter, refoldUnitStrike, refoldByMid)
	mid := tree.mid
	mid.ParentID = &other.ID
	refoldPut(t, e, mid)
	refoldExpect(t, e, run, prov, setter, refoldUnitStrike, wallsRemoval{})
}

// TestWallsRefoldVerifiedRestrictionUpdatedAfter: a captured restriction is a remover like a unit. A restriction
// that binds the run's owner written after create, the captured one rewritten after create, or the captured one
// deleted, each leave every strike unverified.
func TestWallsRefoldVerifiedRestrictionUpdatedAfter(t *testing.T) {
	strike := wallsStrike{Field: fieldAllowed, Entry: "restriction-drops.example", Cause: causeLimits}
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, e *govEnv, captured types.Restriction)
	}{
		{"a binding restriction written since", func(t *testing.T, e *govEnv, _ types.Restriction) {
			seedRestriction(t, e, "refold-later", types.RestrictionSpec{DeniedDomains: &[]string{"later.example"}},
				types.RestrictionLimits{}, userMember(refoldOwner))
		}},
		{"the captured restriction rewritten", func(t *testing.T, e *govEnv, captured types.Restriction) {
			if _, err := e.pg.PutRestriction(context.Background(), captured, "seed"); err != nil {
				t.Fatal(err)
			}
		}},
		{"the captured restriction deleted", func(t *testing.T, e *govEnv, captured types.Restriction) {
			if _, err := e.pg.DeleteRestriction(context.Background(), captured.ID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, tree := refoldSeedTree(t, refoldNoBoundary)
			captured := seedRestriction(t, e, "refold-captured",
				types.RestrictionSpec{AllowedDomains: &[]string{"keep.example", "boundary-drops.example"}},
				types.RestrictionLimits{}, userMember(refoldOwner))
			run := refoldRun(tree.leaf, refoldAge(t, e), captured.ID)
			setter := restrictionProvenance(captured)
			prov := knobProvenance{"allowed_domains": setter}
			refoldExpect(t, e, run, prov, setter, strike,
				wallsRemoval{Layer: policyref.LayerRestriction, Name: "refold-captured", Verified: true})
			refoldExpect(t, e, run, prov, setter, refoldUnitStrike, refoldByMid)
			tc.edit(t, e, captured)
			refoldExpect(t, e, run, prov, setter, strike, wallsRemoval{})
			refoldExpect(t, e, run, prov, setter, refoldUnitStrike, wallsRemoval{})
		})
	}
}
