// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// Session principal in the request context: the unexported keys, the
// exported *FromContext readers internal/api consumes, and the two writers
// Middleware/CallbackHandler use.

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
// session cookie on this request: "invalid_session", "expired_session",
// "revoked_session", or "session_revocation_unavailable" (fail-closed) when
// Revocations is wired; "" when no cookie was presented or it decoded fine.
func SessionRejectedFromContext(ctx context.Context) string {
	reason, _ := ctx.Value(sessionRejectedCtxKey{}).(string)
	return reason
}

// PrincipalFromContext returns the human principal set by Middleware (the OIDC
// "sub" claim), or "" if no SSO session is present. internal/api's
// principalFromRequest calls this first, falling back to the admin-token path.
func PrincipalFromContext(ctx context.Context) string {
	p, _ := ctx.Value(principalCtxKey{}).(string)
	return p
}

// EmailFromContext returns the email claim of the verified session, or "" when
// absent (possible when AllowedEmailDomains is empty). internal/api resolves
// the viewer/operator role from this, since the opaque "sub" can't be matched
// against an admin-writable allowlist.
func EmailFromContext(ctx context.Context) string {
	e, _ := ctx.Value(emailCtxKey{}).(string)
	return e
}

// NameFromContext returns the display-name claim, or "" when absent. Display
// only — every decision keys on the sub/email, never this.
func NameFromContext(ctx context.Context) string {
	n, _ := ctx.Value(nameCtxKey{}).(string)
	return n
}

// RoleFromContext returns the EFFECTIVE Wardyn role, or "" when there is no
// SSO session. Effective, not stamped: in "view as member" mode
// (Session.MemberMode) this always answers RoleUser, clamped once in
// contextWithPrincipal. THIS is the role every authorization decision reads —
// internal/api enforces against it, never against StampedRoleFromContext.
func RoleFromContext(ctx context.Context) string {
	r, _ := ctx.Value(roleCtxKey{}).(string)
	return r
}

// StampedRoleFromContext returns the role sess.Role actually carries — the
// clamp's INPUT, never its output — or "" when there is no SSO session. It
// exists for exactly one purpose: deciding whether to hand a clamped user
// view's session some ADVISORY UI data (the org's user types, for the
// in-view type picker) that only makes sense for someone who is an admin or
// security_admin underneath the clamp. NEVER read this for an authorization
// decision — RoleFromContext (already clamped) is the only role this
// package's own enforcement doc names, and reading the stamped one instead
// would let a clamped session reach whatever that check gates, defeating the
// clamp entirely.
func StampedRoleFromContext(ctx context.Context) string {
	r, _ := ctx.Value(stampedRoleCtxKey{}).(string)
	return r
}

// StampedRoleIsOperatorTier reports whether ctx's STAMPED role (never the
// clamped one) is admin or security_admin. It lives here, as the one place
// that may compare against RoleAdmin/RoleSecurityAdmin for this purpose, so
// internal/api never repeats the raw comparison itself — that package's own
// TestNoAdHocAuthz (G4) ratchets such comparisons down to a fixed, reviewed
// list, on the ground that a tier decision belongs in isOperator/
// isSecurityOperator, never re-derived ad hoc at a call site. Those two
// predicates read the CLAMPED role by design (RoleFromContext) and are wrong
// for this question on purpose: meUserViewTypes (#912, H2) needs the
// UNDERLYING tier precisely because the clamp has already hidden it from
// them.
func StampedRoleIsOperatorTier(ctx context.Context) bool {
	role := StampedRoleFromContext(ctx)
	return role == RoleAdmin || role == RoleSecurityAdmin
}

// UserTypeFromContext returns the resolved user type id, or "" when there is
// no SSO session: the sign-in type, or the user-view type while that's on.
func UserTypeFromContext(ctx context.Context) string {
	t, _ := ctx.Value(userTypeCtxKey{}).(string)
	return t
}

