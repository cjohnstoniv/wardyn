// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The RFC 8693 token exchange a registered portal calls (#1142): POST
// /api/v1/token, authenticated by the portal's own credential (HTTP Basic,
// client_id = the portal id, client_secret = its wdp_ credential), with the
// signed-in person's identity-provider token as subject_token. What comes back
// is an opaque delegated token (wdg_) that acts for that person for
// delegatedTokenTTL and cannot be refreshed or exchanged again.

// delegatedTokenTTL is the delegated token's whole life (owner decision Q4:
// ten minutes, no refresh). A portal that needs longer exchanges the person's
// live token again, which proves the person is still signed in.
const delegatedTokenTTL = 10 * time.Minute

// The RFC 8693 / RFC 6749 wire names this endpoint speaks.
const (
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccessToken   = "urn:ietf:params:oauth:token-type:access_token"
	tokenTypeIDToken       = "urn:ietf:params:oauth:token-type:id_token"
	tokenTypeJWT           = "urn:ietf:params:oauth:token-type:jwt"
)

// delegateActorPrefix names a portal as an audit actor ("delegate:<id>"), the
// shape deviceActor gives a device; isReservedPrincipal refuses it as a person.
const delegateActorPrefix = "delegate:"

// delegationAuthActor names the exchange's client-authentication boundary on
// its auth.fail rows, beside deviceAuthActor.
const delegationAuthActor = "wardyn/delegation"

func delegateActor(id uuid.UUID) string { return delegateActorPrefix + id.String() }

// maxTokenExchangeBody bounds the form body: a subject token is a JWT of a few
// kilobytes; nothing legitimate comes near this.
const maxTokenExchangeBody = 64 << 10

