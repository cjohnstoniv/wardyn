// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// moveTokenRequest IS the SDK's body (dto_alias_test.go pins it).
type moveTokenRequest = client.MoveTokenRequest

// handleMoveAPIToken is POST /api/v1/tokens/{id}/move: an admin links a live admin-minted token to the
// service identity that replaces it (#1508). securityOps (routes.go), the tier of the revoke it precedes.
// The move names the identity and never guesses one, and it never revokes: the old token keeps working
// until DELETE /tokens/{id}, which stays the admin's explicit step once the job uses the new one.
func (s *Server) handleMoveAPIToken(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "api token")
	if !ok {
		return
	}
	ms, ok := s.cfg.Store.(store.TokenMoveStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonPeopleStoreUnavailable, "moving a token requires the Postgres store backend")
		return
	}
	var body moveTokenRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	ctx := r.Context()
	move, err := ms.MoveAPIToken(ctx, id, body.ServiceIdentity, principalFromRequest(r), s.cfg.Now().UTC())
	switch {
	case notFoundIf(w, err, "api token", reasonAPITokenNotFoundEntity):
		return
	case errors.Is(err, store.ErrTokenNotMovable):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonTokenMoveInvalid, "the token is not a live token an admin minted for someone else")
		return
	case errors.Is(err, store.ErrMoveTargetUnknown), errors.Is(err, store.ErrServiceIdentityInactive):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonTokenMoveInvalid, "service_identity: name an existing service identity that is not retired")
		return
	case err != nil:
		writeServerError(w, r, "move api token", err)
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"token.update", id.String(), "success", mustJSON(map[string]any{
			"disposition": types.TokenLinkedToService, "service_identity": move.ServiceIdentity})))
	writeJSON(w, http.StatusOK, types.TokenMoved{TokenMove: move, Disposition: types.TokenLinkedToService})
}

// tokenDisposition is where an admin-minted token stands, from the facts about it. The owner is the token's
// Principal, never its minter: an invitation counts only when it targets the person the token acts as.
// A move row decides a live token before a pending invitation, because it is the admin's explicit choice.
func tokenDisposition(t types.APIToken, f store.TokenDispositionFacts) types.TokenDisposition {
	_, moved := f.Moves[t.ID]
	if t.RevokedAt == nil {
		if moved {
			return types.TokenLinkedToService
		}
		for _, r := range f.Replacements[t.ID] {
			if r.ClaimedAt == nil && r.Target == t.Principal {
				return types.TokenAwaitingOwner
			}
		}
		return types.TokenNeedsDecision
	}
	// The claim revokes the replaced token in its own transaction at the claim's instant, so an equal time
	// is what separates "revoked by the claim" from "revoked earlier by an admin".
	for _, r := range f.Replacements[t.ID] {
		if r.ClaimedAt != nil && r.Target == t.Principal && r.ClaimedAt.Equal(*t.RevokedAt) {
			return types.TokenReplacedByOwner
		}
	}
	if moved {
		return types.TokenMovedToService
	}
	return types.TokenRevoked
}

// addTokenDispositions sets Disposition on every admin-minted row of tokens. A store without the facts
// leaves them empty.
func (s *Server) addTokenDispositions(ctx context.Context, tokens []types.APIToken) error {
	ms, ok := s.cfg.Store.(store.TokenMoveStore)
	if !ok {
		return nil
	}
	var f store.TokenDispositionFacts
	for i := range tokens {
		t := &tokens[i]
		if t.MintedBy == "" || t.MintedBy == t.Principal {
			continue
		}
		if f.Moves == nil { // first admin-minted row: read the facts once
			var err error
			if f, err = ms.TokenDispositionFacts(ctx, s.cfg.Now().UTC()); err != nil {
				return err
			}
		}
		t.Disposition = tokenDisposition(*t, f)
	}
	return nil
}
