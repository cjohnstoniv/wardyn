// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// claimsCtxKey carries the verified run claims through the internal handlers.
type claimsCtxKey struct{}

// localPrincipalCtxKey carries the LOCAL HOST MODE operator principal placed by
// humanOrAdminAuth so principalFromRequest can attribute admin-gated actions to
// the local operator without an OIDC session or an X-Wardyn-Principal header.
type localPrincipalCtxKey struct{}

func withLocalPrincipal(ctx context.Context, p string) context.Context {
	return context.WithValue(ctx, localPrincipalCtxKey{}, p)
}

func localPrincipalFromContext(ctx context.Context) string {
	p, _ := ctx.Value(localPrincipalCtxKey{}).(string)
	return p
}

// oidcHumanCtxKey carries the VERIFIED OIDC session principal (IdP "sub") that
// humanOrAdminAuth resolved for a valid SSO session. actorFromRequest reads this
// instead of re-deriving it from the oidc package's private context key, so the
// auth middleware is the single place that trusts oidc, and human attribution is
// unit-testable without minting a real signed session cookie.
type oidcHumanCtxKey struct{}

func withOIDCHuman(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, oidcHumanCtxKey{}, sub)
}

func oidcHumanFromContext(ctx context.Context) string {
	p, _ := ctx.Value(oidcHumanCtxKey{}).(string)
	return p
}

// errorBody is the uniform JSON error envelope.
type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	// Encode after WriteHeader: an encode failure can only truncate the body,
	// never change the already-sent status. Errors here are unrecoverable.
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// bearerToken extracts a bearer token from the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// isLoopbackHost reports whether the request Host header names a loopback
// destination. FIX #8: it gates the LOCAL-MODE no-auth surface (REST + the attach
// WebSocket) against DNS rebinding. A page served from attacker.com
// (Origin==Host==attacker.com) whose DNS is rebound to 127.0.0.1 passes the
// browser's same-origin check but still sends Host: attacker.com — so restricting
// the no-auth surface to a loopback Host (127.0.0.0/8, ::1, or the literal name
// "localhost", with an optional :port) blocks the rebinding class. Anything else
// (public name, LAN IP, empty Host) is rejected.
//
// Residual (documented, not closed here): this blocks DNS rebinding but not
// DIRECT blind CSRF — a malicious page can fire a no-cors POST straight at
// http://127.0.0.1:<port>, which arrives with Host: 127.0.0.1 and passes this
// gate credential-free in local mode (responses stay opaque, but a state-changing
// POST still fires). Browser Private Network Access preflights mitigate it; a
// stricter option is to also reject a present non-loopback Origin header. Local
// mode is for a trusted single-dev machine — keep it loopback-published.
func isLoopbackHost(host string) bool {
	h := host
	// Strip an optional :port. SplitHostPort also removes IPv6 brackets; it errors
	// when there is no port, in which case h is already the bare host.
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	// A bracketed IPv6 literal with no port (e.g. "[::1]") keeps its brackets.
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	// ParseIP+IsLoopback covers 127.0.0.0/8 and ::1; a hostname like
	// "127.0.0.1.attacker.com" is NOT a valid IP, so it correctly fails.
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// humanOrAdminAuth authenticates the public API with EITHER a valid OIDC SSO
// session cookie OR the admin bearer token. The OIDC middleware runs first and,
// when a valid session is present, stashes the human principal on the context
// and short-circuits past the bearer check (the human is authenticated). When
// no session is present it falls through to adminAuth, so the shared admin token
// keeps working for the CLI. When OIDC is not configured this is exactly
// adminAuth. Fail closed: an absent/invalid session AND an absent/invalid token
// is rejected by adminAuth.
func (s *Server) humanOrAdminAuth(next http.Handler) http.Handler {
	// LOCAL HOST MODE: no SSO/token. Attribute every admin-gated action to the
	// local operator and skip auth entirely. This bypasses ONLY the public-API
	// human/admin gate — internalAuth (sidecar/run-token verification) is a
	// separate middleware and is unaffected, so the sidecar callback path still
	// authenticates. cmd/wardynd refuses LocalMode when bound to an EXPLICIT
	// public IP, but only WARNS (does not refuse) on an unspecified bind
	// (0.0.0.0, the WARDYN_LISTEN default) — operators must bind/publish
	// loopback-only for a real guarantee (the Compose default already
	// publishes 127.0.0.1).
	if s.cfg.LocalMode {
		op := s.cfg.LocalOperator
		if op == "" {
			op = "local:operator"
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// FIX #8 (DNS-rebinding defense): the no-auth local surface must answer
			// ONLY to a loopback Host. Without this a rebinding page
			// (Origin==Host==attacker.com, DNS rebound to 127.0.0.1) passes the
			// browser's same-origin check yet carries no credential, so it would
			// otherwise reach this auth-bypassed surface. Reject any non-loopback
			// Host with 403 BEFORE the bypass. Only LocalMode is gated here; SSO/token
			// modes already require a credential and are unaffected.
			if !isLoopbackHost(r.Host) {
				writeError(w, http.StatusForbidden, "local mode: request Host is not loopback (DNS-rebinding guard)")
				return
			}
			next.ServeHTTP(w, r.WithContext(withLocalPrincipal(r.Context(), op)))
		})
	}
	admin := s.adminAuth(next)
	if s.cfg.OIDC == nil {
		return admin
	}
	// oidc.Middleware sets the principal on the context for a valid session and
	// always calls its next. We branch inside: if the session principal is
	// present we are authenticated and skip the bearer check; otherwise we defer
	// to the admin bearer path.
	return s.cfg.OIDC.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sub := oidc.PrincipalFromContext(r.Context()); sub != "" {
			// Publish the verified human on an api-owned context key so
			// actorFromRequest attributes the action to the real SSO human
			// (and IGNORES any X-Wardyn-Principal header — a real identity won).
			next.ServeHTTP(w, r.WithContext(withOIDCHuman(r.Context(), sub)))
			return
		}
		admin.ServeHTTP(w, r)
	}))
}

