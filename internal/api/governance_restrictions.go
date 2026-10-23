// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Restrictions (the policy tree, DESIGN-TREE §4.1 step 8, §4.3, §4.6): deny-only lists that follow
// people. A governed run of a covered person meets each one, in name order, after its unit's walls and
// the boundary, in every unit they run in. A restriction never counts as a context, and never reaches
// an ungoverned or break-glass run: those resolve before this file is asked (resolveChainCeiling).
// This file is the resolve, the seal layers, the live doors, the approval hook and the revive; the
// write routes are in governance_restriction_routes.go.

// restrictionStore is the store's restrictions, or false when it keeps none (a store that is not
// Postgres), which resolves every run as before.
func (s *Server) restrictionStore() (store.RestrictionStore, bool) {
	if s.cfg.Store == nil {
		return nil, false
	}
	rs, ok := s.cfg.Store.(store.RestrictionStore)
	return rs, ok
}

// restrictionProvenance is the record of a value a restriction decided.
func restrictionProvenance(r types.Restriction) policyref.Provenance {
	return policyref.Provenance{Layer: policyref.LayerRestriction, Name: r.Name}
}

// restrictionsFor is every restriction covering q, in name order. When q's group half cannot be
// evaluated (a stale or truncated snapshot) and a group-keyed restriction exists, it refuses
// errGroupsSnapshotStale: skipping a restriction a group would have matched is a widening.
func (s *Server) restrictionsFor(ctx context.Context, q contextQuery) ([]types.Restriction, error) {
	rs, ok := s.restrictionStore()
	if !ok {
		return nil, nil
	}
	groups := q.groups
	if q.unusable {
		has, err := rs.HasGroupTierRestrictions(ctx)
		if err != nil {
			return nil, err
		}
		if has {
			// Decided and recorded here, as selectByTier does for the ceiling: a display read writes no row.
			if !isDisplayRead(ctx) {
				s.recordRefusal(ctx, nil, authz.Deny(authz.ReasonGroupsSnapshotStale, "governance.ceiling", ""))
			}
			return nil, errGroupsSnapshotStale
		}
		groups = nil
	}
	return rs.ListRestrictionsFor(ctx, q.users, groups, q.userType)
}

// applyRestrictions is DESIGN-TREE §4.1 step 8 over a governed caller's context ceiling c: each
// restriction that covers q is applied with composer.ApplyOverlay in name order, recorded for the run,
// and puts the run in a unit when it had none (the Default unit's id, so the doors, the sweep and the
// queue see it). With none it returns c untouched.
func (s *Server) applyRestrictions(ctx context.Context, q contextQuery, c governanceCeiling) (governanceCeiling, error) {
	list, err := s.restrictionsFor(ctx, q)
	if err != nil || len(list) == 0 {
		return c, err
	}
	return s.withRestrictions(ctx, c, list)
}

// withRestrictions is c met with each of list in turn.
func (s *Server) withRestrictions(ctx context.Context, c governanceCeiling, list []types.Restriction) (governanceCeiling, error) {
	auth := composer.Authority{Ceiling: c.Spec, Limits: c.Limits}
	knobs := make(map[string]policyref.Provenance, len(c.restrictionKnobs))
	for k, v := range c.restrictionKnobs {
		knobs[k] = v
	}
	ids := slices.Clone(c.RestrictionIDs)
	for _, r := range list {
		out, _, err := composer.ApplyOverlay(auth, composer.Overlay{Ceiling: r.Spec.Overlay(), Limits: r.Limits.Overlay()})
		if err != nil {
			return governanceCeiling{}, fmt.Errorf("api: apply restriction %q: %w", r.Name, err)
		}
		for _, ch := range boundaryChanges(auth.Ceiling, out.Ceiling, auth.Limits, out.Limits) {
			if !strings.HasPrefix(ch.Knob, "limits.") {
				knobs[ch.Knob] = restrictionProvenance(r)
			}
		}
		auth = out
		if !slices.Contains(ids, r.ID) {
			ids = append(ids, r.ID)
		}
	}
	if c.Unit == nil {
		def, err := s.resolveUnitByID(ctx, store.DefaultUnitID)
		if err != nil {
			return governanceCeiling{}, fmt.Errorf("api: read the Default unit: %w", err)
		}
		c.Unit = def
	}
	c.Spec, c.Limits, c.RestrictionIDs, c.restrictionKnobs = auth.Ceiling, auth.Limits, ids, knobs
	return c, nil
}

