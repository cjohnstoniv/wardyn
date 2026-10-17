// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Invitations (#1508): an admin invites a named person, and only that person, signed in, claims the
// invitation and so mints their own API token. The invitation holds no secret: the target's session
// is the only authority, so a stolen link reaches nothing. The admin sees metadata and never a token,
// and a claim shares one mint core with POST /me/tokens (mintGuards, prepareMint).
package api

import (
	"cmp"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	// invitationDefaultTTL is how long an invitation stays claimable unless the admin says otherwise.
	invitationDefaultTTL = 72 * time.Hour
	// invitationMaxTTL bounds an admin-chosen lifetime.
	invitationMaxTTL = 14 * 24 * time.Hour
	// invitationMaxPending bounds the pending invitations naming one target.
	invitationMaxPending = 10
	// servicePrincipalPrefix begins the principal of a service identity, which no invitation names.
	servicePrincipalPrefix = "service:"
)

// invitationNotFoundMsg is the one body a missing id, another person's invitation and an invitation
// that is no longer pending (on revoke) answer with, so none of them is an oracle for the others.
const invitationNotFoundMsg = "invitation not found"

// mountInvitationRoutes registers the invitation family: the admin half on securityOps, the
// signed-in person's half beside /me/tokens.
func (s *Server) mountInvitationRoutes(r chi.Router, securityOps chi.Router) {
	securityOps.With(s.refuseSubjectPrincipalParam).Post("/people/{principal}/invitations", s.handleCreateInvitation)
	securityOps.With(s.refuseSubjectPrincipalParam).Get("/people/{principal}/invitations", s.handleListPersonInvitations)
	securityOps.Get("/invitations", s.handleListInvitations)
	securityOps.Delete("/invitations/{id}", s.handleRevokeInvitation)
	r.Get("/me/invitations", s.handleListMyInvitations)
	r.Post("/me/invitations/{id}/claim", s.handleClaimInvitation)
}

func (s *Server) invitationStoreOr501(w http.ResponseWriter) (store.InvitationStore, bool) {
	is, ok := s.cfg.Store.(store.InvitationStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonPeopleStoreUnavailable, "invitations require the Postgres store backend")
	}
	return is, ok
}

// createInvitationRequest IS the SDK's body (dto_alias_test.go pins it).
type createInvitationRequest = client.CreateInvitationRequest

// handleCreateInvitation is POST /api/v1/people/{principal}/invitations, refusing in the order of the
// design's table. The target is exactly the path's principal: never inferred from an email, a role or the caller.
func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	is, ok := s.invitationStoreOr501(w)
	if !ok {
		return
	}
	var req createInvitationRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	target := principalParam(r)
	if !validSubject(target) || s.isReservedPrincipal(target) || strings.HasPrefix(strings.ToLower(target), servicePrincipalPrefix) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonInvitationTargetInvalid,
			"principal: the identity provider's subject exactly, 1-255 printable characters, not a reserved or service identity")
		return
	}
	known, err := s.principalKnown(r, target)
	if err != nil {
		writeServerError(w, r, "look up invitation target", err)
		return
	}
	if !known {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonInvitationTargetUnknown,
			"principal: no such person — create the person first with POST /people")
		return
	}
	now := s.cfg.Now().UTC()
	if req.ReplacesTokenID != nil {
		valid, err := s.replaceableToken(r, target, *req.ReplacesTokenID, now)
		if err != nil {
			writeServerError(w, r, "look up replaced token", err)
			return
		}
		if !valid {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonInvitationReplacesInvalid,
				"replaces_token_id: must be a live token an admin minted for this person")
			return
		}
	}
	expiresIn, ok := invitationLifetime(w, req)
	if !ok {
		return
	}
	tokenName, ok := apiTokenName(w, req.TokenName)
	if !ok {
		return
	}
	inv := types.Invitation{
		ID: uuid.New(), Target: target, CreatedBy: principalFromRequest(r), CreatedAt: now, ExpiresAt: now.Add(expiresIn),
		TokenName: tokenName, ReplacesTokenID: req.ReplacesTokenID,
	}
	if req.TokenTTLSeconds > 0 {
		inv.TokenTTLSeconds = &req.TokenTTLSeconds
	}
	created, err := is.CreateInvitation(ctx, inv, invitationMaxPending, now)
	if errors.Is(err, store.ErrInvitationCap) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonInvitationCapReached, "too many pending invitations for this person — revoke one first")
		return
	}
	if err != nil {
		writeServerError(w, r, "create invitation", err)
		return
	}
	created.ConsolePath = "/account?invitation=" + created.ID.String()
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"invitation.create", created.ID.String(), "success",
		mustJSON(map[string]any{"target": created.Target, "expires_at": created.ExpiresAt, "replaces_token_id": created.ReplacesTokenID})))
	writeJSON(w, http.StatusCreated, created)
}

