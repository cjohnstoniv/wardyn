// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The enter ticket's BROWSER BINDING (#1241). A ticket proves who minted it and
// for which run; on its own it says nothing about which browser presents it. So
// any user could mint a ticket for their OWN run and drive a victim's browser
// through the enter hand-off (a link or an auto-submitted form), landing the
// victim on a session for the attacker's app — login CSRF. In path mode that
// app then shares an origin with the victim's own relayed apps.
//
// The binding closes that. Before it submits the enter form, the console makes
// ONE credentialed fetch to the gateway:
//
//	POST <ui-origin>/__wardyn/bind   body: ticket=<token>   (from the console page)
//	  → Set-Cookie wardyn_ui_bind_<id>=<HMAC(ticket)>; HttpOnly; SameSite=Strict;
//	    Path=/__wardyn/enter; Max-Age=30
//
// and enter (GET and POST) refuses a ticket whose cookie is not there. Why the
// cookie lives on the GATEWAY origin: that is the only origin enter can read a
// cookie from, and a cookie the console origin set never reaches it (different
// host, and a shared parent-domain cookie would reach every sibling host too).
//
// What stops an attacker's page doing the same bind in the victim's browser:
//   - uiBindRefusal: the bind must be a same-site fetch (Fetch Metadata, which
//     page script cannot write) and, when the console's URL is configured, carry
//     the console's own Origin. An attacker's page is cross-site, so it gets no
//     cookie.
//   - SameSite=Strict: even a binding planted by a cross-site top-level
//     navigation is never sent on an enter that a cross-site page started.
//   - The cookie value is an HMAC under the gateway's session key, so it is
//     worth nothing for any other ticket, and enter clears it on use. The
//     ticket is single-use, so the binding is too.
//
// This is why console and gateway must be SAME-SITE (one registrable domain,
// same scheme): a cross-site console's fetch cannot set a first-party cookie on
// the gateway at all, and Open fails with the console's bind error instead.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// uiBindPath is the console's pre-enter fetch. Like uiEnterPath it is
	// unauthenticated; it holds no authority of its own, it only ties a
	// ticket to the browser that fetched it.
	uiBindPath = "/__wardyn/bind"
	// uiBindCookiePrefix names one binding cookie per ticket (the suffix is
	// derived from the HMAC), so two apps opened at once do not overwrite each
	// other's binding. The wardyn_ prefix keeps it inside the namespace the
	// relay strips in both directions.
	uiBindCookiePrefix = "wardyn_ui_bind_"
)

// uiBindURL is where the console sends the bind fetch: the enter endpoint's
// sibling, so host mode's {run} placeholder carries over unchanged.
func (s *Server) uiBindURL() string {
	return strings.TrimSuffix(s.uiEnterBaseURL(), uiEnterPath) + uiBindPath
}

// cspUIBindSrc is the console CSP's connect-src entry for the bind fetch, with
// its leading space, or "" when the gateway is off. It names the bind URL
// itself, path included, so connect-src gains that one endpoint rather than
// the gateway origin a relayed app answers on. Host mode's {run} label becomes
// a wildcard label. A URL that cannot be written safely as a source (a {run}
// outside the first host label, an unexpected byte) is left out: the console's
// bind then fails with its own error rather than the policy widening.
func (s *Server) cspUIBindSrc() string {
	if !s.uiGatewayEnabled() {
		return ""
	}
	scheme, rest, ok := strings.Cut(s.uiBindURL(), "://")
	host, path, _ := strings.Cut(rest, "/")
	if label, tail, found := strings.Cut(host, "."); found && strings.Contains(label, "{run}") {
		host = "*." + tail
	}
	if !ok || (scheme != "http" && scheme != "https") || !cspSafeHost(strings.TrimPrefix(host, "*.")) ||
		strings.ContainsFunc(path, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("/_.-", r)
		}) {
		return ""
	}
	return " " + scheme + "://" + host + "/" + path
}

// uiBindMAC is the binding value for ticket: an HMAC under the gateway's own
// session key, with a label so it can never equal a relay-session signature.
// Nothing about the ticket can be read back from it.
func (s *Server) uiBindMAC(ticket string) string {
	mac := hmac.New(sha256.New, s.cfg.UISessionKey)
	mac.Write([]byte("wardyn-ui-bind\x00" + ticket))
	return hex.EncodeToString(mac.Sum(nil))
}

// uiBindCookieName is the per-ticket cookie name: the prefix plus the first 16
// hex digits of the binding value.
func uiBindCookieName(mac string) string { return uiBindCookiePrefix + mac[:16] }

