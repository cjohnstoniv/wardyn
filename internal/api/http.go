// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// claimsCtxKey carries the verified run claims through the internal handlers.
type claimsCtxKey struct{}

// localPrincipalCtxKey carries the local host mode operator principal placed by
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

// oidcNameCtxKey carries the display-name claim of the same verified OIDC
// session, for /me and nothing else: the console header shows it, no gate
// reads it, no log line carries it (the same hygiene as the email above).
// Published only by the SSO branch of humanOrAdminAuth — the api-token lane
// snapshots no name (api_tokens has no such column) and /me's consumer falls
// back to the email there.
type oidcNameCtxKey struct{}

func withOIDCName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, oidcNameCtxKey{}, name)
}

func oidcNameFromContext(ctx context.Context) string {
	n, _ := ctx.Value(oidcNameCtxKey{}).(string)
	return n
}

// oidcRoleCtxKey carries the Wardyn role (oidc.RoleAdmin / oidc.RoleUser)
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
// Nil and empty are different (see oidc.GroupsFromContext). Empty: the IdP sent
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

// oidcGroupsTruncatedCtxKey carries the truncation bit of that same
// snapshot: sessionGroups sorts the group union and drops the
// alphabetically-last entries once it hits the cookie byte cap, so a human in
// enough groups holds a snapshot that is present, non-nil, and INCOMPLETE.
//
// A truncated snapshot is exactly as unanswerable as a nil one and every group
// -scoped decision must treat it that way. It matters most at the governance
// ceiling: a member whose walling group fell off the cap would resolve to the
// DEPLOYMENT ceiling with no refusal and no audit line — the tier simply
// evaporates. It rides beside the snapshot rather than inside it because the
// snapshot's own type has no room for "and there were more".
//
// Published by BOTH auth branches through withHumanIdentity: the SSO branch
// from the cookie's own bit, the api-token branch from the column stamped at
// mint (a NULL there — a pre-0.7 token — counts as TRUE, fail-closed).
type oidcGroupsTruncatedCtxKey struct{}

func withOIDCGroupsTruncated(ctx context.Context, truncated bool) context.Context {
	return context.WithValue(ctx, oidcGroupsTruncatedCtxKey{}, truncated)
}

func oidcGroupsTruncatedFromContext(ctx context.Context) bool {
	t, _ := ctx.Value(oidcGroupsTruncatedCtxKey{}).(bool)
	return t
}

// oidcExpiryCtxKey carries the same verified OIDC session's expiry, published
// by humanOrAdminAuth next to the principal/email/role for the same reason
// those keys exist: the auth middleware stays the single place that trusts the
// oidc package, and /me (the session-expiry warning) is unit-testable
// without minting a signed session cookie. Zero when there is no SSO session.
type oidcExpiryCtxKey struct{}

func withOIDCExpiry(ctx context.Context, expiry time.Time) context.Context {
	return context.WithValue(ctx, oidcExpiryCtxKey{}, expiry)
}

func oidcExpiryFromContext(ctx context.Context) time.Time {
	t, _ := ctx.Value(oidcExpiryCtxKey{}).(time.Time)
	return t
}

// withHumanIdentity publishes the five keys that TOGETHER describe an
// authenticated human: who they are (sub), the email an admin may have written
// a grant against, the role isOperator gates on, the group snapshot the
// capability resolver matches, and whether that snapshot is COMPLETE. It exists
// so the SSO-session branch and the api-token branch of humanOrAdminAuth cannot
// DRIFT: a sixth identity key added to one path and forgotten on the other is
// exactly how a token would silently resolve to a different permission set than
// the session that minted it — and for a DENY grant, silently resolving to "no
// match" is a breach, not a degradation. Both branches call this and nothing
// else.
//
// groupsTruncated is a parameter rather than something derived from groups
// because it CANNOT be derived: a truncated snapshot and a complete one are
// both non-nil slices of plausible group names. Only the minting side knows,
// which is why it is stamped into the cookie and into the token row.
//
// Session EXPIRY is deliberately NOT here. It is a property of a cookie, not of
// an identity: an api token has no session to expire, so the key stays zero for
// one and is set by the SSO branch alone (see oidcExpiryCtxKey).
func withHumanIdentity(ctx context.Context, sub, email, role string, groups []string, groupsTruncated bool) context.Context {
	ctx = withOIDCHuman(ctx, sub)
	ctx = withOIDCEmail(ctx, email)
	ctx = withOIDCRole(ctx, role)
	ctx = withOIDCGroupsTruncated(ctx, groupsTruncated)
	return withOIDCGroups(ctx, groups)
}