// invitationLifetime is the invitation's lifetime from the request (zero asks for the default), or false after a 400.
func invitationLifetime(w http.ResponseWriter, req createInvitationRequest) (time.Duration, bool) {
	life := invitationDefaultTTL
	switch {
	case req.ExpiresInSeconds < 0 || req.ExpiresInSeconds > int64(invitationMaxTTL/time.Second):
	case req.TokenTTLSeconds < 0 || req.TokenTTLSeconds > apiTokenMaxTTLSeconds:
	default:
		if req.ExpiresInSeconds > 0 {
			life = time.Duration(req.ExpiresInSeconds) * time.Second
		}
		return life, true
	}
	writeErrorReason(w, http.StatusBadRequest, reasonInvitationTTLInvalid,
		"expires_in_seconds: between 1 second and 14 days; token_ttl_seconds: not negative")
	return 0, false
}

// principalKnown reports whether a person row or a bound identity has exactly this principal.
func (s *Server) principalKnown(r *http.Request, principal string) (bool, error) {
	ctx := r.Context()
	if ps, ok := s.cfg.Store.(store.PersonStore); ok {
		switch _, err := ps.GetPerson(ctx, principal); {
		case err == nil:
			return true, nil
		case !errors.Is(err, store.ErrNotFound):
			return false, err
		}
	}
	if ids, ok := s.cfg.Store.(store.PrincipalIdentityStore); ok {
		rows, err := ids.IdentitiesByPrincipal(ctx, principal)
		return len(rows) > 0, err
	}
	return false, nil
}

// replaceableToken reports whether id is a live token of principal that an admin minted for them.
func (s *Server) replaceableToken(r *http.Request, principal string, id uuid.UUID, now time.Time) (bool, error) {
	tokens, err := s.cfg.Store.ListAPITokensByPrincipal(r.Context(), principal)
	if err != nil {
		return false, err
	}
	for _, t := range tokens {
		if t.ID == id {
			return t.RevokedAt == nil && (t.ExpiresAt == nil || t.ExpiresAt.After(now)) && t.MintedBy != "" && t.MintedBy != t.Principal, nil
		}
	}
	return false, nil
}

// handleListInvitations is GET /api/v1/invitations?state=: every invitation, metadata only.
func (s *Server) handleListInvitations(w http.ResponseWriter, r *http.Request) {
	s.listInvitationsFor(w, r, "")
}

// handleListPersonInvitations is GET /api/v1/people/{principal}/invitations.
func (s *Server) handleListPersonInvitations(w http.ResponseWriter, r *http.Request) {
	s.listInvitationsFor(w, r, principalParam(r))
}

// handleListMyInvitations is GET /api/v1/me/invitations: the caller's own pending invitations.
func (s *Server) handleListMyInvitations(w http.ResponseWriter, r *http.Request) {
	sub := oidcHumanFromContext(r.Context())
	if sub == "" {
		writeErrorReason(w, http.StatusForbidden, reasonAPITokenNoHuman, apiTokenNoHumanRefusal)
		return
	}
	s.serveInvitations(w, r, sub, types.InvitationPending)
}

func (s *Server) listInvitationsFor(w http.ResponseWriter, r *http.Request, target string) {
	state := types.InvitationState(r.URL.Query().Get("state"))
	switch state {
	case "", types.InvitationPending, types.InvitationClaimed, types.InvitationExpired, types.InvitationRevoked:
		s.serveInvitations(w, r, target, state)
	default:
		writeErrorReason(w, http.StatusBadRequest, reasonInvitationListParamInvalid, "state: pending, claimed, expired or revoked")
	}
}

func (s *Server) serveInvitations(w http.ResponseWriter, r *http.Request, target string, state types.InvitationState) {
	is, ok := s.invitationStoreOr501(w)
	if !ok {
		return
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	now := s.cfg.Now().UTC()
	servePage(w, r, page, func(p store.Page) ([]types.Invitation, error) {
		return is.ListInvitations(r.Context(), target, state, now, p)
	}, nil)
}

// handleRevokeInvitation is DELETE /api/v1/invitations/{id}: revoke a pending invitation. One that is
// missing, claimed, expired or already revoked answers the same 404.
func (s *Server) handleRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	is, ok := s.invitationStoreOr501(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "invitation")
	if !ok {
		return
	}
	revoked, err := is.RevokeInvitation(r.Context(), id, principalFromRequest(r), s.cfg.Now().UTC())
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvitationNotPending) {
		writeErrorReason(w, http.StatusNotFound, reasonInvitationNotFound, invitationNotFoundMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "revoke invitation", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"invitation.revoke", revoked.ID.String(), "success", mustJSON(map[string]any{"target": revoked.Target})))
	w.WriteHeader(http.StatusNoContent)
}

