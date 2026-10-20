// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Wardyn boundary (#2945) is the outermost layer of every run's effective policy. This file is
// how it narrows one: boundaryMeet, the ONE function the resolve, the run's own policy step and the
// dry-run report all call, so the report is the effect.

// boundaryProvenance is the record of a value the boundary decided. Never locked: the boundary
// forbids only widening, and a lower layer may still narrow what it allows.
var boundaryProvenance = policyref.Provenance{Layer: policyref.LayerBoundary}

// boundaryChange is one knob the boundary narrowed: its name and the value before and after.
type boundaryChange struct {
	Knob   string `json:"knob"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// boundaryMeet is spec and limits narrowed by the boundary: composer.ApplyOverlay with the boundary
// as the overlay over the policy, so a knob the boundary does not name is inherited and one it names
// can only narrow (a meet, never Clamp). Two things the overlay cannot say are done here. A policy
// that allows all egress meets a boundary allowlist as that allowlist, not as nothing, because an
// open run reaches every host the list names. Eligible grants are re-intersected with the comparator
// reintersectGovernanceGrants uses, dropping each grant the boundary's grants do not dominate.
//
// It reports each knob it changed. A meet nothing satisfies is impossible over the knobs the
// boundary holds (types.BoundarySpec); an error is still returned, never a wider policy.
func boundaryMeet(b types.Boundary, spec types.RunPolicySpec, limits types.GovernanceLimits) (types.RunPolicySpec, types.GovernanceLimits, []boundaryChange, error) {
	base := composer.Authority{Ceiling: spec.Clone(), Limits: limits}
	if base.Ceiling.AllowAllEgress && b.Spec.AllowedDomains != nil && (b.Spec.AllowAllEgress == nil || !*b.Spec.AllowAllEgress) {
		base.Ceiling.AllowedDomains = slices.Clone(*b.Spec.AllowedDomains)
	}
	ov := b.Spec.Overlay()
	// A floor at the lowest class bounds nothing: every run launches at CC1 or above, so an unset
	// floor already means CC1. It is left out of the meet, so the fresh-install seed (floor CC1)
	// changes no value and adds no narrowed warning.
	if ov.MinConfinementClass != nil && *ov.MinConfinementClass == types.CC1 {
		ov.MinConfinementClass = nil
	}
	out, _, err := composer.ApplyOverlay(base, composer.Overlay{Ceiling: ov, Limits: b.Limits.Overlay()})
	if err != nil {
		return types.RunPolicySpec{}, types.GovernanceLimits{}, nil, fmt.Errorf("api: meet the Wardyn boundary: %w", err)
	}
	if g := b.Spec.EligibleGrants; g != nil {
		kept := out.Ceiling.EligibleGrants[:0:0]
		for _, grant := range out.Ceiling.EligibleGrants {
			if governanceGrantWithinCeiling(grant, *g) == nil {
				kept = append(kept, grant)
			}
		}
		out.Ceiling.EligibleGrants = kept
	}
	return out.Ceiling, out.Limits, boundaryChanges(spec, out.Ceiling, limits, out.Limits), nil
}

// boundaryChanges lists the knobs that differ between before and after, the policy's compared in the
// canonical form provenance uses (canonicalKnobs), then the run limits the boundary bounds.
func boundaryChanges(before, after types.RunPolicySpec, lb, la types.GovernanceLimits) []boundaryChange {
	var out []boundaryChange
	bv, av := reflect.ValueOf(canonicalKnobs(before)), reflect.ValueOf(canonicalKnobs(after))
	for i, name := range policyKnobFields {
		if name != "" && !reflect.DeepEqual(bv.Field(i).Interface(), av.Field(i).Interface()) {
			out = append(out, boundaryChange{Knob: name, Before: bv.Field(i).Interface(), After: av.Field(i).Interface()})
		}
	}
	if lb.MaxEndAheadSec != la.MaxEndAheadSec {
		out = append(out, boundaryChange{Knob: "limits.max_end_ahead_sec", Before: lb.MaxEndAheadSec, After: la.MaxEndAheadSec})
	}
	if lb.DefaultEndSec != la.DefaultEndSec {
		out = append(out, boundaryChange{Knob: "limits.default_end_sec", Before: lb.DefaultEndSec, After: la.DefaultEndSec})
	}
	return out
}

// boundaryWarnings is the sentence each narrowed knob adds to a run's warnings (Q2: wider profiles
// and policies narrow with a warning; a run is never refused for having been wider).
func boundaryWarnings(changes []boundaryChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, "the Wardyn boundary narrowed "+c.Knob)
	}
	return out
}

// governanceBoundary reads the boundary. A store that keeps none (not Postgres, or no store) is in
// no_limits.
func (s *Server) governanceBoundary(ctx context.Context) (types.Boundary, error) {
	bs, ok := s.cfg.Store.(store.BoundaryStore)
	if !ok {
		return types.Boundary{Mode: types.BoundaryNoLimits}, nil
	}
	b, err := bs.GetBoundary(ctx)
	if err != nil {
		return types.Boundary{}, fmt.Errorf("api: read the Wardyn boundary: %w", err)
	}
	return b, nil
}

// boundaryCeiling is the last step of resolveEffectiveCeiling: the ceiling the profile chain (or the
// deployment, or an operator's own) resolved, met with the boundary in hard_limits. A store error is
// an error, never the unbounded ceiling. Every caller is bound, the admin token's break-glass run
// included: break-glass lifts a profile's walls, never the boundary.
func (s *Server) boundaryCeiling(ctx context.Context, c governanceCeiling) (governanceCeiling, error) {
	b, err := s.governanceBoundary(ctx)
	if err != nil || !b.HardLimits() {
		return c, err
	}
	c.boundary = &b
	spec, limits, changes, err := boundaryMeet(b, c.Spec, c.Limits)
	if err != nil {
		return governanceCeiling{}, err
	}
	c.Spec, c.Limits = spec, limits
	c.Warnings = append(slices.Clone(c.Warnings), boundaryWarnings(changes)...)
	for _, ch := range changes {
		c.boundaryKnobs = append(c.boundaryKnobs, ch.Knob)
	}
	return c, nil
}

// boundaryOperatorCeiling is the create path's ceiling for an operator: {Operator: true}, which
// clamps nothing, plus the boundary's run-limit maximum in hard_limits, so resolveCreateEnd bounds
// the run's end and the run captures it (boundaryEndCap). The operator's policy is bound by
// boundaryRunStep, over the ceiling it resolves itself.
func (s *Server) boundaryOperatorCeiling(ctx context.Context) (governanceCeiling, error) {
	c := governanceCeiling{Operator: true}
	b, err := s.governanceBoundary(ctx)
	if err != nil || !b.HardLimits() {
		return c, err
	}
	c.boundary = &b
	if m := b.Limits.MaxEndAheadSec; m != nil {
		c.Limits.RunLimits = types.RunLimits{UserChangesLimits: true, MaxEndAheadSec: *m}
	}
	return c, nil
}

// bounded reports whether the boundary binds this ceiling's runs: in hard_limits, every run.
func (c governanceCeiling) bounded() bool { return c.boundary != nil }

// boundaryBoundsEnd reports whether the boundary bounds a run's end where governance alone would not
// (an operator's run, or a break-glass run): only when the boundary names a run-limit maximum.
func (c governanceCeiling) boundaryBoundsEnd() bool {
	return c.boundary != nil && c.boundary.Limits.MaxEndAheadSec != nil
}

// boundaryRunStep is resolveRunPolicyFacts' last narrowing step, for every path: an inline policy, a
// stored row (one an unassigned member selects is clamped by nothing else), the no-policy default
// and an operator's unclamped run. Knobs the boundary already narrowed in the ceiling are recorded
// as the boundary's where the ceiling's layer set them.
func (s *Server) boundaryRunStep(ceiling governanceCeiling, spec *types.RunPolicySpec, source *policySourceRecord) ([]string, *runRefusal) {
	if !ceiling.bounded() {
		return nil, nil
	}
	var warns []string
	var err error
	source.knobs.step(boundaryProvenance, spec, func() {
		var changes []boundaryChange
		var met types.RunPolicySpec
		if met, _, changes, err = boundaryMeet(*ceiling.boundary, *spec, types.GovernanceLimits{}); err == nil {
			*spec, warns = met, boundaryWarnings(changes)
		}
	})
	if err != nil {
		return nil, runServerError("meet the Wardyn boundary", err)
	}
	if !ceiling.Operator && source.knobs != nil {
		rec := ceiling.provenance()
		for _, name := range ceiling.boundaryKnobs {
			if source.knobs[name] == rec {
				source.knobs[name] = boundaryProvenance
			}
		}
	}
	return warns, nil
}

// boundaryRecordPolicy is a Record Mode session's policy met with the boundary in hard_limits: the
// session's own spec skips the member clamp and allows all egress to learn, so without this a super
// admin's recording would reach every host. It learns inside the boundary instead: allow-all becomes
// its allowlist, its denies are added and the floor is raised. In no_limits the policy is unchanged.
func boundaryRecordPolicy(ceiling governanceCeiling, policy types.RunPolicySpec) (types.RunPolicySpec, error) {
	if !ceiling.bounded() {
		return policy, nil
	}
	met, _, _, err := boundaryMeet(*ceiling.boundary, policy, types.GovernanceLimits{})
	return met, err
}

// boundaryAdmitsHost reports whether the boundary allows an egress approval to admit host: a host it
// denies, or one outside an allowlist it names, is never admitted in hard_limits.
func boundaryAdmitsHost(b types.Boundary, host string) bool {
	if d := b.Spec.DeniedDomains; d != nil && ceilingDenies(*d, host) {
		return false
	}
	if a := b.Spec.AllowedDomains; a != nil && !ceilingDenies(*a, host) {
		return false
	}
	return true
}

// boundaryEndPlan is the run a change to its end is planned against, and whether the change is
// exempt from its limits. While the boundary names a maximum (boundaryEndCap) every change is held
// to it: a super admin's to the boundary's maximum alone, not to the profile the run launched under,
// anyone else's to the tighter of it and what the run captured. ok=false: the 500 is written.
func (s *Server) boundaryEndPlan(w http.ResponseWriter, r *http.Request, run types.AgentRun) (types.AgentRun, bool, bool) {
	exempt := s.runUngoverned(r.Context())
	capSec, err := s.boundaryEndCap(r.Context(), run)
	if err != nil {
		writeServerError(w, r, "read the Wardyn boundary", err)
		return run, false, false
	}
	switch {
	case capSec == 0:
		return run, exempt, true
	case exempt:
		run.RunLimits, run.GovernanceProfileID = types.RunLimits{UserChangesLimits: true, MaxEndAheadSec: capSec}, nil
	default:
		// Any other caller is held to the limits the run captured, and to the boundary's maximum when
		// that is tighter: a run created in no_limits captured no boundary limit.
		if tighter := composer.TighterSec(run.RunLimits.MaxEndAheadSec, capSec); tighter != run.RunLimits.MaxEndAheadSec {
			run.RunLimits.MaxEndAheadSec, run.GovernanceProfileID = tighter, nil
		}
	}
	return run, false, true
}

// boundaryEndCap is the boundary's bound on a change to run's end, 0 for none: the boundary's maximum
// in hard_limits as it stands now, for every run. A break-glass run is also held to the maximum it
// captured at create, so a boundary set or tightened later binds it and a loosened one frees nothing.
func (s *Server) boundaryEndCap(ctx context.Context, run types.AgentRun) (int, error) {
	capSec, err := s.boundaryMaxEndSec(ctx)
	if err != nil || !run.BreakGlass {
		return capSec, err
	}
	return composer.TighterSec(run.RunLimits.MaxEndAheadSec, capSec), nil
}

// boundaryMaxEndSec is the boundary's run-limit maximum in hard_limits, 0 for none.
func (s *Server) boundaryMaxEndSec(ctx context.Context) (int, error) {
	b, err := s.governanceBoundary(ctx)
	if err != nil || !b.HardLimits() || b.Limits.MaxEndAheadSec == nil {
		return 0, err
	}
	return *b.Limits.MaxEndAheadSec, nil
}

// boundaryFloorAdmits reports whether a run of class may run inside b: in hard_limits, not below the
// boundary's confinement floor. An unset class reads as the lowest, CC1.
func boundaryFloorAdmits(b types.Boundary, class types.ConfinementClass) bool {
	floor := b.Spec.MinConfinementClass
	return !b.HardLimits() || floor == nil || cmp.Or(class, types.CC1).Rank() >= floor.Rank()
}

// dispatchBelowBoundaryFloor refuses, at dispatchAdmitted, a run whose confinement class (fixed at
// create) is below the boundary's floor, as a run created before the floor was raised is: the run is
// failed with a run.dispatch row, never launched below it. A boundary that cannot be read refuses too.
func (s *Server) dispatchBelowBoundaryFloor(ctx context.Context, run types.AgentRun) bool {
	b, err := s.governanceBoundary(ctx)
	if err == nil && boundaryFloorAdmits(b, run.ConfinementClass) {
		return false
	}
	note, msg := "the run's confinement class is below the Wardyn boundary's floor",
		"This run was not launched: its confinement class is below the Wardyn boundary's floor; start a new run"
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the Wardyn boundary at dispatch", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		note, msg = "the Wardyn boundary could not be read", "This run was not launched: the Wardyn boundary could not be read"
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
		run.ID.String(), "failure", mustJSON(map[string]any{"note": note})))
	s.failAndRevoke(context.WithoutCancel(ctx), run.ID, types.RunPending, msg)
	return true
}

// reviveBelowBoundaryFloor refuses a revive of a run whose confinement class is below the
// boundary's floor as it stands now, with the run.revive denied row the other revive doors write.
func (s *Server) reviveBelowBoundaryFloor(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor string) *reviveError {
	b, err := s.governanceBoundary(ctx)
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable, loggedMsg(ctx, "re-check the Wardyn boundary", err))
	}
	if boundaryFloorAdmits(b, run.ConfinementClass) {
		return nil
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
		mustJSON(map[string]any{"subject": run.CreatedBy, "reason": reasonConfinementClassConflict})))
	return reviveRefused(http.StatusConflict, reasonConfinementClassConflict,
		"this run's confinement class "+string(run.ConfinementClass)+" is below the Wardyn boundary's floor "+
			string(*b.Spec.MinConfinementClass)+"; start a new run")
}

// boundaryAdmitApproval is the boundary's bound on an egress approval (approvals.go's egress_host
// gate is the member's): in hard_limits no decider admits a host it denies or does not allow, on any
// run, a break-glass run's included. ok=false: the 403 is written and the approval stays pending. It returns the run, read here when the gate before it had not.
func (s *Server) boundaryAdmitApproval(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest, run types.AgentRun, haveRun bool) (types.AgentRun, bool, bool) {
	b, err := s.governanceBoundary(r.Context())
	if err != nil {
		writeServerError(w, r, "read the Wardyn boundary", err)
		return run, haveRun, false
	}
	if !b.HardLimits() {
		return run, haveRun, true
	}
	if !haveRun {
		if run, err = s.cfg.Store.GetRun(r.Context(), ap.RunID); err != nil {
			writeServerError(w, r, "get run", err)
			return run, false, false
		}
		haveRun = true
	}
	host := approvalHost(ap)
	if boundaryAdmitsHost(b, host) {
		return run, haveRun, true
	}
	writeErrorReason(w, http.StatusForbidden, reasonBoundaryHost,
		"the Wardyn boundary does not allow "+host+", so no approval can admit it on this run — ask a security admin to change the boundary")
	return run, haveRun, false
}
