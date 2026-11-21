// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An issuer portal's pin is checked on every request it makes, not only when a run starts. The people of two
// portals on one issuer are the same people, so what separates the portals is the unit each is pinned to:
// each reaches only the runs whose unit is under its own pin. A console portal keeps owner-wide management.
//
// The pin is read from the current unit tree on every request (a unit moved or deleted since the run started
// changes the answer at the next request). Both the run's unit and the pin unit must exist in the tree read:
// onPath answers true at once for an id equal to the ancestor, so a deleted pin unit would otherwise read as
// "in the pin" for a run whose unit is that id.

// portalPin is the pin of the issuer portal one request came through. applies is false for a request that
// is not delegated, comes through a console portal or a portal with no pin.
type portalPin struct {
	applies bool
	unit    uuid.UUID
	name    string
	present bool
	byID    map[uuid.UUID]types.GovernanceUnit
}

// portalPinOf reads the pin of the portal ctx's request came through. The refusal is a 503 for a portal row
// or unit list that could not be read: it never reads as unpinned.
func (s *Server) portalPinOf(ctx context.Context) (portalPin, *runRefusal) {
	via, ok := audit.DelegationFrom(ctx)
	if !ok {
		return portalPin{}, nil
	}
	unavailable := runError(http.StatusServiceUnavailable, reasonPortalUnitUnavailable, portalUnitUnavailableMsg)
	dr, ok := s.cfg.Store.(store.DelegateReader)
	if !ok {
		return portalPin{}, unavailable
	}
	d, err := dr.GetDelegateByID(ctx, via.Delegate)
	if err != nil {
		slog.ErrorContext(ctx, "portal pin: read the portal", slog.String("delegate", via.Delegate.String()), slog.Any("err", err))
		return portalPin{}, unavailable
	}
	if d.Issuer == "" || d.RunInUnder == nil {
		return portalPin{}, nil
	}
	units, err := s.unitOutlines(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "portal pin: read the units", slog.Any("err", err))
		return portalPin{}, unavailable
	}
	p := portalPin{applies: true, unit: *d.RunInUnder, name: d.RunInUnder.String(), byID: unitsByID(units)}
	if u, found := p.byID[p.unit]; found {
		p.present, p.name = true, u.Name
	}
	return p, nil
}

// holds reports whether a run in unit runUnit (nil is the Default unit) is under the pin. A pin that does
// not apply holds everything; a deleted pin unit, or a run unit the tree no longer has, holds nothing.
func (p portalPin) holds(runUnit *uuid.UUID) bool {
	if !p.applies {
		return true
	}
	if !p.present {
		return false
	}
	id := store.DefaultUnitID
	if runUnit != nil {
		id = *runUnit
	}
	if _, found := p.byID[id]; !found {
		return false
	}
	return onPath(p.byID, id, p.unit)
}

// scope is the pin's unit set for a run list: the pin unit and every unit under it, in the tree as read.
// nil when the pin does not apply (an unscoped list); empty, matching no run, when the pin unit is gone.
func (p portalPin) scope() *store.RunUnitScope {
	if !p.applies {
		return nil
	}
	ids := []uuid.UUID{}
	if p.present {
		for id := range p.byID {
			if onPath(p.byID, id, p.unit) {
				ids = append(ids, id)
			}
		}
	}
	return &store.RunUnitScope{IDs: ids}
}

// refusal is the 403 for a run outside the pin: the create refusal's reason and sentence.
func (p portalPin) refusal() *runRefusal {
	return runError(http.StatusForbidden, reasonPortalUnitScope, fmt.Sprintf(portalUnitScopeMsg, p.name))
}

// portalRunInPin is nil when run is reachable through the issuer portal ctx's request came through: the
// request is not delegated, the portal is a console portal or has no pin, or the run's unit is under the
// pin. It is called only after the owner predicate has passed, so a missing run and another person's run
// keep today's byte-identical 404 and only a person's own run outside the pin answers 403.
func (s *Server) portalRunInPin(ctx context.Context, run types.AgentRun) *runRefusal {
	p, refusal := s.portalPinOf(ctx)
	if refusal != nil {
		return refusal
	}
	if p.holds(run.UnitID) {
		return nil
	}
	return p.refusal()
}
