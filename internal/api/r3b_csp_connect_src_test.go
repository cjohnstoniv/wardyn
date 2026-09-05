// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r3bCSPSegment returns the one directive segment of csp whose name is `name`
// (without the trailing ";"), or "" when the policy carries no such directive.
// Whole-segment, never Contains: a Contains check on a source list passes with
// an extra source appended, which is exactly the widening these pins guard.
func r3bCSPSegment(csp, name string) string {
	for _, seg := range strings.Split(csp, "; ") {
		seg = strings.TrimSpace(strings.TrimSuffix(seg, ";"))
		if seg == name || strings.HasPrefix(seg, name+" ") {
			return seg
		}
	}
	return ""
}

// r3bServedCSP drives one request through the REAL router (securityHeaders is
// the outermost middleware, routes.go) with the given Host and returns the
// Content-Security-Policy it shipped. Host is set on the request struct rather
// than sent on a wire so the splicing case can be expressed at all — that is
// precisely the input net/http would hand a handler behind a proxy or a
// hand-built client, and the header value is what a browser then enforces.
func r3bServedCSP(t *testing.T, h *harness, host string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Host = host
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, r)
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatalf("Host %q: no Content-Security-Policy on the response at all", host)
	}
	return csp
}

// TestR3BCSPConnectSrcNamesThisOriginOnly is F248's pin. The console's ONLY
// outbound channel is the PTY-attach WebSocket, and connect-src is the one
// directive that says where an injected script on this admin-bearing origin may
// ship data. It shipped the bare `ws:` and `wss:` SCHEMES, which match ANY host
// — so the directive bounded nothing. The fix builds it per request from a
// validated r.Host.
//
// Three properties, and all three matter:
//   - a good Host names THIS origin and nothing else (equality, not Contains);
//   - a Host we cannot prove safe collapses to 'self' ALONE, never back to the
//     wildcard form — a wildcard there would be a gift to exactly the client
//     that sent the malformed Host;
//   - a Host carrying CSP punctuation splices no new source and no new
//     directive into the policy.
func TestR3BCSPConnectSrcNamesThisOriginOnly(t *testing.T) {
	h := newHarness(t)

	t.Run("a real Host names this origin's ws/wss and nothing else", func(t *testing.T) {
		const host = "console.example:8443"
		csp := r3bServedCSP(t, h, host)
		got := r3bCSPSegment(csp, "connect-src")
		want := "connect-src 'self' ws://" + host + " wss://" + host
		if got != want {
			t.Errorf("connect-src = %q, want exactly %q (full policy: %q)", got, want, csp)
		}
	})

	// The bare schemes must be gone from the served header on EVERY shape,
	// including the fallbacks: they are the finding.
	for _, host := range []string{
		"console.example:8443",
		"127.0.0.1",
		"[::1]:8080",
		"",
		"evil.example; script-src *",
		"evil.example ws:",
		`evil.example'`,
		"evil.example\nX-Injected: 1",
	} {
		csp := r3bServedCSP(t, h, host)
		seg := r3bCSPSegment(csp, "connect-src")
		for _, bare := range []string{" ws:;", " wss:;", " ws: ", " wss: "} {
			if strings.Contains(seg+";", bare) {
				t.Errorf("Host %q: connect-src = %q still carries the bare scheme %q, which matches any host",
					host, seg, strings.TrimSpace(bare))
			}
		}
		if !strings.HasPrefix(seg, "connect-src 'self'") {
			t.Errorf("Host %q: connect-src = %q must always keep 'self' (the modern-browser grant)", host, seg)
		}
	}

	t.Run("an unsafe Host collapses to 'self' alone and splices nothing", func(t *testing.T) {
		for _, host := range []string{
			"",                                    // HTTP/1.0 or hand-built: never a browser attaching a terminal
			"evil.example; script-src *",          // ';' would open a whole new directive
			"evil.example ws:",                    // a space would append a wildcard source
			`evil.example'`,                       // a quote would forge a keyword source
			"evil.example\nX-Injected: 1",         // CR/LF would forge a whole header
			strings.Repeat("a", 300) + ".example", // absurd length, echoed on every response
		} {
			csp := r3bServedCSP(t, h, host)
			if got := r3bCSPSegment(csp, "connect-src"); got != "connect-src 'self'" {
				t.Errorf("Host %q: connect-src = %q, want exactly \"connect-src 'self'\"", host, got)
			}
			// The rest of the policy must be untouched by whatever the Host said.
			if got := r3bCSPSegment(csp, "script-src"); got != "script-src 'self' 'wasm-unsafe-eval'" {
				t.Errorf("Host %q spliced the policy: script-src = %q (full policy: %q)", host, got, csp)
			}
			if strings.ContainsAny(csp, "\r\n") {
				t.Errorf("Host %q put a newline into the CSP header value: %q", host, csp)
			}
		}
	})
}
