// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Unit-admin grants: POST /governance/units/{id}/admins makes a person or an IdP group an admin of the
// unit and the units below it, and DELETE …/admins/{admin} takes it back. A security admin, or a unit
// admin of a unit strictly above it, may ask; nobody grants on their own unit, so a unit admin cannot
// mint a peer to approve their own widening. A security admin's grant follows
// WARDYN_GOVERNANCE_SECOND_HUMAN, so a single-admin install can create unit admins; a unit admin's is
// always held for a second human. Each row records who proposed and who approved it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// govKindUnitAdmin is the change kind of a held grant or revoke.
const govKindUnitAdmin = types.GovernanceTargetUnitAdmin

// unitAdminRequest is POST /governance/units/{id}/admins's body.
type unitAdminRequest struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
}

// unitAdminPayload is a held grant (the unit and subject) or revoke (also the grant's id).
type unitAdminPayload struct {
	ID          uuid.UUID                   `json:"id,omitzero"`
	UnitID      uuid.UUID                   `json:"unit_id"`
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
}

// validateUnitAdminSubject normalizes a grant's subject as a membership's is (the same match surface):
// a person (sub or email) or an IdP group, never a user type or everyone.
func validateUnitAdminSubject(unit uuid.UUID, req unitAdminRequest) (unitAdminPayload, error) {
	if req.SubjectType != types.CapabilitySubjectUser && req.SubjectType != types.CapabilitySubjectGroup {
		return unitAdminPayload{}, fmt.Errorf("subject_type: %q is not user or group", req.SubjectType)
	}
	m := types.GovernanceMembership{UnitID: unit, SubjectType: req.SubjectType, Subject: req.Subject}
	if err := validateGovernanceMembership(&m); err != nil {
		return unitAdminPayload{}, err
	}
	return unitAdminPayload{UnitID: unit, SubjectType: m.SubjectType, Subject: m.Subject}, nil
}

// handleGrantUnitAdmin is POST /governance/units/{id}/admins: 201 with the grant, or 202 with the held
// change.
func (s *Server) handleGrantUnitAdmin(w http.ResponseWriter, r *http.Request) {
	unit, ok := parseIDParam(w, r, "id", "governance unit")
	if !ok || refuseDefaultUnitWrite(w, unit) {
		return
	}
	var req unitAdminRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	p, err := validateUnitAdminSubject(unit, req)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceUnitAdminInvalid, "invalid unit admin: "+err.Error())
		return
	}
	if !s.requireUnitsInScope(w, r, "governance.unit_admin", true, unit) {
		return
	}
	mode, ok := s.unitWriteMode(w, r)
	if !ok {
		return
	}
	if mode == govQueue {
		s.holdUnitAdmin(w, r, "create", p)
		return
	}
	var saved types.GovernanceUnitAdmin
	err = s.writeGovernance(r.Context(), func(q store.Querier) (err error) {
		if err := lockUnitAdminKey(r.Context(), q, p); err != nil {
			return err
		}
		saved, err = store.GrantUnitAdminQ(r.Context(), q, types.GovernanceUnitAdmin{
			UnitID: p.UnitID, SubjectType: p.SubjectType, Subject: p.Subject, GrantedBy: principalFromRequest(r)})
		return err
	})
	if s.writeUnitAdminError(w, r, err) {
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "governance.unit_admin.grant",
		saved.ID.String(), "success", mustJSON(s.directUnitAdminAudit(r, saved))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindUnitAdmin, saved.ID.String(), "success", nil)
	}
	writeJSON(w, http.StatusCreated, saved)
}

