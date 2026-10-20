// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Wardyn boundary's write surface (#2945): GET/PUT /governance/boundary and POST
// /governance/dry-run, on the security tier (routes.go). A write is a covered governance write: held
// for a second human when WARDYN_GOVERNANCE_SECOND_HUMAN is on, never exempt, since nothing proves a
// replacement only narrows (leaving hard_limits widens every run back to its profile).

const (
	govKindBoundary = types.GovernanceTargetBoundary
	govBoundaryKey  = "boundary"

	// boundaryWriteAction is the boundary's own audit action; its mode and was_mode keys say when a
	// write changed the mode. The second human's propose and approve rows are governance.change.*.
	boundaryWriteAction = "governance.boundary.write"

	// breakGlassReasonHeader carries the reason an admin-token break-glass run gives.
	breakGlassReasonHeader = "X-Wardyn-Break-Glass-Reason"
)

// boundaryState is the part of the boundary a write replaces and an approval compares.
func boundaryState(b types.Boundary) types.BoundaryWrite {
	return types.BoundaryWrite{Mode: b.Mode, Spec: b.Spec, Limits: b.Limits, Contact: b.Contact}
}

// validateBoundary refuses a write the boundary cannot hold: a missing or unknown mode, a bad
// contact, or knobs validateBoundarySpec refuses. An all-empty contact is stored as none.
func validateBoundary(b *types.BoundaryWrite) error {
	if b.Mode != types.BoundaryNoLimits && b.Mode != types.BoundaryHardLimits {
		return errors.New("mode is required: no_limits or hard_limits")
	}
	if b.Contact != nil && b.Contact.IsZero() {
		b.Contact = nil
	}
	if b.Contact != nil {
		if err := policyref.Validate(*b.Contact); err != nil {
			return err
		}
	}
	return validateBoundarySpec(b.Spec, b.Limits)
}

// validateBoundarySpec refuses knobs the boundary cannot hold. They are judged by the validator a
// stored policy passes (an unnamed confinement floor reads as the lowest), so a boundary allowlist,
// deny list or grant is spelled as a policy's is.
func validateBoundarySpec(sp types.BoundarySpec, limits types.BoundaryLimits) error {
	if sp.AllowAllEgress != nil && *sp.AllowAllEgress && sp.AllowedDomains != nil {
		return errors.New("allow_all_egress true allows every host, so allowed_domains bounds nothing: name one of them")
	}
	if m := limits.MaxEndAheadSec; m != nil && *m <= 0 {
		return errors.New("limits.max_end_ahead_sec must be a positive number of seconds; omit it for no limit")
	}
	spec := types.RunPolicySpec{MinConfinementClass: types.CC1}
	if sp.AllowedDomains != nil {
		spec.AllowedDomains = *sp.AllowedDomains
	}
	if sp.DeniedDomains != nil {
		spec.DeniedDomains = *sp.DeniedDomains
	}
	if sp.AllowAllEgress != nil {
		spec.AllowAllEgress = *sp.AllowAllEgress
	}
	if sp.MinConfinementClass != nil {
		spec.MinConfinementClass = *sp.MinConfinementClass
	}
	if sp.EligibleGrants != nil {
		spec.EligibleGrants = *sp.EligibleGrants
	}
	if err := validateAllowedDomainsCount(spec.AllowedDomains); err != nil {
		return err
	}
	return validatePolicySpec(spec)
}

// handleGetBoundary returns the boundary, with the ETag PUT's If-Match is judged against.
func (s *Server) handleGetBoundary(w http.ResponseWriter, r *http.Request) {
	b, err := s.governanceBoundary(r.Context())
	if err != nil {
		writeServerError(w, r, "read the Wardyn boundary", err)
		return
	}
	w.Header().Set("ETag", computeETag(boundaryState(b)))
	writeJSON(w, http.StatusOK, b)
}

