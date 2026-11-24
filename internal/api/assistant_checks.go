// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantPersonCtx is the context of every assistant route a person calls with no session: refused 403
// before any read when the request carries no signed-in human (the admin token, local mode) or comes from a
// caller that is never an operator (a device, a delegated portal, the identity provider's connector, a
// service identity, an approval rule); otherwise the person the request published, after the clamps it went
// through, at the security tier at most, exactly as a session's tool call is. Every check on the route reads
// on this context, under a dry run. Its base ends with r's context.
func (s *Server) assistantPersonCtx(w http.ResponseWriter, r *http.Request) (context.Context, bool) {
	if oidcHumanFromContext(r.Context()) == "" || neverOperator(r.Context()) {
		writeErrorReason(w, http.StatusForbidden, reasonAssistantRoleDenied, "The assistant's checks answer a signed-in person only.") // PROPOSED
		return nil, false
	}
	tctx, _, err := s.assistantToolCtx(r, assistantSnapshotOf(r.Context()))
	if err != nil {
		s.assistantToolCtxRefusal(err).write(w, r)
		return nil, false
	}
	return audit.WithDryRun(tctx), true
}

// assistantPersonSwitch is the organisation's switch and the unit's denial for a person route that serves the
// given purposes: off when every purpose is off, unit_denies when none is left that the person's limits let
// in. The provider states pass: nothing on these routes reaches a provider.
func (s *Server) assistantPersonSwitch(ctx context.Context, purposes ...types.AssistantPurpose) *assistantRefusal {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return assistantRefusalFor(http.StatusInternalServerError, "", "Could not read the settings.") // PROPOSED
	}
	var on []types.AssistantPurpose
	for _, p := range purposes {
		if sc.Assistant != nil && sc.Assistant.On(p) {
			on = append(on, p)
		}
	}
	if len(on) == 0 {
		return assistantRefusalFor(http.StatusConflict, reasonAssistantOff, "The assistant is off for your organisation.") // PROPOSED: the board's words
	}
	lim, err := s.assistantLimitsFor(ctx, sc, oidcHumanFromContext(ctx))
	if err != nil {
		return assistantRefusalFor(http.StatusInternalServerError, "", "Could not read your limits.") // PROPOSED
	}
	for _, p := range on {
		if assistantAvailability(sc.Assistant, p, siteProviders(sc), inspectionDeferredFor, lim.deny, lim.allowed, lim.connected).State != types.AssistantUnitDenies {
			return nil
		}
	}
	return assistantRefusalFor(http.StatusConflict, reasonAssistantUnitDenies, "Your unit does not allow the assistant.") // PROPOSED
}

// assistantPersonTarget checks the body's target with the check of the route that serves that target, on the
// person's capped context, before anything else reads the target's records; the refusal is that route's own.
// next runs with the target read: a unit inside the unit tier's middleware and the save's own scope check; a
// template as GET /templates/{id} reads it; a run as GET /runs/{id} reads it; a change as
// GET /governance/changes/{id} reads it. A run or template with no id is a new draft, with nothing to check.
func (s *Server) assistantPersonTarget(w http.ResponseWriter, tr *http.Request, req types.AssistantChecksRequest, next func(*http.Request, assistantTarget)) {
	rt := assistantTarget{AssistantTarget: types.AssistantTarget{Kind: req.TargetKind, ID: req.Target}}
	if req.Target == "" {
		switch req.TargetKind {
		case types.AssistantTargetRun, types.AssistantTargetTemplate:
			next(tr, rt)
		default:
			writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "Name the target to check.") // PROPOSED
		}
		return
	}
	id, err := uuid.Parse(req.Target)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "The target id is not valid.") // PROPOSED
		return
	}
	switch req.TargetKind {
	case types.AssistantTargetUnit:
		s.requireUnitScope(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.requireUnitsInScope(w, r, "governance.unit", false, id) {
				return
			}
			u, err := s.resolveUnitByID(r.Context(), id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					assistantTargetGone().write(w, r)
				} else {
					writeServerError(w, r, "read unit", err)
				}
				return
			}
			rt.unit, rt.govKind = u, govKindUnit
			next(r, rt)
		})).ServeHTTP(w, tr)
	case types.AssistantTargetTemplate:
		ts, ok := s.templateStoreOr501(w)
		if !ok {
			return
		}
		c, err := s.templateCallerOf(tr.Context(), tr, ts)
		if err != nil {
			writeServerError(w, tr, "read template grants", err)
			return
		}
		t, ok := readableTemplate(w, tr, ts, c, id, 0, false)
		if !ok {
			return
		}
		rt.template = &t
		next(tr, rt)
	case types.AssistantTargetRun:
		run, ok := s.getRunAuthorizedBy(w, tr, id, s.ownsRunOrSponsorOrAdmin)
		if !ok {
			return
		}
		rt.run = &run
		next(tr, rt)
	case types.AssistantTargetChange:
		ch, err := s.cfg.Store.GetGovernanceChange(tr.Context(), id)
		if err == nil && !s.canSeeGovernanceChange(tr, ch) {
			err = store.ErrNotFound
		}
		if notFoundIf(w, err, "governance change", reasonGovernanceChangeNotFound) {
			return
		}
		if err != nil {
			writeServerError(w, tr, "read governance change", err)
			return
		}
		rt.change, rt.govKind = &ch, ch.TargetKind
		next(tr, rt)
	default:
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "target_kind must be run, unit, template or change.") // PROPOSED
	}
}