// errorBody is the uniform JSON error envelope.
type errorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason,omitempty"` // a machine-readable class for the few refusals a console surface acts on; absent everywhere else
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
// destination. It gates the LOCAL-MODE no-auth surface (REST + the attach
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
	// One ceiling per request. This middleware wraps every routed public-API
	// call exactly once, which makes it the only place that can install the
	// per-request memo effectiveCeiling reads (governance.go). Installed for
	// EVERY caller, not just members: an operator short-circuits the resolve
	// anyway, so the memo costs a pointer and removes the case where two sites
	// in one request disagree about who the caller is bounded by.
	next = ceilingMemoMiddleware(next)
	// Local host mode: no SSO/token. Attribute every admin-gated action to the
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
			// DNS-rebinding defense: the no-auth local surface must answer
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
			// served from wardynd itself, so its Origin is this listener's own. On a
			// mutating method, reject a PRESENT Origin that is not THIS listener —
			// closing the direct blind-CSRF the DNS-rebinding guard leaves open.
			//
			// Host *and* port, not merely "is it loopback": ports are not part of a
			// SITE, so a page another process serves at http://127.0.0.1:<other-port>
			// carries a loopback Origin AND is labelled same-site, and a
			// loopback-only rule admitted it — unauthenticated, on every mutating
			// route. The Host gate above already proved r.Host is loopback, so
			// comparing against r.Host is strictly narrower.
			//
			// There is no SECOND accepted name here (sameOriginOrRefuse also accepts
			// the OIDC redirect host, meaningless in local mode), so this arm takes
			// the r.Host half alone — same parse, same ASCII fold, csrf.go. What the
			// two modes DO share is the browser's own cross-site label:
			// Sec-Fetch-Site is unwritable by page script, and a mode that ignored it
			// would refuse the attack only when the attacker happened to send an
			// Origin. Both answer the same sentence — csrfRefusedBody, csrf.go.
			if isMutatingMethod(r.Method) {
				origin := strings.TrimSpace(r.Header.Get("Origin"))
				if isForeignSiteFetch(r) || (origin != "" && !s.originIsRequestHost(r, origin)) {
					s.auditAuthFailedAs(r, csrfActor, csrfAuditReason)
					writeError(w, http.StatusForbidden, "local mode: "+csrfRefusedBody)
					return
				}
			}
			next.ServeHTTP(w, r.WithContext(withLocalPrincipal(r.Context(), op)))
		})
	}
	// The THIRD auth branch sits in front of the admin bearer path, on both
	// halves of the split below: a `wdn_`-prefixed bearer is a per-user API
	// token, anything else is still compared against the single deployment-wide
	// admin token. Both halves get it because a token is a token — an operator
	// who has not configured SSO can still hold one (they simply cannot MINT
	// one; see handleCreateAPIToken, which requires a verified human).
	admin := s.apiTokenAuth(next, s.adminAuth(next))
	if s.cfg.OIDC == nil {
		return admin
	}
	// oidc.Middleware sets the principal on the context for a valid session and
	// always calls its next. We branch inside: if the session principal is
	// present we are authenticated and skip the bearer check; otherwise we defer
	// to the admin bearer path.
	return s.cfg.OIDC.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sub := oidc.PrincipalFromContext(r.Context()); sub != "" {
			// CSRF: this is the COOKIE-authenticated lane — the browser attaches
			// the session to any request a page can cause, so a cross-origin
			// mutation must be refused BEFORE the handler runs (csrf.go states
			// the rules and why the bearer lane below is exempt). Placed at the
			// top of the branch so no handler, and no context the branch
			// publishes, ever sees a forged request.
			if err := s.sameOriginOrRefuse(r); err != nil {
				// Audited on the EXISTING auth.failed action: this refusal
				// short-circuits ABOVE adminAuth, the chokepoint that emits for
				// every other public-API refusal, so without this call a
				// threat-model-registered control would leave no trail at all.
				// The session was VALID here, so no SessionRejectedFromContext
				// reason overrides csrfAuditReason (auditAuthFailedAs).
				s.auditAuthFailedAs(r, csrfActor, csrfAuditReason)
				writeError(w, http.StatusForbidden, err.Error())
				return
			}
			// Publish the verified human on an api-owned context key so
			// actorFromRequest attributes the action to the real SSO human
			// (and IGNORES any X-Wardyn-Principal header — a real identity won).
			// The session email rides along for /me and audit attribution; the
			// session role rides along for isOperator (the session's derived
			// admin/member role is now the sole source of the admin tier — see isOperator).
			//
			// The group snapshot rides along for the capability resolver, copied
			// verbatim, nil included: nil is the pre-0.6-cookie signal, not an
			// empty set (see oidcGroupsCtxKey). Its truncation bit comes
			// with it — a partial snapshot is as unanswerable as a nil one, and
			// only the cookie knows which it is.
			ctx := withHumanIdentity(r.Context(), sub,
				oidc.EmailFromContext(r.Context()),
				oidc.RoleFromContext(r.Context()),
				oidc.GroupsFromContext(r.Context()),
				oidc.GroupsTruncatedFromContext(r.Context()))
			// The display name rides along for /me only — outside
			// withHumanIdentity on purpose: the token lane, which shares that
			// function, has no name to publish and must not grow a fake one.
			ctx = withOIDCName(ctx, oidc.NameFromContext(r.Context()))
			// The session expiry rides along so /me can warn ahead of it —
			// There is no refresh, so the alternative is a silent 401
			// that wipes mid-work console state back to the sign-in gate.
			ctx = withOIDCExpiry(ctx, oidc.ExpiryFromContext(r.Context()))
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// A rejected session cookie answers itself — rejectedSessionAnswer, auth_status.go.
		if reason, status, msg, ok := rejectedSessionAnswer(r); ok {
			s.auditRejectedSession(r, reason) // carries the ERROR log + authStoreErrorInc
			writeError(w, status, msg)
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
// The name predates Session.Role and is kept (rather than renamed to
// requireAdmin) as the smallest honest diff — every existing call site and
// comment already reads "operator" to mean "admin", and the two are now exactly
// the same tier.
//
// isOperator reads the caller's ROLE (oidc.RoleAdmin / oidc.RoleUser,
// derived at OIDC login by internal/auth/oidc's deriveRole and carried on the
// session cookie) — never the OperatorEmails list directly. Config.OperatorEmails
// (WARDYN_OIDC_OPERATOR_EMAILS) still matters: cmd/wardynd feeds the SAME list
// into oidc.Config.LegacyAdminEmails, so an email on it is still an ADDITIONAL
// RoleAdmin match at derivation time (see deriveRole) — the mechanism is not
// deleted, it now flows through Session.Role like every other role signal
// instead of being re-checked here a second time. With WARDYN_OIDC_ROLE_MAP
// unset, deriveRole derives the role from this operator allowlist ALONE (an
// email on it => RoleAdmin, everyone else => RoleUser); only with NEITHER a
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
			// authz.denied: a member denied a reachable admin surface. Low-noise
			// by design (see the audit doc in runs_create.go's denyMemberRequest) —
			// this is the ONE universal chokepoint every admin-gated route funnels
			// through (incl. the attach WS's ticketOrHumanAuth fallback lane), so
			// one audit call here covers all of them.
			s.refuse(w, r, authz.Deny(authz.ReasonAdminSurface, r.URL.Path, ""))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isOperator reports whether the authenticated caller on ctx is a SUPER ADMIN
// — exactly oidc.RoleAdmin. This is the NARROWER of two
// named predicates, and the distinction is load-bearing:
//
//   - isOperator (here): binds credentials, writes the host, administers users,
//     and reaches runs its holder does not own. It is what a role SNAPSHOT
//     stamped on an SSH key or an attach ticket means (sshkeys.go,
//     attach_ticket.go), which is why oidc.RoleSecurityAdmin is not "below"
//     RoleAdmin on a ladder — it is beside it.
//   - isSecurityOperator (below): admin OR security_admin — the security
//     GOVERNANCE surfaces (approvals, audit, permissions, governance profiles).
//
// A predicate here is the tier's ONLY definition; nothing in this package may
// re-derive either from a role comparison of its own. New admin surfaces
// default to this stricter one (they register on the operatorOnly router
// group), which is the safe direction for a migration.
//
// See requireOperator for the rules. A caller with no verified OIDC human on
// ctx (admin token, local mode, or OIDC not configured) is always an admin — a
// single shared credential carries no per-human role to demote. Otherwise the
// caller's role is authoritative and admin-only when it is exactly
// oidc.RoleAdmin (fail closed on any other value, including an unexpectedly
// empty one — decodeSession already refuses to hand out a session with an
// empty role, so this is defense-in-depth, not a real path).
func (s *Server) isOperator(ctx context.Context) bool {
	// A device request carries no OIDC human, which the next branch reads as
	// the admin token. A daemon is never an operator, so refuse it first —
	// whatever file the handler asking was written in.
	if _, isDevice := deviceFromContext(ctx); isDevice {
		return false
	}
	if oidcHumanFromContext(ctx) == "" {
		return true // admin token, local mode, or OIDC not configured: no session role to demote
	}
	return oidcRoleFromContext(ctx) == oidc.RoleAdmin
}

// isSecurityOperator reports whether the caller on ctx may perform SECURITY
// GOVERNANCE actions: read/decide any approval, read the audit chain, write
// permissions/capability grants, author governance profiles. True for
// oidc.RoleAdmin (a super admin is a security admin too — the tiers overlap on
// this surface, they merely do not nest on the run-reach one) and for
// oidc.RoleSecurityAdmin.
//
// Same no-OIDC-human arm as isOperator, deliberately: the admin token and local
// mode are one shared credential with no human to demote, and the break-glass
// recovery path both tiers already depend on. Any drift between the two arms
// would mean an admin-token deployment could reach one tier and not the other.
//
// Ctx-only, and it stays that way: a future delegated/sub-org scope adds
// isSecurityOperatorFor(ctx, scope) and redefines this one in terms of it, so
// scope is never encoded in the role VALUE (which would make the role map a
// tenancy language it cannot be).
//
// The in-handler seams that read THIS predicate rather than isOperator, and
// why (approvals.go carries only same-line pointers — it sits two lines under
// the file-size gate):
//
//   - approvals.go's list, authorizeMemberDecision and the
//     decision_scope=always gate. The latter two are a LOCKSTEP PAIR: deciding
//     an approval and persisting that decision are the same authority, one
//     merely durable, and a tier that may decide but not record would re-decide
//     the identical request forever. All three are authority over the VERDICT
//     and never reach INTO a run — deciding writes a decision, it does not open
//     a PTY, and the attach lanes still require oidc.RoleAdmin.
//   - audit.go's query + export, runs_policy.go's run list, workspaces.go's
//     workspace list, policies.go's four read-redaction sites, and helpers.go's
//     ownsRunOrAdmin — each commented at its own site.
func (s *Server) isSecurityOperator(ctx context.Context) bool {
	if _, isDevice := deviceFromContext(ctx); isDevice {
		return false // the same refusal isOperator makes first
	}
	if oidcHumanFromContext(ctx) == "" {
		return true // same shared-credential arm as isOperator — see above
	}
	role := oidcRoleFromContext(ctx)
	return role == oidc.RoleAdmin || role == oidc.RoleSecurityAdmin
}

// requireSecurityOperator is requireOperator's twin for the SECURITY
// GOVERNANCE route family (the securityOps router group): admin OR
// security_admin pass, a member gets the same 403 and the same authz.denied
// audit shape, distinguished only by reason — "security_admin_surface" rather
// than "admin_surface", so an operator reading the audit log can tell WHICH
// tier a denial was measured against without correlating paths by hand.
//
// The 403 body is byte-identical to requireOperator's on purpose: a member
// learns that they lack the role, never which of the two tiers a given route
// sits on (that is a map of the deployment's admin surface).
func (s *Server) requireSecurityOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isSecurityOperator(r.Context()) {
			s.refuse(w, r, authz.Deny(authz.ReasonSecurityAdminSurface, r.URL.Path, ""))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminAuth gates the public API behind a constant-time bearer compare. An
// empty configured AdminToken denies everything (fail closed): the public API
// must not be unauthenticated. SSO/Dex replaces this in a later milestone.
//
// Every 401 here is audited as auth.failed (#19a) — this is the ONE chokepoint
// every public-API auth failure funnels through (humanOrAdminAuth composes
// oidc.Middleware(adminAuth(...)), and a rejected session cookie still falls
// through to here), so one emit call covers both "adminAuth 401s" and
// "dropped/invalid OIDC session cookies" without a second call site per caller.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			s.auditAuthFailed(r, "admin_token_not_configured")
			writeError(w, http.StatusUnauthorized, "admin token not configured; public API disabled")
			return
		}
		tok, ok := bearerToken(r)
		if !ok {
			s.auditAuthFailed(r, "missing_bearer_token")
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AdminToken)) != 1 {
			s.auditAuthFailed(r, "invalid_admin_token")
			writeError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auditAuthFailed emits auth.failed (actor system) for an authentication
// failure on the public API. reason is the adminAuth-local bounded enum
// ("admin_token_not_configured", "missing_bearer_token",
// "invalid_admin_token"); it is overridden by a more specific
// oidc.SessionRejectedFromContext reason ("invalid_session"/"expired_session"/
// "revoked_session"/"session_revocation_unavailable")
// when a session cookie was ALSO presented and rejected on this same
// request — that is the more actionable signal of the two. Data is
// content-free by construction: a closed reason enum, the request path, and
// the TCP peer — never a user-supplied string.
//
// Rate-bound so a scanner throwing 401s cannot flood the append-only log:
// ponytail: one process-global token bucket, not per-IP — per-IP needs its
// own eviction policy (an unbounded map keyed by attacker-controlled IPs is
// itself a memory-DoS vector) and a global cap already starves a flood from
// any single source; add per-IP if a shared IP (corp NAT) needs to be
// distinguished from an attacker sharing it.
// sessionRevocationUnavailable is oidc.Middleware's fail-closed reason for a
// revocation-store read that errored (oidc.SessionRejectedFromContext's
// documented set). Mirrored as a constant rather than compared as a literal at
// the one site that branches on it, so the value has a name where it is USED and
// a rename upstream is a grep away rather than a silent no-op that turns the
// counter off.
const sessionRevocationUnavailable = "session_revocation_unavailable"

func (s *Server) auditAuthFailed(r *http.Request, reason string) {
	s.auditAuthFailedAs(r, adminAuthActor, reason)
}

// adminAuthActor / internalAuthActor / groundtruthAuthActor /
// internalApprovalActor name WHICH boundary refused, as the auth.failed row's
// Actor. They exist because the row's reason enum alone cannot say
// whether a refusal came from the PUBLIC lane (a human or an API token) or from
// the INTERNAL lane (a sandbox sidecar or the host sensor) — and those are
// different incidents with different runbooks: credential stuffing on the public
// lane, a compromised or probing sidecar on the internal one.
const (
	adminAuthActor        = "wardyn/adminAuth"
	internalAuthActor     = "wardyn/internalAuth"
	groundtruthAuthActor  = "wardyn/internalAuthGroundtruth"
	internalApprovalActor = "wardyn/internalApproval"
	// csrfActor is the CSRF guard's own boundary. NOT adminAuth: that boundary
	// did not refuse this — the session was VALID and the guard short-circuits
	// above it — and a row naming the wrong one sends an incident review to the
	// credential-stuffing runbook for what is a cross-origin page.
	csrfActor = "wardyn/csrf"
	// oidcCallbackActor is the SSO sign-in callback, refusing a login the
	// IdP approved because of what the role map makes of it (a user type
	// that is ambiguous or does not exist).
	oidcCallbackActor = "wardyn/oidcCallback"
)

// auditAuthFailedAs is the ONE rate-bound emit every authentication refusal
// funnels through, public and internal alike. actor names the boundary that
// refused (see the *Actor constants); reason is that boundary's bounded enum.
// Split out of auditAuthFailed so the internal (sandbox/host-sensor) lane gets
// the SAME limiter, the SAME suppressed counter and the SAME content-free row
// shape instead of a second, divergent copy. Without it that lane answers
// 401/400 and records nothing anywhere, so a process inside a sandbox
// brute-forcing run tokens against /api/v1/internal/*, or a sidecar probing for
// the credential-approval path its own handler comment says must never come from
// an untrusted sidecar, leaves no trace at all.
func (s *Server) auditAuthFailedAs(r *http.Request, actor, reason string) {
	// Resolved before the limiter, not after. The row's content is unchanged by
	// the move — but the store-outage arm below has to count every REQUEST, and
	// the limiter drops most of them: measured, 50 requests during a revocation
	// outage produced 5 audit rows and 45 suppressions, so a counter reached
	// only past the limiter would report a tenth of an outage.
	if sr := oidc.SessionRejectedFromContext(r.Context()); sr != "" {
		reason = sr
	}
	// The SSO lane's store outage, counted in the SAME series the api-token lane
	// uses (apitokens.go). Both lanes abandon an authentication because a store
	// read failed; only one of them said so.
	//
	// What the asymmetry cost is not a missing metric, it is a WRONG one: a
	// revocation-store outage 401s every SSO human with "missing bearer token",
	// wrote no log line, left wardyn_auth_store_errors_total at 0 and
	// wardyn_store_up at 1 (that gauge answers a PING, which a pool passes while
	// one table denies a read), and pushed the flood into
	// wardyn_auth_failed_suppressed_total — the series OPERATIONS.md defines as
	// the credential-stuffing signature. An operator following their own runbook
	// was looking for an attacker during a database incident.
	//
	// Error level by internal/audit/sink.go's own rule and by the sibling lane's
	// precedent: an authentication that could not be DECIDED is an ERROR-level
	// fact an operator can alert on, and the client-side symptom names the wrong
	// cause.
	if reason == sessionRevocationUnavailable {
		slog.ErrorContext(r.Context(), "api: session-revocation lookup failed; this request could not be authenticated",
			"path", r.URL.Path)
		s.metrics.authStoreErrorInc()
	}
	// Coalesce before the limiter. The limiter bounds the RATE; it does
	// nothing about a slow, permanent drip — the field report's flood was one row
	// a minute from a single retrying sidecar, which a 1/sec limiter never trips,
	// and it still evicted every real security event out of the console's
	// 1000-row window in minutes. This folds a run of IDENTICAL consecutive
	// refusals into their first row plus one summary row carrying the count.
	// Before the limiter, so the count is the true number of refusals rather than
	// the number that happened to survive rate-limiting.
	absorbed, summary := s.coalesceAuthFailed(actor, reason, r.URL.Path, r.RemoteAddr)
	// The closing streak's summary goes out FIRST, so the row order in the trail
	// matches the order the refusals happened in (and so a test reading "the last
	// row" still reads this request's row, not the previous streak's summary) —
	// and it pays the limiter below exactly like a first row does, because a
	// streak closes on every KEY CHANGE and a client that alternates two paths
	// would otherwise mint one unmetered row per two requests
	// (recordAuthFailedSummary).
	s.recordAuthFailedSummary(s.cfg.BaseCtx, summary)
	// The two are INDEPENDENT, and folding them into an if/else got the streak-cap
	// close wrong: there, the capping refusal both closes the streak (summary
	// non-nil) and is itself absorbed into that summary's count, so an else-arm
	// wrote it verbatim as well — counted twice, once as a row and once in the
	// count — and skipped the suppressed counter for it.
	if absorbed {
		// Counted in the SAME series the limiter's drops are: a dropped auth.failed
		// row is a countable fact whichever mechanism dropped it, and an operator
		// alerting on volume must not have to know which one did. The summary row
		// carries the count as well, so the trail is not the only witness.
		s.metrics.authFailedSuppressedInc()
		return
	}
	if !s.authFailedLimiter.allow(s.cfg.Now()) {
		// Counted, not just dropped. The limiter caps the audit trail at ~1
		// row/sec, so past the burst the trail stops describing the volume it
		// is bounding: a credential-stuffing run and a handful of typos look
		// identical in the audit log, and the attack looks QUIETER the harder
		// it is pushed. This counter is what carries the real rate — a flat
		// auth.failed row count with this series climbing is the signal, and it
		// is a series precisely so it can be alerted on rather than grepped
		// for. Same treatment the spool's torn/quarantined drops already get
		// (metrics.go): a discarded event is a countable fact.
		s.metrics.authFailedSuppressedInc()
		return
	}
	ev := s.auditEvent(nil, types.ActorSystem, actor, "auth.failed", r.URL.Path,
		"failure", mustJSON(map[string]any{"reason": reason}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
}

// auditRunIdentityExpired records run.identity.expired ONCE per run when a
// NON-TERMINAL run presents an expired identity on the internal lane.
//
// This keeps quieting the sidecar from hiding a real failure. The proxy's
// renew loop backs off and gives up instead of retrying a refused renew once
// a minute forever (internal/egress/proxy/renew.go), which stops the audit
// flood — but the case underneath it can be a perfectly
// HEALTHY run: the renewer sleeps half the token's life, so a run that misses
// renewal through more than 30 minutes of control-plane outage (a wardynd
// rollout) holds a dead identity for the rest of its life, and every /internal/*
// call it makes 401s. That fact surfaces as one row, keyed to the run, which
// the cockpit's evidence rail already renders — never an undifferentiated
// pile of auth.failed rows.
//
// The run is LEFT RUNNING: mid-flight work is the owner's to abandon, and the
// remedy the docs give is kill + start a new run. Deliberately quiet otherwise:
//
//   - the once-guard is per run id and in memory, so a wardynd restart may emit a
//     second row for the same run. Acceptable — two rows for one incident, never a
//     row a minute. Process-local for the same reason every other in-memory bound
//     in this package is (replicas>1 is refused by construction).
//   - a TERMINAL run emits nothing: its token is supposed to be dead, a sidecar
//     the teardown has not reaped yet is not news, and that is precisely the
//     population a flood would come from.
//   - only an *identity.ExpiredTokenError reaches here with a run id at all. A
//     revoked, forged or wrong-audience token names no run of ours and stays
//     exactly as coarse as the auth.failed row beside it.
func (s *Server) auditRunIdentityExpired(r *http.Request, verifyErr error) {
	var exp *identity.ExpiredTokenError
	if !errors.As(verifyErr, &exp) || s.cfg.Store == nil {
		return
	}
	// The once-guard runs before the store read, and that ordering is the point.
	// The internal lane carries no rate limiter (routes.go), so a sidecar — or
	// anything replaying a captured token — can present the same dead identity as
	// often as it likes; with the read first, every one of those refusals bought a
	// GetRun, forever, to decide not to write a row it had already written. The
	// claim makes the second and every later refusal for a run free.
	if !s.claimIdentityExpired(exp.RunID) {
		return
	}
	run, gerr := s.cfg.Store.GetRun(r.Context(), exp.RunID)
	if gerr != nil {
		// Released on a store error only. "We could not tell whether this run is
		// live" must not consume the one row the run gets — a control-plane blip
		// is exactly when this evidence matters. A TERMINAL run keeps its claim
		// (it is never going to deserve a row), so the quiet case stays at one
		// read per run, which is what this ordering is for.
		s.releaseIdentityExpired(exp.RunID)
		return
	}
	if isTerminalRunState(run.State) {
		return
	}
	ev := s.auditEvent(&exp.RunID, types.ActorSystem, "wardynd", "run.identity.expired",
		exp.RunID.String(), "failure", mustJSON(map[string]any{
			"run_state": string(run.State),
			"path":      r.URL.Path,
		}))
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
	slog.WarnContext(r.Context(), "api: a running run is presenting an expired identity; its renews are not landing",
		slog.String("run_id", exp.RunID.String()), slog.String("run_state", string(run.State)))
}

// claimIdentityExpired returns true for the FIRST caller to claim a run's single
// run.identity.expired row, and false for every caller after it. Consult and
// insert are one locked operation so two concurrent refusals cannot both win.
//
// Bounded like lastTouch (internal.go's shouldTouch), and for the same reason:
// the key is a run id, a long-lived daemon accumulates one entry per run that
// ever presented a dead token, and nothing else would ever evict them. Clearing
// wholesale past the bound costs at most one extra row per run still refusing —
// the same acceptable duplicate a restart already produces.
func (s *Server) claimIdentityExpired(runID uuid.UUID) bool {
	s.identityExpiredMu.Lock()
	defer s.identityExpiredMu.Unlock()
	if s.identityExpiredSeen == nil {
		s.identityExpiredSeen = map[uuid.UUID]bool{}
	} else if s.identityExpiredSeen[runID] {
		return false
	} else if len(s.identityExpiredSeen) > 4096 {
		clear(s.identityExpiredSeen)
	}
	s.identityExpiredSeen[runID] = true
	return true
}

// releaseIdentityExpired gives a claim back, so a refusal the server could not
// DECIDE about (a failed run read) does not silently spend the run's one row.
func (s *Server) releaseIdentityExpired(runID uuid.UUID) {
	s.identityExpiredMu.Lock()
	delete(s.identityExpiredSeen, runID)
	s.identityExpiredMu.Unlock()
}

// internalAuth verifies a per-run token via identity.Provider.Verify with the
// "wardyn-internal" audience and stashes the resulting claims on the context.
// Any verification error fails closed with 401 (revoked/expired tokens too).
func (s *Server) internalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Identity == nil {
			s.auditAuthFailedAs(r, internalAuthActor, "identity_provider_not_configured")
			writeError(w, http.StatusUnauthorized, "identity provider not configured")
			return
		}
		tok, ok := bearerToken(r)
		if !ok {
			s.auditAuthFailedAs(r, internalAuthActor, "missing_run_token")
			writeError(w, http.StatusUnauthorized, "missing run token")
			return
		}
		claims, err := s.cfg.Identity.Verify(r.Context(), tok, internalAudience)
		if err != nil {
			// Do not leak the verification reason (revoked vs expired vs forged)
			// to the caller. The audit row is equally coarse — one reason for the
			// whole verify failure — so the trail records THAT a run token was
			// refused without telling a brute-forcer which half it got wrong.
			s.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token")
			s.auditRunIdentityExpired(r, err)
			writeError(w, http.StatusUnauthorized, "invalid run token")
			return
		}
		// Claims first, then LIVENESS — see refuseTerminalRun.
		r = r.WithContext(contextWithClaims(r.Context(), claims))
		if !s.refuseTerminalRun(w, r, claims) {
			return
		}
		next.ServeHTTP(w, r)
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
			s.auditAuthFailedAs(r, groundtruthAuthActor, "identity_provider_not_configured")
			writeError(w, http.StatusUnauthorized, "identity provider not configured")
			return
		}
		tok, ok := bearerToken(r)
		if !ok {
			s.auditAuthFailedAs(r, groundtruthAuthActor, "missing_sensor_token")
			writeError(w, http.StatusUnauthorized, "missing sensor token")
			return
		}
		if _, err := s.cfg.Identity.Verify(r.Context(), tok, groundtruthAudience); err != nil {
			// Do not leak the verification reason (revoked vs expired vs
			// wrong-audience vs forged) to the caller; the audit row is equally
			// coarse for the same reason.
			s.auditAuthFailedAs(r, groundtruthAuthActor, "invalid_sensor_token")
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

// ceilingMemoMiddleware installs the per-request ceiling memo. Separate from
// humanOrAdminAuth's body only so the three auth modes (local, SSO, admin token)
// cannot each forget it — it wraps the whole chain once, above the branch.
func ceilingMemoMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(withCeilingMemo(r.Context())))
	})
}
