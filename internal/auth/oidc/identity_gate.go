// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// The leaver gate (migration 0113, 0127): a deactivated identity signs in nowhere, and a session
// issued under an authority epoch that a later suspension has passed stops at its next request.

// DenialIdentityDeactivated is the auth_error code admit returns, and the reason it reports to
// onDenied, for a sign-in whose identity is deactivated or purged. The callback shows the browser
// sign_in_refused instead (the page does not say the identity is deactivated); a portal's token
// exchange records this code.
const DenialIdentityDeactivated = "identity_deactivated"

// ErrIdentityDeactivated is Issue's refusal of a deactivated or purged identity.
var ErrIdentityDeactivated = errors.New("oidc: identity deactivated")

// ErrIdentityConflict is Issue's refusal of a sign-in whose identity clashes with another binding
// (the same principal under a different object id).
var ErrIdentityConflict = errors.New("oidc: identity clashes with another binding")

// IdentityRef is who a sign-in names, as the gate's read-only admission check needs it.
type IdentityRef struct {
	Issuer, TenantID, ObjectID, Principal string
}

// IdentityGate is the per-identity half of admission, for every issuer. nil (the default) is no
// gate: sign-ins record no identity row and carry no epoch.
type IdentityGate interface {
	// Refused reports, without writing, whether the identity a sign-in names is deactivated or
	// purged. An error fails the sign-in closed.
	Refused(ctx context.Context, ref IdentityRef) (bool, error)
	// Issue upserts and binds the sign-in's identity row, re-checks its deactivation and returns
	// the authority epoch the session cookie must carry, all under the row's lock so a suspension
	// either precedes it (ErrIdentityDeactivated) or leaves the cookie's epoch stale. An error
	// fails the sign-in closed.
	Issue(ctx context.Context, f LoginFacts) (epoch int64, err error)
}

// identityRef is who a verified token names once admission has resolved its principal. Only an Entra
// token carrying a tenant and a well-formed object id is keyed by them (loginFacts holds the same rule).
func (a *Authenticator) identityRef(principal string, cc callbackClaims) IdentityRef {
	ref := IdentityRef{Issuer: cmp.Or(cc.issuer, a.cfg.IssuerURL), Principal: principal}
	if id, err := uuid.Parse(cc.oid); a.entra && cc.tid != "" && err == nil {
		ref.TenantID, ref.ObjectID = cc.tid, id.String()
	}
	return ref
}

// admitIdentity is admit's gate step, after the principal is resolved and before any derivation.
// denied is the auth_error code of a refusal, "" when the sign-in may proceed.
func (a *Authenticator) admitIdentity(r *http.Request, f IdentityRef, onDenied func(*http.Request, string)) string {
	if a.cfg.Identities == nil {
		return ""
	}
	refused, err := a.cfg.Identities.Refused(r.Context(), f)
	if err != nil {
		slog.Error("oidc: identity check unavailable, denying login (fail closed)", "error", err)
		return authErrorRoleCheckUnavailable
	}
	if refused {
		slog.Warn("oidc: login denied — the identity is deactivated", "issuer", f.Issuer)
		deny(onDenied, r, DenialIdentityDeactivated)
		return DenialIdentityDeactivated
	}
	return ""
}

// issueIdentity is the callback's issuance step. denied is the auth_error code of a refusal; the
// epoch is stamped on the cookie and rides the context to the login-grant sink.
func (a *Authenticator) issueIdentity(r *http.Request, f LoginFacts, onDenied func(*http.Request, string)) (epoch int64, denied string) {
	if a.cfg.Identities == nil {
		return 0, ""
	}
	epoch, err := a.cfg.Identities.Issue(r.Context(), f)
	switch {
	case err == nil:
		return epoch, ""
	case errors.Is(err, ErrIdentityDeactivated):
		slog.Warn("oidc: login denied at issuance — the identity was deactivated", "issuer", f.Issuer)
		deny(onDenied, r, DenialIdentityDeactivated)
		return 0, authErrorSignInRefused
	case errors.Is(err, ErrIdentityConflict):
		slog.Warn("oidc: login denied — the identity clashes with another binding", "issuer", f.Issuer, "sub", f.Sub)
		return 0, authErrorSignInRefused
	}
	slog.Error("oidc: identity record unavailable, denying login (fail closed)", "error", err)
	return 0, authErrorRoleCheckUnavailable
}

// SessionStatus is what a credential's owner check answers.
type SessionStatus int

// The three answers: usable, cut off by a revocation cutoff, or refused by the identity row.
const (
	SessionLive SessionStatus = iota
	SessionRevoked
	SessionDeactivated
)

// IdentityRevocations is SessionRevocations that also reads the owner's identity rows in the same
// statement, so the identity check adds no round trip. epoch < 0 asks only whether the identity
// is deactivated or purged; otherwise a row whose authority epoch is past epoch refuses too.
type IdentityRevocations interface {
	SessionStatus(ctx context.Context, sub, email string, issuedAt time.Time, epoch int64) (SessionStatus, error)
}

// SessionCutter is SessionRevocations that can also end a person's browser sessions without ending their
// API tokens and SSH keys. A cut is read by IsSessionRevoked and by SessionStatus for a credential that
// carries an epoch (a session cookie), never by SessionStatus for one that does not (epoch < 0: an API
// token, an SSH key). A mover keeps the tokens whose group snapshot does not hold the group they left, so
// the group removal cuts with this and revokes the tokens it means to one by one.
type SessionCutter interface {
	// CutSessions invalidates every current browser session for sub, matched against both identities.
	CutSessions(ctx context.Context, sub string) error
}

// CutSessions cuts sub's browser sessions. A rev that cannot cut without touching tokens (a test double)
// falls back to RevokeSub, which ends the person's tokens and keys too: the safe direction.
func CutSessions(ctx context.Context, rev SessionRevocations, sub string) error {
	if c, ok := rev.(SessionCutter); ok {
		return c.CutSessions(ctx, sub)
	}
	return rev.RevokeSub(ctx, sub)
}

// CheckSession is the one owner check every credential lane asks: the revocation cutoff and, when
// rev can answer it, the owner's identity. A rev that cannot (a test double) checks the cutoff only.
func CheckSession(ctx context.Context, rev SessionRevocations, sub, email string, issuedAt time.Time, epoch int64) (SessionStatus, error) {
	if ir, ok := rev.(IdentityRevocations); ok {
		return ir.SessionStatus(ctx, sub, email, issuedAt, epoch)
	}
	revoked, err := rev.IsSessionRevoked(ctx, sub, email, issuedAt)
	if err != nil || !revoked {
		return SessionLive, err
	}
	return SessionRevoked, nil
}
