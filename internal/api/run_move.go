// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Moving a paused run to another machine: POST /runs/{id}/move and GET /runs/{id}/placements, for the run's owner
// or a super admin. Both answer 409 move_unavailable while moving a run is off (moveAvailable), which it is on
// every server until the release that can move runs, and a refused move writes one run.move denied row.

// errMoveUnavailable is startMove's answer while no move can be made.
var errMoveUnavailable = errors.New("api: moving a run is not available")

// registerRunMoveRoutes mounts the move's routes on the authenticated run router.
func (s *Server) registerRunMoveRoutes(r chi.Router) {
	r.Post("/runs/{id}/move", s.handleMoveRun)
	r.Get("/runs/{id}/placements", s.handleRunPlacements)
}

// handleMoveRun serves POST /runs/{id}/move: the owner, or a super admin, moves a paused run. Another caller gets
// the 404 a missing run gets, so the route is no existence oracle; then a move that is off answers
// move_unavailable before anything else is read.
func (s *Server) handleMoveRun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin)
	if !ok {
		return
	}
	if !s.moveOn(r.Context()) || s.startMove(r.Context(), run) != nil {
		s.refuseMoveUnavailable(w, r, run)
	}
}

// handleRunPlacements serves GET /runs/{id}/placements: a run's moves and placement history, for the owner or a
// super admin. It answers move_unavailable while moving a run is off.
func (s *Server) handleRunPlacements(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	if _, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin); !ok {
		return
	}
	writeErrorReason(w, http.StatusConflict, reasonMoveUnavailable, moveUnavailableMessage)
}

// moveOn reads site config for moveAvailable; a site config that cannot be read reads off.
func (s *Server) moveOn(ctx context.Context) bool {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	return err == nil && moveAvailable(sc)
}

// startMove is the move's only entry: handleMoveRun and the move sweep call it (TestMoveGateCallers). It answers
// errMoveUnavailable while the gate is closed, and in this build at all times: the move's executor is not part of
// it.
func (s *Server) startMove(_ context.Context, _ types.AgentRun) error {
	if !moveGateOpen() {
		return errMoveUnavailable
	}
	return errMoveUnavailable
}

const moveUnavailableMessage = "Moving a run is not available on this server: an admin turns it on with features.run_move.enabled in site config, once a release that can move runs is installed."

// refuseMoveUnavailable answers 409 move_unavailable and writes the refused move's run.move denied row.
func (s *Server) refuseMoveUnavailable(w http.ResponseWriter, r *http.Request, run types.AgentRun) {
	actorType, principal := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, principal, "run.move", run.ID.String(), "denied",
		mustJSON(map[string]any{"reason": reasonMoveUnavailable})))
	writeErrorReason(w, http.StatusConflict, reasonMoveUnavailable, moveUnavailableMessage)
}
