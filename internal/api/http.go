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
	"net/url"
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

// oidcEmailCtxKey carries the email claim of the same verified OIDC session,
// published by humanOrAdminAuth next to the principal. requireOperator reads it
// (never oidc's own context key) for the same reason oidcHumanCtxKey exists: the
// auth middleware stays the single place that trusts the oidc package, and the
// role gate is unit-testable without minting a signed session cookie.
type oidcEmailCtxKey struct{}

func withOIDCEmail(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, oidcEmailCtxKey{}, email)
}

func oidcEmailFromContext(ctx context.Context) string {
	e, _ := ctx.Value(oidcEmailCtxKey{}).(string)
	return e
}

// oidcRoleCtxKey carries the Wardyn role (oidc.RoleAdmin / oidc.RoleMember) B1
// derived for the same verified OIDC session, published by humanOrAdminAuth next
// to the principal/email for the same reason those two keys exist: isOperator
// reads this (never oidc's own context key) so the auth middleware stays the
// single place that trusts the oidc package, and stays unit-testable without
// minting a signed session cookie.
type oidcRoleCtxKey struct{}

func withOIDCRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, oidcRoleCtxKey{}, role)
}

func oidcRoleFromContext(ctx context.Context) string {
	r, _ := ctx.Value(oidcRoleCtxKey{}).(string)
	return r
}

// oidcGroupsCtxKey carries the login-time group snapshot of the same verified
// OIDC session (oidc.Session.Groups), published by humanOrAdminAuth next to the
// principal/email/role and for the same reason those keys exist: the auth
// middleware stays the single place that trusts the oidc package. It is what a
// `group`-subject capability grant matches against — see capabilities.go.
//
// NIL AND EMPTY ARE DIFFERENT (see oidc.GroupsFromContext). Empty: the IdP sent
// no usable group identity, and group grants genuinely do not apply. Nil:
// either there is no SSO session at all, or the human holds a PRE-0.6 cookie
// that predates the field. A caller that already knows a session is present
// must read nil as "snapshot unavailable, re-login required" and surface it as
// groups_snapshot_stale — never as "this human has no groups", which would
// silently withhold every group grant they hold and be unexplainable from the
// admin side.
type oidcGroupsCtxKey struct{}

func withOIDCGroups(ctx context.Context, groups []string) context.Context {
	return context.WithValue(ctx, oidcGroupsCtxKey{}, groups)
}

func oidcGroupsFromContext(ctx context.Context) []string {
	g, _ := ctx.Value(oidcGroupsCtxKey{}).([]string)
	return g
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

// isMutatingMethod reports whether m is a state-changing HTTP method.
func isMutatingMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// isLoopbackOrigin reports whether an Origin header value points at a loopback
// host. A malformed or opaque origin (e.g. "null") is treated as NON-loopback so
// a mutating request carrying it is rejected (fail closed).
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Host)
}

// isLoopbackRemoteAddr reports whether the request's TCP peer (r.RemoteAddr, set
// by the server from the accepted connection) is a loopback address. Unlike the
// Host header, a LAN/remote client CANNOT spoof this — it is the load-bearing gate
// for the LOCAL-MODE no-auth surface against a DIRECT network client: a peer that
// hits <host-ip>:<port> with a forged "Host: 127.0.0.1" passes isLoopbackHost but
// arrives from a non-loopback peer. This does NOT replace isLoopbackHost: browser
// DNS-rebinding runs in the victim's own browser (loopback peer, attacker Host),
// which the Host check catches. We deliberately do NOT install middleware.RealIP
// (see server.go), so r.RemoteAddr is the real socket peer, never an X-Forwarded-For.
// A malformed/empty RemoteAddr fails closed (treated as non-loopback).
func isLoopbackRemoteAddr(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
// Direct blind CSRF is ALSO closed: on a mutating method the local-mode gate
// rejects a PRESENT non-loopback Origin header (see the handler below), so a
// malicious page's no-cors POST straight at http://127.0.0.1:<port> — which carries
// the attacker's Origin — is refused even though its Host is 127.0.0.1. CLI/API
// clients send no Origin; the embedded UI is same-origin (loopback). Local mode is
// still for a trusted single-dev machine — keep it loopback-published.
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
			// N1 (direct-network guard): the no-auth local surface must answer ONLY to
			// a loopback TCP PEER. The Host header (checked next) is forgeable by a
			// direct socket client, so a LAN peer hitting <host-ip>:<port> with a
			// forged "Host: 127.0.0.1" would otherwise reach this auth-bypassed
			// surface even when wardynd is bound to 0.0.0.0 (the WARDYN_LISTEN
			// default). RemoteAddr is the accepted-connection peer and cannot be
			// spoofed remotely. Sidecar callbacks use internalAuth (a separate
			// middleware) and never reach here, so they are unaffected.
			// LocalTrustForwarder relaxes this for the compose topology only, where the
			// peer is always the docker gateway and LAN protection is the loopback
			// PUBLISH (see the Config field doc). The Host gate below still applies.
			if !s.cfg.LocalTrustForwarder && !isLoopbackRemoteAddr(r.RemoteAddr) {
				writeError(w, http.StatusForbidden, "local mode: request peer is not loopback (bind wardynd to 127.0.0.1, set WARDYN_LOCAL_TRUST_FORWARDER when behind a loopback-only publish, or configure auth)")
				return
			}
			// FIX #8 (DNS-rebinding defense): the no-auth local surface must answer
			// ONLY to a loopback Host. Without this a rebinding page
			// (Origin==Host==attacker.com, DNS rebound to 127.0.0.1) passes the
			// browser's same-origin check yet carries no credential, so it would
			// otherwise reach this auth-bypassed surface. The rebinding request runs
			// in the victim's own browser (loopback peer, so the RemoteAddr gate above
			// passes) — this Host gate is what stops it. Reject any non-loopback Host
			// with 403. Only LocalMode is gated here; SSO/token modes already require a
			// credential and are unaffected.
			if !isLoopbackHost(r.Host) {
				writeError(w, http.StatusForbidden, "local mode: request Host is not loopback (DNS-rebinding guard)")
				return
			}
			// Blind-CSRF guard: a malicious page can fire a no-cors
			// state-changing POST straight at http://127.0.0.1:<port>. It arrives with
			// Host: 127.0.0.1 (passing the loopback gate above) yet carries the
			// attacker's Origin. CLI/API clients send NO Origin; the embedded UI is
			// served from wardynd itself, so its Origin is loopback. On a mutating
			// method, reject a PRESENT non-loopback Origin — closing the direct
			// blind-CSRF the DNS-rebinding guard alone leaves open.
			if isMutatingMethod(r.Method) {
				if origin := r.Header.Get("Origin"); origin != "" && !isLoopbackOrigin(origin) {
					writeError(w, http.StatusForbidden, "local mode: cross-origin state-changing request rejected (CSRF guard)")
					return
				}
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
			// The session email rides along for /me and audit attribution; the
			// session role rides along for isOperator (B1's derived admin/member
			// role is now the sole source of the admin tier — see isOperator).
			ctx := withOIDCHuman(r.Context(), sub)
			ctx = withOIDCEmail(ctx, oidc.EmailFromContext(r.Context()))
			ctx = withOIDCRole(ctx, oidc.RoleFromContext(r.Context()))
			// The group snapshot rides along for the capability resolver. Copied
			// verbatim, nil included: nil is the pre-0.6-cookie signal, not an
			// empty set (see oidcGroupsCtxKey).
			ctx = withOIDCGroups(ctx, oidc.GroupsFromContext(r.Context()))
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		admin.ServeHTTP(w, r)
	}))
}

