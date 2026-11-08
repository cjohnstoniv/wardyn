// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Unit deciders: PUT /governance/units/{id}/deciders replaces the rules that say who besides the
// security tier may decide each kind of hold for runs launched in the unit (types/deciders.go). The
// rules are decision power, so they are written like a unit-admin grant: only for a unit strictly
// below one the caller governs, always held when a unit admin writes them, and bound by a "deciders"
// lock above the unit. A security admin's write follows WARDYN_GOVERNANCE_SECOND_HUMAN. This file is
// the write, its held change and its apply, and the rules and their sources in GET /governance.
package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// govKindUnitDeciders is the change kind of a held deciders write.
const govKindUnitDeciders = types.GovernanceTargetUnitDeciders

// unitDecidersRequest is PUT /governance/units/{id}/deciders's body. No rules clears the unit's own.
type unitDecidersRequest struct {
	Rules []types.DeciderRule `json:"rules"`
}

// unitDecidersPayload is a held write: the unit and the rules, validated.
type unitDecidersPayload struct {
	UnitID uuid.UUID           `json:"unit_id"`
	Rules  []types.DeciderRule `json:"rules"`
}

// unitDecidersDiffView is a unit's rules as an approver reads them.
type unitDecidersDiffView struct {
	UnitID   uuid.UUID           `json:"unit_id"`
	UnitName string              `json:"unit_name"`
	Rules    []types.DeciderRule `json:"rules"`
}

func (s *Server) unitDeciderStore() store.UnitDeciderStore {
	st, _ := s.cfg.Store.(store.UnitDeciderStore)
	return st
}

// handlePutUnitDeciders is PUT /governance/units/{id}/deciders: 200 with the stored rules, or 202 with
// the held change.
func (s *Server) handlePutUnitDeciders(w http.ResponseWriter, r *http.Request) {
	unit, ok := parseIDParam(w, r, "id", "governance unit")
	if !ok {
		return
	}
	var req unitDecidersRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	rules, err := types.ValidateDeciderRules(req.Rules)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidDeciders, "invalid deciders: "+err.Error())
		return
	}
	if !s.requireUnitsInScope(w, r, "governance.unit_deciders", true, unit) {
		return
	}
	mode, ok := s.unitWriteMode(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if mode == govQueue {
		s.holdUnitDeciders(w, r, unitDecidersPayload{UnitID: unit, Rules: rules})
		return
	}
	var saved types.UnitDeciders
	err = s.writeGovernance(ctx, func(q store.Querier) (err error) {
		if err := lockDecidersKey(ctx, q, unit); err != nil {
			return err
		}
		row, all, err := s.deciderWriteCheck(ctx, q, unit)
		if err != nil {
			return err
		}
		if err := s.deciderBoundRefusal(ctx, q, row, all, rules); err != nil {
			return err
		}
		saved, err = store.SetUnitDecidersQ(ctx, q, unit, rules, principalFromRequest(r))
		return err
	})
	if s.writeDecidersError(w, r, err) {
		return
	}
	data := unitDecidersAuditData(unit, rules, principalFromRequest(r), "")
	data["unit_scope"] = s.auditUnitScope(ctx, unit, true)
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), "governance.deciders.set",
		unit.String(), "success", mustJSON(data)))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindUnitDeciders, unit.String(), "success", nil)
	}
	writeJSON(w, http.StatusOK, saved)
}

// writeDecidersError answers a failed write and reports true; false for nil.
func (s *Server) writeDecidersError(w http.ResponseWriter, r *http.Request, err error) bool {
	var refusal *unitWriteError
	switch {
	case err == nil:
		return false
	case errors.As(err, &refusal):
		writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonGovernanceUnitNotFoundByID, "governance unit not found")
	default:
		writeServerError(w, r, "write unit deciders", err)
	}
	return true
}

// lockDecidersKey serializes the direct write to one unit's rules with the approval of a held change to
// them. The graph lock comes first, as a grant's does: a lock and a re-parent change what binds the unit.
func lockDecidersKey(ctx context.Context, q store.Querier, unit uuid.UUID) error {
	if err := store.LockGovernanceGraph(ctx, q); err != nil {
		return err
	}
	return store.LockGovernanceTarget(ctx, q, govKindUnitDeciders, unit.String())
}