// handlePutBoundary replaces the boundary: applied directly with the switch off (or by the admin
// token, audited as the bypass), held for a second human with it on.
func (s *Server) handlePutBoundary(w http.ResponseWriter, r *http.Request) {
	bs, ok := s.cfg.Store.(store.BoundaryStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonBoundaryInvalid, "this store keeps no Wardyn boundary")
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var body types.BoundaryWrite
	if !decodeStrict(w, r, &body) {
		return
	}
	if err := validateBoundary(&body); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonBoundaryInvalid, "invalid Wardyn boundary: "+err.Error())
		return
	}
	cur, err := bs.GetBoundary(r.Context())
	if err != nil {
		writeServerError(w, r, "read the Wardyn boundary", err)
		return
	}
	before := boundaryState(cur)
	if !ifMatchSatisfied(r, computeETag(before)) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonBoundaryStale,
			"If-Match does not match the current Wardyn boundary — GET /governance/boundary again and retry")
		return
	}
	if mode == govQueue {
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindBoundary, op: "replace", key: govBoundaryKey, payload: body,
			before: before, after: body, changed: changedPaths(before, body), baseState: before,
		})
		return
	}
	saved, err := bs.PutBoundary(r.Context(), body, principalFromRequest(r))
	if err != nil {
		writeServerError(w, r, "write the Wardyn boundary", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		boundaryWriteAction, govBoundaryKey, "success", mustJSON(boundaryAuditData(before, body))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindBoundary, govBoundaryKey, "success", nil)
	}
	w.Header().Set("ETag", computeETag(boundaryState(saved)))
	writeJSON(w, http.StatusOK, saved)
}

func boundaryAuditData(before, after types.BoundaryWrite) map[string]any {
	return map[string]any{"mode": after.Mode, "was_mode": before.Mode, "changed": changedPaths(before, after)}
}

// applyBoundaryChange applies a held replacement inside the decision transaction: the target lock,
// the boundary as it now stands compared with what the proposal reviewed, then the write on q.
func applyBoundaryChange(_ *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var held types.BoundaryWrite
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := validateBoundary(&held); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonBoundaryInvalid, "invalid Wardyn boundary: %s", err.Error())
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindBoundary); err != nil {
		return govApplied{}, err
	}
	cur, err := store.GetBoundaryQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	before := boundaryState(cur)
	if computeETag(before) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if _, err := store.PutBoundaryQ(ctx, q, held, principalFromRequest(r)); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: boundaryWriteAction, target: govBoundaryKey, data: boundaryAuditData(before, held)}, nil
}

// boundaryReport is POST /governance/dry-run's answer: every layer and row the boundary as drafted
// would narrow in hard_limits, knob by knob, computed by boundaryMeet, the function the resolve calls.
// Rows the boundary leaves as they are are not listed.
type boundaryReport struct {
	Deployment     []boundaryChange       `json:"deployment"`
	Profiles       []boundaryReportRow    `json:"profiles"`
	StoredPolicies []boundaryReportRow    `json:"stored_policies"`
	Assignments    []boundaryReportAssign `json:"assignments"`
}

type boundaryReportRow struct {
	ID      uuid.UUID        `json:"id"`
	Name    string           `json:"name"`
	Changes []boundaryChange `json:"changes"`
}

type boundaryReportAssign struct {
	ID          uuid.UUID        `json:"id"`
	SubjectType string           `json:"subject_type"`
	Subject     string           `json:"subject"`
	ProfileID   uuid.UUID        `json:"profile_id"`
	ProfileName string           `json:"profile_name"`
	Changes     []boundaryChange `json:"changes"`
}

