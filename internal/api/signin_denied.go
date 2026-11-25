// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The auth.fail reasons the sign-in callback emits: the role map named a user
// type for this person that is ambiguous, or that does not exist; the
// person's subject is a reserved principal (isReservedPrincipal); or the email
// policy refused it (#155: no email_verified claim, email_verified false, an
// email outside the allowed domains). Equal to oidc.DenialUserTypeAmbiguous /
// DenialUserTypeUnknown / DenialReservedPrincipal / DenialEmailVerifiedAbsent /
// DenialEmailUnverified / DenialEmailDomain;
// restated here so the reason enum stays readable from this package alone.
// authFailedReservedPrincipal is also the reason a session or wdn_ token
// carrying a reserved principal is refused with at request time.
const (
	authFailedUserTypeAmbiguous   = "user_type_ambiguous"
	authFailedUserTypeUnknown     = oidc.DenialUserTypeUnknown
	authFailedReservedPrincipal   = oidc.DenialReservedPrincipal
	authFailedEmailVerifiedAbsent = oidc.DenialEmailVerifiedAbsent
	authFailedEmailUnverified     = oidc.DenialEmailUnverified
	authFailedEmailDomain         = oidc.DenialEmailDomain
)

// auditSignInDenied records a sign-in the callback refused over its user type,
// a reserved subject or the email policy as auth.fail. Any other reason records nothing: the
// row's enum is closed.
func (s *Server) auditSignInDenied(r *http.Request, reason string) {
	switch reason {
	case oidc.DenialUserTypeAmbiguous:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedUserTypeAmbiguous)
	case oidc.DenialUserTypeUnknown:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedUserTypeUnknown)
	case oidc.DenialReservedPrincipal:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedReservedPrincipal)
	case oidc.DenialEmailVerifiedAbsent:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedEmailVerifiedAbsent)
	case oidc.DenialEmailUnverified:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedEmailUnverified)
	case oidc.DenialEmailDomain:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedEmailDomain)
	case oidc.DenialIdentityDeactivated:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedIdentityDeactivated)
	}
}

// auditIdPLogout records a back- or front-channel logout from the identity provider as the
// session.revoke row an admin's sessions-only revoke writes, marked with its source.
func (s *Server) auditIdPLogout(r *http.Request, source, sub string, err error) {
	s.auditIdPLogoutCtx(r.Context(), source, sub, err, nil)
}

// auditIdPLogoutCtx is auditIdPLogout for a cut that has no request: a held back-channel logout applied once
// the provider answered. extra adds fields to the row's data (queued_at and token_sha256 for a held one).
func (s *Server) auditIdPLogoutCtx(ctx context.Context, source, sub string, err error, extra map[string]any) {
	data := map[string]any{"scope": "sub", "sub": sub, "sessions_only": true, "tokens_revoked": 0, "ssh_keys_deleted": 0,
		"cli_sessions_ended": store.CLIEndedFrom(ctx).Count(), "source": source}
	for k, v := range extra {
		data[k] = v
	}
	outcome := "success"
	if err != nil {
		outcome, data["error"] = "failure", auditStep(ctx, auditStepSignOut, err, slog.String("sub", sub), slog.String("source", source))
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, oidcCallbackActor, "session.revoke", sub, outcome, mustJSON(data)))
}

// backchannelLogoutRatePerSec and backchannelLogoutBurst bound, process-wide, the logout tokens that
// fail to verify on the anonymous back-channel logout route: each one can make the verifier refetch
// the provider's keys. Past the bound a POST is refused before it is verified. A token that verifies
// gives its charge back, so a provider's own burst of logouts is refused only while forged ones are.
const (
	backchannelLogoutRatePerSec = 1.0
	backchannelLogoutBurst      = 5.0
)

// backchannelExemptKey marks a back-channel request the limiter did not charge, because the authenticator is
// pending and has room to hold the token (see backchannelLogout).
type backchannelExemptKey struct{}

// backchannelExempt is the value of backchannelExemptKey; charged is set when the request was charged to the
// limiter afterwards, so a successful cut refunds it like any other.
type backchannelExempt struct{ charged atomic.Bool }

// backchannelLogout serves POST /auth/backchannel-logout within backchannelLimiter, with 429 past it. While the
// authenticator is pending (the provider has not been reached) and its queue has room, the route is exempt from
// the limiter: a provider signing out many people at once must not lose the later ones to 429, and the queue's
// own cap bounds the intake. A POST that then fails the shape checks is charged after the fact (logoutQueueSink).
func (s *Server) backchannelLogout() http.HandlerFunc {
	s.cfg.OIDC.AttachLogoutQueueSink(logoutQueueSink{s: s})
	next := s.cfg.OIDC.BackchannelLogoutHandler(s.isReservedPrincipal, func(r *http.Request, sub string, err error) {
		if ex, _ := r.Context().Value(backchannelExemptKey{}).(*backchannelExempt); ex == nil || ex.charged.Load() {
			s.backchannelLimiter.refund("", s.cfg.Now())
		}
		s.auditIdPLogout(r, "idp_back_channel_logout", sub, err)
	})
	return func(w http.ResponseWriter, r *http.Request) {
		pending := s.cfg.OIDC.Pending()
		exempt := pending && s.cfg.OIDC.LogoutQueueStats().Held < oidc.LogoutQueueCap
		if !exempt && !s.backchannelLimiter.allow("", s.cfg.Now()) {
			if pending {
				s.cfg.OIDC.NoteLogoutRateLimited(r.Context())
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "temporarily_unavailable", "error_description": "too many logout tokens; retry later"})
			return
		}
		ctx, _ := store.WithCLIEnded(r.Context())
		if exempt {
			ctx = context.WithValue(ctx, backchannelExemptKey{}, &backchannelExempt{})
		}
		next(w, r.WithContext(ctx))
	}
}