// handleUIBind sets the binding cookie for one ticket. It does not look the
// ticket up: whether the ticket is real, unexpired and the caller's is enter's
// decision, and the cookie is worthless without the ticket it was made from.
func (s *Server) handleUIBind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if reason := s.uiBindRefusal(r); reason != "" {
		// Audited because the console cannot say why: a refused bind carries no
		// CORS headers, so its fetch fails exactly as an unreachable gateway
		// does. Rate-bound like every other unauthenticated refusal here.
		if s.authFailedLimiter.allow(s.cfg.Now()) {
			s.auditUI(nil, types.ActorHuman, "unknown", "ui.authorize", "", "denied", map[string]any{
				"reason":         reason,
				"sec_fetch_site": uiAuditHeader(r, "Sec-Fetch-Site"),
				"origin":         uiAuditHeader(r, "Origin"),
			})
		}
		writeError(w, http.StatusForbidden, "the UI gateway only binds a ticket for the Wardyn console")
		return
	}
	if r.URL.Query().Has("ticket") {
		writeError(w, http.StatusBadRequest, "the ticket must be a form field, not a query parameter")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUIEnterFormBytes)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form body")
		return
	}
	ticket := r.PostForm.Get("ticket")
	if ticket == "" {
		writeError(w, http.StatusBadRequest, "missing ticket")
		return
	}
	mac := s.uiBindMAC(ticket)
	http.SetCookie(w, &http.Cookie{
		Name:     uiBindCookieName(mac),
		Value:    mac,
		Path:     s.uiBasePath() + uiEnterPath,
		MaxAge:   int(attachTicketTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.cfg.OIDCSecureCookies,
	})
	// The console reads the status, so the fetch must be a readable CORS
	// response; uiBindRefusal has already decided this Origin may have it.
	w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Add("Vary", "Origin")
	w.WriteHeader(http.StatusNoContent)
}

// The reasons a bind is refused, as the ui.authorize/denied row names them:
// stable identifiers for a SIEM rule, not copy.
const (
	uiBindReasonNotSameSite      = "bind_not_same_site"
	uiBindReasonOriginNotConsole = "bind_origin_not_console"
)

// uiBindRefusal decides who may bind, returning "" to allow or the audit reason
// to refuse. The fetch must be labelled "same-site" by the browser: the console
// and the gateway are two hosts of one site, while an attacker's page is
// "cross-site" and the gateway's own relayed pages (the sandbox's code) are
// "same-origin". A missing label refuses — every browser the console supports
// sends it, and this check fails closed.
//
// Whenever the console's URL is configured (WARDYN_OIDC_REDIRECT_URL, which
// compose sets with or without SSO) the Origin must also be the console's: see
// uiOriginIsConsole. That narrows "same-site" to the console alone: not a
// sibling host on the same domain, not another run's host in host mode, and not
// another local server on a different loopback port. With no console URL there
// is nothing to compare against — r.Host here is the GATEWAY, and an Origin
// equal to it is the sandbox's own code — so same-site is the rule; that
// happens only without SSO (oidc.New refuses an empty redirect URL), where there
// is one principal and no other user's session to push into a browser.
func (s *Server) uiBindRefusal(r *http.Request) string {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "same-site") {
		return uiBindReasonNotSameSite
	}
	if s.cfg.OIDCRedirectURL == "" {
		return ""
	}
	if !uiOriginIsConsole(strings.TrimSpace(r.Header.Get("Origin")), s.cfg.OIDCRedirectURL) {
		return uiBindReasonOriginNotConsole
	}
	return ""
}

// uiOriginIsConsole reports whether origin is the console at consoleURL: the
// same scheme and host (csrf.go's originHost, so a default port compares
// equal). Unlike csrf.go's r.Host rule, both sides are the browser's own view,
// so the scheme is compared.
//
// One alias is accepted: when the console URL names a loopback host
// (localhost, 127.0.0.1, [::1]), an Origin naming another of those three at the
// SAME port and scheme is the same console. A console published on loopback
// answers on each name, and an operator opens whichever the installer printed
// (compose writes a localhost redirect URL; README and the installer print
// 127.0.0.1). The port is what keeps this tight: another loopback port is
// another process, which is same-site with a loopback gateway, and stays
// refused. A cross-site page never gets here (Sec-Fetch-Site).
func uiOriginIsConsole(origin, consoleURL string) bool {
	oHost, ok := originHost(origin)
	cHost, ok2 := originHost(consoleURL)
	oURL, err := url.Parse(origin)
	cURL, err2 := url.Parse(strings.TrimSpace(consoleURL))
	if !ok || !ok2 || err != nil || err2 != nil || !asciiEqualFold(oURL.Scheme, cURL.Scheme) {
		return false
	}
	if asciiEqualFold(oHost, cHost) {
		return true
	}
	return uiLoopbackName(oURL.Hostname()) && uiLoopbackName(cURL.Hostname()) && uiPort(oURL) == uiPort(cURL)
}

// uiLoopbackName is exactly localhost, 127.0.0.1 or ::1 — not the rest of
// 127.0.0.0/8, which another process can bind at the console's port.
func uiLoopbackName(h string) bool {
	ip := net.ParseIP(h)
	return strings.EqualFold(h, "localhost") || ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback)
}

// uiPort is u's port with the scheme's default filled in.
func uiPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// uiAuditHeader is a request header as an audit value: attacker-supplied, so
// bounded.
func uiAuditHeader(r *http.Request, name string) string {
	v := r.Header.Get(name)
	if len(v) > 256 {
		v = v[:256]
	}
	return v
}

// uiTicketBound reports whether this browser holds the binding cookie for
// ticket, and clears that cookie either way it was found: a binding is used
// once.
func (s *Server) uiTicketBound(w http.ResponseWriter, r *http.Request, ticket string) bool {
	if ticket == "" {
		return false
	}
	mac := s.uiBindMAC(ticket)
	c, err := r.Cookie(uiBindCookieName(mac))
	if err != nil {
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name: c.Name, Value: "", Path: s.uiBasePath() + uiEnterPath, MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.cfg.OIDCSecureCookies,
	})
	return hmac.Equal([]byte(c.Value), []byte(mac))
}
