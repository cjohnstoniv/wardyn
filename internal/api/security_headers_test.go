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
// the two CSP directives that are easy to "tidy" into an outage: font-src data:
// (the built CSS embeds its woff2) and connect-src ws: (the PTY attach).
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
			"font-src 'self' data:",       // console webfont is a data: URI
			"connect-src 'self' ws: wss:", // PTY attach WebSocket
		} {
			if !strings.Contains(csp, directive) {
				t.Errorf("%s: CSP %q missing %q", path, csp, directive)
			}
		}
	}
}