// handleRevokeUnitAdmin is DELETE /governance/units/{id}/admins/{admin}: 204, or 202 with the held
// change.
func (s *Server) handleRevokeUnitAdmin(w http.ResponseWriter, r *http.Request) {
	unit, ok := parseIDParam(w, r, "id", "governance unit")
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "admin", "unit admin")
	if !ok {
		return
	}
	if !s.requireUnitsInScope(w, r, "governance.unit_admin", true, unit) {
		return
	}
	mode, ok := s.unitWriteMode(w, r)
	if !ok {
		return
	}
	st := s.unitAdminStore()
	if st == nil {
		writeErrorReason(w, http.StatusNotFound, reasonGovernanceUnitAdminNotFound, "unit admin not found")
		return
	}
	cur, err := st.GetUnitAdmin(r.Context(), unit, id)
	if s.writeUnitAdminError(w, r, err) {
		return
	}
	if mode == govQueue {
		s.holdUnitAdmin(w, r, "delete", unitAdminPayload{ID: cur.ID, UnitID: cur.UnitID, SubjectType: cur.SubjectType, Subject: cur.Subject})
		return
	}
	var removed types.GovernanceUnitAdmin
	err = s.writeGovernance(r.Context(), func(q store.Querier) (err error) {
		if err := lockUnitAdminKey(r.Context(), q, unitAdminPayload{UnitID: cur.UnitID, SubjectType: cur.SubjectType, Subject: cur.Subject}); err != nil {
			return err
		}
		removed, err = store.RevokeUnitAdminQ(r.Context(), q, unit, id)
		return err
	})
	if s.writeUnitAdminError(w, r, err) {
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "governance.unit_admin.revoke",
		removed.ID.String(), "success", mustJSON(s.directUnitAdminAudit(r, removed))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindUnitAdmin, removed.ID.String(), "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeUnitAdminError answers a failed grant or revoke and reports true; false for nil.
func (s *Server) writeUnitAdminError(w http.ResponseWriter, r *http.Request, err error) bool {
	var refusal *unitWriteError
	switch {
	case err == nil:
		return false
	case errors.As(err, &refusal):
		writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
	case errors.Is(err, store.ErrConflict):
		writeErrorReason(w, http.StatusConflict, reasonGovernanceUnitAdminInvalid, "the subject is already an admin of this unit")
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonGovernanceUnitAdminNotFound, "unit admin not found")
	default:
		writeServerError(w, r, "write unit admin", err)
	}
	return true
}

// lockUnitAdminKey serializes the direct write to one grant with the approval of a held change to it.
func lockUnitAdminKey(ctx context.Context, q store.Querier, p unitAdminPayload) error {
	return store.LockGovernanceTarget(ctx, q, govKindUnitAdmin, p.UnitID.String(), string(p.SubjectType), p.Subject)
}

// unitAdminState is a grant's target as a proposal sees it: the grant (nil when none).
func unitAdminState(ctx context.Context, q store.Querier, p unitAdminPayload, forUpdate bool) (any, *types.GovernanceUnitAdmin, error) {
	cur, err := store.GetUnitAdminByKeyQ(ctx, q, p.UnitID, string(p.SubjectType), p.Subject, forUpdate)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return map[string]any{"grant": nil}, nil, nil
	case err != nil:
		return nil, nil, err
	}
	return map[string]any{"grant": cur.ID}, &cur, nil
}

// unitAdminDiffView is a grant as the reviewer reads it.
type unitAdminDiffView struct {
	UnitID      uuid.UUID                   `json:"unit_id"`
	UnitName    string                      `json:"unit_name"`
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
}

// holdUnitAdmin stores a grant (op create) or revoke (op delete) as a pending change.
func (s *Server) holdUnitAdmin(w http.ResponseWriter, r *http.Request, op string, p unitAdminPayload) {
	ctx := r.Context()
	var base any
	var cur *types.GovernanceUnitAdmin
	var unitName string
	err := s.readGovernanceState(r, func(q store.Querier) (err error) {
		row, err := store.GetGovernanceUnitQ(ctx, q, p.UnitID, false)
		if err != nil {
			return err
		}
		unitName = row.Name
		base, cur, err = unitAdminState(ctx, q, p, false)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonGovernanceUnitNotFoundByID, "governance unit not found")
		return
	case err != nil:
		writeServerError(w, r, "hold unit admin", err)
		return
	case op == "create" && cur != nil:
		writeErrorReason(w, http.StatusConflict, reasonGovernanceUnitAdminInvalid, "the subject is already an admin of this unit")
		return
	case op == "delete" && cur == nil:
		writeErrorReason(w, http.StatusNotFound, reasonGovernanceUnitAdminNotFound, "unit admin not found")
		return
	}
	view := &unitAdminDiffView{UnitID: p.UnitID, UnitName: unitName, SubjectType: p.SubjectType, Subject: p.Subject}
	prop := govProposal{
		kind: govKindUnitAdmin, op: op, key: membershipKey(p.UnitID, p.SubjectType, p.Subject),
		payload: p, baseState: base,
	}
	if op == "create" {
		prop.after, prop.changed = view, changedPaths(nil, view)
	} else {
		prop.before, prop.changed = view, changedPaths(view, nil)
	}
	s.proposeGovernanceChange(w, r, prop)
}