// requireOperator is the ADMIN authorization tier layered on top of
// humanOrAdminAuth (which only AUTHENTICATES). It is nested inside that group on
// the admin half of the role split — member = read + launch/own runs; admin =
// configure the deployment (managed harness credential, policies, workspaces,
// site-config), write/delete SECRETS, DECIDE any approval, and mint an attach
// ticket for any run — and refuses a signed-in MEMBER with 403 (see routes in
// Server.Handler). Read routes are untouched, and so is launching a run: a
// member uses the product, it just cannot configure it, hold credential
// material, or reach another user's run (see getRunAuthorized elsewhere in this
// package for the owner-or-admin tier that sits BETWEEN member and admin).
//
// The name predates B1's Session.Role and is kept (rather than renamed to
// requireAdmin) as the smallest honest diff — every existing call site and
// comment already reads "operator" to mean "admin", and the two are now exactly
// the same tier.
//
// isOperator reads the caller's ROLE (B1's oidc.RoleAdmin / oidc.RoleMember,
// derived at OIDC login by internal/auth/oidc's deriveRole and carried on the
// session cookie) — never the OperatorEmails list directly. Config.OperatorEmails
// (WARDYN_OIDC_OPERATOR_EMAILS) still matters: cmd/wardynd feeds the SAME list
// into oidc.Config.LegacyAdminEmails, so an email on it is still an ADDITIONAL
// RoleAdmin match at derivation time (see deriveRole) — the mechanism is not
// deleted, it now flows through Session.Role like every other role signal
// instead of being re-checked here a second time. With WARDYN_OIDC_ROLE_MAP
// unset, deriveRole derives the role from this operator allowlist ALONE (an
// email on it => RoleAdmin, everyone else => RoleMember); only with NEITHER a
// role map nor an allowlist does every signed-in human default to RoleAdmin
// (true pre-0.5) — see oidc.deriveRole.
//
// Admin-token and local-mode callers are ALWAYS admins. Both are a single
// shared credential with no per-human identity to key a role off — the token IS
// the admin — which is the documented ceiling of this gate, not an oversight.
func (s *Server) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isOperator(r.Context()) {
			// Do not name the allowlist/role-map's members — the caller learns
			// only that they are not an admin.
			writeError(w, http.StatusForbidden, "requires admin role")
			// authz.denied: a member denied a reachable admin surface. Low-noise
			// by design (see the audit doc in runs_create.go's denyMemberCustomImage) —
			// this is the ONE universal chokepoint every admin-gated route funnels
			// through (incl. the attach WS's ticketOrHumanAuth fallback lane), so
			// one audit call here covers all of them.
			s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
				"authz.denied", r.URL.Path, "denied", mustJSON(map[string]any{"reason": "admin_surface", "method": r.Method})))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isOperator reports whether the authenticated caller on ctx may perform
// admin-only actions. See requireOperator for the rules. A caller with no
// verified OIDC human on ctx (admin token, local mode, or OIDC not configured)
// is always an admin — a single shared credential carries no per-human role to
// demote. Otherwise the caller's role is authoritative and admin-only when it
// is exactly oidc.RoleAdmin (fail closed on any other value, including an
// unexpectedly empty one — B1's decodeSession already refuses to hand out a
// session with an empty role, so this is defense-in-depth, not a real path).
func (s *Server) isOperator(ctx context.Context) bool {
	if oidcHumanFromContext(ctx) == "" {
		return true // admin token, local mode, or OIDC not configured: no session role to demote
	}
	return oidcRoleFromContext(ctx) == oidc.RoleAdmin
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