// assistantPersonRoute opens a person route: the 403 rule and the context, the body, the tool bucket, the
// switches for purposes, then the target check, and next with the target read.
func (s *Server) assistantPersonRoute(w http.ResponseWriter, r *http.Request, purposes []types.AssistantPurpose, next func(http.ResponseWriter, *http.Request, types.AssistantChecksRequest, assistantTarget)) {
	tctx, ok := s.assistantPersonCtx(w, r)
	if !ok {
		return
	}
	var req types.AssistantChecksRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, msg)
		return
	}
	if req.TargetKind == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "target_kind must be run, unit, template or change.") // PROPOSED
		return
	}
	if req.TargetKind != types.AssistantTargetChange && len(req.Draft) == 0 {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "Name a draft to check.") // PROPOSED
		return
	}
	tr := r.WithContext(tctx)
	if s.assistantToolRateRefused(w, tr, oidcHumanFromContext(tctx)) {
		return
	}
	if ref := s.assistantPersonSwitch(tctx, purposes...); ref != nil {
		ref.write(w, tr)
		return
	}
	s.assistantPersonTarget(w, tr, req, func(tr *http.Request, rt assistantTarget) { next(w, tr, req, rt) })
}

// assistantChecksAnswer is POST /assistant/checks's answer: the browser's checks strip.
type assistantChecksAnswer struct {
	Valid           bool                  `json:"valid"`
	Errors          []assistantFieldError `json:"errors"`
	Review          *changeReview         `json:"review"`
	Risk            assistantRiskView     `json:"risk"`
	ApprovalsNeeded int                   `json:"approvals_needed"`
	Dropped         any                   `json:"dropped"`
}

// assistantRiskView is a risk with both sides always named, null when not graded.
type assistantRiskView struct {
	Before *riskGrade `json:"before"`
	After  *riskGrade `json:"after"`
}

// handleAssistantChecks is POST /assistant/checks {target_kind, target, draft}: the checks the compose editor
// shows, with no model. It serves the compose purpose.
func (s *Server) handleAssistantChecks(w http.ResponseWriter, r *http.Request) {
	s.assistantPersonRoute(w, r, []types.AssistantPurpose{types.AssistantCompose}, func(w http.ResponseWriter, tr *http.Request, req types.AssistantChecksRequest, rt assistantTarget) {
		sub, ref := s.assistantSubjectFor(tr.Context(), rt, req.Draft)
		if ref != nil {
			ref.write(w, tr)
			return
		}
		p, ref := s.assistantPropose(tr, sub, nil)
		if ref != nil {
			ref.write(w, tr)
			return
		}
		out := assistantChecksAnswer{Valid: p.Valid, Errors: p.Errors, Review: p.Review, ApprovalsNeeded: p.ApprovalsNeeded, Dropped: p.Dropped}
		risk, err := s.assistantRisk(tr.Context(), sub, s.assistantReviewCaller(tr.Context()))
		if err != nil {
			assistantCheckFailed(err).write(w, tr)
			return
		}
		if risk != nil {
			out.Risk = assistantRiskView{Before: risk.Before, After: &risk.After}
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// assistantReplayAnswer is POST /assistant/replay's answer: REPLAY's result, with its run list cut to runs
// the caller may read, and the fixed basis every answer carries.
type assistantReplayAnswer struct {
	replayResult
	Computed bool   `json:"computed"`
	Basis    string `json:"basis"`
}

// handleAssistantReplay is POST /assistant/replay {target_kind, target, draft}: the counts of the last 30 days
// of runs under the draft's proposed rules, for a person. It serves both purposes, so it is refused only when
// neither is left. The run list keeps real names, since it never reaches a provider, and only runs the caller
// may read; the organisation's run-history switch does not apply.
func (s *Server) handleAssistantReplay(w http.ResponseWriter, r *http.Request) {
	purposes := []types.AssistantPurpose{types.AssistantCompose, types.AssistantReview}
	s.assistantPersonRoute(w, r, purposes, func(w http.ResponseWriter, tr *http.Request, req types.AssistantChecksRequest, rt assistantTarget) {
		sub, ref := s.assistantSubjectFor(tr.Context(), rt, req.Draft)
		if ref != nil {
			ref.write(w, tr)
			return
		}
		res, ok, ref := s.assistantDraftReplay(tr, sub)
		if ref != nil {
			ref.write(w, tr)
			return
		}
		runs := res.Runs[:0:0]
		for _, run := range res.Runs {
			if s.ownsRunOrSponsorOrAdmin(tr, types.AgentRun{ID: run.RunID, CreatedBy: run.Owner, CreatedAt: run.CreatedAt}) {
				runs = append(runs, run)
			}
		}
		res.Runs = runs
		writeJSON(w, http.StatusOK, assistantReplayAnswer{replayResult: res, Computed: ok, Basis: assistantReplayBasis})
	})
}
