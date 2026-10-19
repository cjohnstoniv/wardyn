// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// OpenID Connect Back-Channel Logout 1.0: the identity provider posts a signed logout token to a
// URL the administrator registered with it, and Wardyn cuts that person's browser sessions the way a
// sign-out does (CutSessions). Mounted only when Config.BackchannelLogout is on.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const (
	// backchannelLogoutEvent is the key the logout token's events claim must carry.
	backchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"
	// backchannelBodyMax bounds the form body: a logout token is a few hundred bytes.
	backchannelBodyMax = 16 << 10
	// backchannelIATWindow is how far a logout token's iat may sit from now, either way.
	backchannelIATWindow = 5 * time.Minute
)

// BackchannelLogoutEnabled reports whether the logout route should be mounted at all.
func (a *Authenticator) BackchannelLogoutEnabled() bool { return a.cfg.BackchannelLogout }

// BackchannelLogoutHandler serves the logout endpoint. A token the provider signed for this client
// that names a sub cuts that person's browser sessions: 200 once cut, 400 for any token that is not
// one (the answer the specification names), 503 when the cut cannot be written. A token with a sid
// and no sub is refused, since sessions are not tracked by sid: that half of the specification stays
// unsupported. A sub reserved(sub) names is refused too, as the sign-in callback refuses it: a
// provider cannot spell a mechanism's principal into a cut. onCut, when set, hears each cut, with its
// error, for the audit row.
//
// ponytail: no jti replay cache. A token replayed inside backchannelIATWindow cuts the person's
// sessions again, ending a sign-in made since; the token only travels provider to wardynd over TLS.
func (a *Authenticator) BackchannelLogoutHandler(reserved func(sub string) bool, onCut func(r *http.Request, sub string, err error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, backchannelBodyMax)
		if err := r.ParseForm(); err != nil {
			backchannelRefuse(w, http.StatusBadRequest, "invalid_request", "the body is not a form")
			return
		}
		sub, problem := a.logoutTokenSub(r.Context(), r.PostForm.Get("logout_token"))
		if problem != "" {
			backchannelRefuse(w, http.StatusBadRequest, "invalid_request", problem)
			return
		}
		if reserved != nil && reserved(sub) {
			backchannelRefuse(w, http.StatusBadRequest, "invalid_request", "the sub names a reserved principal")
			return
		}
		if a.cfg.Revocations == nil {
			backchannelRefuse(w, http.StatusServiceUnavailable, "temporarily_unavailable", "session revocation is not configured")
			return
		}
		err := CutSessions(r.Context(), a.cfg.Revocations, sub)
		if onCut != nil {
			onCut(r, sub, err)
		}
		if err != nil {
			backchannelRefuse(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the session cut could not be written")
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// logoutTokenSub verifies raw as a logout token for this client and returns its sub, or the reason
// it is not one. The provider's signature, issuer, audience and expiry are the ID token verifier's.
func (a *Authenticator) logoutTokenSub(ctx context.Context, raw string) (sub, problem string) {
	if raw == "" {
		return "", "logout_token is missing"
	}
	tok, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return "", "the logout token did not verify"
	}
	var claims map[string]json.RawMessage
	var events, event map[string]json.RawMessage
	if err := tok.Claims(&claims); err != nil || json.Unmarshal(claims["events"], &events) != nil {
		return "", "the logout token claims are unreadable"
	}
	switch {
	// The event's value must be a JSON object: null, a string or a number is not one.
	case json.Unmarshal(events[backchannelLogoutEvent], &event) != nil || event == nil:
		return "", "the logout token carries no back-channel logout event object"
	case claims["nonce"] != nil:
		return "", "a logout token carries no nonce"
	case len(claims["jti"]) == 0:
		return "", "the logout token has no jti"
	case tok.IssuedAt.IsZero() || absDuration(time.Since(tok.IssuedAt)) > backchannelIATWindow:
		return "", "the logout token iat is too far from now"
	case tok.Subject == "":
		return "", "the logout token names no sub; sessions are not cut by sid"
	}
	return tok.Subject, ""
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func backchannelRefuse(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}
