// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
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
//   - connect-src names ws:/wss: explicitly. 'self' matching a same-origin
//     WebSocket is CSP3 behavior WebKit has historically not implemented, and
//     the PTY attach must not silently die there.
//   - media-src names github.com and release-assets.githubusercontent.com: the
//     Getting Started demo episodes (ui/src/app/lib/demo-videos.ts) are GitHub
//     release assets, loaded only on explicit click (no autoplay, no
//     prefetch). The download link 302s from the first host to the second —
//     CSP checks the redirect target, not just the link — and GitHub has moved
//     that host before, so RELEASING.md's re-shoot step re-verifies it live.
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
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; " +
		"object-src 'none'; connect-src 'self' ws: wss:; " +
		"media-src 'self' https://github.com https://release-assets.githubusercontent.com; " +
		"script-src 'self' 'wasm-unsafe-eval'; " +
		"style-src 'self' 'unsafe-inline'; font-src 'self' data:"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
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