// applyUnitAdminChange applies a held grant or revoke inside the decision transaction: the key lock,
// the grant as it now stands compared with what the proposal reviewed, then the write. The approver
// is recorded on the grant.
func applyUnitAdminChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var p unitAdminPayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := lockUnitAdminKey(ctx, q, p); err != nil {
		return govApplied{}, err
	}
	base, cur, err := unitAdminState(ctx, q, p, true)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(base) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	_, approver := actorFromRequest(r)
	if ch.Op == "delete" {
		if cur == nil {
			return govApplied{}, store.ErrGovernanceChangeStale
		}
		removed, err := store.RevokeUnitAdminQ(ctx, q, p.UnitID, cur.ID)
		if err != nil {
			return govApplied{}, err
		}
		return govApplied{action: "governance.unit_admin.revoke", target: removed.ID.String(),
			data: unitAdminAuditData(removed, ch.ProposedBy, approver)}, nil
	}
	saved, err := store.GrantUnitAdminQ(ctx, q, types.GovernanceUnitAdmin{
		UnitID: p.UnitID, SubjectType: p.SubjectType, Subject: p.Subject, GrantedBy: ch.ProposedBy, ApprovedBy: approver})
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, writeRefusal(http.StatusNotFound, reasonGovernanceUnitNotFoundByID, "governance unit not found")
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: "governance.unit_admin.grant", target: saved.ID.String(),
		data: unitAdminAuditData(saved, ch.ProposedBy, approver)}, nil
}

// unitAdminAuditData is a grant or revoke row's data: the grant, the actor who proposed it and who
// approved it ("" when it applied directly). unit_scope, the scope it was decided in, is read outside
// the write's transaction: by directUnitAdminAudit, and by the decision for an approved change.
func unitAdminAuditData(g types.GovernanceUnitAdmin, actor, approver string) map[string]any {
	return map[string]any{
		"unit_id": g.UnitID, "subject_type": g.SubjectType, "subject": g.Subject, "actor": actor, "approver": approver,
	}
}

// directUnitAdminAudit is the row data of a grant or revoke that applied directly: the caller is its
// actor, in the scope strictly above the granted unit, and no one approved it.
func (s *Server) directUnitAdminAudit(r *http.Request, g types.GovernanceUnitAdmin) map[string]any {
	data := unitAdminAuditData(g, principalFromRequest(r), "")
	data["unit_scope"] = s.auditUnitScope(r.Context(), g.UnitID, true)
	return data
}

// auditUnitScope is the scope a caller writes a unit in, for its audit row: "security" for a security
// caller, else the id of the nearest unit at or above unit (strictly above when strict) one of their
// grants is on, "" when none is (an approval's re-read can find the grant gone).
func (s *Server) auditUnitScope(ctx context.Context, unit uuid.UUID, strict bool) string {
	if s.isSecurityOperator(ctx) {
		return "security"
	}
	grants, err := s.callerUnitGrants(ctx)
	if err != nil || len(grants) == 0 {
		return ""
	}
	units, err := s.unitTreeRows(ctx)
	if err != nil {
		return ""
	}
	byID := unitsByID(units)
	cur := &unit
	if strict {
		cur = byID[unit].ParentID
	}
	for n := 0; cur != nil && n < store.MaxUnitDepthCap; n++ {
		if slices.ContainsFunc(grants, func(g types.GovernanceUnitAdmin) bool { return g.UnitID == *cur }) {
			return cur.String()
		}
		cur = byID[*cur].ParentID
	}
	return ""
}

// ── who may approve a unit change ────────────────────────────────────────────

// unitChangeScope is the units a held change of a unit kind touches, which an approving unit admin
// must govern (strict: strictly above), from the change's key, payload and stored diff. ok is false
// for a change only a security admin may decide: a promotion, a unit with no parent, a payload that
// does not decode.
func unitChangeScope(ch types.GovernanceChange) (units []uuid.UUID, strict, ok bool) {
	var diff governanceDiffBody
	if json.Unmarshal(ch.Diff, &diff) != nil {
		return nil, false, false
	}
	switch ch.TargetKind {
	case govKindUnit:
		id, err := uuid.Parse(ch.TargetKey)
		if err != nil {
			return nil, false, false
		}
		if ch.Op == "delete" {
			return []uuid.UUID{id}, true, true
		}
		var before, after struct {
			ParentID *uuid.UUID `json:"parent_id"`
		}
		_ = json.Unmarshal(diff.After, &after)
		if ch.Op == "update" {
			_ = json.Unmarshal(diff.Before, &before)
			units = append(units, id)
			if sameJSON(before.ParentID, after.ParentID) {
				return units, false, true
			}
		}
		if after.ParentID == nil {
			return nil, false, false
		}
		return append(units, *after.ParentID), false, true
	case govKindMembership:
		unit, _, _, keyed := splitMembershipKey(ch.TargetKey)
		if !keyed || ch.Op == "promote" {
			return nil, false, false
		}
		units = []uuid.UUID{unit}
		switch ch.Op {
		case "move":
			var p membershipTreePayload
			if json.Unmarshal(ch.Payload, &p) != nil || p.UnitID == uuid.Nil {
				return nil, false, false
			}
			units = append(units, p.UnitID)
		case "delete":
			for _, a := range diff.Activates {
				units = append(units, a.UnitID)
			}
		}
		return units, false, true
	case govKindUnitAdmin:
		unit, _, _, keyed := splitMembershipKey(ch.TargetKey)
		return []uuid.UUID{unit}, true, keyed
	}
	return nil, false, false
}

