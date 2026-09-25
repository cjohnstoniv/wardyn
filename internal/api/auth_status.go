// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// What the public API answers a caller whose SESSION COOKIE was presented and
// rejected, and what it answers when a handler's store read fails. Both are
// response-shaping helpers that belong beside writeError in http.go and live
// here instead only because that file sits against the 1000-line gate.

// The four reasons oidc.Middleware publishes on the request context
// (oidc.SessionRejectedFromContext). Mirrored as constants rather than compared
// as literals so the values have names where they are USED and a rename
// upstream is a grep away rather than a silent no-op.
//
// sessionRevocationUnavailable is declared in http.go, beside the auth.fail
// emit that already branches on it.
const (
	sessionExpired = "expired_session"
	sessionRevoked = "revoked_session"
	sessionInvalid = "invalid_session"
)

// DRAFT (M2 canon pending)
//
// The bodies a rejected session gets. Before these, an OIDC-only deployment
// (admin token unset — which cmd/wardynd's own startup line recommends)
// answered expired, revoked, tampered and unverifiable sessions ALL with
// "admin token not configured; public API disabled": a sentence about the
// deployment's configuration, handed to a human whose session simply ran out,
// while the computed reason went only to the audit row.
//
// sessionUnavailableMsg is the one that is not a 401 at all. An
// IsSessionRevoked store error yields no principal, so the request fell through
// to adminAuth and answered 401 "missing bearer token" — the console read that
// as "unauthed", showed the sign-in gate, the human signed in successfully (the
// OIDC callback never consults revocations), and the next request 401'd again:
// a sign-in LOOP through a Postgres incident, which the console's own contract
// says must never be reported as a credential problem.
const (
	sessionExpiredMsg     = "your session expired — sign in again"
	sessionRevokedMsg     = "your session was revoked — sign in again"
	sessionInvalidMsg     = "your session could not be verified — sign in again"
	sessionUnavailableMsg = "this deployment cannot check whether your session was revoked, so it cannot " +
		"authenticate you right now — this is a control-plane database problem, not a problem with your " +
		"credentials; retry shortly"
)

// sessionRejectionResponse maps one of oidc.Middleware's rejection reasons to
// the answer the caller gets, and reports whether this branch owns the answer
// at all.
//
// ok=false for "" (no cookie was presented — the ordinary CLI/API client, which
// must keep the generic bearer answer) and for any reason this build does not
// know: a reason added upstream falls through to the unchanged adminAuth path
// rather than being answered by a default that guesses its severity.
func sessionRejectionResponse(reason string) (status int, msg string, ok bool) {
	switch reason {
	case sessionRevocationUnavailable:
		// 503, NOT 401: the credential may well be perfectly good; this
		// deployment simply cannot decide. Retry-later is the honest answer and
		// it is what stops the sign-in loop.
		return http.StatusServiceUnavailable, sessionUnavailableMsg, true
	case sessionExpired:
		return http.StatusUnauthorized, sessionExpiredMsg, true
	case sessionRevoked:
		return http.StatusUnauthorized, sessionRevokedMsg, true
	case sessionInvalid:
		return http.StatusUnauthorized, sessionInvalidMsg, true
	}
	return 0, "", false
}

// rejectedSessionAnswer is sessionRejectionResponse for a whole request: it
// reads the reason oidc.Middleware published and refuses to answer when the
// caller ALSO presented a bearer token, so a valid CLI/API token arriving
// alongside a stale browser cookie still authenticates through adminAuth
// exactly as it did before.
// The reason is returned alongside the answer so the caller can hand it to
// auditAuthFailed by NAME: the auth.fail reason enum is guarded against
// docs/AUDIT-ACTIONS.md by a source walk, and passing "" there (letting
// auditAuthFailedAs re-resolve it from the same context) reads to that guard as
// an unnamed reason.
func rejectedSessionAnswer(r *http.Request) (reason string, status int, msg string, ok bool) {
	if _, hasBearer := bearerToken(r); hasBearer {
		return "", 0, "", false
	}
	reason = oidc.SessionRejectedFromContext(r.Context())
	status, msg, ok = sessionRejectionResponse(reason)
	return reason, status, msg, ok
}

// auditRejectedSession emits this boundary's auth.fail row for a rejected
// session cookie.
//
// The switch is not ceremony. auditAuthFailedAs substitutes the context's
// rejection reason for whatever it is handed, so one call passing a variable
// would be correct at runtime — but the auth.fail `reason` enum is fenced
// against docs/AUDIT-ACTIONS.md by a SOURCE walk (cmd/wardynd's
// TestAuthFailedReasonEnumIsDocumented), which can only see reasons written as
// literals or named constants at a call site. Each arm therefore passes the
// reason it actually is, which is both true and visible to the fence.
func (s *Server) auditRejectedSession(r *http.Request, reason string) {
	switch reason {
	case sessionRevocationUnavailable:
		s.auditAuthFailed(r, sessionRevocationUnavailable)
	case sessionExpired:
		s.auditAuthFailed(r, sessionExpired)
	case sessionRevoked:
		s.auditAuthFailed(r, sessionRevoked)
	default:
		s.auditAuthFailed(r, sessionInvalid)
	}
}