// logoutQueueSink writes the audit rows of the held back-channel logouts and applies a verified one.
type logoutQueueSink struct{ s *Server }

// ChargeLogout charges the limiter for a request that skipped it under the pending exemption but reached the
// discovered path, and for a held logout that arrived during the drain and is checked after the publish. A
// request the limiter already charged passes. It reports false when the limiter refuses.
func (k logoutQueueSink) ChargeLogout(ctx context.Context) bool {
	ex, _ := ctx.Value(backchannelExemptKey{}).(*backchannelExempt)
	if ex == nil && !oidc.LogoutLate(ctx) {
		return true
	}
	if !k.s.backchannelLimiter.allow("", k.s.cfg.Now()) {
		return false
	}
	if ex != nil {
		ex.charged.Store(true)
	}
	return true
}

// Noted writes one session.revoke.hold row per queued, refused or unverifiable logout, and one
// session.revoke_hold.coalesce row per rate-limit summary. The target is never a person and never caller text:
// logout_token:<sha256 of the token>, or the fixed route name when no token was posted. The unverified sub is
// data.sub_unverified, which the seal stores as a sponsor field. A malformed POST, or one whose sub is reserved,
// that came in under the pending exemption is charged to the limiter here; past it, it writes no row and is counted as rate limited.
func (k logoutQueueSink) Noted(ctx context.Context, ev oidc.LogoutQueueEvent) {
	s := k.s
	switch ev.State {
	case oidc.LogoutApplied:
		return // the cut's own session.revoke row is its record (Apply)
	case oidc.LogoutRateLimitedSummary:
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, oidcCallbackActor, "session.revoke_hold.coalesce", backchannelLogoutTarget, "failure",
			mustJSON(map[string]any{"count": ev.Count, "window_start": ev.WindowStart, "window_end": ev.WindowEnd})))
		return
	case oidc.LogoutRefusedUnreadable, oidc.LogoutRefusedReserved:
		if ctx.Value(backchannelExemptKey{}) != nil && !s.backchannelLimiter.allow("", s.cfg.Now()) {
			s.cfg.OIDC.NoteLogoutRateLimited(ctx)
			return
		}
	}
	target := backchannelLogoutTarget
	data := map[string]any{"state": ev.State, "received_at": ev.ReceivedAt, "sub_verified": false}
	if ev.TokenSHA256 != "" {
		target = "logout_token:" + ev.TokenSHA256
		data["token_sha256"] = ev.TokenSHA256
	}
	if ev.SubUnverified != "" {
		data["sub_unverified"] = ev.SubUnverified
	}
	if ev.Problem != "" {
		data["problem"] = ev.Problem
	}
	outcome := "failure"
	if ev.State == oidc.LogoutQueued {
		outcome = "success"
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, oidcCallbackActor, "session.revoke.hold", target, outcome, mustJSON(data)))
}

// Apply cuts sub's sessions for a held logout, as the discovered path does, and records the cut as the ordinary
// session.revoke row with the time the token was queued and its hash, which pairs it with its hold row.
func (k logoutQueueSink) Apply(ctx context.Context, sub string, receivedAt time.Time, sha string) error {
	ctx, _ = store.WithCLIEnded(ctx)
	if k.s.cfg.SessionRevocations == nil {
		return errors.New("session revocation is not configured")
	}
	err := oidc.CutSessions(ctx, k.s.cfg.SessionRevocations, sub)
	k.s.auditIdPLogoutCtx(ctx, "idp_back_channel_logout", sub, err, map[string]any{"queued_at": receivedAt, "token_sha256": sha})
	return err
}

// backchannelLogoutTarget is the fixed target of a hold row that names no token, and of the summary row.
const backchannelLogoutTarget = "backchannel_logout"

// mountFrontchannelLogout mounts GET /auth/frontchannel-logout, the identity provider's front-channel
// logout loaded in the person's browser, only when WARDYN_OIDC_FRONTCHANNEL_LOGOUT is on.
func (s *Server) mountFrontchannelLogout(r chi.Router) {
	if s.cfg.OIDC.FrontchannelLogoutEnabled() {
		r.Get("/auth/frontchannel-logout", s.frontchannelLogout())
	}
}

// frontchannelLogout serves GET /auth/frontchannel-logout within frontchannelLimiter, bounded like the
// back-channel route: every request spends a charge and a cut gives it back, so requests naming no
// sign-in (forged or stale sids) are refused with 429 past backchannelLogoutBurst, then one a second.
// Every answer may be framed by the identity provider's origin, and by no other.
func (s *Server) frontchannelLogout() http.HandlerFunc {
	next := s.cfg.OIDC.FrontchannelLogoutHandler(func(r *http.Request, sub string, err error) {
		s.frontchannelLimiter.refund("", s.cfg.Now())
		s.auditIdPLogout(r, "idp_front_channel_logout", sub, err)
	})
	origin := s.cfg.OIDC.FrontchannelFrameOrigin()
	return func(w http.ResponseWriter, r *http.Request) {
		allowFramingBy(w.Header(), origin)
		if !s.frontchannelLimiter.allow("", s.cfg.Now()) {
			w.Header().Set("Cache-Control", "no-cache, no-store")
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "temporarily_unavailable", "error_description": "too many logout requests; retry later"})
			return
		}
		ctx, _ := store.WithCLIEnded(r.Context())
		next(w, r.WithContext(ctx))
	}
}