// handleBoundaryDryRun reports what the drafted boundary (the body's spec and limits, judged in
// hard_limits whatever its mode) would narrow. It writes nothing.
func (s *Server) handleBoundaryDryRun(w http.ResponseWriter, r *http.Request) {
	var body types.BoundaryWrite
	if !decodeStrict(w, r, &body) {
		return
	}
	if err := validateBoundarySpec(body.Spec, body.Limits); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonBoundaryInvalid, "invalid Wardyn boundary: "+err.Error())
		return
	}
	report, err := s.boundaryDryRun(r.Context(), types.Boundary{Mode: types.BoundaryHardLimits, Spec: body.Spec, Limits: body.Limits})
	if err != nil {
		writeServerError(w, r, "Wardyn boundary dry run", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// boundaryDryRun meets the deployment default, each profile as it composes, each stored policy and so
// each assignment with b. The values shown pass the read redaction a security admin's read does.
func (s *Server) boundaryDryRun(ctx context.Context, b types.Boundary) (boundaryReport, error) {
	meet := func(spec types.RunPolicySpec, limits types.GovernanceLimits) ([]boundaryChange, error) {
		after, afterLimits, _, err := boundaryMeet(b, spec, limits)
		if err != nil {
			return nil, err
		}
		return boundaryChanges(redactSpecForRead(spec, true), redactSpecForRead(after, true), limits, afterLimits), nil
	}
	report := boundaryReport{Profiles: []boundaryReportRow{}, StoredPolicies: []boundaryReportRow{}, Assignments: []boundaryReportAssign{}}
	var err error
	if report.Deployment, err = meet(s.cfg.DefaultPolicy, types.GovernanceLimits{}); err != nil {
		return boundaryReport{}, err
	}
	if s.cfg.Store == nil {
		return report, nil
	}
	profiles, err := s.resolveAllProfiles(ctx)
	if err != nil {
		return boundaryReport{}, fmt.Errorf("list governance profiles: %w", err)
	}
	byProfile := map[uuid.UUID]boundaryReportRow{}
	for _, p := range profiles {
		if p.Err != nil || p.Resolved == nil {
			continue // a profile that does not compose resolves to no ceiling at all
		}
		changes, err := meet(p.Resolved.Ceiling, p.Resolved.Limits)
		if err != nil {
			return boundaryReport{}, err
		}
		if len(changes) > 0 {
			row := boundaryReportRow{ID: p.Row.ID, Name: p.Row.Name, Changes: changes}
			report.Profiles = append(report.Profiles, row)
			byProfile[p.Row.ID] = row
		}
	}
	policies, err := s.cfg.Store.ListPolicies(ctx)
	if err != nil {
		return boundaryReport{}, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range policies {
		changes, err := meet(p.Spec, types.GovernanceLimits{})
		if err != nil {
			return boundaryReport{}, err
		}
		if len(changes) > 0 {
			report.StoredPolicies = append(report.StoredPolicies, boundaryReportRow{ID: p.ID, Name: p.Name, Changes: changes})
		}
	}
	assignments, err := s.cfg.Store.ListGovernanceAssignments(ctx)
	if err != nil {
		return boundaryReport{}, fmt.Errorf("list governance assignments: %w", err)
	}
	for _, a := range assignments {
		if row, ok := byProfile[a.ProfileID]; ok {
			report.Assignments = append(report.Assignments, boundaryReportAssign{ID: a.ID, SubjectType: string(a.SubjectType),
				Subject: a.Subject, ProfileID: a.ProfileID, ProfileName: row.Name, Changes: row.Changes})
		}
	}
	return report, nil
}

type breakGlassKey struct{}

// breakGlassReason is the reason the admin token gave for this request's break-glass run, or "".
func breakGlassReason(ctx context.Context) string {
	reason, _ := ctx.Value(breakGlassKey{}).(string)
	return reason
}

// admitBreakGlass is POST /runs' break-glass door (#2946), run before anything resolves a ceiling, and
// at preflight and policy preview, so they answer as the launch does. An admin-token run is a
// break-glass run only when it would lift something (breakGlassLifts). Then it must give a reason in
// X-Wardyn-Break-Glass-Reason (refused 400 without one), and with one the request is stamped so the
// run is tagged break_glass. The boundary binds the run either way. Every other caller, and an
// admin-token request that lifts nothing, passes untouched: the header means nothing to them.
func (s *Server) admitBreakGlass(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if !isAdminTokenCaller(r) {
		return r, true
	}
	lifts, err := s.breakGlassLifts(r.Context())
	if err != nil {
		writeServerError(w, r, "read the Wardyn boundary", err)
		return r, false
	}
	if !lifts {
		return r, true
	}
	reason := strings.TrimSpace(r.Header.Get(breakGlassReasonHeader))
	if reason == "" || utf8.RuneCountInString(reason) > maxGovernanceRejectReasonLen || !controlCharFree(reason) {
		writeErrorReason(w, http.StatusBadRequest, reasonBreakGlassReasonRequired,
			"the Wardyn boundary is in hard_limits and governance profiles exist: an admin-token run lifts their walls, so it is a break-glass run and needs a reason in the "+breakGlassReasonHeader+
				" header (at most 512 characters, no control characters)")
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), breakGlassKey{}, reason)), true
}

// breakGlassLifts reports whether an admin-token run would lift anything: the boundary is in
// hard_limits and at least one governance profile exists, whose walls the admin token stands outside.
// The deployment default does not count: the admin token never was held to it. With no profile there
// is nothing to lift, so the run is an ordinary one, bound by the boundary, untagged.
func (s *Server) breakGlassLifts(ctx context.Context) (bool, error) {
	b, err := s.governanceBoundary(ctx)
	if err != nil || !b.HardLimits() {
		return false, err
	}
	profiles, err := s.resolveAllProfiles(ctx)
	return len(profiles) > 0, err
}

// stampBreakGlass records a break-glass run on its run.create datum and writes its run.break_glass.open
// row. A run that is not one writes nothing new.
func (s *Server) stampBreakGlass(ctx context.Context, runID uuid.UUID, actorType types.ActorType, actor string, data map[string]any) {
	reason := breakGlassReason(ctx)
	if reason == "" {
		return
	}
	data["break_glass"], data["break_glass_reason"] = true, reason
	s.recordAudit(ctx, s.auditEvent(&runID, actorType, actor, "run.break_glass.open", runID.String(), "success",
		mustJSON(map[string]any{"reason": reason})))
}