// unitDecidersState is a unit's rules as a proposal sees them: the rules (nil when the unit has none).
func unitDecidersState(ctx context.Context, q store.Querier, unit uuid.UUID, forUpdate bool) (any, *types.UnitDeciders, error) {
	cur, err := store.GetUnitDecidersQ(ctx, q, unit, forUpdate)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return map[string]any{"rules": nil}, nil, nil
	case err != nil:
		return nil, nil, err
	}
	return map[string]any{"rules": cur.Rules}, &cur, nil
}

// deciderLockOver is the nearest lock on the deciders knob over a unit placed under parent: each unit
// from parent up, then the Wardyn boundary in hard_limits. A unit's own lock does not bind its own rules.
func deciderLockOver(all []types.GovernanceUnit, parent *uuid.UUID, b types.Boundary) (knobLock, bool) {
	byID := unitsByID(all)
	seen := map[uuid.UUID]bool{}
	for cur := parent; cur != nil && !seen[*cur] && len(seen) <= store.MaxUnitDepthCap; {
		row, ok := byID[*cur]
		if !ok {
			break
		}
		seen[*cur] = true
		if slices.Contains(row.LockedKnobs, types.DeciderLockKnob) {
			return knobLock{locker: row.Name, contact: nonZeroContact(row.Contact)}, true
		}
		cur = row.ParentID
	}
	if b.HardLimits() && slices.Contains(b.LockedKnobs, types.DeciderLockKnob) {
		return knobLock{locker: boundaryLocker, contact: nonZeroContact(b.Contact)}, true
	}
	return knobLock{}, false
}

// deciderWriteCheck reads the unit on q and refuses a write to its rules: 404 for a unit that is gone,
// 409 unit_knob_locked when a lock above it names the deciders knob. It binds every writer, a security
// admin and the admin token included. It returns the unit's row and every unit it read.
func (s *Server) deciderWriteCheck(ctx context.Context, q store.Querier, unit uuid.UUID) (types.GovernanceUnit, []types.GovernanceUnit, error) {
	all, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return types.GovernanceUnit{}, nil, err
	}
	row, ok := unitsByID(all)[unit]
	if !ok {
		return types.GovernanceUnit{}, nil, writeRefusal(http.StatusNotFound, reasonGovernanceUnitNotFoundByID, "governance unit not found")
	}
	b, err := s.boundaryQ(ctx, q)
	if err != nil {
		return types.GovernanceUnit{}, nil, err
	}
	if l, locked := deciderLockOver(all, row.ParentID, b); locked {
		return types.GovernanceUnit{}, nil, lockedRefusal(types.DeciderLockKnob, l)
	}
	return row, all, nil
}

// checkDeciderLockWrite is err, or, when err is nil, the refusal for a unit write that would leave rules
// under a deciders lock: it newly locks the knob while a unit below states rules, or it moves the unit
// under a parent that locks it while the unit or one below it states rules. Clearing their values would
// widen them and freezing them under the lock would strand them, so the write names them and waits for
// them to be cleared (the same 409 a knob lock over a differing value gives). q is the write's
// transaction, nil for a security caller's.
func (s *Server) checkDeciderLockWrite(ctx context.Context, q store.Querier, err error, p types.GovernanceUnit, all []types.GovernanceUnit) error {
	cur, exists := unitsByID(all)[p.ID]
	newlyLocks := slices.Contains(p.LockedKnobs, types.DeciderLockKnob) && !slices.Contains(cur.LockedKnobs, types.DeciderLockKnob)
	moved := exists && !sameJSON(cur.ParentID, p.ParentID)
	if err != nil || (!newlyLocks && !moved) {
		return err
	}
	rows, err := s.listUnitDeciders(ctx, q)
	if err != nil {
		return err
	}
	hasRules := func(id uuid.UUID) bool {
		return slices.ContainsFunc(rows, func(x types.UnitDeciders) bool { return x.UnitID == id && len(x.Rules) > 0 })
	}
	proposed := append(slices.DeleteFunc(slices.Clone(all), func(x types.GovernanceUnit) bool { return x.ID == p.ID }), p)
	below := descendantsOf(proposed, p.ID)
	var differ []string
	if newlyLocks {
		for _, d := range below {
			if hasRules(d.ID) {
				differ = append(differ, d.Name)
			}
		}
		if len(differ) > 0 {
			slices.Sort(differ)
			return writeRefusal(http.StatusConflict, reasonUnitKnobLocked,
				"%s cannot be locked: %s set rules; clear them first", types.DeciderLockKnob, strings.Join(differ, ", "))
		}
	}
	if !moved {
		return nil
	}
	// The moved unit and the units below it come under the locks over their new place; a lock they
	// were already under is not new. The boundary's lock is the same on both sides and not compared.
	for _, d := range append([]types.GovernanceUnit{p}, below...) {
		_, was := deciderLockOver(all, unitsByID(all)[d.ID].ParentID, types.Boundary{})
		if _, now := deciderLockOver(proposed, d.ParentID, types.Boundary{}); now && !was && hasRules(d.ID) {
			differ = append(differ, d.Name)
		}
	}
	if len(differ) == 0 {
		return nil
	}
	slices.Sort(differ)
	return writeRefusal(http.StatusConflict, reasonUnitKnobLocked,
		"%s is locked above the new place: %s set rules; clear them first", types.DeciderLockKnob, strings.Join(differ, ", "))
}

