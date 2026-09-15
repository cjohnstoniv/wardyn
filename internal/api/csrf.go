// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// csrf.go — the same-origin guard for a COOKIE-authenticated mutating request.
//
// WHY A COOKIE NEEDS THIS AND A BEARER DOES NOT. A session cookie is AMBIENT
// authority: the browser attaches it to a request any page can cause, so
// `fetch("https://wardyn.example/api/v1/...", {method:"POST", credentials:
// "include"})` from evil.example runs as the signed-in human. A bearer token is
// not ambient — an attacker's page cannot read it or make the browser send it —
// so the CLI/CI lane is exempt BY CONSTRUCTION here: the guard hangs off the
// OIDC SESSION branch of humanOrAdminAuth (http.go), which a bearer caller
// never enters. `SameSite=Lax` on the cookie is a real mitigation and is why
// this is a hardening rather than an open hole, but it is set by the BROWSER's
// rules, not ours: it does not cover a top-level form POST on every engine, it
// is relaxed for the two-minute "Lax+POST" window some browsers still grant a
// fresh cookie, and a deployment fronting Wardyn on a shared parent domain gets
// no protection from it against a same-SITE sibling at all. The server must
// decide this itself.
//
// This lives in its own file because internal/api/http.go sits at 959 lines
// against the 1000-line file-size gate (scripts/check-file-size.sh).
//
// OUT OF SCOPE — the per-run UI-sandbox gateway cookie (`wardyn_ui_sess`,
// uigateway.go). That is a DIFFERENT credential on a DIFFERENT listener and
// origin (boundary B10), reached through its own middleware which never calls
// this guard; widening to it is its own change with its own threat model, and
// this file deliberately does not pretend to cover it.

// DRAFT (M2 canon pending)
const (
	// csrfRefusedBody is the one sentence both guards answer with. The
	// LocalMode arm in http.go prefixes "local mode: " and otherwise says
	// exactly this — they are one refusal, phrased once.
	csrfRefusedBody = "cross-origin state-changing request rejected (CSRF guard)"
)

// errCrossOriginRefused is the sentinel sameOriginOrRefuse returns. Its text IS
// the 403 body, so the caller writes err.Error() and no second string exists to
// drift.
var errCrossOriginRefused = errors.New(csrfRefusedBody)

// sameOriginOrRefuse decides whether a cookie-authenticated request may change
// state. It returns nil to allow and errCrossOriginRefused to refuse; the
// caller answers 403 and does not reach the handler.
//
// The rules, in order:
//
//  1. A non-mutating method is never refused. GET/HEAD/OPTIONS change nothing,
//     and refusing them would break the console's own reads.
//  2. `Sec-Fetch-Site: cross-site` ⇒ refuse. Fetch Metadata is set by the
//     BROWSER and cannot be written by page script, so when it says cross-site
//     the request did not originate on our origin — whatever the Origin header
//     says. Checked FIRST for that reason. Only "cross-site" refuses:
//     "same-site" is a sibling host on the same registrable domain, which the
//     Origin rule below judges on its own merits rather than on the browser's
//     coarser label.
//  3. No Origin AND no Sec-Fetch-Site ⇒ allow. That is a CLI/API client (curl,
//     the wardyn CLI, a CI job); browsers send one or the other on every
//     mutating request, so this fallthrough is not reachable from a page. It is
//     also the only rule here that is deliberately open, and it is open because
//     a cookie-less client is not the CSRF threat.
//  4. A PRESENT Origin must name this deployment: either r.Host, or the host of
//     the configured OIDC redirect URL. Anything else — including an Origin
//     that is malformed, opaque ("null", what a sandboxed iframe or some
//     cross-site redirects send), or carries no host at all — is REFUSED. Fail
//     closed on every unparseable input, exactly like isLoopbackOrigin.
//
// WHY TWO ACCEPTED HOSTS. Behind a TLS-terminating ingress the browser's Origin
// is the public console name while r.Host is whatever the proxy forwards
// (a service name, an internal FQDN). s.cfg.OIDCRedirectURL is the browser's
// own view of this deployment — the IdP redirects a real human back to it, so
// it is operator-configured and attacker-unwritable — which makes it the right
// second name. It is never empty where it matters: oidc.New REFUSES a Config
// without a RedirectURL and cmd/wardynd feeds the same flag to both, so a
// deployment that HAS a session to forge always has this host. When SSO is not
// configured it is empty and only r.Host is accepted, which is correct: there
// is no session cookie there at all.
//
// WHY SCHEME IS NOT COMPARED. Same reason: that same ingress terminates TLS, so
// the browser sends `Origin: https://console.example` while wardynd serves
// plain HTTP behind it. Comparing schemes would refuse every TLS-fronted
// deployment; the host is what identifies the origin we care about, and an
// attacker who can serve http:// on our own hostname already owns the origin.
//
// Hosts compare case-insensitively and INCLUDE the port — "host:port" is what
// an origin is. ponytail: no default-port normalization ("https://x" vs
// "x:443"); the redirect-URL rule already covers the deployment shape where
// r.Host carries a port the browser does not, and a normalizer here would be
// speculative parsing on a security path.
func (s *Server) sameOriginOrRefuse(r *http.Request) error {
	if !isMutatingMethod(r.Method) {
		return nil
	}
	if isCrossSiteFetch(r) {
		return errCrossOriginRefused
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	host, ok := originHost(origin)
	if !ok {
		return errCrossOriginRefused
	}
	if strings.EqualFold(host, r.Host) {
		return nil
	}
	if redirect, ok := originHost(s.cfg.OIDCRedirectURL); ok && strings.EqualFold(host, redirect) {
		return nil
	}
	return errCrossOriginRefused
}

// isCrossSiteFetch reports whether the BROWSER labelled this request cross-site
// (Fetch Metadata). Page script cannot set Sec-Fetch-Site — it is a forbidden
// header name — so a present "cross-site" is trustworthy evidence. An absent
// header proves nothing (a non-browser client, or an older browser) and is
// never treated as a pass on its own; the Origin rule still runs.
func isCrossSiteFetch(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site")
}

// originHost returns the host[:port] of an Origin (or any absolute URL). ok is
// false for everything without a host — a parse error, an opaque origin
// ("null"), a scheme with no authority ("file:///x") — so every caller FAILS
// CLOSED on input it could not understand rather than guessing.
func originHost(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "", false
	}
	return u.Host, true
}
