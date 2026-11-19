// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Platform envelopes: PUT and DELETE /governance/units/{id}/envelope name the groups security approved
// for the unit that is a platform's root (table governance_envelopes, types.GovernanceEnvelope). The
// envelope changes only who must approve a write below that root; it enforces nothing at run time.
//
// Security tier only (mountGovernanceRoutes hands these two routes the securityOps group), and a
// covered governance write: under WARDYN_GOVERNANCE_SECOND_HUMAN a set and a clear are held as
// governance_envelope changes.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	govKindEnvelope = types.GovernanceTargetEnvelope

	envelopeSetAction    = "governance.envelope.set"
	envelopeDeleteAction = "governance.envelope.delete"
)

// envelopeRequest is PUT /governance/units/{id}/envelope's body.
type envelopeRequest struct {
	ApprovedGroups []string `json:"approved_groups"`
}

// envelopeChangePayload is a held set or clear: the unit and, for a set, the canonical groups.
type envelopeChangePayload struct {
	UnitID         uuid.UUID `json:"unit_id"`
	ApprovedGroups []string  `json:"approved_groups,omitempty"`
}

// envelopeView is the part of an envelope a write replaces and an approval compares; who set it and
// when are not part of what an approval compares.
type envelopeView struct {
	UnitID         uuid.UUID `json:"unit_id"`
	ApprovedGroups []string  `json:"approved_groups"`
}

// envelopeState is the envelope as a proposal saw it; nil when the unit has none.
func envelopeState(e types.GovernanceEnvelope, found bool) *envelopeView {
	if !found {
		return nil
	}
	return &envelopeView{UnitID: e.UnitID, ApprovedGroups: e.ApprovedGroups}
}

func (s *Server) envelopeStore(w http.ResponseWriter) (store.EnvelopeStore, bool) {
	es, ok := s.cfg.Store.(store.EnvelopeStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonEnvelopeInvalid, "this store keeps no envelopes")
	}
	return es, ok
}

// governanceEnvelopes is every envelope for GET /governance: none (an empty list) on a store that
// keeps none.
func (s *Server) governanceEnvelopes(ctx context.Context) ([]types.GovernanceEnvelope, error) {
	es, ok := s.cfg.Store.(store.EnvelopeStore)
	if !ok {
		return []types.GovernanceEnvelope{}, nil
	}
	return es.ListEnvelopes(ctx)
}

// envelopeRefusal turns the store's write-time refusals into the wire refusals; any other error is
// returned unchanged.
func (s *Server) envelopeRefusal(ctx context.Context, err error) error {
	var taken *store.ErrEnvelopeGroupTaken
	switch {
	case errors.Is(err, store.ErrNotFound):
		return writeRefusal(http.StatusBadRequest, reasonEnvelopeInvalid, "invalid envelope: the unit does not exist")
	case errors.Is(err, store.ErrEnvelopeNested):
		return writeRefusal(http.StatusConflict, reasonEnvelopeNested,
			"an envelope already covers this unit's path: a unit above or below it has one, and only one envelope may apply to a unit")
	case errors.As(err, &taken):
		name := taken.UnitID.String()
		if leaf, gerr := s.unitLeafName(ctx, taken.UnitID); gerr == nil {
			name = leaf
		}
		return writeRefusal(http.StatusConflict, reasonEnvelopeGroupTaken,
			"the group %q is already approved on the envelope of unit %q: a group sits on at most one envelope", taken.Group, name)
	}
	return err
}

// envelopeUnitRefusal refuses the Default unit, whose walls the deployment sets.
func envelopeUnitRefusal(unit uuid.UUID) *unitWriteError {
	if unit == store.DefaultUnitID {
		return writeRefusal(http.StatusBadRequest, reasonEnvelopeInvalid, "invalid envelope: the Default unit cannot be a platform's root")
	}
	return nil
}

