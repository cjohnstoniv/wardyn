// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Delegated run management (#1142): a registered portal acts for the person
// signed in to it. The portal exchanges that person's own live identity-
// provider token for a ten-minute delegated token (delegation_exchange.go);
// this file is the lane that token authenticates on.
//
// The invariant is "no impersonation; delegation is recorded as delegation".
// The lane publishes the PERSON through withHumanIdentity — so ownership,
// secrets, drives and the governance ceiling all resolve on them with no
// per-feature awareness — clamped to the user role, and marks the context as
// delegated, which does three things no handler has to remember:
//
//   - delegationAllowed confines the token to a fixed list of routes; every
//     other route, including any added later, answers 403 delegation_scope;
//   - isOperator and isSecurityOperator refuse a delegated context outright;
//   - recordAudit (and audit.DelegationRecorder, for every other writer on
//     the shared chain) stamps data.via = {delegate, grant} on every row the
//     request writes, so the portal is on the record beside the person.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// delegatedTokenPrefix marks a delegated bearer, delegateCredentialPrefix a
// portal's own credential. Routing and leak-scanning hints like
// apiTokenPrefix; the 256 bits after them are the boundary.
const (
	delegatedTokenPrefix     = "wdg_"
	delegateCredentialPrefix = "wdp_"
)

// delegationAllowed is the whole of what a delegated token may reach, keyed as
// chi reports a matched route: "METHOD pattern". Anything not here answers 403
// delegation_scope — a route added to the router later is refused until
// someone adds it to this list on purpose.
//
// The attach-ticket route is here under both its names (issue #658's alias is
// the same handler); revive, approvals, secrets, tokens, SSH keys and every
// admin route are deliberately absent.
var delegationAllowed = map[string]bool{
	"POST /api/v1/runs":                    true,
	"POST /api/v1/runs/preflight":          true,
	"GET /api/v1/runs":                     true,
	"GET /api/v1/runs/{id}":                true,
	"PATCH /api/v1/runs/{id}":              true,
	"POST /api/v1/runs/{id}/kill":          true,
	"POST /api/v1/runs/{id}/attach-ticket": true,
	"POST /api/v1/runs/{id}/attach/ticket": true,
	"GET /api/v1/me":                       true,
}

// delegationRefusal is the 403 body a delegated token gets on any other route.
const delegationRefusal = "a delegated token cannot reach this action — the person must do it themselves"

// delegatedTokenAuth is the delegated lane, mounted by humanOrAdminAuth in
// front of apiTokenAuth. A bearer without delegatedTokenPrefix goes straight
// to fallback. An unknown, expired or cut-off token, or one whose portal was
// revoked, also falls through to fallback, whose adminAuth answers the single
// 401 every unrecognised bearer gets (apiTokenAuth's no-oracle rule); a store
// failure is a 503, never a 401.
//
// On success the person is published at the USER role whatever their own
// role is: a portal never carries admin reach (owner decision Q1). Then the
// route must be on delegationAllowed.
func (s *Server) delegatedTokenAuth(next, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		ds, isDS := s.cfg.Store.(store.DelegateStore)
		if !ok || !strings.HasPrefix(tok, delegatedTokenPrefix) || !isDS {
			fallback.ServeHTTP(w, r)
			return
		}
		t, err := ds.GetDelegatedTokenByRaw(r.Context(), tok, s.cfg.Now().UTC())
		if errors.Is(err, store.ErrNotFound) {
			fallback.ServeHTTP(w, r)
			return
		}
		if err != nil {
			slog.ErrorContext(r.Context(), "api: delegated-token lookup failed; this request could not be authenticated",
				"error", err, "path", r.URL.Path)
			s.metrics.authStoreErrorInc()
			writeError(w, http.StatusServiceUnavailable, "delegated token lookup failed")
			return
		}
		// The person's own cutoff (POST /sessions/revoke), against the token's
		// created_at — apiTokenAuth's rule, and its fail-closed 503.
		if s.cfg.SessionRevocations != nil {
			revoked, rerr := s.cfg.SessionRevocations.IsSessionRevoked(r.Context(), t.Principal, t.Email, t.CreatedAt)
			if rerr != nil {
				slog.ErrorContext(r.Context(), "api: session-revocation lookup failed; this delegated token could not be authenticated",
					"error", rerr, "path", r.URL.Path)
				s.metrics.authStoreErrorInc()
				writeError(w, http.StatusServiceUnavailable, "delegated token lookup failed")
				return
			}
			if revoked {
				fallback.ServeHTTP(w, r)
				return
			}
		}
		if t.Principal == "" || s.isReservedPrincipal(t.Principal) {
			fallback.ServeHTTP(w, r)
			return
		}
		ctx := withHumanIdentity(r.Context(), t.Principal, t.Email, oidc.RoleUser, t.UserType, t.Groups, t.GroupsTruncated)
		r = r.WithContext(audit.WithDelegation(ctx, types.DelegationVia{Delegate: t.DelegateID, Grant: t.ID}))
		if rctx := chi.RouteContext(r.Context()); rctx == nil || !delegationAllowed[r.Method+" "+rctx.RoutePattern()] {
			d := authz.Deny(authz.ReasonDelegationScope, r.URL.Path, "")
			writeErrorReason(w, http.StatusForbidden, string(authz.ReasonDelegationScope), delegationRefusal)
			s.recordRefusal(r.Context(), r, d)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// createdVia is the run row's created_via: the portal a delegated request
// came through, nil otherwise (owner decision Q8).
func createdVia(ctx context.Context) *uuid.UUID {
	if via, ok := audit.DelegationFrom(ctx); ok {
		return &via.Delegate
	}
	return nil
}

// neverOperator reports a caller neither admin tier may answer true for,
// whatever its context holds: a device, or a portal acting for a person (a
// delegated person is published at the user role anyway; this holds even if
// a later edit publishes one without a human). isOperator and
// isSecurityOperator ask it before their no-human arm.
func neverOperator(ctx context.Context) bool {
	_, isDevice := deviceFromContext(ctx)
	_, delegated := audit.DelegationFrom(ctx)
	return isDevice || delegated
}