// StampedUserTypeFromContext returns the user type stamped at sign-in, which
// the user view does not change: the view's fallback when no chosen type is
// remembered. "" when there is no SSO session.
func StampedUserTypeFromContext(ctx context.Context) string {
	t, _ := ctx.Value(stampedUserTypeCtxKey{}).(string)
	return t
}

// UserViewDroppedFromContext returns the type whose deletion turned this
// session's user view off (Session.UserViewDropped), or "".
func UserViewDroppedFromContext(ctx context.Context) string {
	t, _ := ctx.Value(userViewDroppedCtxKey{}).(string)
	return t
}

// UserViewDroppedNameFromContext returns that same type's cached display name
// (Session.UserViewDroppedName), or "" when there is none — either no drop is
// recorded, or the cookie predates this field. Pairs with
// UserViewDroppedFromContext's id.
func UserViewDroppedNameFromContext(ctx context.Context) string {
	n, _ := ctx.Value(userViewDroppedNameCtxKey{}).(string)
	return n
}

// viewedUserType is the type a session's controls resolve against: the
// chosen type while the user view is on, the stamped one otherwise (and for
// a view entered before a type could be chosen).
func viewedUserType(sess Session) string {
	if sess.MemberMode && sess.UserViewType != "" {
		return sess.UserViewType
	}
	return sess.UserType
}

// ExpiryFromContext returns when the verified session expires, or the zero
// time when absent. No refresh — the session dies outright — so the console
// surfaces it as an advance warning rather than a surprise 401.
func ExpiryFromContext(ctx context.Context) time.Time {
	t, _ := ctx.Value(expiryCtxKey{}).(time.Time)
	return t
}

// GroupsFromContext returns the login-time group snapshot — the subjects a
// `group` capability grant matches.
//
// NIL AND EMPTY MEAN DIFFERENT THINGS: empty non-nil means the IdP sent no
// usable group identity, so grants genuinely don't apply. Nil means no SSO
// session, or a pre-0.6 cookie predating this field — internal/api surfaces
// that as groups_snapshot_stale rather than "no groups".
func GroupsFromContext(ctx context.Context) []string {
	g, _ := ctx.Value(groupsCtxKey{}).([]string)
	return g
}

// GroupsTruncatedFromContext reports whether the group snapshot is PARTIAL
// (hit the cookie byte cap). A caller must treat true exactly like a nil
// snapshot — no group-scoped decision can be made from it (PF-26).
func GroupsTruncatedFromContext(ctx context.Context) bool {
	t, _ := ctx.Value(groupsTruncatedCtxKey{}).(bool)
	return t
}

// MemberModeFromContext reports whether this session is in "view as member"
// mode — an admin treated as a member for the rest of the session.
// RoleFromContext already answers member when true, so authorization needs
// nothing from this; it exists only for the console banner and the
// credential doors (API-token mint refuses, since a member-stamped token
// would be re-stamped admin at next login; SSH keys are accepted capped).
func MemberModeFromContext(ctx context.Context) bool {
	m, _ := ctx.Value(memberModeCtxKey{}).(bool)
	return m
}

// MemberPreviewNoCredential reports whether this session is in the
// NO-CREDENTIAL posture of member mode — "view as a new member who has not
// signed in" (Session.MemberModeNoCredential). Published as
// `MemberMode && MemberModeNoCredential`, so it implies MemberModeFromContext.
//
// Authorizes and clamps nothing: the one thing it moves is whether a
// per-user model-credential read answers "absent" (previewHidesOwnCredential),
// letting every already-fail-closed path reach the not-signed-in state.
func MemberPreviewNoCredential(ctx context.Context) bool {
	m, _ := ctx.Value(memberPreviewNoCredCtxKey{}).(bool)
	return m
}

