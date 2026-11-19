// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// An envelope's approved groups: a unit admin adds such a group to a unit at or below the envelope's
// root, or moves its membership between two such units, without a second human. The security tier's
// approval of the group is that decision. Anything the proof does not cover takes today's path: a
// person or user type, a group off the list, a unit outside the envelope, a write that changes which
// legacy row a person is selected by, and any envelope that cannot be read. Deletes are unchanged: a
// last membership's delete can widen people back to contexts outside the envelope.
package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errNoEnvelopeProof ends an approved-group write's transaction when the proof no longer holds on it.
var errNoEnvelopeProof = errors.New("api: no envelope approves this group here")

// approvedGroupEnvelopeQ is the envelope at or above unit that approves group, read on q; ok is false
// when none does.
func approvedGroupEnvelopeQ(ctx context.Context, q store.Querier, unit uuid.UUID, group string) (types.GovernanceEnvelope, bool, error) {
	env, found, err := store.NearestEnvelopeQ(ctx, q, unit, false)
	if err != nil || !found || !slices.Contains(env.ApprovedGroups, group) {
		return types.GovernanceEnvelope{}, false, err
	}
	return env, true, nil
}

// upsertApprovedGroupMembership writes a unit admin's add of an approved group directly and reports
// true once it has answered. It reports false, having written nothing, whenever the proof does not
// hold, so the caller goes on with today's path (the belonging check, then the hold). The list is read
// first on the store and again on the write's transaction under the graph lock, which envelope writes
// take too.
func (s *Server) upsertApprovedGroupMembership(w http.ResponseWriter, r *http.Request, a types.GovernanceMembership) bool {
	ctx := r.Context()
	if a.SubjectType != types.CapabilitySubjectGroup || !s.isUnitAdminCaller(ctx) {
		return false
	}
	st, ok := s.cfg.Store.(store.EnvelopeStore)
	if !ok {
		return false
	}
	if env, found, err := st.NearestEnvelope(ctx, a.UnitID, false); err != nil || !found || !slices.Contains(env.ApprovedGroups, a.Subject) {
		return false
	}
	a.ID = uuid.New()
	a.CreatedBy = principalFromRequest(r)
	var env types.GovernanceEnvelope
	var saved types.GovernanceMembership
	err := s.writeGovernance(ctx, func(q store.Querier) error {
		if err := store.LockGovernanceGraph(ctx, q); err != nil {
			return err
		}
		if err := store.LockGovernanceMembershipKey(ctx, q, string(a.SubjectType), a.Subject); err != nil {
			return err
		}
		if err := s.requireUnitScopeQ(ctx, q, false, a.UnitID); err != nil {
			return err
		}
		e, approved, err := approvedGroupEnvelopeQ(ctx, q, a.UnitID, a.Subject)
		if err != nil {
			return err
		}
		if !approved {
			return errNoEnvelopeProof
		}
		cur, err := readMembershipState(ctx, q, a.UnitID, string(a.SubjectType), a.Subject, true)
		if err != nil {
			return err
		}
		shift, err := upsertShiftQ(ctx, q, cur.current, a)
		if err != nil {
			return err
		}
		if len(shift.activates)+len(shift.vacates) > 0 {
			return errNoEnvelopeProof
		}
		env = e
		saved, err = store.UpsertGovernanceMembershipQ(ctx, q, a)
		return err
	})
	if err != nil {
		// Every failure, a refusal included, rolls the write back and takes today's path, which answers
		// it with its own reads.
		return false
	}
	status := http.StatusCreated
	if saved.ID != a.ID {
		status = http.StatusOK
	}
	data := unitScopeAudit(membershipWriteAuditData(saved), r, s.auditUnitScope(ctx, saved.UnitID, false))
	data["envelope"] = env.UnitID.String()
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.membership.write", saved.ID.String(), "success", mustJSON(data)))
	writeJSON(w, status, saved)
	return true
}

// envelopeApprovesMoveQ is moveMembershipInScope's narrowing proof for an approved group: the moved row
// is a group, and one envelope, at or above both the unit it leaves and unit to, approves it. Read on
// the move's transaction; a read error proves nothing, so the move is held.
func (s *Server) envelopeApprovesMoveQ(ctx context.Context, q store.Querier, row types.GovernanceMembership, to uuid.UUID) bool {
	if row.SubjectType != types.CapabilitySubjectGroup || !s.isUnitAdminCaller(ctx) {
		return false
	}
	if _, ok := s.cfg.Store.(store.EnvelopeStore); !ok {
		return false
	}
	from, ok, err := approvedGroupEnvelopeQ(ctx, q, row.UnitID, row.Subject)
	if err != nil || !ok {
		return false
	}
	dest, ok, err := approvedGroupEnvelopeQ(ctx, q, to, row.Subject)
	return err == nil && ok && dest.UnitID == from.UnitID
}