// adminAuth gates the public API behind a constant-time bearer compare. An
// empty configured AdminToken denies everything (fail closed): the public API
// must not be unauthenticated. SSO/Dex replaces this in a later milestone.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			writeError(w, http.StatusUnauthorized, "admin token not configured; public API disabled")
			return
		}
		tok, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AdminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// internalAuth verifies a per-run token via identity.Provider.Verify with the
// "wardyn-internal" audience and stashes the resulting claims on the context.
// Any verification error fails closed with 401 (revoked/expired tokens too).
func (s *Server) internalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Identity == nil {
			writeError(w, http.StatusUnauthorized, "identity provider not configured")
			return
		}
		tok, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing run token")
			return
		}
		claims, err := s.cfg.Identity.Verify(r.Context(), tok, internalAudience)
		if err != nil {
			// Do not leak the verification reason (revoked vs expired vs forged).
			writeError(w, http.StatusUnauthorized, "invalid run token")
			return
		}
		ctx := contextWithClaims(r.Context(), claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// internalAuthGroundtruth gates the host-scoped eBPF ground-truth ingest
// endpoint. It parallels internalAuth but verifies the host-sensor token
// against the SEPARATE audience groundtruthAudience ("wardyn-groundtruth"),
// NOT internalAudience. This audience separation is the security boundary: a
// ground-truth token grants ONLY audit-write on /internal/groundtruth — it can
// never be presented to the mint/approval endpoints (those verify
// internalAudience and would reject it), so a compromised host sensor cannot
// mint credentials or decide approvals. Any verification error fails closed
// with 401 (revoked/expired/wrong-audience all collapse to "invalid").
//
// Unlike internalAuth, we do NOT stash run claims on the context: the sensor is
// host-scoped, not per-run, so there is no run identity to bind. The handler
// validates body-supplied run ids against agent_runs instead (see
// handleGroundtruthEvents). The token's only job here is to prove the caller is
// the trusted host sensor.
func (s *Server) internalAuthGroundtruth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Identity == nil {
			writeError(w, http.StatusUnauthorized, "identity provider not configured")
			return
		}
		tok, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing sensor token")
			return
		}
		if _, err := s.cfg.Identity.Verify(r.Context(), tok, groundtruthAudience); err != nil {
			// Do not leak the verification reason (revoked vs expired vs
			// wrong-audience vs forged).
			writeError(w, http.StatusUnauthorized, "invalid sensor token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func contextWithClaims(ctx context.Context, c *identity.Claims) context.Context {
	return context.WithValue(ctx, claimsCtxKey{}, c)
}

// claimsFromContext returns the verified run claims placed by internalAuth.
func claimsFromContext(r *http.Request) (*identity.Claims, error) {
	c, ok := r.Context().Value(claimsCtxKey{}).(*identity.Claims)
	if !ok || c == nil {
		return nil, errors.New("api: missing run claims on context")
	}
	return c, nil
}
