// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A portal can be pinned to one governance unit at registration (run_in_under). A run launched through
// it is allowed only when the context the run resolved to is that unit or one below it. The check reads
// the PICKED context, so a person whose default pick is another platform's unit is refused as well as
// one who names it. The person's restrictions are applied after the pick and still clamp what the pin
// admits. The default is unpinned, which checks nothing: a portal registered without run_in_under
// behaves exactly as it did.

const (
	portalUnitScopeMsg       = "This portal starts runs only in %s and the units below it. Choose a context there."
	portalUnitUnavailableMsg = "This portal's unit could not be checked right now. Try again shortly."
	portalUnitInvalidMsg     = "The run_in_under field must name an existing governance unit, by id or exact name."
)

// stepPortalUnit refuses a delegated launch whose resolved context is outside its portal's pinned unit.
func (s *Server) stepPortalUnit(f *runFold, rec *foldRecorder) bool {
	return !s.portalUnitRefusal(f.r.Context(), f.ceiling, f.promoted).write(s, f.w, f.r)
}

// portalUnitRefusal is nil for a launch that did not come through a pinned portal. A queued run's
// start replays the fold on the unit its create froze, which this check already admitted, so it asks
// nothing then. A portal row or unit list that cannot be read refuses with 503; it never reads as
// unpinned.
func (s *Server) portalUnitRefusal(ctx context.Context, ceiling governanceCeiling, promoted bool) *runRefusal {
	via, ok := audit.DelegationFrom(ctx)
	if !ok || promoted {
		return nil
	}
	dr, ok := s.cfg.Store.(store.DelegateReader)
	if !ok {
		return runError(http.StatusServiceUnavailable, reasonPortalUnitUnavailable, portalUnitUnavailableMsg)
	}
	d, err := dr.GetDelegateByID(ctx, via.Delegate)
	if err != nil {
		slog.ErrorContext(ctx, "portal unit: read the portal", slog.String("delegate", via.Delegate.String()), slog.Any("err", err))
		return runError(http.StatusServiceUnavailable, reasonPortalUnitUnavailable, portalUnitUnavailableMsg)
	}
	if d.RunInUnder == nil {
		return nil
	}
	units, err := s.unitOutlines(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "portal unit: read the units", slog.Any("err", err))
		return runError(http.StatusServiceUnavailable, reasonPortalUnitUnavailable, portalUnitUnavailableMsg)
	}
	byID := unitsByID(units)
	picked := store.DefaultUnitID
	if ceiling.Unit != nil {
		picked = ceiling.Unit.ID
	}
	if onPath(byID, picked, *d.RunInUnder) {
		return nil
	}
	name := d.RunInUnder.String()
	if u, ok := byID[*d.RunInUnder]; ok {
		name = u.Name
	}
	return runError(http.StatusForbidden, reasonPortalUnitScope, fmt.Sprintf(portalUnitScopeMsg, name))
}

// resolvePortalUnit is the unit a registration's run_in_under names, by id or exact name: nil for none,
// else a unit that exists. It writes the refusal and returns false when the unit is unknown or the
// units cannot be read.
func (s *Server) resolvePortalUnit(w http.ResponseWriter, r *http.Request, raw string) (*uuid.UUID, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	units, err := s.unitOutlines(r.Context())
	if err != nil {
		writeServerError(w, r, "register delegate: read the units", err)
		return nil, false
	}
	id, parseErr := uuid.Parse(raw)
	for _, u := range units {
		if (parseErr == nil && u.ID == id) || (parseErr != nil && u.Name == raw) {
			return &u.ID, true
		}
	}
	writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegateRunInUnderInvalid, portalUnitInvalidMsg)
	return nil, false
}