// listUnitDeciders is every unit's rules on q, or on the store when q is nil; none for a store that keeps none.
func (s *Server) listUnitDeciders(ctx context.Context, q store.Querier) ([]types.UnitDeciders, error) {
	switch {
	case q != nil:
		return store.ListUnitDecidersQ(ctx, q)
	case s.unitDeciderStore() != nil:
		return s.unitDeciderStore().ListUnitDeciders(ctx)
	}
	return nil, nil
}

// boundaryDeciderLockRefusal is the 409 for a boundary write that newly locks the deciders knob while a
// unit states rules: the boundary's lock binds every unit, top level included, so their rules would
// freeze. Only a boundary in hard_limits binds. q is the write's transaction, nil outside one.
func (s *Server) boundaryDeciderLockRefusal(ctx context.Context, q store.Querier, body types.BoundaryWrite, cur types.Boundary, units []types.GovernanceUnit) error {
	if body.LockedKnobs == nil {
		return nil
	}
	after := types.Boundary{Mode: body.Mode, LockedKnobs: *body.LockedKnobs}
	if !after.HardLimits() || !slices.Contains(after.LockedKnobs, types.DeciderLockKnob) ||
		(cur.HardLimits() && slices.Contains(cur.LockedKnobs, types.DeciderLockKnob)) {
		return nil
	}
	rows, err := s.listUnitDeciders(ctx, q)
	if err != nil {
		return err
	}
	var differ []string
	for _, u := range units {
		if slices.ContainsFunc(rows, func(x types.UnitDeciders) bool { return x.UnitID == u.ID && len(x.Rules) > 0 }) {
			differ = append(differ, u.Name)
		}
	}
	if len(differ) == 0 {
		return nil
	}
	slices.Sort(differ)
	return writeRefusal(http.StatusConflict, reasonUnitKnobLocked,
		"%s cannot be locked by the boundary: %s set rules; clear them first", types.DeciderLockKnob, strings.Join(differ, ", "))
}

// holdUnitDeciders stores a write as a pending change.
func (s *Server) holdUnitDeciders(w http.ResponseWriter, r *http.Request, p unitDecidersPayload) {
	ctx := r.Context()
	var base any
	var cur *types.UnitDeciders
	var row types.GovernanceUnit
	err := s.readGovernanceState(r, func(q store.Querier) (err error) {
		var all []types.GovernanceUnit
		if row, all, err = s.deciderWriteCheck(ctx, q, p.UnitID); err != nil {
			return err
		}
		if err = s.deciderBoundRefusal(ctx, q, row, all, p.Rules); err != nil {
			return err
		}
		base, cur, err = unitDecidersState(ctx, q, p.UnitID, false)
		return err
	})
	if s.writeDecidersError(w, r, err) {
		return
	}
	before := &unitDecidersDiffView{UnitID: p.UnitID, UnitName: row.Name, Rules: []types.DeciderRule{}}
	if cur != nil {
		before.Rules = cur.Rules
	}
	after := &unitDecidersDiffView{UnitID: p.UnitID, UnitName: row.Name, Rules: p.Rules}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindUnitDeciders, op: "set", key: p.UnitID.String(), payload: p, baseState: base,
		before: before, after: after, changed: changedPaths(before, after),
	})
}

