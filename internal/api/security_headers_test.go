// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestSecurityHeadersOnEveryResponse is the regression for the published
// residual risk (threatmodel/THREAT-MODEL.md §5): the console is a full-admin
// surface, so every response — the anonymous /healthz, a 401 from the admin-gated
// API, and the SPA — must carry the clickjacking/sniffing defenses. It also pins
// the CSP directives that are easy to loosen without noticing: font-src data:
// (the built CSS embeds its woff2), connect-src ws: (the PTY attach), and
// media-src's exact two hosts (a wildcard here would let the console's
// admin-bearing origin embed arbitrary third-party media).
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newHarness(t)

	want := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	}
	// /healthz is anonymous; /api/v1/runs without a bearer is a 401 — the error
	// path must be covered too.
	for _, path := range []string{"/healthz", "/api/v1/runs"} {
		w := do(t, h.srv, http.MethodGet, path, "", "")
		for name, val := range want {
			if got := w.Header().Get(name); got != val {
				t.Errorf("%s: %s = %q, want %q", path, name, got, val)
			}
		}
		csp := w.Header().Get("Content-Security-Policy")
		for _, directive := range []string{
			"default-src 'self'",
			"frame-ancestors 'none'",
			"font-src 'self' data:",                // console webfont is a data: URI
			"connect-src 'self' ws: wss:",          // PTY attach WebSocket
			"script-src 'self' 'wasm-unsafe-eval'", // recording replay player instantiates WASM
			"media-src 'self' https://github.com https://release-assets.githubusercontent.com", // demo episodes are GitHub release assets, loaded only on click
		} {
			if !strings.Contains(csp, directive) {
				t.Errorf("%s: CSP %q missing %q", path, csp, directive)
			}
		}
		// The recording player needs 'wasm-unsafe-eval' (WASM compile only), NOT
		// the far broader 'unsafe-eval' (arbitrary JS eval/Function). Guard against
		// a future "just add unsafe-eval" shortcut: the leading space+quote can't
		// match inside 'wasm-unsafe-eval'.
		if strings.Contains(csp, " 'unsafe-eval'") {
			t.Errorf("%s: CSP %q must not grant 'unsafe-eval' (use 'wasm-unsafe-eval')", path, csp)
		}
		// media-src must name the two exact hosts, never open up to a wildcard —
		// this origin holds the admin bearer, so a broad media-src would let any
		// page it can be tricked into loading embed arbitrary third-party media
		// under it.
		if strings.Contains(csp, "media-src *") || strings.Contains(csp, "media-src https:") {
			t.Errorf("%s: CSP %q must not grant a wildcard media-src", path, csp)
		}
	}
}
