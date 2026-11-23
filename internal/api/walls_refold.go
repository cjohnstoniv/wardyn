// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Walls view's re-fold: which layer removed an entry from a run's walls. It composes the run's own unit
// chain, its captured restrictions and the boundary again, as they read now, with an observer on each resolve
// step (composeChainObserved, withRestrictionsObserved, boundCeilingObserved), and names the one step that
// removed the entry. The name is verified only when nothing the fold read can differ from what the run was
// created under; otherwise the view names no remover.

// wallsStepKind is the layer one resolve step belongs to. Each is the policyref layer of the same name.
type wallsStepKind string

const (
	wallsRefoldDeployment  wallsStepKind = policyref.LayerDeployment
	wallsRefoldUnit        wallsStepKind = policyref.LayerUnit
	wallsRefoldRestriction wallsStepKind = policyref.LayerRestriction
	wallsRefoldBoundary    wallsStepKind = policyref.LayerBoundary
)

// wallsStep is one resolve step as an observer sees it: the walls entering it and the walls leaving it.
type wallsStep struct {
	Kind          wallsStepKind
	ID            uuid.UUID           // zero for deployment and boundary
	Name          string              // the unit's or restriction's; the deployment's is "deployment", the boundary's empty
	UpdatedAt     *time.Time          // nil for deployment
	Before, After types.RunPolicySpec // Clone()s, built only when an observer is set
}

// wallsObserver is handed each step of a resolve, in the order the steps run.
type wallsObserver func(wallsStep)

// wallsStrike is one entry the explain causes struck from a run's walls. Field is "allowed_domains" or
// "denied_domains"; Cause is the explain cause.
type wallsStrike struct{ Field, Entry, Cause string }

// wallsRemoval names the layer that removed one strike's entry. The zero value means "no verified remover".
type wallsRemoval struct {
	Layer, Name string
	Verified    bool
}

// wallsVerifyMargin is how much older than its run every unit, restriction and boundary a re-fold reads must
// be for the re-fold to name a remover. Both times are the database's: a run's created_at is stamped on the
// database's clock when the run is inserted, and a write to a unit, a restriction or the boundary sets
// updated_at = now(). now() is the start of the writing transaction, so an edit already in flight when the
// run read its ceiling can carry an updated_at earlier than the run's created_at. The margin covers that edit.
const wallsVerifyMargin = 60 * time.Second

// wallsRefoldMaxStrikes is the most strikes one re-fold names a remover for; the rest get the zero removal.
const wallsRefoldMaxStrikes = 256

// wallsRefoldDeploymentStep is the step that starts a chain from the deployment default: nothing enters it.
func wallsRefoldDeploymentStep(after types.RunPolicySpec) wallsStep {
	return wallsStep{Kind: wallsRefoldDeployment, Name: policyref.SourceDeployment, After: after.Clone()}
}

// wallsRefoldUnitStep is row's step. before is already the resolver's own copy, handed over as it is.
func wallsRefoldUnitStep(row types.GovernanceUnit, before, after types.RunPolicySpec) wallsStep {
	at := row.UpdatedAt
	return wallsStep{Kind: wallsRefoldUnit, ID: row.ID, Name: row.Name, UpdatedAt: &at, Before: before, After: after.Clone()}
}

// wallsRefoldRestrictionStep is r's step.
func wallsRefoldRestrictionStep(r types.Restriction, before, after types.RunPolicySpec) wallsStep {
	at := r.UpdatedAt
	return wallsStep{Kind: wallsRefoldRestriction, ID: r.ID, Name: r.Name, UpdatedAt: &at, Before: before.Clone(), After: after.Clone()}
}

// wallsRefoldBoundaryStep is the boundary's step. Its UpdatedAt is a copy, nil when the boundary has none.
func wallsRefoldBoundaryStep(b types.Boundary, before, after types.RunPolicySpec) wallsStep {
	st := wallsStep{Kind: wallsRefoldBoundary, Before: before.Clone(), After: after.Clone()}
	if b.UpdatedAt != nil {
		at := *b.UpdatedAt
		st.UpdatedAt = &at
	}
	return st
}

