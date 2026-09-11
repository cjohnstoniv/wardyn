// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// Session principal in the request context: the unexported keys, the
// exported *FromContext readers internal/api consumes, and the two writers
// Middleware/CallbackHandler use. Split from oidc.go by seam (the file-size
// gate); no behaviour lives here that oidc.go's doc does not describe.

import (
	"context"
	"time"
)

// sessionRejectedCtxKey carries the reason a presented OIDC session cookie
// was rejected, for the integrator's auth-failure audit emit.
type sessionRejectedCtxKey struct{}

func withSessionRejected(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, sessionRejectedCtxKey{}, reason)
}

// SessionRejectedFromContext returns why Middleware rejected a presented
// session cookie on this request: "invalid_session" for a tampered/malformed
// cookie, "expired_session" for a valid-but-expired one, "revoked_session"
// for a valid cookie the revocation store has cut off, or
// "session_revocation_unavailable" when that store errored (fail-closed) —
// the last two only when Revocations is wired. Returns "" when no session
// cookie was presented at all (the ordinary non-browser-client case) or the
// session decoded fine.
func SessionRejectedFromContext(ctx context.Context) string {
	reason, _ := ctx.Value(sessionRejectedCtxKey{}).(string)
	return reason
}

// PrincipalFromContext returns the human principal set by Middleware, or ""
// if no SSO session is present on the context. The returned value is the OIDC
// "sub" claim (a stable opaque identifier from the IdP).
//
// Integration note: internal/api's principalFromRequest should call this first
// and fall back to the admin-token path when the result is "".
func PrincipalFromContext(ctx context.Context) string {
	p, _ := ctx.Value(principalCtxKey{}).(string)
	return p
}

// EmailFromContext returns the email claim of the session Middleware verified,
// or "" when there is no SSO session (or the IdP returned no email — which is
// possible whenever AllowedEmailDomains is empty, since that is the only check
// that requires one). It is the identity internal/api resolves the minimal
// viewer/operator role from; the "sub" is opaque and cannot be matched against
// an operator allowlist an admin can actually write down.
func EmailFromContext(ctx context.Context) string {
	e, _ := ctx.Value(emailCtxKey{}).(string)
	return e
}

// NameFromContext returns the display-name ("name") claim of the session
// Middleware verified, or "" when there is no SSO session or the IdP sent
// none. Display only — internal/api shows it in the console header and
// nothing else reads it; the identity every decision is keyed on stays the
// sub (PrincipalFromContext) and the allowlist identity stays the email.
func NameFromContext(ctx context.Context) string {
	n, _ := ctx.Value(nameCtxKey{}).(string)
	return n
}

// RoleFromContext returns the Wardyn role (RoleAdmin or RoleMember) derived
// for the session Middleware verified, or "" when there is no SSO session.
// This package only DERIVES and CARRIES the role — see CallbackHandler /
// deriveRole for how it is computed. Enforcing it (deciding what an admin vs
// a member may do) belongs to internal/api, the same split
// PrincipalFromContext/EmailFromContext already follow.
func RoleFromContext(ctx context.Context) string {
	r, _ := ctx.Value(roleCtxKey{}).(string)
	return r
}

// ExpiryFromContext returns when the session Middleware verified will expire,
// or the zero time when there is no SSO session. W31-S1-7: there is no
// refresh — the session dies outright at this instant — so the console
// surfaces it as an advance warning instead of a surprise 401 that wipes
// mid-work state back to the sign-in gate.
func ExpiryFromContext(ctx context.Context) time.Time {
	t, _ := ctx.Value(expiryCtxKey{}).(time.Time)
	return t
}

// GroupsFromContext returns the login-time group snapshot of the session
// Middleware verified — the subjects a `group` capability grant matches.
//
// NIL AND EMPTY MEAN DIFFERENT THINGS and callers must keep them apart. Empty
// non-nil: this session was minted by 0.6+, the IdP sent no usable group
// identity, and group grants genuinely do not apply. Nil: either there is no
// SSO session at all, or the human is holding a PRE-0.6 cookie that predates
// the field — group grants cannot be evaluated for them until they log in
// again, which is what internal/api surfaces as groups_snapshot_stale rather
// than silently reporting "no groups".
//
// Same DERIVES-not-ENFORCES split as RoleFromContext: this package carries the
// snapshot, internal/api decides what it permits.
func GroupsFromContext(ctx context.Context) []string {
	g, _ := ctx.Value(groupsCtxKey{}).([]string)
	return g
}

// GroupsTruncatedFromContext reports whether the session's group snapshot is
// PARTIAL — sessionGroups hit the cookie byte cap and dropped entries (see
// Session.GroupsTruncated).
//
// A caller must treat true exactly as it treats a nil snapshot: the group
// identity is not answerable, so no group-scoped decision can be made from it.
// Reading it as "these are all their groups" is the silent tier evaporation
// PF-26 names.
func GroupsTruncatedFromContext(ctx context.Context) bool {
	t, _ := ctx.Value(groupsTruncatedCtxKey{}).(bool)
	return t
}

// contextWithPrincipal stores the verified session's sub, email, role, and
// group snapshot (with its truncation bit) on the context (read back via
// PrincipalFromContext / EmailFromContext / RoleFromContext /
// GroupsFromContext / GroupsTruncatedFromContext).
//
// Groups is stored even when nil, and that is not a wasted WithValue: a nil
// value and an absent key both read back as nil, so this line costs nothing to
// get right and keeps contextWithPrincipal free of a special case that would
// only ever be re-added later.
func contextWithPrincipal(ctx context.Context, sess Session) context.Context {
	ctx = context.WithValue(ctx, principalCtxKey{}, sess.Sub)
	ctx = context.WithValue(ctx, emailCtxKey{}, sess.Email)
	ctx = context.WithValue(ctx, nameCtxKey{}, sess.Name)
	ctx = context.WithValue(ctx, groupsCtxKey{}, sess.Groups)
	ctx = context.WithValue(ctx, groupsTruncatedCtxKey{}, sess.GroupsTruncated)
	ctx = context.WithValue(ctx, roleCtxKey{}, sess.Role)
	return context.WithValue(ctx, expiryCtxKey{}, sess.Expiry)
}

// principalCtxKey is the context key for the human SSO principal.
// Unexported: use PrincipalFromContext.
type principalCtxKey struct{}

// emailCtxKey is the context key for the session's email claim.
// Unexported: use EmailFromContext.
type emailCtxKey struct{}

// nameCtxKey is the context key for the session's display-name claim.
// Unexported: use NameFromContext.
type nameCtxKey struct{}

// roleCtxKey is the context key for the session's derived role.
// Unexported: use RoleFromContext.
type roleCtxKey struct{}

// groupsCtxKey is the context key for the session's login-time group snapshot.
// Unexported: use GroupsFromContext.
type groupsCtxKey struct{}

// groupsTruncatedCtxKey is the context key for that snapshot's PF-26
// truncation bit. Unexported: use GroupsTruncatedFromContext.
type groupsTruncatedCtxKey struct{}

// expiryCtxKey is the context key for the session's expiry.
// Unexported: use ExpiryFromContext.
type expiryCtxKey struct{}
