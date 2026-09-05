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
// admin-bearing origin embed arbitrary third-party media) — and base-uri /
// object-src 'none', which the threat model publishes as SHIPPED mitigations
// for the admin-token-in-web-storage residual and which nothing pinned: the
// testing lens deleted both from the served header and the whole internal/api
// suite stayed green.
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
			"base-uri 'none'",                      // no <base> can retarget the console's relative URLs
			"object-src 'none'",                    // no <object>/<embed> plugin surface on the admin origin
			"font-src 'self' data:",                // console webfont is a data: URI
			"script-src 'self' 'wasm-unsafe-eval'", // recording replay player instantiates WASM
			"media-src 'self' https://github.com https://release-assets.githubusercontent.com", // demo episodes are GitHub release assets, loaded only on click
		} {
			if !strings.Contains(csp, directive) {
				t.Errorf("%s: CSP %q missing %q", path, csp, directive)
			}
		}
		// connect-src is the PTY attach's grant AND the only directive bounding
		// where an injected script on this admin-bearing origin may ship data, so
		// it is checked as a WHOLE SEGMENT (like media-src below), never with
		// Contains: a Contains check passes with an extra source appended, and the
		// bare `ws:`/`wss:` SCHEMES this used to carry matched ANY host, which
		// made the directive bound nothing at all. It is now built per request
		// from r.Host, and do() drives every request with Host 127.0.0.1.
		const wantConnect = "connect-src 'self' ws://127.0.0.1 wss://127.0.0.1"
		var gotConnect string
		for _, seg := range strings.Split(csp, "; ") {
			if strings.HasPrefix(seg, "connect-src ") {
				gotConnect = strings.TrimSuffix(seg, ";")
			}
		}
		if gotConnect != wantConnect {
			t.Errorf("%s: CSP connect-src = %q, want exactly %q", path, gotConnect, wantConnect)
		}
		for _, bare := range []string{"ws:", "wss:"} {
			if strings.Contains(gotConnect+" ", " "+bare+" ") {
				t.Errorf("%s: CSP connect-src = %q carries the bare scheme %q, which matches ANY host",
					path, gotConnect, bare)
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
		// Equality on the whole segment, not Contains: a Contains check passes
		// with a third host appended, which is exactly the widening this guards.
		const wantMedia = "media-src 'self' https://github.com https://release-assets.githubusercontent.com"
		var gotMedia string
		for _, seg := range strings.Split(csp, "; ") {
			if strings.HasPrefix(seg, "media-src ") {
				gotMedia = strings.TrimSuffix(seg, ";")
			}
		}
		if gotMedia != wantMedia {
			t.Errorf("%s: CSP media-src = %q, want exactly %q", path, gotMedia, wantMedia)
		}
	}
}
