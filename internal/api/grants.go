// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
)

// handleListGrants returns the credential-grant eligibility records for a run as
// a JSON array. Eligibility is NOT issuance: these are the grants the run may
// request (some never minted), so the endpoint surfaces records the UI would
// otherwise lose when it synthesizes grants from credential.mint audit events.
//
// Auth/error conventions mirror GET /runs/{id}: an invalid id is 400, an unknown
// run is 404 (the run must exist first), and a store error is 500.
func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	// Confirm the run exists first so an unknown run behaves like GET /runs/{id}
	// (404) rather than silently returning an empty array.
	if _, err := s.cfg.Store.GetRun(ctx, id); err != nil {
		if notFoundIf(w, err, "run") {
			return
		}
		writeError(w, http.StatusInternalServerError, "get run: "+err.Error())
		return
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list grants: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, grants)
}