// unitChangeApprover is the approver of a held unit, membership or grant change: a security caller,
// or a unit admin governing every unit it touches (unitChangeScope). The proposer is excluded by the
// decision's distinct-human rule.
func unitChangeApprover(ch types.GovernanceChange) govApprover {
	units, strict, ok := unitChangeScope(ch)
	if !ok {
		return securityApprover
	}
	return unitScopeApprover(strict, units...)
}

// changeApprover is one who may decide a held change: any security admin, or a unit admin grant (a
// person or a group) whose unit covers every unit the change touches.
type changeApprover struct {
	Kind        string                      `json:"kind"`
	Name        string                      `json:"name,omitempty"`
	UnitName    string                      `json:"unit_name,omitempty"`
	SubjectType types.CapabilitySubjectType `json:"subject_type,omitempty"`
}

// approversFor lists who may decide a change touching units (strict: strictly below the approver's
// unit): any security admin first, then each grant covering them all, the proposer's own user grants
// left out (a group grant cannot be told apart from its members, and the decision refuses the proposer
// anyway). A store that cannot be read lists the security admins only.
func (s *Server) approversFor(ctx context.Context, units []uuid.UUID, strict bool, proposer, proposerEmail string) []changeApprover {
	out := []changeApprover{{Kind: "security"}}
	st := s.unitAdminStore()
	if st == nil || len(units) == 0 {
		return out
	}
	grants, err := st.ListUnitAdmins(ctx)
	if err != nil {
		return out
	}
	all, err := s.unitTreeRows(ctx)
	if err != nil {
		return out
	}
	byID := unitsByID(all)
	covers := func(g types.GovernanceUnitAdmin, unit uuid.UUID) bool {
		cur := &unit
		if strict {
			cur = byID[unit].ParentID
		}
		for n := 0; cur != nil && n < store.MaxUnitDepthCap; n++ {
			if *cur == g.UnitID {
				return true
			}
			cur = byID[*cur].ParentID
		}
		return false
	}
	self := []string{types.CanonicalUserSubject(proposer), types.CanonicalUserSubject(proposerEmail)}
	for _, g := range grants {
		if g.SubjectType == types.CapabilitySubjectUser && slices.Contains(self, g.Subject) {
			continue
		}
		if !slices.ContainsFunc(units, func(u uuid.UUID) bool { return !covers(g, u) }) {
			out = append(out, changeApprover{Kind: "unit_admin", Name: g.Subject, UnitName: byID[g.UnitID].Name, SubjectType: g.SubjectType})
		}
	}
	slices.SortStableFunc(out[1:], func(a, b changeApprover) int {
		return strings.Compare(a.UnitName+"\x00"+a.Name, b.UnitName+"\x00"+b.Name)
	})
	return out
}

// changeApprovers is approversFor a held change: unit kinds by the units they touch, every other kind
// a security admin decides (or, for an operator-tier kind, a super admin: none is listed then).
func (s *Server) changeApprovers(ctx context.Context, ch types.GovernanceChange) []changeApprover {
	kind, ok := governanceChangeKinds[ch.TargetKind]
	if !ok {
		return nil
	}
	if units, strict, ok := unitChangeScope(ch); ok {
		return s.approversFor(ctx, units, strict, ch.ProposedBy, ch.ProposedByEmail)
	}
	if kind.forChange(ch).approver.reason == securityApprover.reason {
		return []changeApprover{{Kind: "security"}}
	}
	return nil
}

// governanceChangeRead is a held change as GET /governance/changes/{id} answers it: the change and who
// may decide it.
type governanceChangeRead struct {
	types.GovernanceChange
	Approvers []changeApprover `json:"approvers,omitempty"`
}