// restrictionRunStep records which knobs a restriction decided, once the run's own steps have set
// their records: a knob still recorded as the ceiling's layer that a restriction narrowed is the
// restriction's (the innermost layer, DESIGN-TREE §4.3).
func (c governanceCeiling) restrictionRunStep(knobs knobProvenance) {
	if knobs == nil || c.Operator {
		return
	}
	rec := c.provenance()
	for name, by := range c.restrictionKnobs {
		if knobs[name] == rec {
			knobs[name] = by
		}
	}
}

// restrictionContacts is the contact of each restriction by name, for the provenance read.
func (s *Server) restrictionContacts(ctx context.Context, ids []uuid.UUID) map[string]*policyref.Contact {
	rs, ok := s.restrictionStore()
	if !ok || len(ids) == 0 {
		return nil
	}
	list, err := rs.GetRestrictionsByID(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: restriction contacts could not be read", slog.Any("err", err))
		return nil
	}
	out := make(map[string]*policyref.Contact, len(list))
	for _, r := range list {
		if r.Contact != nil && !r.Contact.IsZero() {
			out[r.Name] = r.Contact
		}
	}
	return out
}

// restrictionSealLayers is run's captured restrictions as deny-only seal layers, by their current
// specs (a deleted id is skipped). A restriction that cannot be read seals the run to no egress and
// no credential, as an unread boundary does.
func (s *Server) restrictionSealLayers(ctx context.Context, run types.AgentRun) []sealLayer {
	if len(run.RestrictionIDs) == 0 {
		return nil
	}
	rs, ok := s.restrictionStore()
	var list []types.Restriction
	var err error
	if ok {
		list, err = rs.GetRestrictionsByID(ctx, run.RestrictionIDs)
	}
	if !ok || err != nil {
		slog.ErrorContext(ctx, "wardynd: read the run's restrictions at the seal; the run is sealed to no egress",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		off := false
		return []sealLayer{{rec: policyref.Provenance{Layer: policyref.LayerRestriction}, egress: types.CeilingOverlay{AllowedDomains: &[]string{}, AllowAllEgress: &off},
			grants: &[]types.GrantSpec{}}}
	}
	layers := make([]sealLayer, 0, len(list))
	for _, r := range list {
		layers = append(layers, sealLayer{rec: restrictionProvenance(r),
			egress:         types.CeilingOverlay{DeniedDomains: r.Spec.DeniedDomains, AllowAllEgress: r.Spec.AllowAllEgress},
			withheldGrants: r.Spec.WithheldGrants})
	}
	return layers
}

// foldRestrictionLimits is p with the current limits of run's captured restrictions met into it, so a
// door that reads the run's unit sees a restriction too. A run whose unit is gone keeps its
// restrictions: they bind alone.
func (s *Server) foldRestrictionLimits(ctx context.Context, run types.AgentRun, p *ResolvedUnit) (*ResolvedUnit, error) {
	if len(run.RestrictionIDs) == 0 {
		return p, nil
	}
	rs, ok := s.restrictionStore()
	if !ok {
		return p, nil
	}
	list, err := rs.GetRestrictionsByID(ctx, run.RestrictionIDs)
	if err != nil {
		return nil, fmt.Errorf("read the run's restrictions: %w", err)
	}
	if len(list) == 0 {
		return p, nil
	}
	if p == nil {
		p = &ResolvedUnit{ID: store.DefaultUnitID, Name: "Default"}
	}
	out := *p
	auth := composer.Authority{Ceiling: out.Ceiling, Limits: out.Limits}
	for _, r := range list {
		met, _, err := composer.ApplyOverlay(auth, composer.Overlay{Limits: r.Limits.Overlay()})
		if err != nil {
			return nil, fmt.Errorf("meet restriction %q: %w", r.Name, err)
		}
		auth = met
	}
	out.Limits = auth.Limits
	return &out, nil
}

// restrictionRunLimits is l met with the current run limits of run's captured restrictions, for the
// sweep that holds a live run to them.
func (s *Server) restrictionRunLimits(ctx context.Context, run types.AgentRun, l types.RunLimits) (types.RunLimits, error) {
	rs, ok := s.restrictionStore()
	if !ok || len(run.RestrictionIDs) == 0 {
		return l, nil
	}
	list, err := rs.GetRestrictionsByID(ctx, run.RestrictionIDs)
	if err != nil {
		return l, err
	}
	auth := composer.Authority{Limits: types.GovernanceLimits{RunLimits: l}}
	for _, r := range list {
		if auth, _, err = composer.ApplyOverlay(auth, composer.Overlay{Limits: r.Limits.Overlay()}); err != nil {
			return l, fmt.Errorf("meet restriction %q: %w", r.Name, err)
		}
	}
	return auth.Limits.RunLimits, nil
}

// restrictionAttribution is the policy a refusal names for a run with no unit walls of its own (the
// Default unit): the first captured restriction, with its contact, since it alone narrowed the run.
func (s *Server) restrictionAttribution(ctx context.Context, run types.AgentRun) *policyref.Ref {
	rs, ok := s.restrictionStore()
	if !ok || len(run.RestrictionIDs) == 0 {
		return nil
	}
	list, err := rs.GetRestrictionsByID(ctx, run.RestrictionIDs)
	if err != nil || len(list) == 0 {
		return nil
	}
	return policyref.Project(policyref.SourceUnit, list[0].Name, list[0].Contact) // the wire knows two sources; a restriction reads as a named profile
}

// admitApprovalBounds is the boundary's bound on an egress approval, then the restrictions'.
func (s *Server) admitApprovalBounds(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest, run types.AgentRun, haveRun bool) (types.AgentRun, bool, bool) {
	run, haveRun, ok := s.boundaryAdmitApproval(w, r, ap, run, haveRun)
	if !ok {
		return run, haveRun, false
	}
	return s.restrictionAdmitApproval(w, r, ap, run, haveRun)
}

// restrictionAdmitApproval is the restrictions' bound on an egress approval, beside the boundary's:
// for any decider, a host a restriction the run captured denies is not admitted. ok=false: the 403 is
// written and the approval stays pending. It returns the run, read here when the gate before it had not.
func (s *Server) restrictionAdmitApproval(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest, run types.AgentRun, haveRun bool) (types.AgentRun, bool, bool) {
	rs, ok := s.restrictionStore()
	if !ok {
		return run, haveRun, true
	}
	if !haveRun {
		var err error
		if run, err = s.cfg.Store.GetRun(r.Context(), ap.RunID); err != nil {
			writeServerError(w, r, "get run", err)
			return run, false, false
		}
		haveRun = true
	}
	if len(run.RestrictionIDs) == 0 {
		return run, haveRun, true
	}
	list, err := rs.GetRestrictionsByID(r.Context(), run.RestrictionIDs)
	if err != nil {
		writeServerError(w, r, "read the run's restrictions", err)
		return run, haveRun, false
	}
	host := approvalHost(ap)
	for _, rst := range list {
		if rst.Spec.DeniedDomains != nil && ceilingDenies(*rst.Spec.DeniedDomains, host) {
			contact := "the restriction's owner"
			if rst.Contact != nil && rst.Contact.Owner != "" {
				contact = rst.Contact.Owner
			}
			writeErrorReason(w, http.StatusForbidden, reasonRestrictionHost,
				fmt.Sprintf("%s blocks %s for this run's owner; ask %s", rst.Name, host, contact))
			return run, haveRun, false
		}
	}
	return run, haveRun, true
}

// reviveRestrictions is the restrictions a revive of run holds it to. The owner's own session
// re-resolves them in full, as a launch does. Anyone else (a super admin's revive, an admin restart)
// knows the owner only by the sub and user type on the run, and groups are unknowable: the captured
// restrictions, each at its current spec, plus the current ones that name the owner's sub, their
// stamped type, or everyone. Both only add, so a revive never narrows less than the run launched.
func (s *Server) reviveRestrictions(ctx context.Context, run types.AgentRun) ([]types.Restriction, error) {
	rs, ok := s.restrictionStore()
	if !ok {
		return nil, nil
	}
	captured, err := rs.GetRestrictionsByID(ctx, run.RestrictionIDs)
	if err != nil {
		return nil, err
	}
	var current []types.Restriction
	if sub := oidcHumanFromContext(ctx); sub != "" && sub == run.CreatedBy && !oidcGroupsTruncatedFromContext(ctx) {
		subj, serr := s.callerSubjects(ctx)
		if serr != nil && !errors.Is(serr, errUserTypeUnknown) {
			return nil, serr
		}
		if serr == nil && !subj.stale {
			current, err = rs.ListRestrictionsFor(ctx, subj.users, subj.groups, subj.userType)
			return mergeRestrictions(captured, current), err
		}
	}
	current, err = rs.ListRestrictionsFor(ctx, []string{types.CanonicalUserSubject(run.CreatedBy)}, nil, run.UserType)
	return mergeRestrictions(captured, current), err
}

// mergeRestrictions is the union of a and b by id, in name order.
func mergeRestrictions(a, b []types.Restriction) []types.Restriction {
	out := slices.Clone(a)
	for _, r := range b {
		if !slices.ContainsFunc(out, func(x types.Restriction) bool { return x.ID == r.ID }) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(x, y types.Restriction) int { return strings.Compare(x.Name, y.Name) })
	return out
}

// withRevivedRestrictions adds list to a revive's owner ceiling: their denied hosts and placements,
// and their ids for the run to be re-captured with.
func (c ownerCeiling) withRevivedRestrictions(list []types.Restriction) ownerCeiling {
	c.restrictionIDs = c.restrictionIDs[:0:0]
	for _, r := range list {
		c.restrictionIDs = append(c.restrictionIDs, r.ID)
		if r.Spec.DeniedDomains != nil {
			c.deny = append(slices.Clone(c.deny), *r.Spec.DeniedDomains...)
		}
		if r.Limits.DenyPlacements != nil {
			c.denyPlacements = append(slices.Clone(c.denyPlacements), *r.Limits.DenyPlacements...)
		}
	}
	return c
}

// restrictionDefaultUnit is whether p is the Default unit a restriction put a run in, which needs no
// membership.
func restrictionDefaultUnit(p *ResolvedUnit) bool { return p != nil && p.ID == store.DefaultUnitID }

// saveRevivedRestrictions persists the restrictions a revive holds run to when they differ from the
// ones captured (a restriction added since, or one the revive found by the owner's sub). The ids only
// widen the set the run is bound by, so a failed write refuses the revive rather than start it looser.
func (s *Server) saveRevivedRestrictions(ctx context.Context, run types.AgentRun, captured []uuid.UUID) *reviveError {
	if slices.Equal(run.RestrictionIDs, captured) {
		return nil
	}
	set, ok := s.cfg.Store.(store.RunRestrictionSetter)
	if !ok {
		return nil
	}
	if err := set.SetRunRestrictionIDs(ctx, run.ID, run.RestrictionIDs); err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonOwnerUnitUnreadable, loggedMsg(ctx, "record the run's restrictions", err))
	}
	return nil
}
