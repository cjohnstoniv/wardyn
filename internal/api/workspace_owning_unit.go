// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PUT /workspaces/{id}/owning-unit pins a workspace to a unit (or unpins it with a null unit_id). A
// run that attaches the workspace runs in a context at or below the pin (DESIGN-TREE §4.1 step 4,
// pinCandidates). It is on the security tier. Moving a pin to a unit below the old one, or pinning an
// unpinned workspace, only narrows who may use the workspace and is written at once; any other move is
// a covered write, held for a second human when WARDYN_GOVERNANCE_SECOND_HUMAN is on.

const (
	govKindWorkspaceOwningUnit = types.GovernanceTargetWorkspaceOwningUnit
	workspaceOwningUnitAction  = "workspace.owning_unit.set"
)

// owningUnitRequest is the PUT body: the unit to pin, or null to unpin.
type owningUnitRequest struct {
	UnitID *uuid.UUID `json:"unit_id"`
}

// owningUnitChange is a held pin move: the workspace and the unit to pin (nil unpins).
type owningUnitChange struct {
	WorkspaceID uuid.UUID  `json:"workspace_id"`
	UnitID      *uuid.UUID `json:"unit_id"`
}

// owningUnitState is the pin as a proposal sees it and an approval compares.
func owningUnitState(unit *uuid.UUID) map[string]any { return map[string]any{"owning_unit_id": unit} }

// owningUnitRefusal is why unit cannot be a workspace's pin among units, or nil: it must exist, and not
// be the Default unit (which every person is in, so a pin there pins nothing).
func owningUnitRefusal(units []types.GovernanceUnit, unit *uuid.UUID) *unitWriteError {
	if unit == nil {
		return nil
	}
	if *unit == store.DefaultUnitID {
		return writeRefusal(http.StatusUnprocessableEntity, reasonWorkspaceOwningUnitDefault,
			"the Default unit cannot own a workspace: send unit_id null to unpin it")
	}
	if _, ok := unitsByID(units)[*unit]; !ok {
		return writeRefusal(http.StatusUnprocessableEntity, reasonGovernanceUnitNotFoundByID, "governance profile not found")
	}
	return nil
}

// owningUnitNarrows reports whether moving a pin from old to next only narrows it: pinning an unpinned
// workspace, a pin that does not move, or a move to a unit below the old one.
func owningUnitNarrows(units []types.GovernanceUnit, old, next *uuid.UUID) bool {
	switch {
	case old == nil:
		return true
	case next == nil:
		return false
	case *old == *next:
		return true
	}
	byID := unitsByID(units)
	cur := byID[*next].ParentID
	for n := 0; cur != nil && n <= store.MaxUnitDepthCap; n++ {
		if *cur == *old {
			return true
		}
		cur = byID[*cur].ParentID
	}
	return false
}

func (s *Server) handleSetWorkspaceOwningUnit(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var body owningUnitRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	ws, err := s.cfg.Store.GetWorkspace(r.Context(), id)
	if notFoundIf(w, err, "workspace", reasonWorkspaceNotFound) {
		return
	} else if err != nil {
		writeServerError(w, r, "read workspace", err)
		return
	}
	units, err := s.cfg.Store.ListGovernanceUnits(r.Context())
	if err != nil {
		writeServerError(w, r, "list governance profiles", err)
		return
	}
	if e := owningUnitRefusal(units, body.UnitID); e != nil {
		writeErrorReason(w, e.status, e.reason, e.msg)
		return
	}
	if mode == govQueue && !owningUnitNarrows(units, ws.OwningUnitID, body.UnitID) {
		s.proposeGovernanceChange(w, r, govProposal{kind: govKindWorkspaceOwningUnit, op: "set", key: id.String(),
			payload: owningUnitChange{WorkspaceID: id, UnitID: body.UnitID},
			before:  owningUnitState(ws.OwningUnitID), after: owningUnitState(body.UnitID),
			changed:   []string{"owning_unit_id"},
			baseState: owningUnitState(ws.OwningUnitID)})
		return
	}
	_, err = s.cfg.Store.SetWorkspaceOwningUnit(r.Context(), id, body.UnitID)
	if notFoundIf(w, err, "workspace", reasonWorkspaceNotFound) {
		return
	} else if err != nil {
		writeServerError(w, r, "set workspace owning unit", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		workspaceOwningUnitAction, id.String(), "success", mustJSON(map[string]any{"from": ws.OwningUnitID, "to": body.UnitID})))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindWorkspaceOwningUnit, id.String(), "success", nil)
	}
	saved, err := s.cfg.Store.GetWorkspace(r.Context(), id)
	if err != nil {
		writeServerError(w, r, "read workspace", err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// applyWorkspaceOwningUnitChange applies a held pin move inside the decision transaction: the target
// lock, the pin as it now stands compared with what the proposal reviewed, the unit checked again,
// then the write on q.
func applyWorkspaceOwningUnitChange(_ *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var held owningUnitChange
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindWorkspaceOwningUnit, held.WorkspaceID.String()); err != nil {
		return govApplied{}, err
	}
	cur, err := store.WorkspaceOwningUnitQ(ctx, q, held.WorkspaceID)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(owningUnitState(cur)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	units, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	if e := owningUnitRefusal(units, held.UnitID); e != nil {
		return govApplied{}, e
	}
	if err := store.SetWorkspaceOwningUnitQ(ctx, q, held.WorkspaceID, held.UnitID); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: workspaceOwningUnitAction, target: held.WorkspaceID.String(),
		data: map[string]any{"from": cur, "to": held.UnitID}}, nil
}
