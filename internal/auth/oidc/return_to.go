// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"encoding/base64"
	"net/http"
	"net/url"
	pathpkg "path"
	"strings"
)

// maxReturnTo bounds the path a sign-in remembers; a longer one is dropped, not cut.
const maxReturnTo = 512

// safeReturnTo is path when it is a console route a sign-in may send the browser back to, "" otherwise.
// Default-deny: one leading "/" and nothing that could name another origin (a second "/", a backslash,
// a scheme, a host) or split a header (a control character). The auth and API routes are not pages.
func safeReturnTo(path string) string {
	if path == "" || len(path) > maxReturnTo || path[0] != '/' || strings.HasPrefix(path, "//") {
		return ""
	}
	for _, c := range path {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return ""
		}
	}
	u, err := url.Parse(path)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return ""
	}
	// A dot segment is refused outright, encoded or not (u.Path is decoded): a browser resolves it, so
	// /x/../api/... or, under a base path, /../wardyn/api/... would land on a route refused below.
	if clean := pathpkg.Clean(u.Path); clean != u.Path && clean+"/" != u.Path {
		return ""
	}
	for _, route := range []string{"/auth", "/api"} {
		if u.Path == route || strings.HasPrefix(u.Path, route+"/") {
			return ""
		}
	}
	return path
}

// rememberReturnTo keeps the page a sign-in began from (/auth/login?return_to=) for the callback. A
// login without a usable one clears an earlier attempt's, so it never lands on a stale page. Only
// LoginHandler calls it: the callback's own restarts must keep the cookie.
func (a *Authenticator) rememberReturnTo(w http.ResponseWriter, r *http.Request) {
	if p := safeReturnTo(r.URL.Query().Get("return_to")); p != "" {
		http.SetCookie(w, a.loginCookie(returnCookieName, base64.RawURLEncoding.EncodeToString([]byte(p))))
	} else if a.hasCookie(r, returnCookieName) {
		a.expireCookie(w, returnCookieName)
	}
}

// consumeReturnTo is where an approved sign-in goes: the remembered page, else the console root. The
// cookie is read as untrusted input (a sibling host can plant the plain name) and checked again.
func (a *Authenticator) consumeReturnTo(w http.ResponseWriter, r *http.Request) string {
	dest := "/"
	if c, err := r.Cookie(a.cookieName(returnCookieName)); err == nil {
		if raw, derr := base64.RawURLEncoding.DecodeString(c.Value); derr == nil {
			if p := safeReturnTo(string(raw)); p != "" {
				dest = p
			}
		}
		a.expireCookie(w, returnCookieName)
	}
	return a.cfg.BasePath + dest
}
