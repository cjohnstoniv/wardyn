// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// OpenID Connect Front-Channel Logout 1.0: the identity provider's sign-out page loads a URL the
// administrator registered with it, in a hidden frame of the person's browser, naming its issuer
// (iss) and the provider session (sid). Wardyn cuts that person's browser sessions the way
// back-channel logout does (CutSessions). Mounted only when Config.FrontchannelLogout is on.
//
// The session is found by sid, never by the session cookie: the cookie is SameSite=Lax, which a
// browser does not send to a cross-site frame, and browsers that block third-party cookies would drop
// it anyway. So each sign-in records its ID token's sid against its person (SessionSIDs), and the
// logout reads that row, cuts, and only then deletes it. Nothing in the request is signed: a forged one can only sign a person
// out, never in, which is why it carries no token.

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SessionSIDs maps a sign-in's provider session id to the person it signed in (store.PG, migration 0229).
type SessionSIDs interface {
	// RecordOIDCSessionSID upserts the person a sign-in under issuer's sid established, kept until
	// expiresAt, the latest the session it signed in can still be live.
	RecordOIDCSessionSID(ctx context.Context, issuer, sid, sub string, expiresAt time.Time) error
	// LookupOIDCSessionSID returns issuer's sid row's person; ok is false with no row.
	LookupOIDCSessionSID(ctx context.Context, issuer, sid string) (sub string, ok bool, err error)
	// ForgetOIDCSessionSID deletes issuer's sid row, if any.
	ForgetOIDCSessionSID(ctx context.Context, issuer, sid string) error
}

// FrontchannelLogoutEnabled reports whether the front-channel logout route should be mounted at all.
func (a *Authenticator) FrontchannelLogoutEnabled() bool { return a.cfg.FrontchannelLogout }

// FrontchannelFrameOrigin is the issuer's origin (scheme://host), the one page allowed to frame the
// front-channel logout answer; empty, and so no page, when the issuer URL has no plain http(s) origin.
func (a *Authenticator) FrontchannelFrameOrigin() string {
	u, err := url.Parse(a.cfg.IssuerURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.ContainsAny(u.Host, " ;,'\"") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// recordSessionSID records an admitted sign-in's sid when front-channel logout is on, kept until
// sessionEnd, the latest sess can still be live: its expiry, or the renewal cap past sign-in when a
// renewal reaches further. A sign-in
// whose token has no sid, or whose row cannot be written, still signs in: only the front-channel
// logout of that session is lost, which is the setting-off behaviour, and the failure is logged.
func (a *Authenticator) recordSessionSID(ctx context.Context, cc callbackClaims, sess Session) {
	if !a.cfg.FrontchannelLogout || a.cfg.SessionSIDs == nil || cc.sid == "" {
		return
	}
	end := sess.Expiry
	if capEnd := sess.IssuedAt.Add(a.cfg.SessionRenewCap); a.cfg.SessionRenewCap > 0 && capEnd.After(end) {
		end = capEnd
	}
	if err := a.cfg.SessionSIDs.RecordOIDCSessionSID(ctx, cc.issuer, cc.sid, sess.Sub, end.UTC()); err != nil {
		slog.WarnContext(ctx, "oidc: the sign-in's sid was not recorded, so front-channel logout cannot end this session", "err", err)
	}
}

// FrontchannelLogoutHandler serves the front-channel logout endpoint. It needs both iss and sid, as a
// client registered with frontchannel_logout_session_required receives them, and iss must be this
// client's issuer: 400 otherwise. A sid that leads to a sign-in cuts that person's browser sessions,
// 200 once cut; a sid that leads to none answers 200 too, so a sid cannot be probed; 503 when the
// lookup or the cut cannot be done. The sid row is deleted only after the cut, so a failed cut keeps
// it and the provider's reloaded sign-out page retries (CutSessions twice is harmless). onCut, when set, hears each cut, with its error, for the audit row.
//
// The api layer lets only the issuer's origin frame this answer (FrontchannelFrameOrigin); every other
// route keeps frame-ancestors 'none'.
func (a *Authenticator) FrontchannelLogoutHandler(onCut func(r *http.Request, sub string, err error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store")
		w.Header().Set("Pragma", "no-cache")
		iss, sid := r.URL.Query().Get("iss"), r.URL.Query().Get("sid")
		switch {
		case iss == "" || sid == "":
			backchannelRefuse(w, http.StatusBadRequest, "invalid_request", "iss and sid are both required; the session is found by sid")
			return
		case iss != a.cfg.IssuerURL:
			backchannelRefuse(w, http.StatusBadRequest, "invalid_request", "iss is not this client's issuer")
			return
		case a.cfg.Revocations == nil || a.cfg.SessionSIDs == nil:
			backchannelRefuse(w, http.StatusServiceUnavailable, "temporarily_unavailable", "session revocation is not configured")
			return
		}
		sub, ok, err := a.cfg.SessionSIDs.LookupOIDCSessionSID(r.Context(), iss, sid)
		if err != nil {
			backchannelRefuse(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the session lookup failed")
			return
		}
		if !ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		err = CutSessions(r.Context(), a.cfg.Revocations, sub)
		if onCut != nil {
			onCut(r, sub, err)
		}
		if err != nil {
			backchannelRefuse(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the session cut could not be written")
			return
		}
		if err := a.cfg.SessionSIDs.ForgetOIDCSessionSID(r.Context(), iss, sid); err != nil {
			slog.WarnContext(r.Context(), "oidc: the sid row of a front-channel logout was not deleted; the prune takes it", "err", err)
		}
		w.WriteHeader(http.StatusOK)
	}
}
