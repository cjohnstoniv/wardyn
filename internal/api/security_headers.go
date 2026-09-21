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

// securityHeaders sets the console's defense-in-depth response headers on EVERY
// response — /healthz, /auth/*, /api/v1/* and the SPA alike. The console is a
// full-admin surface (it holds the admin bearer in web storage and drives
// approve/deny/kill), so frame-ancestors 'none' is load-bearing: a framed click
// originates INSIDE the wardyn origin, which means the local-mode CSRF Origin
// guard (http.go) would pass it.
//
// Each CSP directive is deliberate — do not "tidy" them:
//   - style-src allows 'unsafe-inline': xterm injects a theme <style> at
//     runtime, Radix sets style attributes, and the no-UI-bundle fallback page
//     (ui.go fallbackStatusPage) carries its own inline <style>.
//   - font-src allows data:: the built console CSS embeds its woff2 inline, and
//     data: is not covered by 'self'.
//   - connect-src is built PER REQUEST (cspConnectSrc) and names THIS origin's
//     ws://host / wss://host, never the bare ws:/wss: schemes. 'self' is the
//     primary grant on a modern browser and already covers the same-origin
//     WebSocket; the explicit host form is the LEGACY-WebKit fallback, because
//     'self' matching a same-origin WebSocket is CSP3 behavior WebKit has
//     historically not implemented and the PTY attach must not silently die
//     there. Caveat worth knowing behind a reverse proxy that REWRITES Host:
//     the host form then names the upstream rather than the browser's origin,
//     and the same-origin case rides on 'self' alone — which is exactly the
//     modern-browser path, so the console still attaches.
//   - media-src is built at BOOT (cspMediaSrc, not per request like connect-src
//     — s.cfg.DemoVideoBaseURL is operator-set once and validated at boot by
//     ValidateDemoVideoBaseURL, never sandbox- or request-controlled). Unset,
//     it names github.com and release-assets.githubusercontent.com: the
//     Getting Started demo episodes (ui/src/app/lib/demo-videos.ts) are GitHub
//     release assets, loaded only on explicit click (no autoplay, no
//     prefetch). The download link 302s from the first host to the second —
//     CSP checks the redirect target, not just the link — and GitHub has moved
//     that host before, so RELEASING.md's re-shoot step re-verifies it live.
//     WARDYN_DEMO_VIDEO_BASE_URL re-points this at an operator-run mirror for
//     an air-gapped deployment where github.com is unreachable; the value
//     still runs through cspSafeHost before it is interpolated, the same
//     filter cspConnectSrc's per-request host does, even though it is already
//     boot-validated — this is a security response header, so an unfiltered
//     value here would be a header-injection hole regardless of the source.
//   - script-src is 'self' plus 'wasm-unsafe-eval' — the RECORDING replay player
//     (asciinema-player, a WASM VT core) calls WebAssembly.instantiate(), which
//     browsers refuse under a bare default-src 'self'. 'wasm-unsafe-eval' permits
//     WASM compilation ONLY; it is not 'unsafe-eval' (no JS eval/Function), so
//     scripts stay locked to same-origin. Without it the player renders its chrome
//     but never plays (duration stuck at --:--). The live attach terminal (xterm,
//     no WASM) is unaffected either way. run-ui-e2e asserts on console errors so a
//     future CSP tightening against a WASM component can't silently regress this.
//
// No HSTS: the default posture is plain http on loopback, where an HSTS header
// would poison every other localhost port.
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

// cspConnectSrc builds the connect-src source list for ONE request. The console's
// only outbound channel is the PTY-attach WebSocket, and it goes to this
// deployment and nowhere else; the bare ws:/wss: SCHEMES match ANY host, so the
// one directive bounding where an injected script on the admin-bearing origin
// may ship data would not bound anything if it emitted them.
//
// 'self' is always granted and is the whole answer on a modern browser. The
// host form is added only as the legacy-WebKit fallback described above.
//
// host is r.Host — an ATTACKER-CONTROLLED request header interpolated into a
// response header, so the byte filter below is load-bearing rather than
// cosmetic: a space, ';', quote or newline would splice a new source (or a whole
// new directive) into the policy the browser then enforces. A host we cannot
// prove safe therefore collapses to 'self' ALONE — never to the ws:/wss:
// wildcard, which would hand the very client that sent the malformed Host the
// unbounded form. An EMPTY Host takes that same 'self'-only path: HTTP/1.1
// requires Host, so every browser sends one on every request, and an empty one
// is an HTTP/1.0 speaker or a hand-built request — never something attaching a
// terminal. Nothing that needs the legacy fallback can arrive without a Host.
func cspConnectSrc(host string) string {
	if !cspSafeHost(host) {
		return "'self'"
	}
	return "'self' ws://" + host + " wss://" + host
}

// cspMediaSrc builds the media-src directive (without the trailing "; ") for
// the demo episode player. base is api.Config.DemoVideoBaseURL — already
// validated at boot by ValidateDemoVideoBaseURL (https://, no userinfo, no
// query/fragment) — but this function re-derives and re-filters the host
// through cspSafeHost anyway rather than trusting that upstream check alone:
// it is the same discipline cspConnectSrc applies to the per-request Host,
// and the two functions share the one helper so "safe to put in a CSP
// source" is decided in exactly one place. A base that fails to parse or
// whose host fails cspSafeHost collapses to 'self' alone, never to the
// GitHub fallback — a config that already passed boot validation should
// never reach either branch, so this is defense in depth, not a real path.
//
// Empty base (the default, unset WARDYN_DEMO_VIDEO_BASE_URL) reproduces
// today's two hardcoded GitHub hosts byte-for-byte — the download link 302s
// from github.com to release-assets.githubusercontent.com, so both must be
// granted. A configured base names ONLY its own origin: the operator's
// mirror is the one place those episodes can come from once it is set.
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
