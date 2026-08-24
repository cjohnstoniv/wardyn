// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSplitHostPortStripsAllTrailingDots pins the D1 fix at the parser funnel:
// every FQDN-root spelling of a host reduces to the SAME canonical dot-free
// host. Before the fix "evil.com..:443" survived as "evil.com." (one TrimSuffix)
// and missed a dot-free deny key.
func TestSplitHostPortStripsAllTrailingDots(t *testing.T) {
	cases := []struct {
		in       string
		defPort  int
		wantHost string
		wantPort int
	}{
		{"evil.com..:443", 443, "evil.com", 443},
		{"evil.com.", 443, "evil.com", 443},
		{"evil.com...:80", 80, "evil.com", 80},
		{"EVIL.COM..:443", 443, "evil.com", 443},
		{"evil.com", 80, "evil.com", 80}, // no over-trim of a legit host
		{"evil.com:8443", 80, "evil.com", 8443},
	}
	for _, c := range cases {
		h, p := splitHostPort(c.in, c.defPort)
		if h != c.wantHost || p != c.wantPort {
			t.Errorf("splitHostPort(%q,%d) = (%q,%d), want (%q,%d)", c.in, c.defPort, h, p, c.wantHost, c.wantPort)
		}
	}
}

// TestTrailingDotDenyBypass is the end-to-end proof of D1: under allow_all_egress
// with "evil.com" on denied_domains, every trailing-dot spelling of the host is
// DENIED (403, upstream never reached). RED before the fix: the multi-dot forms
// evaded the deny key and were allowed through to the upstream.
func TestTrailingDotDenyBypass(t *testing.T) {
	for _, hostport := range []string{"evil.com..:80", "evil.com.", "evil.com...:80"} {
		t.Run(hostport, func(t *testing.T) {
			cu := captureUpstream(t, false, "")
			p, buf := newTestProxy(t, types.RunPolicySpec{
				AllowAllEgress: true,
				DeniedDomains:  []string{"evil.com"},
			}, upstreamAddr(cu.srv), nil, nil)

			rec := httptest.NewRecorder()
			req := mustProxyReq(t, http.MethodGet, "http://"+hostport+"/")
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("host %q: status = %d, want 403 (deny)", hostport, rec.Code)
			}
			if cu.reached {
				t.Fatalf("host %q: upstream reached — deny key evaded", hostport)
			}
			if d := lastDecision(t, buf); d.Decision != egress.Deny {
				t.Fatalf("host %q: decision = %q, want deny", hostport, d.Decision)
			}
		})
	}
}
