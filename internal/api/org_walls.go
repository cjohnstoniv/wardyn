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

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The org walls' write surface (#2945): GET/PUT /governance/walls and POST /governance/walls/dry-run,
// on the security tier (routes.go). A write is a covered governance write: held for a second human
// when WARDYN_GOVERNANCE_SECOND_HUMAN is on, never exempt, since nothing proves a replacement only
// narrows (turning the walls off widens every run back to its profile).

const (
	govKindOrgWalls = types.GovernanceTargetOrgWalls
	govOrgWallsKey  = "org_walls"

	// The walls' own audit actions: a write that leaves enabled as it was, and one that turns them on
	// or off. The second human's propose and approve rows are governance.change.*.
	orgWallsWriteAction   = "governance.walls.write"
	orgWallsEnableAction  = "governance.walls.enable"
	orgWallsDisableAction = "governance.walls.disable"

	// breakGlassReasonHeader carries the reason an admin-token run gives for running past the walls.
	breakGlassReasonHeader = "X-Wardyn-Break-Glass-Reason"
)

// wallsState is the part of the walls a write replaces and an approval compares.
func wallsState(w types.OrgWalls) types.OrgWallsWrite {
	return types.OrgWallsWrite{Enabled: w.Enabled, Spec: w.Spec, Limits: w.Limits}
}

// validateOrgWalls refuses a document the walls cannot hold. The knobs are judged by the validator a
// stored policy passes (an unnamed confinement floor reads as the lowest), so a walls allowlist,
// deny list or grant is spelled as a policy's is.
func validateOrgWalls(w types.OrgWallsWrite) error {
	sp := w.Spec
	if sp.AllowAllEgress != nil && *sp.AllowAllEgress && sp.AllowedDomains != nil {
		return errors.New("allow_all_egress true allows every host, so allowed_domains bounds nothing: name one of them")
	}
	if m := w.Limits.MaxEndAheadSec; m != nil && *m <= 0 {
		return errors.New("limits.max_end_ahead_sec must be a positive number of seconds; omit it for no wall")
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

// handleGetOrgWalls returns the walls, with the ETag PUT's If-Match is judged against.
func (s *Server) handleGetOrgWalls(w http.ResponseWriter, r *http.Request) {
	walls, err := s.orgWalls(r.Context())
	if err != nil {
		writeServerError(w, r, "read org walls", err)
		return
	}
	w.Header().Set("ETag", computeETag(wallsState(walls)))
	writeJSON(w, http.StatusOK, walls)
}

// handlePutOrgWalls replaces the walls: applied directly with the switch off (or by the admin token,
// audited as the bypass), held for a second human with it on.
func (s *Server) handlePutOrgWalls(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.cfg.Store.(store.OrgWallsStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonOrgWallsInvalid, "this store keeps no org walls")
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var body types.OrgWallsWrite
	if !decodeStrict(w, r, &body) {
		return
	}
	if err := validateOrgWalls(body); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonOrgWallsInvalid, "invalid org walls: "+err.Error())
		return
	}
	cur, err := ws.GetOrgWalls(r.Context())
	if err != nil {
		writeServerError(w, r, "read org walls", err)
		return
	}
	before := wallsState(cur)
	if !ifMatchSatisfied(r, computeETag(before)) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonOrgWallsStale,
			"If-Match does not match the current org walls — GET /governance/walls again and retry")
		return
	}
	if mode == govQueue {
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindOrgWalls, op: "replace", key: govOrgWallsKey, payload: body,
			before: before, after: body, changed: changedPaths(before, body), baseState: before,
		})
		return
	}
	saved, err := ws.PutOrgWalls(r.Context(), body, principalFromRequest(r))
	if err != nil {
		writeServerError(w, r, "write org walls", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		orgWallsAction(before, body), govOrgWallsKey, "success", mustJSON(orgWallsAuditData(before, body))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindOrgWalls, govOrgWallsKey, "success", nil)
	}
	w.Header().Set("ETag", computeETag(wallsState(saved)))
	writeJSON(w, http.StatusOK, saved)
}

// orgWallsAction is the audit action of a write: enable or disable when it flips the switch.
func orgWallsAction(before, after types.OrgWallsWrite) string {
	switch {
	case after.Enabled && !before.Enabled:
		return orgWallsEnableAction
	case !after.Enabled && before.Enabled:
		return orgWallsDisableAction
	}
	return orgWallsWriteAction
}

func orgWallsAuditData(before, after types.OrgWallsWrite) map[string]any {
	return map[string]any{"enabled": after.Enabled, "was_enabled": before.Enabled, "changed": changedPaths(before, after)}
}

// applyOrgWallsChange applies a held replacement inside the decision transaction: the target lock,
// the walls as they now stand compared with what the proposal reviewed, then the write on q.
func applyOrgWallsChange(_ *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var held types.OrgWallsWrite
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := validateOrgWalls(held); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonOrgWallsInvalid, "invalid org walls: %s", err.Error())
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindOrgWalls); err != nil {
		return govApplied{}, err
	}
	cur, err := store.GetOrgWallsQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	before := wallsState(cur)
	if computeETag(before) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if _, err := store.PutOrgWallsQ(ctx, q, held, principalFromRequest(r)); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: orgWallsAction(before, held), target: govOrgWallsKey, data: orgWallsAuditData(before, held)}, nil
}

