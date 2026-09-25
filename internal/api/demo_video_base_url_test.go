// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestValidateDemoVideoBaseURL exercises WARDYN_DEMO_VIDEO_BASE_URL's boot
// gate: the same rule shape validateOneLLMGateway enforces for an internal
// model gateway (https://, no userinfo, non-empty host, no query, no
// fragment), plus the unset -> "" baseline that keeps the CSP and
// episodeUrl's fallback byte-identical to today.
func TestValidateDemoVideoBaseURL(t *testing.T) {
	cases := []struct {
		name, raw string
		ok        bool
		want      string
	}{
		{"unset -> empty, byte-identical to today", "", true, ""},
		{"good mirror hostname", "https://videos.airgapped.example", true, "https://videos.airgapped.example"},
		{"path prefix preserved, trailing slash trimmed", "https://videos.airgapped.example/wardyn-demos/", true, "https://videos.airgapped.example/wardyn-demos"},
		{"RFC1918 literal allowed", "https://10.40.1.5:8443/videos", true, "https://10.40.1.5:8443/videos"},
		{"rule 1: http refused", "http://videos.airgapped.example", false, ""},
		{"rule 2: userinfo refused", "https://user:pass@videos.airgapped.example", false, ""},
		{"rule 3: empty host refused", "https:///path", false, ""},
		{"rule 6: query refused", "https://videos.airgapped.example?x=1", false, ""},
		{"rule 7: fragment refused", "https://videos.airgapped.example#x", false, ""},
		{"malformed URL refused", "https://[::", false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateDemoVideoBaseURL(c.raw)
			if c.ok && err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if !c.ok {
				if err == nil {
					t.Fatalf("expected an error, got nil (out=%q)", got)
				}
				if !strings.Contains(err.Error(), "WARDYN_DEMO_VIDEO_BASE_URL") {
					t.Errorf("refusal %q does not name the var", err.Error())
				}
				if got != "" {
					t.Fatalf("a refused value must return %q, got %q", "", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestCSPMediaSrc pins cspMediaSrc's two shapes directly, independent of the
// HTTP plumbing: empty base reproduces today's two hardcoded GitHub hosts,
// and a configured base names ONLY its own https origin. An unsafe/malformed
// value collapses to 'self' alone rather than the GitHub fallback — the same
// fail-closed posture cspConnectSrc uses for an unsafe request Host.
func TestCSPMediaSrc(t *testing.T) {
	cases := []struct {
		name, base, want string
	}{
		{"unset -> the two hardcoded GitHub hosts", "", "media-src 'self' https://github.com https://release-assets.githubusercontent.com"},
		{"configured mirror -> its origin only", "https://videos.airgapped.example/wardyn-demos", "media-src 'self' https://videos.airgapped.example"},
		{"mirror with a port -> origin includes it", "https://videos.airgapped.example:8443/wardyn-demos", "media-src 'self' https://videos.airgapped.example:8443"},
		{"malformed base (already boot-validated so should never reach here) -> 'self' alone", "https://[::", "media-src 'self'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cspMediaSrc(c.base); got != c.want {
				t.Errorf("cspMediaSrc(%q) = %q, want %q", c.base, got, c.want)
			}
		})
	}
}

// demoVideoServedCSP drives one /healthz request through the REAL router
// (s.securityHeaders is the outermost middleware, routes.go) for a Server
// built with the given DemoVideoBaseURL, and returns the served CSP.
func demoVideoServedCSP(t *testing.T, base string) string {
	t.Helper()
	srv := New(Config{DemoVideoBaseURL: base})
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatalf("no Content-Security-Policy on the response at all")
	}
	return csp
}

// r3bCSPSegment (r3b_csp_connect_src_test.go) returns one whole directive
// segment by name; reused here so media-src is compared as a WHOLE SEGMENT,
// never with Contains — a Contains check passes with an extra source
// silently appended.

// TestSecurityHeaders_DemoVideoBaseURLUnsetIsByteIdentical is the CHECK's
// explicit pin: WARDYN_DEMO_VIDEO_BASE_URL unset must produce a CSP
// byte-identical to before this feature existed — the exact same
// media-src segment TestSecurityHeadersOnEveryResponse already pins, proven
// here again as a dedicated, named contract rather than relying on that
// other test's coverage alone.
func TestSecurityHeaders_DemoVideoBaseURLUnsetIsByteIdentical(t *testing.T) {
	const wantMedia = "media-src 'self' https://github.com https://release-assets.githubusercontent.com"
	csp := demoVideoServedCSP(t, "")
	if got := r3bCSPSegment(csp, "media-src"); got != wantMedia {
		t.Errorf("media-src = %q, want exactly %q (full policy: %q)", got, wantMedia, csp)
	}
}

// TestSecurityHeaders_DemoVideoBaseURLSetNamesMirrorOrigin is the CHECK's
// "done when" half: with WARDYN_DEMO_VIDEO_BASE_URL set, media-src names
// ONLY the mirror's origin — not the GitHub hosts, not both.
func TestSecurityHeaders_DemoVideoBaseURLSetNamesMirrorOrigin(t *testing.T) {
	csp := demoVideoServedCSP(t, "https://videos.airgapped.example/wardyn-demos")
	const wantMedia = "media-src 'self' https://videos.airgapped.example"
	if got := r3bCSPSegment(csp, "media-src"); got != wantMedia {
		t.Errorf("media-src = %q, want exactly %q (full policy: %q)", got, wantMedia, csp)
	}
	if strings.Contains(csp, "github.com") {
		t.Errorf("media-src still names github.com with a mirror configured: %q", csp)
	}
}