// wallsRefold names, for each strike, the layer that removed its entry, aligned with strikes. It re-folds the
// run's unit chain, the restrictions the run captured and the boundary, as they read now, and finds each
// entry's remover by the clamp's rule (wallsRefoldRemover). A removal is Verified only when all of these hold:
//   - the fold took no step from the deployment default, which has no version;
//   - every chain unit (a standalone root and a pass-through unit included), every restriction the run captured
//     (each one still existing), the boundary when it is in hard_limits, and every restriction that binds the
//     run's creator by subject or user type now, was last written at least wallsVerifyMargin before the run;
//   - when the fold took no boundary step, a limits strike's prov records no boundary layer on any knob, since
//     a boundary that changed a knob at create and is gone now cannot be re-folded;
//   - exactly one step removed the entry, and that step is not later in the fold than allowedSetter, the layer
//     create recorded as setting allowed_domains.
//
// prov is the run's create-time provenance; with none, nothing verifies. A read or compose error is logged and
// leaves every removal zero, and the fold runs as a display read, so it records no refusal.
func (s *Server) wallsRefold(ctx context.Context, run types.AgentRun, prov knobProvenance, allowedSetter policyref.Provenance, strikes []wallsStrike) []wallsRemoval {
	out := make([]wallsRemoval, len(strikes))
	if len(strikes) == 0 || prov == nil {
		return out
	}
	ctx = withDisplayRead(ctx)
	fold, err := s.wallsRefoldRead(ctx, run)
	if err != nil {
		slog.WarnContext(ctx, "walls: the re-fold of a run's walls failed; no remover is verified", "run_id", run.ID, "err", err)
		return out
	}
	if !fold.settled {
		return out
	}
	setter, placed := fold.rank(allowedSetter)
	if !placed {
		return out
	}
	limitsBlocked := !fold.has(wallsRefoldBoundary) && wallsRefoldRecordsBoundary(prov)
	for i := range min(len(strikes), wallsRefoldMaxStrikes) {
		st := strikes[i]
		if st.Cause == causeLimits && limitsBlocked {
			continue
		}
		j, found := wallsRefoldRemover(fold.steps, st)
		if !found {
			continue
		}
		step := fold.steps[j]
		if r, inFold := fold.rank(step.refoldProvenance()); inFold && r <= setter {
			out[i] = wallsRemoval{Layer: string(step.Kind), Name: step.Name, Verified: true}
		}
	}
	return out
}

// wallsRefoldRecordsBoundary reports whether prov records the boundary as the layer of any knob.
func wallsRefoldRecordsBoundary(prov knobProvenance) bool {
	for _, p := range prov {
		if p.Layer == policyref.LayerBoundary {
			return true
		}
	}
	return false
}

// wallsRefoldFold is one re-fold: its steps in the order they ran, and what it read to take them.
type wallsRefoldFold struct {
	steps        []wallsStep
	chain        []types.GovernanceUnit // leaf first
	restrictions []types.Restriction    // the run's captured restrictions that still exist, in name order
	// settled says no step is the deployment's and nothing read was written within wallsVerifyMargin of the run.
	settled bool
}

// wallsRefoldRead reads and composes run's walls with an observer on every step. A run with no unit folds to
// nothing and is never settled.
func (s *Server) wallsRefoldRead(ctx context.Context, run types.AgentRun) (wallsRefoldFold, error) {
	var fold wallsRefoldFold
	chain, err := s.wallsChainRead(ctx, run)
	if err != nil || len(chain) == 0 {
		return fold, err
	}
	fold.chain = chain
	obs := func(st wallsStep) { fold.steps = append(fold.steps, st) }
	res, err := s.composeChainObserved(chain, obs)
	if err != nil {
		return fold, err
	}
	c := governanceCeiling{Spec: res.Ceiling.Clone(), Limits: res.Limits, Unit: res}
	rs, haveRestrictions := s.restrictionStore()
	if len(run.RestrictionIDs) > 0 {
		if !haveRestrictions {
			return fold, errors.New("api: the run names restrictions and the store keeps none")
		}
		if fold.restrictions, err = rs.GetRestrictionsByID(ctx, run.RestrictionIDs); err != nil {
			return fold, err
		}
		if c, err = s.withRestrictionsObserved(ctx, c, fold.restrictions, obs); err != nil {
			return fold, err
		}
	}
	b, err := s.governanceBoundary(ctx)
	if err != nil {
		return fold, err
	}
	if _, err = boundCeilingObserved(b, c, obs); err != nil {
		return fold, err
	}
	var binding []types.Restriction
	if haveRestrictions {
		// The creator's groups are unknowable here, as for a revive by anyone but the owner, so a group-bound
		// restriction counts only through the run's captured ids.
		subjects := []string{types.CanonicalUserSubject(run.CreatedBy)}
		if binding, err = rs.ListRestrictionsFor(ctx, subjects, nil, run.UserType); err != nil {
			return fold, err
		}
	}
	fold.settled = fold.predates(run, binding)
	return fold, nil
}

