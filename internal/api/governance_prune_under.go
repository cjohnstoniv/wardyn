// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// A scoped prune (`wardyn governance set --under <unit> --prune`) plans its deletes and clears from one
// read of the subtree, then sends each by id. Another admin can move a unit out of the subtree between
// that read and a write, and the write would then reach outside it. So each prune write names the
// subtree's root as ?under=<unit id>, and the four prune routes (a unit delete, a membership delete, a
// deciders write and a grant revoke) refuse it, under the graph lock their write takes, when the target
// unit is no longer at or below that unit. A held write is judged when it is proposed, and its base then
// covers the unit's chain, so it is stale when it is approved after a move. A request without under is
// unchanged.

const notUnderMsg = "This unit is no longer under the unit named in the under parameter, so nothing was written. Read governance again and retry."

// errNotUnder is the 409 for a prune write whose unit is no longer under the unit it named.
var errNotUnder = writeRefusal(http.StatusConflict, reasonGovernanceUnitNotUnder, "%s", notUnderMsg)

// pruneUnder is the request's ?under= unit, nil when absent; a malformed id is answered 400 (ok=false).
func pruneUnder(w http.ResponseWriter, r *http.Request) (*uuid.UUID, bool) {
	raw := r.URL.Query().Get("under")
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidIDParam, "invalid under unit id")
		return nil, false
	}
	return &id, true
}

// chainReaches reports whether chain, a unit and the units above it, holds under. An empty chain (the unit
// is gone) reaches it: a missing target is the write's own 404.
func chainReaches(chain []types.GovernanceUnit, under uuid.UUID) bool {
	return len(chain) == 0 || slices.ContainsFunc(chain, func(u types.GovernanceUnit) bool { return u.ID == under })
}

// requireUnderQ refuses a prune write to unit with errNotUnder when unit, read on q, is no longer at or below
// under. nil under refuses nothing. q holds the graph lock wherever the write is applied there.
func (s *Server) requireUnderQ(ctx context.Context, q store.Querier, under *uuid.UUID, unit uuid.UUID) error {
	if under == nil {
		return nil
	}
	chain, err := unitChainQ(ctx, q, unit, false)
	if err != nil {
		return err
	}
	return s.requireUnderChainQ(ctx, q, under, chain)
}

// requireUnderChainQ is requireUnderQ on a unit chain already read on q. A unit admin's under must be a unit
// they govern, so the answer tells them nothing about the tree above their units.
func (s *Server) requireUnderChainQ(ctx context.Context, q store.Querier, under *uuid.UUID, chain []types.GovernanceUnit) error {
	if under == nil || len(chain) == 0 {
		return nil
	}
	if !chainReaches(chain, *under) {
		return errNotUnder
	}
	in, err := s.unitScopeQ(ctx, q, *under, false)
	if err != nil {
		return err
	}
	if !in {
		return errNotUnder
	}
	return nil
}

// underChainQ is unit's chain for a held deciders write or revoke naming under, read on q under the graph
// lock: errNotUnder when the unit is no longer at or below under (requireUnderChainQ). nil when under is nil.
func (s *Server) underChainQ(ctx context.Context, q store.Querier, under *uuid.UUID, unit uuid.UUID) ([]types.GovernanceUnit, error) {
	if under == nil {
		return nil, nil
	}
	if err := store.LockGovernanceGraph(ctx, q); err != nil {
		return nil, err
	}
	chain, err := unitChainQ(ctx, q, unit, false)
	if err != nil {
		return nil, err
	}
	return chain, s.requireUnderChainQ(ctx, q, under, chain)
}

// underBase is base, a held deciders write's or revoke's own state, with the unit chain its under was
// checked on; base itself when chain is empty (no under was named). The change's base hash then covers
// the chain, as a held unit or membership delete's does, so a move of the unit or of a unit above it
// makes the change stale. The payload gains no field, so a replica that predates under still decodes it:
// that replica computes base alone and answers stale, where a new field would leave the change pending.
func underBase(base any, chain []types.GovernanceUnit) any {
	if len(chain) == 0 {
		return base
	}
	return map[string]any{"base": base, "unit_chain": chain}
}

// heldBaseStale is the decision's base check of a held deciders write or revoke: nil when ch was proposed
// on base as it stands, alone (no under) or with unit's chain as it stands (underBase), read on the
// decision's transaction under the graph lock; store.ErrGovernanceChangeStale otherwise.
func heldBaseStale(ctx context.Context, q store.Querier, ch types.GovernanceChange, base any, unit uuid.UUID) error {
	if computeETag(base) == ch.BaseHash {
		return nil
	}
	chain, err := unitChainQ(ctx, q, unit, false)
	if err != nil {
		return err
	}
	if computeETag(underBase(base, chain)) != ch.BaseHash {
		return store.ErrGovernanceChangeStale
	}
	return nil
}

// answerNotUnder writes errNotUnder for an error carrying it and reports true; false for any other error.
func answerNotUnder(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, errNotUnder) {
		return false
	}
	writeErrorReason(w, errNotUnder.status, errNotUnder.reason, errNotUnder.msg)
	return true
}

// deleteUnitUnder is a security caller's direct unit delete with under: the check and the delete on one
// transaction under the graph lock. It returns the deleted unit's name.
func (s *Server) deleteUnitUnder(ctx context.Context, id, under uuid.UUID) (name string, err error) {
	err = s.writeGovernance(ctx, func(q store.Querier) error {
		if err := store.LockGovernanceGraph(ctx, q); err != nil {
			return err
		}
		if err := s.requireUnderQ(ctx, q, &under, id); err != nil {
			return err
		}
		row, err := store.GetGovernanceUnitQ(ctx, q, id, false)
		if err != nil {
			return err
		}
		name = row.Name
		return store.DeleteGovernanceUnitQ(ctx, q, id)
	})
	return name, err
}

// deleteMembershipUnder is a security caller's direct membership delete with under: the membership's unit
// checked and the row deleted on one transaction under the graph lock. It returns the deleted row.
func (s *Server) deleteMembershipUnder(ctx context.Context, id, under uuid.UUID) (removed types.GovernanceMembership, err error) {
	err = s.writeGovernance(ctx, func(q store.Querier) error {
		if err := store.LockGovernanceGraph(ctx, q); err != nil {
			return err
		}
		row, err := store.GetGovernanceMembershipQ(ctx, q, id, true)
		if err != nil {
			return err
		}
		if err := s.requireUnderQ(ctx, q, &under, row.UnitID); err != nil {
			return err
		}
		removed = row
		return store.DeleteGovernanceMembershipQ(ctx, q, id)
	})
	return removed, err
}
