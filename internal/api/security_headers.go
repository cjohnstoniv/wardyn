// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// securityHeaders sets the console's defense-in-depth headers on EVERY response.
// The console is full-admin (admin bearer in web storage), so frame-ancestors
// 'none' is load-bearing: a framed click originates INSIDE the origin and would
// pass the local-mode CSRF Origin guard (http.go). No HSTS: plain http on
// loopback, where HSTS would poison every localhost port. Each directive:
//   - style-src 'unsafe-inline': xterm's runtime theme <style>, Radix style
//     attributes, fallbackStatusPage. font-src data:: inline woff2. connect-src
//     and media-src: see cspConnectSrc (per request), cspMediaSrc (at boot).
//   - script-src 'wasm-unsafe-eval': the recording replay player compiles WASM;
//     it permits WASM ONLY (no JS eval), and without it replay never plays.
//     run-ui-e2e asserts on console errors so a tightening cannot regress it.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; " +
		"object-src 'none'; connect-src %s; " +
		"%s; " +
		"script-src 'self' 'wasm-unsafe-eval'; " +
		"style-src 'self' 'unsafe-inline'; font-src 'self' data:"
	mediaSrc := cspMediaSrc(s.cfg.DemoVideoBaseURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", fmt.Sprintf(csp, cspConnectSrc(r.Host), mediaSrc))
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// no-store by DEFAULT, overridden by the SPA/asset handler (ui.go), which
		// Sets its own Cache-Control after this middleware has run.
		//
		// The API carried no Cache-Control and no validator at all, and the OIDC
		// lane authenticates by COOKIE rather than Authorization — which is the
		// combination RFC 9111 lets a shared cache store and reuse heuristically.
		// One member's run list served to another out of an interposed proxy is
		// not a risk worth carrying for a header. Defaulting here rather than
		// listing API prefixes means a route added tomorrow inherits it.
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// cspConnectSrc builds connect-src for ONE request. The only outbound channel is
// the PTY-attach WebSocket to this deployment; the bare ws:/wss: SCHEMES match ANY
// host, so emitting them would unbound where an injected script may ship data.
// 'self' covers the same-origin WebSocket on a modern browser; the ws://host /
// wss://host form is the legacy-WebKit fallback (no CSP3 'self' WebSocket match).
// Behind a Host-rewriting proxy it names the upstream, and 'self' alone carries it.
//
// host is r.Host, ATTACKER-CONTROLLED, so the byte filter is load-bearing: a
// space, ';', quote or newline would splice a source or directive into the policy.
// An unsafe or EMPTY Host collapses to 'self' ALONE, never to the ws:/wss:
// wildcard; an empty Host is never a browser attaching a terminal.
func cspConnectSrc(host string) string {
	if !cspSafeHost(host) {
		return "'self'"
	}
	return "'self' ws://" + host + " wss://" + host
}

// cspMediaSrc builds the media-src directive (without the trailing "; ") for the
// demo episode player, once at boot: base is api.Config.DemoVideoBaseURL,
// operator-set and validated by ValidateDemoVideoBaseURL, never request-controlled.
// The host still goes through cspSafeHost (the one place "safe in a CSP source" is
// decided) — this is a response header, so an unfiltered value would be a
// header-injection hole. A base that fails either check collapses to 'self' alone,
// never to the GitHub fallback (defense in depth; boot validation prevents it).
// Empty base grants github.com AND release-assets.githubusercontent.com: the
// download 302s from one to the other, CSP checks the redirect target, and GitHub
// has moved that host before (RELEASING.md re-verifies it). A configured base
// (WARDYN_DEMO_VIDEO_BASE_URL, an air-gapped mirror) names ONLY its own origin.
func cspMediaSrc(base string) string {
	if base == "" {
		return "media-src 'self' https://github.com https://release-assets.githubusercontent.com"
	}
	// ValidateDemoVideoBaseURL's rule 1 already refuses anything but https://,
	// so the scheme is hardcoded here rather than echoed from u.Scheme —
	// cspSafeHost filters the host, not the scheme, and there is no reason to
	// give this interpolation a second field to get wrong.
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || !cspSafeHost(u.Host) {
		return "media-src 'self'"
	}
	return "media-src 'self' https://" + u.Host
}

// cspSafeHost reports whether host is safe to interpolate into a CSP source.
// Deliberately stricter than net/http's own Host validation (httpguts.
// ValidHostHeader admits bytes that are harmless in a request line but splice a
// policy here): only the bytes a real authority needs — letters, digits, '.',
// '-', ':' for the port, and '[' / ']' for an IPv6 literal — plus a length cap,
// because this value is echoed on EVERY response.
func cspSafeHost(host string) bool {
	if host == "" || len(host) > 253+6 { // longest DNS name + ":65535"
		return false
	}
	return strings.IndexFunc(host, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return false
		case r == '.', r == '-', r == ':', r == '[', r == ']':
			return false
		}
		return true
	}) < 0
}

// handleReadyz is the READINESS probe: unlike /healthz (liveness — "is the
// process up"), it proves the store is actually reachable. /healthz alone
// reported "ok" unconditionally, so a dead/unreachable Postgres still read
// healthy — a dead DB never took the pod out of the Service's endpoint list.
// Deliberately anonymous like /healthz (discloses nothing beyond up/down) and
// deliberately a SEPARATE endpoint from /healthz rather than teaching it to
// fail: liveness/startup also point at /healthz in the chart, and a DB blip
// must not restart-loop a pod that is otherwise fine.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), storePingTimeout)
	defer cancel()
	if s.cfg.Store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	if err := s.cfg.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error", "postgres": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
