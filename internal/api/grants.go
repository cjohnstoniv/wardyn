// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// handleListGrants returns the credential-grant eligibility records for a run as
// a JSON array, paginated by ?limit=&offset= (see parseListPage). Eligibility is
// NOT issuance: these are the grants the run may request (some never minted),
// so the endpoint surfaces records the UI would otherwise lose when it
// synthesizes grants from credential.mint audit events.
//
// Auth/error conventions mirror GET /runs/{id}: an invalid id is 400, an unknown
// run is 404 (the run must exist first), and a store error is 500.
func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	// Confirm the run exists AND is owned (or the caller is an admin) first, so
	// an unknown OR foreign run behaves like GET /runs/{id} (404) rather than
	// silently returning an empty array — see getRunAuthorized.
	if _, ok := s.getRunAuthorized(w, r, id); !ok {
		return
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	// GrantsByRunPager, not the plain Pager: the query is already scoped
	// WHERE run_id=$1 (ListGrantsByRun), so an absent implementation falls
	// back safely to the full fetch + in-Go window (servePage's allFn) rather
	// than needing a fail-closed guard the way RunsByCreatorPager does.
	var pageFn func(store.Page) ([]types.CredentialGrant, error)
	if pg, ok := s.cfg.Store.(store.GrantsByRunPager); ok {
		pageFn = func(p store.Page) ([]types.CredentialGrant, error) { return pg.ListGrantsByRunPage(ctx, id, p) }
	}
	servePage(w, r, page, pageFn, func() ([]types.CredentialGrant, error) {
		return s.cfg.Store.ListGrantsByRun(ctx, id)
	})
}