// wallsReport is POST /governance/walls/dry-run's answer: every layer and row the walls as drafted
// would narrow, knob by knob, computed by wallsMeet, the function the resolve calls once they are on.
// Rows the walls leave as they are are not listed.
type wallsReport struct {
	Deployment     []wallsChange       `json:"deployment"`
	Profiles       []wallsReportRow    `json:"profiles"`
	StoredPolicies []wallsReportRow    `json:"stored_policies"`
	Assignments    []wallsReportAssign `json:"assignments"`
}

type wallsReportRow struct {
	ID      uuid.UUID     `json:"id"`
	Name    string        `json:"name"`
	Changes []wallsChange `json:"changes"`
}

type wallsReportAssign struct {
	ID          uuid.UUID     `json:"id"`
	SubjectType string        `json:"subject_type"`
	Subject     string        `json:"subject"`
	ProfileID   uuid.UUID     `json:"profile_id"`
	ProfileName string        `json:"profile_name"`
	Changes     []wallsChange `json:"changes"`
}

// handleOrgWallsDryRun reports what the drafted walls (the body, judged as if on) would narrow. It
// writes nothing.
func (s *Server) handleOrgWallsDryRun(w http.ResponseWriter, r *http.Request) {
	var body types.OrgWallsWrite
	if !decodeStrict(w, r, &body) {
		return
	}
	if err := validateOrgWalls(body); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonOrgWallsInvalid, "invalid org walls: "+err.Error())
		return
	}
	report, err := s.wallsDryRun(r.Context(), types.OrgWalls{Enabled: true, Spec: body.Spec, Limits: body.Limits})
	if err != nil {
		writeServerError(w, r, "org walls dry run", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// wallsDryRun meets the deployment default, each profile as it composes, each stored policy and so
// each assignment with walls. The values shown pass the read redaction a security admin's read does.
func (s *Server) wallsDryRun(ctx context.Context, walls types.OrgWalls) (wallsReport, error) {
	meet := func(spec types.RunPolicySpec, limits types.GovernanceLimits) ([]wallsChange, error) {
		after, afterLimits, _, err := wallsMeet(walls, spec, limits)
		if err != nil {
			return nil, err
		}
		return wallsChanges(redactSpecForRead(spec, true), redactSpecForRead(after, true), limits, afterLimits), nil
	}
	report := wallsReport{Profiles: []wallsReportRow{}, StoredPolicies: []wallsReportRow{}, Assignments: []wallsReportAssign{}}
	var err error
	if report.Deployment, err = meet(s.cfg.DefaultPolicy, types.GovernanceLimits{}); err != nil {
		return wallsReport{}, err
	}
	if s.cfg.Store == nil {
		return report, nil
	}
	profiles, err := s.resolveAllProfiles(ctx)
	if err != nil {
		return wallsReport{}, fmt.Errorf("list governance profiles: %w", err)
	}
	byProfile := map[uuid.UUID]wallsReportRow{}
	for _, p := range profiles {
		if p.Err != nil || p.Resolved == nil {
			continue // a profile that does not compose resolves to no ceiling at all
		}
		changes, err := meet(p.Resolved.Ceiling, p.Resolved.Limits)
		if err != nil {
			return wallsReport{}, err
		}
		if len(changes) > 0 {
			row := wallsReportRow{ID: p.Row.ID, Name: p.Row.Name, Changes: changes}
			report.Profiles = append(report.Profiles, row)
			byProfile[p.Row.ID] = row
		}
	}
	policies, err := s.cfg.Store.ListPolicies(ctx)
	if err != nil {
		return wallsReport{}, fmt.Errorf("list policies: %w", err)
	}
	for _, p := range policies {
		changes, err := meet(p.Spec, types.GovernanceLimits{})
		if err != nil {
			return wallsReport{}, err
		}
		if len(changes) > 0 {
			report.StoredPolicies = append(report.StoredPolicies, wallsReportRow{ID: p.ID, Name: p.Name, Changes: changes})
		}
	}
	assignments, err := s.cfg.Store.ListGovernanceAssignments(ctx)
	if err != nil {
		return wallsReport{}, fmt.Errorf("list governance assignments: %w", err)
	}
	for _, a := range assignments {
		if row, ok := byProfile[a.ProfileID]; ok {
			report.Assignments = append(report.Assignments, wallsReportAssign{ID: a.ID, SubjectType: string(a.SubjectType),
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

// admitBreakGlass is POST /runs' break-glass door (#2946), run before anything resolves a ceiling.
// While the walls are on, the admin token's run must give a reason in X-Wardyn-Break-Glass-Reason
// (refused 400 without one); with one, the request is stamped so the walls do not narrow that run,
// and its row is tagged break_glass. Every other caller, and every request while the walls are off,
// passes untouched: the header means nothing to them.
func (s *Server) admitBreakGlass(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if !isAdminTokenCaller(r) {
		return r, true
	}
	walls, err := s.orgWalls(r.Context())
	if err != nil {
		writeServerError(w, r, "read org walls", err)
		return r, false
	}
	if !walls.Enabled {
		return r, true
	}
	reason := strings.TrimSpace(r.Header.Get(breakGlassReasonHeader))
	if reason == "" || utf8.RuneCountInString(reason) > maxGovernanceRejectReasonLen || !controlCharFree(reason) {
		writeErrorReason(w, http.StatusBadRequest, reasonBreakGlassReasonRequired,
			"the org walls are on: an admin-token run is a break-glass run and needs a reason in the "+breakGlassReasonHeader+
				" header (at most 512 characters, no control characters)")
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), breakGlassKey{}, reason)), true
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
