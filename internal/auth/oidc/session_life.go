// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The console session's lifetime behaviours: activity renewal within an absolute cap
// (Config.SessionRenewCap), the inactivity timeout (Config.SessionIdleTimeout), the silent
// re-authentication attempt (Config.SilentReauth), the login hint (Config.LoginHint) and the
// browser-session cookie (Config.BrowserSessionCookie, in encodeSession). Each is off at its zero
// value, and off writes the same headers and cookies as a build without it.
//
// Renewal moves only Expiry: IssuedAt, AuthorityEpoch and RevocationSeq stay as minted, so a
// revocation cut or suspension ends every copy of a renewed cookie exactly as it ends the original.
//
// The idle timeout bounds an unattended browser left signed in. It is no defence against a copied
// cookie: whoever holds a copy can keep it active, though never past its expiry or the renewal cap.
package oidc

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// deviceMarkerMaxAge is how long the reauth and hint cookies outlive the sign-in that set them.
const deviceMarkerMaxAge = 30 * 24 * time.Hour

// maxLoginHint bounds the login_hint read back from the hint cookie.
const maxLoginHint = 256

// sessionActivity reports whether a request counts as the person using the console: a write (any
// method but GET, HEAD and OPTIONS) the browser labels same-origin. Sec-Fetch-Site is set by the
// browser and no page script can set it, so neither the console's background reads nor a forged
// cross-site request renews a session or resets its idle clock.
//
// ponytail: writes only, so a person who only reads meets the idle timeout; pairing this check
// with a console-declared activity signal is the upgrade.
func sessionActivity(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// lastActive is when the session last saw console activity: LastActive, else IssuedAt.
func lastActive(sess Session) time.Time {
	if sess.LastActive != 0 {
		return time.Unix(sess.LastActive, 0).UTC()
	}
	return sess.IssuedAt
}

// idleExpired reports whether SessionIdleTimeout has passed since the session's last activity. A
// session with no IssuedAt and no activity is older than any timeout.
func (a *Authenticator) idleExpired(sess Session, now time.Time) bool {
	idle := a.cfg.SessionIdleTimeout
	if idle <= 0 {
		return false
	}
	last := lastActive(sess)
	return last.IsZero() || now.Sub(last) > idle
}

// touchSession re-mints the session cookie for a request that is console activity, when the idle
// clock or the renewal moves it, and returns the session the request carries on.
//
// The idle clock is re-stamped at most once a tenth of the timeout, so the cookie is not rewritten on
// every write; a session can therefore end up to a tenth of the timeout early. The member-mode view
// fields are re-minted as the request carried them: a re-mint racing a view switch can restore the
// view (or, racing an entry, the person's own tier), which never raises the effective role above the
// person's role, and the next switch corrects it.
func (a *Authenticator) touchSession(w http.ResponseWriter, r *http.Request, sess Session) Session {
	if (a.cfg.SessionIdleTimeout <= 0 && a.cfg.SessionRenewCap <= 0) || !sessionActivity(r) {
		return sess
	}
	now := time.Now().UTC()
	next, changed := sess, false
	if idle := a.cfg.SessionIdleTimeout; idle > 0 && now.Sub(lastActive(sess)) >= idle/10 {
		next.LastActive, changed = now.Unix(), true
	}
	if expiry, span, ok := a.renewedExpiry(sess, now); ok {
		next.Expiry, next.RenewSpan, changed = expiry, span, true
	}
	if !changed {
		return sess
	}
	cookie, err := a.encodeSession(next)
	if err != nil {
		return sess
	}
	http.SetCookie(w, cookie)
	return next
}

// renewedExpiry is the later expiry SessionRenewCap gives a session past half its span, and that
// span in seconds. The span is the session's original length; the result never passes IssuedAt plus
// the cap, nor IssuedAt plus RoleStampTTL, after which Middleware refuses the session anyway.
func (a *Authenticator) renewedExpiry(sess Session, now time.Time) (time.Time, int64, bool) {
	limit := a.cfg.SessionRenewCap
	if limit <= 0 || sess.IssuedAt.IsZero() {
		return time.Time{}, 0, false
	}
	span := time.Duration(sess.RenewSpan) * time.Second
	if span <= 0 {
		span = sess.Expiry.Sub(sess.IssuedAt).Truncate(time.Second)
	}
	if span <= 0 || sess.Expiry.Sub(now) >= span/2 {
		return time.Time{}, 0, false
	}
	deadline := sess.IssuedAt.Add(limit)
	if ttl := a.cfg.RoleStampTTL; ttl > 0 && sess.IssuedAt.Add(ttl).Before(deadline) {
		deadline = sess.IssuedAt.Add(ttl)
	}
	expiry := now.Add(span)
	if expiry.After(deadline) {
		expiry = deadline
	}
	if !expiry.After(sess.Expiry) {
		return time.Time{}, 0, false
	}
	return expiry, int64(span / time.Second), true
}

// deviceCookie is a marker that outlives the session (reauthCookieName, hintCookieName), with every
// login cookie's posture but a longer life.
func (a *Authenticator) deviceCookie(name, value string) *http.Cookie {
	c := a.loginCookie(name, value)
	c.MaxAge = int(deviceMarkerMaxAge / time.Second)
	return c
}

// hasCookie reports whether the request carries a non-empty cookie of that name.
func (a *Authenticator) hasCookie(r *http.Request, name string) bool {
	c, err := r.Cookie(a.cookieName(name))
	return err == nil && c.Value != ""
}

// silentEligible reports whether this sign-in may first ask with prompt=none: SilentReauth is on and
// this browser completed a sign-in it has not signed out of since. The marker is set only by a
// completed sign-in and cleared by sign-out, so after an explicit sign-out the next sign-in is
// interactive.
func (a *Authenticator) silentEligible(r *http.Request) bool {
	return a.cfg.SilentReauth && a.hasCookie(r, reauthCookieName)
}

// reauthOptions are startLogin's login_hint and prompt=none parameters. A silent request sets the
// marker the callback reads; a non-silent one clears a marker an earlier attempt left.
func (a *Authenticator) reauthOptions(w http.ResponseWriter, r *http.Request, silent bool) []oauth2.AuthCodeOption {
	var opts []oauth2.AuthCodeOption
	if hint := a.loginHint(r); hint != "" {
		opts = append(opts, oauth2.SetAuthURLParam("login_hint", hint))
	}
	if silent {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "none"))
		http.SetCookie(w, a.loginCookie(silentCookieName, "1"))
	} else if a.hasCookie(r, silentCookieName) {
		a.expireCookie(w, silentCookieName)
	}
	return opts
}

