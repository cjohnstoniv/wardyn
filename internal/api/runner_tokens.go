// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// handleListRunnerTokens is GET /api/v1/runners/tokens?owner=&limit=&offset=: the registration tokens
// still redeemable, newest first, so a token minted by mistake can be found and revoked, and one
// person's can be told apart from the rest. Never a token value: only its hash is stored, and the wire
// type serializes neither. Not gated on runners being on, so a token left outstanding when runners
// were turned off stays visible.
func (s *Server) handleListRunnerTokens(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	owner := r.URL.Query().Get("owner")
	tokens, truncated, err := pagedItems(page, func(p store.Page) ([]types.RunnerRegistrationToken, error) {
		return rs.ListUnusedRunnerRegistrationTokensPage(r.Context(), s.cfg.Now().UTC(), owner, p)
	}, nil)
	if err != nil {
		writeServerError(w, r, "list runner registration tokens", err)
		return
	}
	if truncated {
		w.Header().Set("X-Wardyn-Truncated", "true")
	}
	if tokens == nil {
		tokens = []types.RunnerRegistrationToken{}
	}
	writeJSON(w, http.StatusOK, tokens)
}

// handleRevokeRunnerToken is DELETE /api/v1/runners/tokens/{id}: the token stops being redeemable at
// once. A token already redeemed, revoked or expired is 404 and writes no row; a runner it already
// registered is revoked through the runner routes instead. Like the list, not gated on runners being
// on: revoking is always available to the security tier.
func (s *Server) handleRevokeRunnerToken(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "registration token")
	if !ok {
		return
	}
	t, err := rs.RevokeRunnerRegistrationToken(r.Context(), id, s.cfg.Now().UTC())
	if notFoundIf(w, err, "registration token", reasonRunnerTokenNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "revoke runner registration token", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		placement.ActionRunnerTokenRevoke, t.ID.String(), "success", mustJSON(map[string]any{"owner": t.Owner, "minted_by": t.MintedBy, "expires_at": t.ExpiresAt})))
	w.WriteHeader(http.StatusNoContent)
}
