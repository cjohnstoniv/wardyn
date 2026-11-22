// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An issuer portal's people are known only through that portal, so no one adds them to a unit by hand. The pin
// the portal was registered with is their place to run: through that portal, the pin unit and every unit under
// it are their contexts for ordinary placement, at the user role. The contexts are built per request from the
// portal row and the unit tree as they stand, and are never stored.
//
// They count for placement only. A strict workspace or an organisation component with a unit pin is a protected
// resource's own condition, met only by a membership the person really holds, so a request carrying one is
// judged on actual memberships alone. The same holds when a run is extended, resumed or revived: the pin stands
// in for a membership only for a run with no such resource.

// errPortalUnitUnavailable: the portal row or the unit tree could not be read while placing an issuer portal's person.
var errPortalUnitUnavailable = errors.New("api: the portal's unit could not be read")

// portalPinsPerson is whether d is a live issuer portal with a pin and principal is one of its own people.
func portalPinsPerson(d types.Delegate, principal string) bool {
	if d.Issuer == "" || d.IssuerKey == "" || d.RevokedAt != nil || d.RunInUnder == nil {
		return false
	}
	prefix := types.PortalPersonPrefix + d.IssuerKey + ":"
	p := types.CanonicalUserSubject(principal)
	return strings.HasPrefix(p, prefix) && len(p) > len(prefix)
}

// stepsToAncestor counts the parent steps from id up to ancestor, which onPath has already found on its path.
func stepsToAncestor(byID map[uuid.UUID]types.GovernanceUnit, id, ancestor uuid.UUID) int {
	cur, n := &id, 0
	for ; cur != nil && *cur != ancestor && n <= store.MaxUnitDepthCap; n++ {
		cur = byID[*cur].ParentID
	}
	return n
}

// portalPinHoldsOwner is ownerMembershipRefusal's stand-in for a membership: true when the run was started by
// an issuer portal for its own person, that portal is still registered, its pin is on path (the run's unit
// and the units above it), and the run carries no strict workspace and no organisation component with a unit
// pin. Such a resource needs a membership the owner really holds, so for its run the pin never stands in. A
// resource that can no longer be read as ordinary (deleted since) counts as protected.
func (s *Server) portalPinHoldsOwner(ctx context.Context, run types.AgentRun, path map[uuid.UUID]bool) (bool, error) {
	if run.CreatedVia == nil || !types.IsPortalPersonPrincipal(run.CreatedBy) {
		return false, nil
	}
	dr, ok := s.cfg.Store.(store.DelegateReader)
	if !ok {
		return false, nil
	}
	d, err := dr.GetDelegateByID(ctx, *run.CreatedVia)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the portal that started the run: %w", err)
	}
	if !portalPinsPerson(d, run.CreatedBy) || !path[*d.RunInUnder] {
		return false, nil
	}
	protected, err := s.runHasStrictResource(ctx, run)
	if err != nil {
		return false, err
	}
	return !protected, nil
}

// runHasStrictResource is whether the run's stored attachments include a workspace whose pin is now strict or an
// organisation component now pinned to a unit, read as they stand. One that is gone counts as protected: what
// it was cannot be shown to be ordinary.
func (s *Server) runHasStrictResource(ctx context.Context, run types.AgentRun) (bool, error) {
	ids := run.WorkspaceIDs
	if run.WorkspaceID != nil {
		ids = append(append([]uuid.UUID(nil), ids...), *run.WorkspaceID)
	}
	for _, id := range ids {
		ws, err := s.cfg.Store.GetWorkspace(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("read the run's workspace: %w", err)
		}
		if ws.OwningUnitID != nil && ws.OwningUnitStrict {
			return true, nil
		}
	}
	comps, err := s.runComponentSnapshot(ctx, run.ID)
	if err != nil {
		return false, err
	}
	cs, _ := s.cfg.Store.(store.ComponentStore)
	for _, c := range comps {
		if c.SelfDefined {
			continue
		}
		if c.ComponentID == nil || cs == nil {
			return true, nil
		}
		row, err := cs.GetComponent(ctx, *c.ComponentID, "")
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("read the run's component: %w", err)
		}
		if row.OwningUnitID != nil {
			return true, nil
		}
	}
	return false, nil
}
