// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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
// a covered write, held for a second human when WARDYN_GOVERNANCE_SECOND_HUMAN is on. A strict pin
// (strict: true) drops the ancestor mapping: only a person whose own unit is the pin or below it runs
// on the workspace. Making a pin strict narrows and is written at once; making it plain is held.

const (
	govKindWorkspaceOwningUnit = types.GovernanceTargetWorkspaceOwningUnit
	workspaceOwningUnitAction  = "workspace.owning_unit.set"
)

// owningUnitRequest is the PUT body: the unit to pin, or null to unpin, and whether the pin is strict
// (absent keeps the current strictness when a unit is given; a null unit always clears it).
type owningUnitRequest struct {
	UnitID *uuid.UUID `json:"unit_id"`
	Strict *bool      `json:"strict,omitempty"`
}

// owningUnitChange is a held pin move: the workspace, the unit to pin (nil unpins) and the strictness
// resolved at proposal. A payload stored before the switch decodes to false, every row's value then.
type owningUnitChange struct {
	WorkspaceID uuid.UUID  `json:"workspace_id"`
	UnitID      *uuid.UUID `json:"unit_id"`
	Strict      bool       `json:"strict,omitempty"`
}

// owningUnitState is the pin as a proposal sees it and an approval compares. The strict key is there
// only when the pin is strict, so a change proposed before the switch hashes the same against a row
// that has not moved.
func owningUnitState(unit *uuid.UUID, strict bool) map[string]any {
	st := map[string]any{"owning_unit_id": unit}
	if strict {
		st["owning_unit_strict"] = true
	}
	return st
}

// owningUnitStrictNext is the strictness a write leaves: the request's when it gives one, else the
// current one, and never strict with no unit.
func owningUnitStrictNext(req *bool, cur bool, unit *uuid.UUID) bool {
	if unit == nil {
		return false
	}
	if req != nil {
		return *req
	}
	return cur
}

// workspacePinNarrows reports whether a pin write only narrows: the unit only narrows
// (owningUnitNarrows) and a strict pin stays strict.
func workspacePinNarrows(units []types.GovernanceUnit, old, next *uuid.UUID, oldStrict, nextStrict bool) bool {
	return owningUnitNarrows(units, old, next) && (nextStrict || !oldStrict)
}

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
	if body.UnitID == nil && body.Strict != nil && *body.Strict {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonWorkspaceOwningUnitStrictNeedsUnit,
			"a strict pin needs a unit: send a unit_id, or leave strict out to unpin")
		return
	}
	ws, err := s.cfg.Store.GetWorkspace(r.Context(), id)
	if notFoundIf(w, err, "workspace", reasonWorkspaceNotFound) {
		return
	} else if err != nil {
		writeServerError(w, r, "read workspace", err)
		return
	}
	units, err := s.unitOutlines(r.Context())
	if err != nil {
		writeServerError(w, r, "list governance profiles", err)
		return
	}
	if e := owningUnitRefusal(units, body.UnitID); e != nil {
		writeErrorReason(w, e.status, e.reason, e.msg)
		return
	}
	next := owningUnitStrictNext(body.Strict, ws.OwningUnitStrict, body.UnitID)
	if mode == govQueue && !workspacePinNarrows(units, ws.OwningUnitID, body.UnitID, ws.OwningUnitStrict, next) {
		s.holdOwningUnit(w, r, id, ws.OwningUnitID, ws.OwningUnitStrict, body.UnitID, next)
		return
	}
	var cur owningUnitPin
	err = s.writeGovernance(r.Context(), func(q store.Querier) (err error) {
		cur, err = setOwningUnitQ(r.Context(), q, id, body, mode == govQueue)
		return err
	})
	var refusal *unitWriteError
	switch {
	case errors.Is(err, errHoldForApproval):
		// The pin moved since it was read, and this move no longer only narrows the one it found.
		s.holdOwningUnit(w, r, id, cur.from, cur.fromStrict, body.UnitID, cur.next)
		return
	case errors.As(err, &refusal):
		writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
		return
	case notFoundIf(w, err, "workspace", reasonWorkspaceNotFound):
		return
	case err != nil:
		writeServerError(w, r, "set workspace owning unit", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		workspaceOwningUnitAction, id.String(), "success", mustJSON(map[string]any{"from": cur.from, "to": body.UnitID, "strict": cur.next})))
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

// holdOwningUnit stores the move of workspace id's pin from (from, fromStrict) to (unit, strict) as a
// pending change, from being the pin the proposal saw.
func (s *Server) holdOwningUnit(w http.ResponseWriter, r *http.Request, id uuid.UUID, from *uuid.UUID, fromStrict bool, unit *uuid.UUID, strict bool) {
	var changed []string
	if !samePinUnit(from, unit) {
		changed = append(changed, "owning_unit_id")
	}
	if fromStrict != strict {
		changed = append(changed, "owning_unit_strict")
	}
	s.proposeGovernanceChange(w, r, govProposal{kind: govKindWorkspaceOwningUnit, op: "set", key: id.String(),
		payload: owningUnitChange{WorkspaceID: id, UnitID: unit, Strict: strict},
		before:  owningUnitState(from, fromStrict), after: owningUnitState(unit, strict),
		changed:   changed,
		baseState: owningUnitState(from, fromStrict)})
}

// samePinUnit reports whether two pins name the same unit (nil: unpinned).
func samePinUnit(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// owningUnitPin is a direct write's reading of the pin as it stands in the transaction (from,
// fromStrict) and the strictness the write leaves (next).
type owningUnitPin struct {
	from             *uuid.UUID
	fromStrict, next bool
}

// setOwningUnitQ is a direct pin write on q, under the graph lock and the target lock a held move's
// approval takes: the pin and the units as they now stand, the unit checked again, and with held
// writes on (queue) errHoldForApproval unless the write still only narrows that pin, not only the one
// the handler read before the transaction.
func setOwningUnitQ(ctx context.Context, q store.Querier, id uuid.UUID, body owningUnitRequest, queue bool) (cur owningUnitPin, err error) {
	if err = store.LockGovernanceGraph(ctx, q); err != nil {
		return cur, err
	}
	if err = store.LockGovernanceTarget(ctx, q, govKindWorkspaceOwningUnit, id.String()); err != nil {
		return cur, err
	}
	if cur.from, cur.fromStrict, err = store.WorkspaceOwningUnitQ(ctx, q, id); err != nil {
		return cur, err
	}
	cur.next = owningUnitStrictNext(body.Strict, cur.fromStrict, body.UnitID)
	units, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return cur, err
	}
	if e := owningUnitRefusal(units, body.UnitID); e != nil {
		return cur, e
	}
	if queue && !workspacePinNarrows(units, cur.from, body.UnitID, cur.fromStrict, cur.next) {
		return cur, errHoldForApproval
	}
	return cur, store.SetWorkspaceOwningUnitQ(ctx, q, id, body.UnitID, cur.next)
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
	cur, curStrict, err := store.WorkspaceOwningUnitQ(ctx, q, held.WorkspaceID)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(owningUnitState(cur, curStrict)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	units, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	if e := owningUnitRefusal(units, held.UnitID); e != nil {
		return govApplied{}, e
	}
	if err := store.SetWorkspaceOwningUnitQ(ctx, q, held.WorkspaceID, held.UnitID, held.Strict && held.UnitID != nil); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: workspaceOwningUnitAction, target: held.WorkspaceID.String(),
		data: map[string]any{"from": cur, "to": held.UnitID, "strict": held.Strict}}, nil
}