// predates reports whether nothing the fold read can differ from what run was created under: no deployment
// step, every unit, restriction and boundary written at least wallsVerifyMargin before the run, every captured
// restriction still there, and no restriction binding the creator now written since.
func (f wallsRefoldFold) predates(run types.AgentRun, binding []types.Restriction) bool {
	bound := run.CreatedAt.Add(-wallsVerifyMargin)
	old := func(t time.Time) bool { return !t.IsZero() && !t.After(bound) }
	for _, u := range f.chain {
		if !old(u.UpdatedAt) {
			return false
		}
	}
	for _, st := range f.steps {
		if st.Kind == wallsRefoldDeployment || st.UpdatedAt == nil || !old(*st.UpdatedAt) {
			return false
		}
	}
	for _, id := range run.RestrictionIDs {
		if !slices.ContainsFunc(f.restrictions, func(r types.Restriction) bool { return r.ID == id }) {
			return false
		}
	}
	for _, r := range binding {
		if !old(r.UpdatedAt) {
			return false
		}
	}
	return true
}

// has reports whether the fold took a step of kind.
func (f wallsRefoldFold) has(kind wallsStepKind) bool {
	return slices.ContainsFunc(f.steps, func(st wallsStep) bool { return st.Kind == kind })
}

// rank is where layer p sits in the fold's order: the deployment, the chain's units from the root to the leaf,
// the restrictions in name order, the boundary, then the person's walls and the wall preset, which create
// applies after the ceiling. ok is false for a layer the fold cannot place: the run's own policy, an operator's
// run, or a unit or restriction the fold did not read. Unit and restriction names are unique.
func (f wallsRefoldFold) rank(p policyref.Provenance) (int, bool) {
	n, m := len(f.chain), len(f.restrictions)
	switch p.Layer {
	case policyref.LayerDeployment:
		return 0, true
	case policyref.LayerUnit:
		if i := slices.IndexFunc(f.chain, func(u types.GovernanceUnit) bool { return u.Name == p.Name }); i >= 0 {
			return n - i, true
		}
	case policyref.LayerRestriction:
		if i := slices.IndexFunc(f.restrictions, func(r types.Restriction) bool { return r.Name == p.Name }); i >= 0 {
			return n + 1 + i, true
		}
	case policyref.LayerBoundary:
		return n + 1 + m, true
	case policyref.LayerPersonal, policyref.LayerWallPreset:
		return n + 2 + m, true
	}
	return 0, false
}

// refoldProvenance is the record of the layer st belongs to.
func (st wallsStep) refoldProvenance() policyref.Provenance {
	return policyref.Provenance{Layer: string(st.Kind), Name: st.Name}
}

// wallsRefoldRemover is the index of the one step that removed st's entry. An allowed entry is removed, by the
// clamp's rule (composer.Clamp), by a step whose walls entering it hold the entry or allow all egress and whose
// walls leaving it do neither; a list holds an entry when one item equals it once both are lower-cased and
// trimmed. A denied entry is removed by the step whose deny-list starts covering it (ceilingDenies). ok is false
// when no step removed it ("not on any wall's list"), when more than one did, and for any other field.
func wallsRefoldRemover(steps []wallsStep, st wallsStrike) (int, bool) {
	var removes func(wallsStep) bool
	switch st.Field {
	case fieldAllowed:
		removes = func(s wallsStep) bool {
			return wallsRefoldAllows(s.Before, st.Entry) && !wallsRefoldAllows(s.After, st.Entry)
		}
	case fieldDenied:
		removes = func(s wallsStep) bool {
			return !ceilingDenies(s.Before.DeniedDomains, st.Entry) && ceilingDenies(s.After.DeniedDomains, st.Entry)
		}
	default:
		return 0, false
	}
	found := -1
	for i, s := range steps {
		if !removes(s) {
			continue
		}
		if found >= 0 {
			return 0, false
		}
		found = i
	}
	return found, found >= 0
}

// wallsRefoldAllows is the clamp's membership: spec allows all egress, or its allowlist holds entry exactly
// once both are lower-cased and trimmed.
func wallsRefoldAllows(spec types.RunPolicySpec, entry string) bool {
	if spec.AllowAllEgress {
		return true
	}
	e := strings.ToLower(strings.TrimSpace(entry))
	return slices.ContainsFunc(spec.AllowedDomains, func(x string) bool { return strings.ToLower(strings.TrimSpace(x)) == e })
}