// oauthError is RFC 6749 §5.2's error body.
type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="wardyn-delegation"`)
	}
	writeJSON(w, status, oauthError{Error: code, Description: desc})
}

// handleTokenExchange is POST /api/v1/token. Every refusal fails closed and
// mints nothing. The order is the security shape: the portal proves itself
// first (401 invalid_client), then the request is checked for shape, then the
// person's own token is verified exactly as a sign-in would admit them
// (400 invalid_grant), then the person's session cutoff and the portal's group
// scope are applied (400 invalid_grant / 403 access_denied), and only then is
// a token minted. Every decision past client authentication writes a
// delegation.exchange row naming the portal.
func (s *Server) handleTokenExchange(w http.ResponseWriter, r *http.Request) {
	// The authority time, taken before the body is read — handleCreateAPIToken's
	// reason: created_at is compared to the person's session cutoff, and a
	// caller must not be able to stretch it past one.
	authorizedAt := s.cfg.Now().UTC()
	d, ds, ok := s.authenticateDelegate(w, r)
	if !ok {
		return
	}
	subjectToken, ok := tokenExchangeForm(w, r)
	if !ok {
		return
	}
	deny := func(status int, code, target, reason string) {
		s.recordAudit(r.Context(), s.auditEvent(nil, types.ActorSystem, delegateActor(d.ID), "delegation.exchange",
			target, "denied", mustJSON(map[string]any{"reason": reason})))
		writeOAuthError(w, status, code, "the subject token was refused")
	}
	if s.cfg.OIDC == nil {
		deny(http.StatusBadRequest, "invalid_grant", "", "sso_not_configured")
		return
	}
	if strings.HasPrefix(subjectToken, delegatedTokenPrefix) {
		deny(http.StatusBadRequest, "invalid_grant", "", "subject_token_delegated")
		return
	}
	sess, denied := s.cfg.OIDC.VerifySubjectToken(r, subjectToken, d.IdPClientID, s.isReservedPrincipal)
	if denied != "" {
		deny(http.StatusBadRequest, "invalid_grant", "", denied)
		return
	}
	if s.cfg.SessionRevocations != nil {
		revoked, err := s.cfg.SessionRevocations.IsSessionRevoked(r.Context(), sess.Sub, sess.Email, sess.IssuedAt)
		if err != nil {
			slog.ErrorContext(r.Context(), "api: session-revocation lookup failed; token exchange refused", "error", err)
			s.metrics.authStoreErrorInc()
			writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "session revocation lookup failed")
			return
		}
		if revoked {
			deny(http.StatusBadRequest, "invalid_grant", sess.Sub, "session_revoked")
			return
		}
	}
	// The portal's scope is one group (owner decision Q5), matched against the
	// snapshot the person's own token just produced — never anything the
	// portal says.
	if !slices.Contains(sess.Groups, d.Group) {
		s.recordAudit(r.Context(), s.auditEvent(nil, types.ActorSystem, delegateActor(d.ID), "delegation.exchange",
			sess.Sub, "denied", mustJSON(map[string]any{"reason": "outside_scope", "group": d.Group})))
		writeOAuthError(w, http.StatusForbidden, "access_denied", "this portal cannot act for this person")
		return
	}
	raw := newBearer(delegatedTokenPrefix)
	t, err := ds.MintDelegatedToken(r.Context(), types.DelegatedToken{
		ID: uuid.New(), DelegateID: d.ID, Principal: sess.Sub, Email: sess.Email, UserType: sess.UserType,
		Groups: sess.Groups, GroupsTruncated: sess.GroupsTruncated,
		CreatedAt: authorizedAt, ExpiresAt: authorizedAt.Add(delegatedTokenTTL),
	}, raw, authorizedAt)
	if err != nil {
		slog.ErrorContext(r.Context(), "api: mint delegated token failed", "error", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "mint delegated token failed")
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, types.ActorSystem, delegateActor(d.ID), "delegation.exchange",
		t.Principal, "success", mustJSON(map[string]any{
			"grant": t.ID, "expires_at": t.ExpiresAt, "user_type": t.UserType, "role": sess.Role,
		})))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":      raw,
		"issued_token_type": tokenTypeAccessToken,
		"token_type":        "Bearer",
		"expires_in":        int(delegatedTokenTTL / time.Second),
	})
}

// authenticateDelegate is the exchange's client authentication: HTTP Basic,
// the portal id as the user and its wdp_ credential as the password, resolved
// to a LIVE registered portal. Every failure is the same 401 invalid_client
// and an auth.fail row; a store failure is a 503.
func (s *Server) authenticateDelegate(w http.ResponseWriter, r *http.Request) (types.Delegate, store.DelegateStore, bool) {
	ds, ok := s.cfg.Store.(store.DelegateStore)
	if !ok {
		writeOAuthError(w, http.StatusNotImplemented, "server_error", "delegation requires the Postgres store backend")
		return types.Delegate{}, nil, false
	}
	refuse := func(reason string) (types.Delegate, store.DelegateStore, bool) {
		s.auditAuthFailedAs(r, delegationAuthActor, reason)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "portal authentication failed")
		return types.Delegate{}, nil, false
	}
	id, secret, ok := r.BasicAuth()
	if !ok || !strings.HasPrefix(secret, delegateCredentialPrefix) {
		return refuse("missing_client_credential")
	}
	d, err := ds.GetDelegateByRaw(r.Context(), secret)
	if errors.Is(err, store.ErrNotFound) {
		return refuse("invalid_client_credential")
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "api: portal credential lookup failed", "error", err)
		s.metrics.authStoreErrorInc()
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "portal lookup failed")
		return types.Delegate{}, nil, false
	}
	if id != d.ID.String() {
		return refuse("invalid_client_credential")
	}
	return d, ds, true
}

// tokenExchangeForm checks the exchange request's shape and returns its
// subject_token. Only the form BODY is read: a token in a query string ends up
// in access logs.
func tokenExchangeForm(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenExchangeBody)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body must be an application/x-www-form-urlencoded token request")
		return "", false
	}
	f := r.PostForm
	switch {
	case f.Get("grant_type") != grantTypeTokenExchange:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "only "+grantTypeTokenExchange+" is supported")
	case f.Has("actor_token"):
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "actor_token is not accepted: the authenticated portal is the actor")
	case f.Get("requested_token_type") != "" && f.Get("requested_token_type") != tokenTypeAccessToken:
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "only an access token can be issued")
	case f.Get("subject_token") == "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "subject_token is required")
	case !slices.Contains([]string{tokenTypeAccessToken, tokenTypeIDToken, tokenTypeJWT}, f.Get("subject_token_type")):
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "subject_token_type must be an access token, an ID token or a JWT")
	default:
		return f.Get("subject_token"), true
	}
	return "", false
}