// diffGroups is the groups added to and removed from before to reach after, each sorted.
func diffGroups(before, after []string) (added, removed []string) {
	added, removed = []string{}, []string{}
	for _, g := range after {
		if !slices.Contains(before, g) {
			added = append(added, g)
		}
	}
	for _, g := range before {
		if !slices.Contains(after, g) {
			removed = append(removed, g)
		}
	}
	return added, removed
}

func envelopeAuditData(unit uuid.UUID, before, after []string) map[string]any {
	added, removed := diffGroups(before, after)
	return map[string]any{"unit_id": unit, "added": added, "removed": removed}
}

// handlePutEnvelope is PUT /governance/units/{id}/envelope: 200 with the envelope, or 202 with the held
// change. A body names approved_groups and nothing else.
func (s *Server) handlePutEnvelope(w http.ResponseWriter, r *http.Request) {
	unit, ok := parseIDParam(w, r, "id", "governance unit")
	if !ok {
		return
	}
	if _, ok := s.envelopeStore(w); !ok {
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var req envelopeRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	groups, err := types.ValidateEnvelope(req.ApprovedGroups)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonEnvelopeInvalid, "invalid envelope: "+err.Error())
		return
	}
	if e := envelopeUnitRefusal(unit); e != nil {
		writeErrorReason(w, e.status, e.reason, e.msg)
		return
	}
	if mode == govQueue {
		s.holdEnvelopeSet(w, r, unit, groups)
		return
	}
	var prev, saved types.GovernanceEnvelope
	err = s.writeGovernance(r.Context(), func(q store.Querier) error {
		if err := store.LockGovernanceGraph(r.Context(), q); err != nil {
			return err
		}
		var err error
		if prev, err = store.GetEnvelopeQ(r.Context(), q, unit); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		saved, err = store.SetEnvelopeQ(r.Context(), q, unit, groups, principalFromRequest(r))
		return err
	})
	if writeMembershipTreeError(w, r, "write envelope", s.envelopeRefusal(r.Context(), err)) {
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		envelopeSetAction, unit.String(), "success", mustJSON(envelopeAuditData(unit, prev.ApprovedGroups, saved.ApprovedGroups))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindEnvelope, unit.String(), "success", nil)
	}
	writeJSON(w, http.StatusOK, saved)
}