// claimInvitationRequest IS the SDK's body (dto_alias_test.go pins it).
type claimInvitationRequest = client.ClaimInvitationRequest

// notPendingBody is the 409 an invitation's own target hears, with the state it is in.
type notPendingBody struct {
	errorBody
	State types.InvitationState `json:"state"`
}

// handleClaimInvitation is POST /api/v1/me/invitations/{id}/claim: the target, signed in, mints their
// own token. Every answer to anyone else is the 404 of a missing id.
func (s *Server) handleClaimInvitation(w http.ResponseWriter, r *http.Request) {
	// Stamped before the body is read, for the reason handleCreateAPIToken gives.
	authorizedAt := s.cfg.Now().UTC()
	var req claimInvitationRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	sub, ok := s.mintGuards(w, r, "me.invitations.claim")
	if !ok {
		return
	}
	is, ok := s.invitationStoreOr501(w)
	if !ok {
		return
	}
	ctx := r.Context()
	id, perr := uuid.Parse(chi.URLParam(r, "id"))
	var inv types.Invitation
	err := perr
	if perr == nil {
		inv, err = is.GetInvitation(ctx, id, sub)
	}
	if errors.Is(err, store.ErrNotFound) || perr != nil {
		writeErrorReason(w, http.StatusNotFound, reasonInvitationNotFound, invitationNotFoundMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "read invitation", err)
		return
	}
	if state := inv.StateAt(authorizedAt); state != types.InvitationPending {
		s.refuseNotPending(w, r, inv.ID, state)
		return
	}
	name, ok := apiTokenName(w, cmp.Or(req.Name, inv.TokenName))
	if !ok {
		return
	}
	requested := req.TTLSeconds
	if b := inv.TokenTTLSeconds; b != nil && (requested == 0 || requested > *b) {
		requested = *b
	}
	lifetime, clampedFrom, ok := s.apiTokenLifetime(w, requested)
	if !ok {
		return
	}
	row, ok := s.prepareMint(w, r, sub, authorizedAt, name, lifetime)
	if !ok {
		return
	}
	row.ID = uuid.New()
	plaintext := newBearer(apiTokenPrefix)
	claimed, err := is.ClaimInvitation(guardOwner(ctx, sub), store.InvitationClaim{ID: inv.ID, Target: sub, Now: authorizedAt, Token: row, Raw: plaintext})
	if errors.Is(err, store.ErrInvitationNotPending) {
		s.refuseNotPending(w, r, inv.ID, s.invitationState(r, is, inv.ID, sub))
		return
	}
	if err != nil {
		writeServerError(w, r, "claim invitation", err)
		return
	}
	claimed.Token.Token = plaintext
	s.auditClaim(r, claimed, clampedFrom)
	writeJSON(w, http.StatusCreated, claimed.Token)
}

// refuseNotPending answers the target's own 409 and records the denied claim.
func (s *Server) refuseNotPending(w http.ResponseWriter, r *http.Request, id uuid.UUID, state types.InvitationState) {
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"invitation.claim", id.String(), "denied", mustJSON(map[string]any{"reason": reasonInvitationNotPending, "state": state})))
	writeJSON(w, http.StatusConflict, notPendingBody{
		errorBody: errorBody{Error: "this invitation is " + string(state), Reason: reasonInvitationNotPending}, State: state,
	})
}

// invitationState re-reads the target's invitation after a lost race; claimed is the only state a race can leave.
func (s *Server) invitationState(r *http.Request, is store.InvitationStore, id uuid.UUID, target string) types.InvitationState {
	inv, err := is.GetInvitation(r.Context(), id, target)
	if err != nil {
		return types.InvitationClaimed
	}
	return inv.StateAt(s.cfg.Now().UTC())
}

// auditClaim writes the rows of a committed claim: the token, the claim, and the token it retired.
func (s *Server) auditClaim(r *http.Request, c store.InvitationClaimed, clampedFrom time.Duration) {
	ctx, actor, actorType := r.Context(), principalFromRequest(r), actorTypeFromRequest(r)
	s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "token.create", c.Token.ID.String(), "success",
		mustJSON(mintAuditDetail(c.Token, clampedFrom, &c.Invitation.ID))))
	s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "invitation.claim", c.Invitation.ID.String(), "success",
		mustJSON(map[string]any{"token_id": c.Token.ID})))
	if c.Retired != nil {
		s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "token.revoke", c.Retired.ID.String(), "success",
			mustJSON(map[string]any{"principal": c.Retired.Principal, "name": c.Retired.Name, "minted_by": c.Retired.MintedBy, "reason": "replaced_by_owner"})))
	}
}