// loginHint is the account the hint cookie names, or "" when LoginHint is off or none is readable.
func (a *Authenticator) loginHint(r *http.Request) string {
	if !a.cfg.LoginHint {
		return ""
	}
	c, err := r.Cookie(a.cookieName(hintCookieName))
	if err != nil {
		return ""
	}
	hint, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(hint) > maxLoginHint {
		return ""
	}
	return string(hint)
}

// consumeSilentMarker reports whether this callback answers a prompt=none request, expiring the
// marker when present.
func (a *Authenticator) consumeSilentMarker(w http.ResponseWriter, r *http.Request) bool {
	if !a.hasCookie(r, silentCookieName) {
		return false
	}
	a.expireCookie(w, silentCookieName)
	return true
}

// silentFallback restarts a refused prompt=none sign-in as the ordinary interactive one, once: the
// restart is never silent, so a provider that keeps refusing reaches its own sign-in page.
func (a *Authenticator) silentFallback(w http.ResponseWriter, r *http.Request, silent bool, code string) bool {
	if !silent {
		return false
	}
	slog.Info("oidc: the identity provider could not sign in without interaction; continuing with the interactive sign-in",
		"issuer", a.cfg.IssuerURL, "error", code)
	a.startLogin(w, r, true, false)
	return true
}

// rememberDevice sets, after a completed sign-in, the markers SilentReauth and LoginHint read.
func (a *Authenticator) rememberDevice(w http.ResponseWriter, cc callbackClaims) {
	if a.cfg.SilentReauth {
		http.SetCookie(w, a.deviceCookie(reauthCookieName, "1"))
	}
	if a.cfg.LoginHint && cc.loginHint != "" && len(cc.loginHint) <= maxLoginHint {
		http.SetCookie(w, a.deviceCookie(hintCookieName, base64.RawURLEncoding.EncodeToString([]byte(cc.loginHint))))
	}
}

// forgetDevice clears, at sign-out, every marker the request carries, whatever the settings are now.
func (a *Authenticator) forgetDevice(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{reauthCookieName, hintCookieName, silentCookieName} {
		if a.hasCookie(r, name) {
			a.clearCookie(w, name)
		}
	}
}

// expireCookie deletes a login-scoped marker with every login cookie's posture.
func (a *Authenticator) expireCookie(w http.ResponseWriter, name string) {
	stale := a.loginCookie(name, "")
	stale.MaxAge = -1
	http.SetCookie(w, stale)
}
