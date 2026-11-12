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

// PUT /components/{id}/owning-unit pins an organisation component to a unit (or unpins it with a null
// unit_id). Only a run whose context is that unit or a unit below it may attach the component, whatever
// component grants the person holds: the pin is strict, judged in the context choice
// (strictPinCandidates), never at the attach door. A running run whose component's pin no longer covers
// its unit is given no credential (liveWithholding), and is not revived, restarted or extended
// (componentPinRefusal). A person's own row cannot be pinned, and a person's own CLI sign-in to a
// component is not a run, so no pin applies to it. It is on the security tier. Pinning an unpinned
// component, or moving the pin to a unit below the old one, only narrows and is written at once; any
// other move is a covered write, held for a second human when WARDYN_GOVERNANCE_SECOND_HUMAN is on.

const (
	govKindComponentOwningUnit = types.GovernanceTargetComponentOwningUnit
	componentOwningUnitAction  = "component.owning_unit.set"
)

// componentOwningUnitRequest is the PUT body: the unit to pin, or null to unpin.
type componentOwningUnitRequest struct {
	UnitID *uuid.UUID `json:"unit_id"`
}

// componentOwningUnitChange is a held pin move: the component and the unit to pin (nil unpins).
type componentOwningUnitChange struct {
	ComponentID uuid.UUID  `json:"component_id"`
	UnitID      *uuid.UUID `json:"unit_id"`
}

// componentUnitRefusal is why unit cannot be a component's pin among units, or nil: not the Default
// unit, and a unit that exists (owningUnitRefusal).
func componentUnitRefusal(units []types.GovernanceUnit, unit *uuid.UUID) *unitWriteError {
	if unit != nil && *unit == store.DefaultUnitID {
		return writeRefusal(http.StatusUnprocessableEntity, reasonComponentOwningUnitDefault,
			"the Default unit cannot own a component: send unit_id null to unpin it")
	}
	return owningUnitRefusal(units, unit)
}

