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

// csrfAuditReason is the `reason` both arms emit on the EXISTING auth.failed
// action (docs/AUDIT-ACTIONS.md) — no new action, no new row shape, and the
// same rate limiter and coalescer every other refusal in this middleware goes
// through (auth_failed_coalesce.go keys on actor+reason+path+source IP, so a
// page hammering one route folds into one row plus a summary). It is emitted
// because a security control that refuses SILENTLY cannot answer either
// question an operator has at 3am: "is someone attacking this?" and "why did
// the console stop saving?". Not a closed enum member by accident — it joins
// the enum the auth.failed row documents.
const csrfAuditReason = "cross_origin_refused"

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
//  2. A PRESENT `Sec-Fetch-Site` that is neither `same-origin` nor `none`
//     ⇒ refuse. Fetch Metadata is set by the BROWSER and cannot be written by
//     page script, so its label is trustworthy evidence about where the request
//     came from — whatever the Origin header says. Checked FIRST for that
//     reason. `same-site` refuses too (S2-02): it is a sibling host on a shared
//     registrable domain, the exact case SameSite=Lax does not bind, and the
//     Origin rule below cannot judge it when the browser sent no Origin at all.
//  3. No Origin AND no Sec-Fetch-Site ⇒ allow. That is a CLI/API client (curl,
//     the wardyn CLI, a CI job); browsers send one or the other on every
//     mutating request, so this fallthrough is not reachable from a page. It is
//     also the only rule here that is deliberately open, and it is open because
//     a cookie-less client is not the CSRF threat.
//  4. A PRESENT Origin must name this deployment: either r.Host, or the host of
//     the configured OIDC redirect URL. Anything else — including an Origin
//     that is malformed, opaque ("null", what a sandboxed iframe or some
//     cross-site redirects send), or carries no host at all — is REFUSED. Fail
//     closed on every unparseable input, exactly like originIsRequestHost (the
//     LocalMode arm's own comparison, which reads the same parse).
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
// an origin is — after originHost drops a port that IS the scheme's default.
// That normalization is not cosmetic: a browser omits :443 from an https
// Origin, an operator writing WARDYN_OIDC_REDIRECT_URL is free to spell it, and
// behind an ingress r.Host matches neither, so without it the second accepted
// name evaporates and every console write 403s on a config that looks correct.
// r.Host itself is NOT normalized — we cannot know the scheme the browser used
// to reach the ingress in front of us — so a deployment whose r.Host carries an
// explicit :443 is matched by the redirect-URL rule rather than the r.Host one.
func (s *Server) sameOriginOrRefuse(r *http.Request) error {
	if !isMutatingMethod(r.Method) {
		return nil
	}
	if isForeignSiteFetch(r) {
		return errCrossOriginRefused
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	if s.originNamesThisDeployment(r, origin) {
		return nil
	}
	return errCrossOriginRefused
}

// originNamesThisDeployment reports whether a PRESENT Origin names one of the
// two hosts this deployment answers to: r.Host, or the host of the configured
// OIDC redirect URL (the ingress case — see WHY TWO ACCEPTED HOSTS above). It
// is the ONE predicate three callers share (the session guard above, the
// PTY-attach socket below, and — for its r.Host half — http.go's LocalMode
// arm), so "which origins are us" cannot diverge between them. Fails closed on
// an Origin originHost could not read.
func (s *Server) originNamesThisDeployment(r *http.Request, origin string) bool {
	host, ok := originHost(origin)
	if !ok {
		return false
	}
	if asciiEqualFold(host, r.Host) {
		return true
	}
	redirect, ok := originHost(s.cfg.OIDCRedirectURL)
	return ok && asciiEqualFold(host, redirect)
}

// originIsRequestHost is the r.Host half on its own — what LocalMode compares,
// where there is no second name and an Origin naming any OTHER loopback
// listener is another process's page, not ours.
func (s *Server) originIsRequestHost(r *http.Request, origin string) bool {
	host, ok := originHost(origin)
	return ok && asciiEqualFold(host, r.Host)
}

// asciiEqualFold is strings.EqualFold restricted to ASCII. A HOST comparison
// must not do more than it says: strings.EqualFold applies Unicode simple case
// folding, under which U+212A KELVIN SIGN folds equal to "k", so
// `https://<KELVIN>.example` compared equal to the host `k.example`. An Origin
// header is always ASCII/punycode, so nothing legitimate is lost — and nothing
// a Unicode table decides is gained.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// isForeignSiteFetch reports whether the BROWSER labelled this request as
// coming from anywhere but this very origin (Fetch Metadata). Page script
// cannot set Sec-Fetch-Site — it is a forbidden header name — so a present
// label is trustworthy evidence. An absent header proves nothing (a non-browser
// client, or an older browser) and is never treated as a pass on its own; the
// Origin rule still runs.
//
// ONLY "same-origin" and "none" (a user-initiated navigation) pass. It used to
// refuse "cross-site" alone, which left "same-site" — a sibling host on a
// shared registrable domain — to the Origin rule, and a browser that omits
// Origin on a top-level form POST gave that rule nothing to judge: the request
// fell through the CLI/API arm and mutated state (S2-02). An unknown future
// label refuses for the same reason: this is the fail-closed half of the guard.
func isForeignSiteFetch(r *http.Request) bool {
	fs := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
	return fs != "" && !strings.EqualFold(fs, "same-origin") && !strings.EqualFold(fs, "none")
}

// originHost returns the comparable host[:port] of a SERIALISED ORIGIN
// (scheme://host[:port]) — what the Origin header carries, and the shape
// WARDYN_OIDC_REDIRECT_URL must also have. ok is false for everything else, so
// every caller FAILS CLOSED on input it could not understand rather than
// guessing: a parse error, an opaque origin ("null"), a scheme with no
// authority ("file:///x"), a scheme-relative reference ("//host") and a URL
// carrying USERINFO ("https://user@host", where the host is not where a reader
// expects it). The last two are not browser-reachable — the Origin header is
// always a serialised origin — but this function is now read by
// attachOriginRefused and by config, and a shape whose host is not obvious to
// a human reader has no business being silently accepted on a security path.
//
// A port equal to the SCHEME'S DEFAULT is dropped (https:443, http:80), so
// "https://console.example" and "https://console.example:443" compare equal.
// TrimSuffix, not net.SplitHostPort, because SplitHostPort strips the brackets
// an IPv6 literal needs to compare against r.Host ("[::1]:8080").
func originHost(origin string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Host == "" || u.Scheme == "" || u.User != nil {
		return "", false
	}
	switch u.Scheme {
	case "https":
		return strings.TrimSuffix(u.Host, ":443"), true
	case "http":
		return strings.TrimSuffix(u.Host, ":80"), true
	}
	return u.Host, true
}

// OriginHostReadable reports whether raw is a serialised origin (or a URL)
// whose host originHost can read — exported for cmd/wardynd's boot-time
// WARDYN_OIDC_REDIRECT_URL check, so boot and the guard fail closed on exactly
// the same shapes rather than on two hand-kept lists.
func OriginHostReadable(raw string) bool {
	_, ok := originHost(raw)
	return ok
}

// attachOriginRefused decides the PTY-attach WebSocket's ORIGIN, in place of
// coder/websocket's OriginPatterns.
//
// WHY NOT OriginPatterns. They are path.Match GLOBS, not literals
// (authenticateOrigin: `path.Match(lower(pattern), lower(u.Host))`), so every
// `*`, `?` and `[` in the configured redirect host is a metacharacter — and an
// IPv6-literal redirect URL always carries `[…]`. `https://[2001:db8::1]:8443`
// became the character class `[2001:db8::1]:8443`, which MATCHES the foreign,
// browser-reachable origin host `0:8443` and does NOT match the ingress host it
// was configured for: wrong in both directions at once.
//
// So the comparison is ours, and it is the same one the console's CSRF guard
// makes — r.Host or the redirect host, ASCII-folded, no patterns. An ABSENT
// Origin is allowed, which is the library's own behaviour for a non-browser
// client (the CLI attaches with a bearer/ticket and no Origin); every other
// origin, including one originHost cannot read, is refused BEFORE the upgrade.
func (s *Server) attachOriginRefused(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	// ONE PREDICATE PER MODE, the same split the REST surface makes: LocalMode
	// has no second name (there is no ingress and no IdP in front of a
	// single-developer daemon), so it compares r.Host alone. Without this the
	// two surfaces diverge on a deployment that runs LocalMode WITH OIDC
	// configured — the attach socket would accept a redirect host the REST
	// routes refuse, which is exactly what originNamesThisDeployment's doc
	// promises cannot happen.
	if s.cfg.LocalMode {
		return !s.originIsRequestHost(r, origin)
	}
	return !s.originNamesThisDeployment(r, origin)
}
