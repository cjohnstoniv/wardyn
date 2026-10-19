// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The organisation walls (#2945) are the outermost limit on every run's effective policy. This file
// is how they narrow one: wallsMeet, the ONE function the resolve, the run's own policy step and the
// dry-run report all call, so the report is the effect.

// wallsProvenance is the record of a value the walls decided. Never locked: the walls forbid only
// widening, and a lower layer may still narrow what they allow.
var wallsProvenance = policyref.Provenance{Layer: policyref.LayerWalls}

// wallsChange is one knob the walls narrowed: its name and the value before and after.
type wallsChange struct {
	Knob   string `json:"knob"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// wallsMeet is spec and limits narrowed by the walls: composer.ApplyOverlay with the walls as the
// overlay over the policy, so a knob the walls do not name is inherited and one they name can only
// narrow (a meet, never Clamp). Two things the overlay cannot say are done here. A policy that
// allows all egress meets a walls allowlist as that allowlist, not as nothing, because an open run
// reaches every host the list names. Eligible grants are re-intersected with the comparator
// reintersectGovernanceGrants uses, dropping each grant the walls' grants do not dominate.
//
// It reports each knob it changed. A meet nothing satisfies is impossible over the knobs the walls
// hold (types.OrgWallsSpec); an error is still returned, never a wider policy.
func wallsMeet(w types.OrgWalls, spec types.RunPolicySpec, limits types.GovernanceLimits) (types.RunPolicySpec, types.GovernanceLimits, []wallsChange, error) {
	base := composer.Authority{Ceiling: spec.Clone(), Limits: limits}
	if base.Ceiling.AllowAllEgress && w.Spec.AllowedDomains != nil && (w.Spec.AllowAllEgress == nil || !*w.Spec.AllowAllEgress) {
		base.Ceiling.AllowedDomains = slices.Clone(*w.Spec.AllowedDomains)
	}
	out, _, err := composer.ApplyOverlay(base, composer.Overlay{Ceiling: w.Spec.Overlay(), Limits: w.Limits.Overlay()})
	if err != nil {
		return types.RunPolicySpec{}, types.GovernanceLimits{}, nil, fmt.Errorf("api: meet the org walls: %w", err)
	}
	if g := w.Spec.EligibleGrants; g != nil {
		kept := out.Ceiling.EligibleGrants[:0:0]
		for _, grant := range out.Ceiling.EligibleGrants {
			if governanceGrantWithinCeiling(grant, *g) == nil {
				kept = append(kept, grant)
			}
		}
		out.Ceiling.EligibleGrants = kept
	}
	return out.Ceiling, out.Limits, wallsChanges(spec, out.Ceiling, limits, out.Limits), nil
}

// wallsChanges lists the knobs that differ between before and after, the policy's compared in the
// canonical form provenance uses (canonicalKnobs), then the run limits the walls bound.
func wallsChanges(before, after types.RunPolicySpec, lb, la types.GovernanceLimits) []wallsChange {
	var out []wallsChange
	bv, av := reflect.ValueOf(canonicalKnobs(before)), reflect.ValueOf(canonicalKnobs(after))
	for i, name := range policyKnobFields {
		if name != "" && !reflect.DeepEqual(bv.Field(i).Interface(), av.Field(i).Interface()) {
			out = append(out, wallsChange{Knob: name, Before: bv.Field(i).Interface(), After: av.Field(i).Interface()})
		}
	}
	if lb.MaxEndAheadSec != la.MaxEndAheadSec {
		out = append(out, wallsChange{Knob: "limits.max_end_ahead_sec", Before: lb.MaxEndAheadSec, After: la.MaxEndAheadSec})
	}
	if lb.DefaultEndSec != la.DefaultEndSec {
		out = append(out, wallsChange{Knob: "limits.default_end_sec", Before: lb.DefaultEndSec, After: la.DefaultEndSec})
	}
	return out
}

// wallsWarnings is the sentence each narrowed knob adds to a run's warnings (Q2: wider profiles
// and policies narrow with a warning; a run is never refused for having been wider).
func wallsWarnings(changes []wallsChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, "the org walls narrowed "+c.Knob)
	}
	return out
}

// orgWalls reads the walls. A store that keeps none (not Postgres, or no store) has none.
func (s *Server) orgWalls(ctx context.Context) (types.OrgWalls, error) {
	ws, ok := s.cfg.Store.(store.OrgWallsStore)
	if !ok {
		return types.OrgWalls{}, nil
	}
	w, err := ws.GetOrgWalls(ctx)
	if err != nil {
		return types.OrgWalls{}, fmt.Errorf("api: read the org walls: %w", err)
	}
	return w, nil
}

// wallCeiling is the last step of resolveEffectiveCeiling: the ceiling the profile chain (or the
// deployment, or an operator's own) resolved, met with the walls when they are on. A store error is
// an error, never the unwalled ceiling. The admin token's break-glass run (breakGlassReason) is the
// one caller the walls do not narrow; its end is capped at the walls' run-limit maximum instead.
func (s *Server) wallCeiling(ctx context.Context, c governanceCeiling) (governanceCeiling, error) {
	w, err := s.orgWalls(ctx)
	if err != nil || !w.Enabled {
		return c, err
	}
	c.walls = &w
	if breakGlassReason(ctx) != "" {
		c.breakGlass = true
		if m := w.Limits.MaxEndAheadSec; m != nil {
			c.Limits.RunLimits.MaxEndAheadSec = composer.TighterSec(c.Limits.RunLimits.MaxEndAheadSec, *m)
		}
		return c, nil
	}
	spec, limits, changes, err := wallsMeet(w, c.Spec, c.Limits)
	if err != nil {
		return governanceCeiling{}, err
	}
	c.Spec, c.Limits = spec, limits
	c.Warnings = append(slices.Clone(c.Warnings), wallsWarnings(changes)...)
	for _, ch := range changes {
		c.wallKnobs = append(c.wallKnobs, ch.Knob)
	}
	return c, nil
}

// wallOperatorCeiling is the create path's ceiling for an operator: {Operator: true}, which clamps
// nothing, plus the walls' run-limit maximum while they are on, so resolveCreateEnd bounds the run's
// end and the run captures it (wallsEndCap). The operator's policy is bound by wallsRunStep, over the ceiling it resolves itself.
func (s *Server) wallOperatorCeiling(ctx context.Context) (governanceCeiling, error) {
	c := governanceCeiling{Operator: true}
	w, err := s.orgWalls(ctx)
	if err != nil || !w.Enabled {
		return c, err
	}
	c.walls, c.breakGlass = &w, breakGlassReason(ctx) != ""
	if m := w.Limits.MaxEndAheadSec; m != nil {
		c.Limits.RunLimits = types.RunLimits{UserChangesLimits: true, MaxEndAheadSec: *m}
	}
	return c, nil
}

// walled reports whether the walls bind this ceiling's runs.
func (c governanceCeiling) walled() bool { return c.walls != nil && !c.breakGlass }

// wallsBoundEnd reports whether the walls bound a run's end where governance alone would not (an
// operator's run, or a break-glass run): only when the walls name a run-limit maximum.
func (c governanceCeiling) wallsBoundEnd() bool {
	return c.walls != nil && c.walls.Limits.MaxEndAheadSec != nil
}

// wallsRunStep is resolveRunPolicyFacts' last narrowing step, for every path: an inline policy, a
// stored row (one an unassigned member selects is clamped by nothing else), the no-policy default
// and an operator's unclamped run. Knobs the walls already narrowed in the ceiling are recorded as
// the walls' where the ceiling's layer set them.
func (s *Server) wallsRunStep(ceiling governanceCeiling, spec *types.RunPolicySpec, source *policySourceRecord) ([]string, *runRefusal) {
	if !ceiling.walled() {
		return nil, nil
	}
	var warns []string
	var err error
	source.knobs.step(wallsProvenance, spec, func() {
		var changes []wallsChange
		var met types.RunPolicySpec
		if met, _, changes, err = wallsMeet(*ceiling.walls, *spec, types.GovernanceLimits{}); err == nil {
			*spec, warns = met, wallsWarnings(changes)
		}
	})
	if err != nil {
		return nil, runServerError("meet the org walls", err)
	}
	if !ceiling.Operator && source.knobs != nil {
		rec := ceiling.provenance()
		for _, name := range ceiling.wallKnobs {
			if source.knobs[name] == rec {
				source.knobs[name] = wallsProvenance
			}
		}
	}
	return warns, nil
}

// wallsAdmitHost reports whether the walls allow an egress approval to admit host: a host the walls
// deny, or one outside an allowlist they name, is never admitted while they are on.
func wallsAdmitHost(w types.OrgWalls, host string) bool {
	if d := w.Spec.DeniedDomains; d != nil && ceilingDenies(*d, host) {
		return false
	}
	if a := w.Spec.AllowedDomains; a != nil && !ceilingDenies(*a, host) {
		return false
	}
	return true
}

// wallsEndPlan is the run a change to its end is planned against, and whether the change is exempt
// from its limits. A super admin's is, except while the walls bind it (wallsEndCap): then it is held
// to the walls' maximum alone, not to the profile the run launched under. ok=false: the 500 is written.
func (s *Server) wallsEndPlan(w http.ResponseWriter, r *http.Request, run types.AgentRun) (types.AgentRun, bool, bool) {
	if !s.runUngoverned(r.Context()) {
		return run, false, true
	}
	capSec, err := s.wallsEndCap(r.Context(), run)
	if err != nil {
		writeServerError(w, r, "read org walls", err)
		return run, false, false
	}
	if capSec == 0 {
		return run, true, true
	}
	run.RunLimits, run.GovernanceProfileID = types.RunLimits{UserChangesLimits: true, MaxEndAheadSec: capSec}, nil
	return run, false, true
}

// wallsEndCap is the walls' bound on a super admin's change to run's end, 0 for none: a break-glass
// run's captured maximum always, any other run's the walls' maximum while they are on and name one.
func (s *Server) wallsEndCap(ctx context.Context, run types.AgentRun) (int, error) {
	if run.BreakGlass {
		return run.RunLimits.MaxEndAheadSec, nil
	}
	w, err := s.orgWalls(ctx)
	if err != nil || !w.Enabled || w.Limits.MaxEndAheadSec == nil {
		return 0, err
	}
	return *w.Limits.MaxEndAheadSec, nil
}

// wallsAdmitApproval is the walls' bound on an egress approval (approvals.go's egress_host gate is
// the member's): while the walls are on, no decider admits a host they deny or do not allow, except
// on a break-glass run, which they do not bind. ok=false: the 403 is written and the approval stays
// pending. It returns the run, read here when the gate before it had not.
func (s *Server) wallsAdmitApproval(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest, run types.AgentRun, haveRun bool) (types.AgentRun, bool, bool) {
	walls, err := s.orgWalls(r.Context())
	if err != nil {
		writeServerError(w, r, "read org walls", err)
		return run, haveRun, false
	}
	if !walls.Enabled {
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
	if run.BreakGlass || wallsAdmitHost(walls, host) {
		return run, haveRun, true
	}
	writeErrorReason(w, http.StatusForbidden, reasonOrgWallsHost,
		"the org walls do not allow "+host+", so no approval can admit it on this run — ask a security admin to change the walls")
	return run, haveRun, false
}