// contextWithPrincipal stores the verified session's sub, email, EFFECTIVE
// role, and group snapshot on the context (read back via the *FromContext
// readers above). Groups is stored even when nil — a no-op WithValue that
// avoids a special case.
func contextWithPrincipal(ctx context.Context, sess Session) context.Context {
	ctx = context.WithValue(ctx, principalCtxKey{}, sess.Sub)
	ctx = context.WithValue(ctx, emailCtxKey{}, sess.Email)
	ctx = context.WithValue(ctx, nameCtxKey{}, sess.Name)
	ctx = context.WithValue(ctx, groupsCtxKey{}, sess.Groups)
	ctx = context.WithValue(ctx, groupsTruncatedCtxKey{}, sess.GroupsTruncated)
	// THE MEMBER-MODE CLAMP — the only place it's applied. This is the sole
	// read of sess.Role in the product, so clamping here clamps everything
	// downstream by construction rather than every call site remembering a
	// second predicate. DOWNWARD ONLY: sess.Role is never rewritten, so
	// toggling off restores it verbatim.
	role := sess.Role
	if sess.MemberMode {
		role = RoleUser
	}
	ctx = context.WithValue(ctx, roleCtxKey{}, role)
	// The user view publishes the type it looks through in place of the
	// stamped one, so every control resolves as for a person of that type.
	ctx = context.WithValue(ctx, userTypeCtxKey{}, viewedUserType(sess))
	ctx = context.WithValue(ctx, stampedUserTypeCtxKey{}, sess.UserType)
	ctx = context.WithValue(ctx, userViewDroppedCtxKey{}, sess.UserViewDropped)
	ctx = context.WithValue(ctx, userViewDroppedNameCtxKey{}, sess.UserViewDroppedName)
	// The UNCLAMPED role, published alongside the effective one above -- see
	// StampedRoleFromContext's own doc for why this exists and the one rule
	// its use is bound by.
	ctx = context.WithValue(ctx, stampedRoleCtxKey{}, sess.Role)
	ctx = context.WithValue(ctx, memberModeCtxKey{}, sess.MemberMode)
	// ANDed with the mode, not copied: a cookie hand-built with "mmnc" alone
	// is inert. Identity (sess.Sub) is untouched by both.
	ctx = context.WithValue(ctx, memberPreviewNoCredCtxKey{}, sess.MemberMode && sess.MemberModeNoCredential)
	return context.WithValue(ctx, expiryCtxKey{}, sess.Expiry)
}

// principalCtxKey is the context key for the human SSO principal; use PrincipalFromContext.
type principalCtxKey struct{}

// emailCtxKey is the context key for the session's email claim; use EmailFromContext.
type emailCtxKey struct{}

// nameCtxKey is the context key for the session's display-name claim; use NameFromContext.
type nameCtxKey struct{}

// roleCtxKey is the context key for the session's derived role; use RoleFromContext.
type roleCtxKey struct{}

// userTypeCtxKey is the context key for the session's user type; use UserTypeFromContext.
type userTypeCtxKey struct{}

// stampedUserTypeCtxKey is the context key for the sign-in type; use StampedUserTypeFromContext.
type stampedUserTypeCtxKey struct{}

// userViewDroppedCtxKey is the context key for the dropped view's type; use UserViewDroppedFromContext.
type userViewDroppedCtxKey struct{}

// userViewDroppedNameCtxKey is the context key for the dropped type's cached name; use UserViewDroppedNameFromContext.
type userViewDroppedNameCtxKey struct{}

// stampedRoleCtxKey is the context key for the session's UNCLAMPED role; use StampedRoleFromContext.
type stampedRoleCtxKey struct{}

// groupsCtxKey is the context key for the session's login-time group snapshot; use GroupsFromContext.
type groupsCtxKey struct{}

// groupsTruncatedCtxKey is the context key for that snapshot's PF-26 truncation bit; use GroupsTruncatedFromContext.
type groupsTruncatedCtxKey struct{}

// memberModeCtxKey is the context key for the session's "view as member" flag; use MemberModeFromContext.
type memberModeCtxKey struct{}

// memberPreviewNoCredCtxKey is the context key for the no-credential posture of that flag; use MemberPreviewNoCredential.
type memberPreviewNoCredCtxKey struct{}

// expiryCtxKey is the context key for the session's expiry; use ExpiryFromContext.
type expiryCtxKey struct{}