// handleDeleteEnvelope is DELETE /governance/units/{id}/envelope: 204, or 202 with the held change.
// A unit with no envelope answers 204 and writes nothing.
func (s *Server) handleDeleteEnvelope(w http.ResponseWriter, r *http.Request) {
	unit, ok := parseIDParam(w, r, "id", "governance unit")
	if !ok {
		return
	}
	es, ok := s.envelopeStore(w)
	if !ok {
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	if mode == govQueue {
		cur, err := es.GetEnvelope(r.Context(), unit)
		if errors.Is(err, store.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			writeServerError(w, r, "read envelope", err)
			return
		}
		view := envelopeState(cur, true)
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindEnvelope, op: "delete", key: unit.String(), payload: envelopeChangePayload{UnitID: unit},
			before: view, changed: changedPaths(view, nil), baseState: view,
		})
		return
	}
	var removed types.GovernanceEnvelope
	err := s.writeGovernance(r.Context(), func(q store.Querier) (err error) {
		removed, err = store.DeleteEnvelopeQ(r.Context(), q, unit)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if writeMembershipTreeError(w, r, "delete envelope", err) {
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		envelopeDeleteAction, unit.String(), "success", mustJSON(envelopeAuditData(unit, removed.ApprovedGroups, nil))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindEnvelope, unit.String(), "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// holdEnvelopeSet stores a validated set as a pending change. The refusals a write would give (an
// unknown unit, a nested envelope, a group on another) answer at the proposal too, and again at the
// approval, when the tree may have moved.
func (s *Server) holdEnvelopeSet(w http.ResponseWriter, r *http.Request, unit uuid.UUID, groups []string) {
	var cur types.GovernanceEnvelope
	found := false
	err := s.readGovernanceState(r, func(q store.Querier) error {
		if err := store.CheckEnvelopeQ(r.Context(), q, unit, groups); err != nil {
			return err
		}
		var gerr error
		cur, gerr = store.GetEnvelopeQ(r.Context(), q, unit)
		found = gerr == nil
		if errors.Is(gerr, store.ErrNotFound) {
			return nil
		}
		return gerr
	})
	if writeMembershipTreeError(w, r, "read envelope", s.envelopeRefusal(r.Context(), err)) {
		return
	}
	base, after := envelopeState(cur, found), &envelopeView{UnitID: unit, ApprovedGroups: groups}
	var before any // a create has no before view, and a nil pointer inside an any is not nil
	if base != nil {
		before = base
	}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindEnvelope, op: "set", key: unit.String(), payload: envelopeChangePayload{UnitID: unit, ApprovedGroups: groups},
		before: before, after: after, changed: changedPaths(before, after), baseState: base,
	})
}

// applyEnvelopeChange applies a held set or clear inside the decision transaction: the graph lock and
// the target lock, the envelope as it now stands compared with what the proposal reviewed, then the
// write on q with the checks run again.
func applyEnvelopeChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var p envelopeChangePayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if e := envelopeUnitRefusal(p.UnitID); e != nil {
		return govApplied{}, e
	}
	if err := store.LockGovernanceGraph(ctx, q); err != nil {
		return govApplied{}, err
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindEnvelope, p.UnitID.String()); err != nil {
		return govApplied{}, err
	}
	cur, err := store.GetEnvelopeQ(ctx, q, p.UnitID)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return govApplied{}, err
	}
	if computeETag(envelopeState(cur, found)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if ch.Op == "delete" {
		removed, err := store.DeleteEnvelopeQ(ctx, q, p.UnitID)
		if err != nil {
			return govApplied{}, s.envelopeRefusal(ctx, err)
		}
		return govApplied{action: envelopeDeleteAction, target: p.UnitID.String(), data: envelopeAuditData(p.UnitID, removed.ApprovedGroups, nil)}, nil
	}
	groups, err := types.ValidateEnvelope(p.ApprovedGroups)
	if err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonEnvelopeInvalid, "invalid envelope: %s", err.Error())
	}
	saved, err := store.SetEnvelopeQ(ctx, q, p.UnitID, groups, ch.ProposedBy)
	if err != nil {
		return govApplied{}, s.envelopeRefusal(ctx, err)
	}
	return govApplied{action: envelopeSetAction, target: p.UnitID.String(), data: envelopeAuditData(p.UnitID, cur.ApprovedGroups, saved.ApprovedGroups)}, nil
}

// envelopeSetByForUnitAdmins is the set_by a unit admin reads on an envelope.
const envelopeSetByForUnitAdmins = "security"

// envelopesInScope narrows envelopes for a unit admin: those on a unit of their subtrees (in) and
// those on a unit above a subtree root, which is the envelope their units sit under. roots are the
// subtree roots (subtreeUnits). Each reads set_by as envelopeSetByForUnitAdmins: the setter is a security
// admin, whose principal is not shown to a lower tier.
func envelopesInScope(envelopes []types.GovernanceEnvelope, rows []types.GovernanceUnit, in map[uuid.UUID]bool, roots []uuid.UUID) []types.GovernanceEnvelope {
	byID := unitsByID(rows)
	above := map[uuid.UUID]bool{}
	for _, root := range roots {
		cur := byID[root].ParentID
		for n := 0; cur != nil && !above[*cur] && n <= len(rows); n++ {
			above[*cur] = true
			row, ok := byID[*cur]
			if !ok {
				break
			}
			cur = row.ParentID
		}
	}
	out := []types.GovernanceEnvelope{}
	for _, e := range envelopes {
		if in[e.UnitID] || above[e.UnitID] {
			e.SetBy = envelopeSetByForUnitAdmins
			out = append(out, e)
		}
	}
	return out
}
