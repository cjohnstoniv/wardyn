// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"cmp"
	"net/http"
)

// hostCookiePrefix is the RFC 6265bis cookie prefix a browser only stores
// from a Set-Cookie that is Secure, Path=/ and carries no Domain attribute.
const hostCookiePrefix = "__Host-"

// CookieName is the name a console cookie is written and read under. With
// secure cookies on it carries the __Host- prefix, so the browser holds it
// host-only: no sibling host under the console's registrable domain (a
// relayed sandbox page among them) can plant a Domain= cookie the console
// would read as its session or its sign-in state. The plain name is never
// read in that posture, not even as a fallback, because the plain name is
// exactly what a sibling can plant. Over plain HTTP (secure off) a browser
// refuses every __Host- cookie, so the plain name is the only one that works.
func CookieName(secure bool, name string) string {
	if secure {
		return hostCookiePrefix + name
	}
	return name
}

// CookiePath is the Path of every console cookie. __Host- requires Path=/,
// so with secure cookies on it is "/" even under WARDYN_BASE_PATH: a
// neighbouring application on the console's own host then receives the
// cookies too, which grants it nothing it lacks already, since it shares the
// console's origin and can call the API with them attached. Over plain HTTP
// the cookies stay scoped to the base path.
func CookiePath(secure bool, basePath string) string {
	if secure {
		return "/"
	}
	return cmp.Or(basePath, "/")
}

func (a *Authenticator) cookieName(name string) string {
	return CookieName(a.cfg.SecureCookies, name)
}

func (a *Authenticator) cookiePath() string {
	return CookiePath(a.cfg.SecureCookies, a.cfg.BasePath)
}

// loginCookie returns a short-lived HttpOnly SameSite=Lax cookie. These are
// one-time cookies used during the login flow; they expire after 10 minutes.
// Secure is set from cfg.SecureCookies so the login leg matches the session
// cookie: marked Secure only under TLS (direct or terminated), false over plain
// HTTP (else the browser drops them and the demo login breaks).
func (a *Authenticator) loginCookie(name, value string) *http.Cookie {
	return &http.Cookie{
		Name:     a.cookieName(name),
		Value:    value,
		Path:     a.cookiePath(),
		MaxAge:   600, // 10 minutes
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cfg.SecureCookies,
	}
}

// clearCookieAt instructs the browser to delete a named cookie at path. It is
// Secure whenever the cookie was: a browser ignores a __Host- Set-Cookie that
// is not, so an unmarked clear would leave the cookie in place.
func (a *Authenticator) clearCookieAt(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: a.cookieName(name), Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.cfg.SecureCookies,
	})
}

// clearCookie instructs the browser to delete a named cookie at the cookie path.
func (a *Authenticator) clearCookie(w http.ResponseWriter, name string) {
	a.clearCookieAt(w, name, a.cookiePath())
}

// clearSessionCookie clears wardyn_session at the cookie path and, when that
// is a base path, ALSO at Path=/ — a session from before a same-host
// migration onto the base carries that path, and cookie identity is
// name+domain+path (RFC 6265), so the base-path clear alone would leave it
// authenticating. No extra header when the cookie path is already "/".
func (a *Authenticator) clearSessionCookie(w http.ResponseWriter) {
	a.clearCookie(w, sessionCookieName)
	if a.cookiePath() != "/" {
		a.clearCookieAt(w, sessionCookieName, "/")
	}
}