// applyUnitDecidersChange applies a held write inside the decision transaction: the key lock, the lock
// over the unit as it stands now, the rules as they now stand compared with what the proposal reviewed,
// then the write.
func applyUnitDecidersChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var p unitDecidersPayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonInvalidDeciders, "the held change does not decode: %v", err)
	}
	rules, err := types.ValidateDeciderRules(p.Rules)
	if err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonInvalidDeciders, "invalid deciders: %v", err)
	}
	if err := lockDecidersKey(ctx, q, p.UnitID); err != nil {
		return govApplied{}, err
	}
	base, _, err := unitDecidersState(ctx, q, p.UnitID, true)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(base) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	row, all, err := s.deciderWriteCheck(ctx, q, p.UnitID)
	if err != nil {
		return govApplied{}, err
	}
	if err := s.deciderBoundRefusal(ctx, q, row, all, rules); err != nil {
		return govApplied{}, err
	}
	if _, err := store.SetUnitDecidersQ(ctx, q, p.UnitID, rules, ch.ProposedBy); err != nil {
		return govApplied{}, err
	}
	_, approver := actorFromRequest(r)
	return govApplied{action: "governance.deciders.set", target: p.UnitID.String(),
		data: unitDecidersAuditData(p.UnitID, rules, ch.ProposedBy, approver)}, nil
}

// unitDecidersAuditData is a write's row data: the unit, the rules now in force, who proposed it and who
// approved it ("" when it applied directly).
func unitDecidersAuditData(unit uuid.UUID, rules []types.DeciderRule, actor, approver string) map[string]any {
	return map[string]any{"unit_id": unit, "rules": rules, "actor": actor, "approver": approver}
}

// governanceDecidersView is one unit's entry in GET /governance's deciders: its own rules; per kind, the
// nearest unit with a rule (from), every unit on the chain with one, nearest first (bounded_by), and the
// rule those leave once met (effective; an empty who means the security tier alone).
type governanceDecidersView struct {
	UnitID    uuid.UUID                    `json:"unit_id"`
	Rules     []types.DeciderRule          `json:"rules"`
	From      map[string]uuid.UUID         `json:"from,omitempty"`
	BoundedBy map[string][]uuid.UUID       `json:"bounded_by,omitempty"`
	Effective map[string]types.DeciderRule `json:"effective,omitempty"`
}

// governanceDeciders is the deciders of every unit that has rules or inherits some, computed here so the
// console keeps no second copy of the walk or of the meet. A store that keeps none lists none.
func (s *Server) governanceDeciders(ctx context.Context, units []unitResolution) ([]governanceDecidersView, error) {
	st := s.unitDeciderStore()
	if st == nil {
		return nil, nil
	}
	rows, err := st.ListUnitDeciders(ctx)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	flat := make([]types.GovernanceUnit, len(units))
	for i, u := range units {
		flat[i] = u.Row
	}
	t := newDeciderTree(rows, flat)
	var out []governanceDecidersView
	for _, u := range flat {
		from, bounded, effective := t.boundedBy(u.ID)
		if len(t.rules[u.ID]) == 0 && len(from) == 0 {
			continue
		}
		v := governanceDecidersView{UnitID: u.ID, Rules: t.rules[u.ID], From: from, BoundedBy: bounded, Effective: effective}
		if v.Rules == nil {
			v.Rules = []types.DeciderRule{}
		}
		out = append(out, v)
	}
	return out, nil
}

// scopeDecidersRead keeps the entries of the units a unit admin's read lists.
func scopeDecidersRead(all []governanceDecidersView, units []governanceUnitView) []governanceDecidersView {
	var out []governanceDecidersView
	for _, d := range all {
		if slices.ContainsFunc(units, func(u governanceUnitView) bool { return u.ID == d.UnitID }) {
			out = append(out, d)
		}
	}
	return out
}