func (s *Server) handleSetComponentOwningUnit(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "component")
	if !ok {
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var body componentOwningUnitRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	cs, row, units, ok := s.componentPinTarget(w, r, id, body.UnitID)
	if !ok {
		return
	}
	if mode == govQueue && !owningUnitNarrows(units, row.OwningUnitID, body.UnitID) {
		s.holdComponentOwningUnit(w, r, id, row.OwningUnitID, body.UnitID)
		return
	}
	var from *uuid.UUID
	err := s.writeGovernance(r.Context(), func(q store.Querier) (err error) {
		from, err = setComponentOwningUnitQ(r.Context(), q, id, body.UnitID, mode == govQueue)
		return err
	})
	var refusal *unitWriteError
	switch {
	case errors.Is(err, errHoldForApproval):
		// The pin moved since it was read, and this move no longer only narrows the one it found.
		s.holdComponentOwningUnit(w, r, id, from, body.UnitID)
		return
	case errors.As(err, &refusal):
		writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
		return
	case notFoundIf(w, err, "component", reasonComponentNotFound):
		return
	case err != nil:
		writeServerError(w, r, "set component owning unit", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		componentOwningUnitAction, id.String(), "success", mustJSON(map[string]any{"from": from, "to": body.UnitID})))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindComponentOwningUnit, id.String(), "success", nil)
	}
	saved, err := cs.GetComponent(r.Context(), id, "")
	if err != nil {
		writeServerError(w, r, "read component", err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// componentPinTarget reads what a pin write needs before it decides, or writes the refusal and returns
// false: the component store (501 without one), the organisation's row (404 for an unknown id and for
// a person's row alike), the units, and the unit checked (componentUnitRefusal).
func (s *Server) componentPinTarget(w http.ResponseWriter, r *http.Request, id uuid.UUID, unit *uuid.UUID) (store.ComponentStore, types.Component, []types.GovernanceUnit, bool) {
	cs, ok := s.componentStoreOr501(w)
	if !ok {
		return nil, types.Component{}, nil, false
	}
	row, err := cs.GetComponent(r.Context(), id, "")
	if notFoundIf(w, err, "component", reasonComponentNotFound) {
		return nil, types.Component{}, nil, false
	} else if err != nil {
		writeServerError(w, r, "read component", err)
		return nil, types.Component{}, nil, false
	}
	units, err := s.unitOutlines(r.Context())
	if err != nil {
		writeServerError(w, r, "list governance profiles", err)
		return nil, types.Component{}, nil, false
	}
	if e := componentUnitRefusal(units, unit); e != nil {
		writeErrorReason(w, e.status, e.reason, e.msg)
		return nil, types.Component{}, nil, false
	}
	return cs, row, units, true
}

// holdComponentOwningUnit stores the move of component id's pin from from to unit as a pending change,
// from being the pin the proposal saw.
func (s *Server) holdComponentOwningUnit(w http.ResponseWriter, r *http.Request, id uuid.UUID, from, unit *uuid.UUID) {
	s.proposeGovernanceChange(w, r, govProposal{kind: govKindComponentOwningUnit, op: "set", key: id.String(),
		payload: componentOwningUnitChange{ComponentID: id, UnitID: unit},
		before:  owningUnitState(from, false), after: owningUnitState(unit, false),
		changed:   []string{"owning_unit_id"},
		baseState: owningUnitState(from, false)})
}

// setComponentOwningUnitQ is a direct pin write on q, under the graph lock and the target lock a held
// move's approval takes: the pin and the units as they now stand, the unit checked again, and with
// held writes on (queue) errHoldForApproval unless the move still only narrows that pin. from is the
// pin as it now stands.
func setComponentOwningUnitQ(ctx context.Context, q store.Querier, id uuid.UUID, unit *uuid.UUID, queue bool) (from *uuid.UUID, err error) {
	if err = store.LockGovernanceGraph(ctx, q); err != nil {
		return nil, err
	}
	if err = store.LockGovernanceTarget(ctx, q, govKindComponentOwningUnit, id.String()); err != nil {
		return nil, err
	}
	if from, err = store.ComponentOwningUnitQ(ctx, q, id); err != nil {
		return nil, err
	}
	units, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return from, err
	}
	if e := componentUnitRefusal(units, unit); e != nil {
		return from, e
	}
	if queue && !owningUnitNarrows(units, from, unit) {
		return from, errHoldForApproval
	}
	return from, store.SetComponentOwningUnitQ(ctx, q, id, unit)
}

// applyComponentOwningUnitChange applies a held pin move inside the decision transaction: the target
// lock, the pin as it now stands compared with what the proposal reviewed, the unit checked again,
// then the write on q.
func applyComponentOwningUnitChange(_ *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var held componentOwningUnitChange
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindComponentOwningUnit, held.ComponentID.String()); err != nil {
		return govApplied{}, err
	}
	cur, err := store.ComponentOwningUnitQ(ctx, q, held.ComponentID)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(owningUnitState(cur, false)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	units, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	if e := componentUnitRefusal(units, held.UnitID); e != nil {
		return govApplied{}, e
	}
	if err := store.SetComponentOwningUnitQ(ctx, q, held.ComponentID, held.UnitID); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: componentOwningUnitAction, target: held.ComponentID.String(),
		data: map[string]any{"from": cur, "to": held.UnitID}}, nil
}

// componentPinnedOut is the name of the first organisation component in comps (a run's snapshot)
// whose unit pin does not cover the run's unit, or "" when none does or the run has no unit. A
// component since deleted is skipped: componentGoneRefusal answers it, and its grants say nothing any
// more. A read error is returned, never read as "no pin".
func (s *Server) componentPinnedOut(ctx context.Context, run types.AgentRun, comps []types.RunComponent) (string, error) {
	cs, ok := s.cfg.Store.(store.ComponentStore)
	if run.UnitID == nil || !ok {
		return "", nil
	}
	var byID map[uuid.UUID]types.GovernanceUnit
	for _, c := range comps {
		if c.SelfDefined || c.ComponentID == nil {
			continue
		}
		row, err := cs.GetComponent(ctx, *c.ComponentID, "")
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read the run's component: %w", err)
		}
		if row.OwningUnitID == nil {
			continue
		}
		if byID == nil {
			units, err := s.unitOutlines(ctx)
			if err != nil {
				return "", err
			}
			byID = unitsByID(units)
		}
		if !onPath(byID, *run.UnitID, *row.OwningUnitID) {
			return row.Name, nil
		}
	}
	return "", nil
}
