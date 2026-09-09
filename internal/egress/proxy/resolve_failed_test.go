// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// F055: a name the proxy could not RESOLVE is not a private-address block.
//
// evaluate() used to throw egressTarget's error away and stamp every
// post-resolution denial "builtin:private-ip", so a resolver outage, an
// NXDOMAIN and a zero-answer lookup all arrived in the decision stream as the
// SSRF guard — with literalIPDenialDetail's "declare it under internal_hosts"
// advice attached. That advice cannot fix a DNS fault, and it points an
// operator at loosening an SSRF control in response to one.
//
// These pins hold both halves: the three resolver outcomes audit as themselves
// (still DENIED — fail closed is unchanged, only the attribution moved), and a
// host that really does resolve into private space still audits
// builtin:private-ip with the site-config advice that actually fixes it.

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// allowedHost is on the policy's allowlist in every case below, so the deny is
// provably caused by the RESOLVER and never by policy — the control at the end
// reaches the upstream with the same policy and a working resolver.
const allowedHost = "allowed.example"

func TestResolveFailure_AuditedAsResolveFailed_NotPrivateIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  resolver
	}{
		{"resolver outage", fakeResolver{err: errors.New("dial udp 127.0.0.53:53: connect: connection refused")}},
		{"NXDOMAIN", fakeResolver{err: &net.DNSError{Err: "no such host", Name: allowedHost, IsNotFound: true}}},
		{"zero answers", fakeResolver{m: map[string][]net.IP{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, buf := newInternalHostsProxy(t,
				types.RunPolicySpec{AllowedDomains: []string{allowedHost}},
				tc.res, nil, nil, nil, "127.0.0.1:1")

			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://"+allowedHost+"/"))

			// Fail CLOSED, exactly as before: only the label changed.
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 — a name that did not resolve must still be denied", rec.Code)
			}
			d := lastDecision(t, buf)
			if d.Decision != egress.Deny {
				t.Fatalf("decision = %q, want deny", d.Decision)
			}
			if d.RuleSource != "builtin:resolve-failed" {
				t.Errorf("rule_source = %q, want builtin:resolve-failed — auditing a DNS fault as the "+
					"private-address guard tells the operator to widen an SSRF control over an outage", d.RuleSource)
			}
			if got := rec.Header().Get(egressHeaderReason); got != "builtin:resolve-failed" {
				t.Errorf("%s = %q, want builtin:resolve-failed", egressHeaderReason, got)
			}
			if got := rec.Header().Get(egressHeaderDetail); got != resolveFailedDetail {
				t.Errorf("%s = %q, want the resolve-failure sentence %q", egressHeaderDetail, got, resolveFailedDetail)
			}
			if body := rec.Body.String(); !strings.Contains(body, resolveFailedDetail) {
				t.Errorf("403 body = %q, want it to carry the resolve-failure sentence", body)
			}
			// The wrong advice, specifically: internal_hosts lifts the
			// address-range guard and can do nothing about a resolver.
			if got := rec.Header().Get(egressHeaderDetail); strings.Contains(got, "internal_hosts") {
				t.Errorf("%s = %q — a resolve failure must never advise widening the SSRF guard", egressHeaderDetail, got)
			}
		})
	}

	// The control: same policy, same host, a resolver that answers — so the
	// three denials above are caused by resolution and by nothing else.
	t.Run("control: a working resolver allows the same host", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
		defer up.Close()
		p, buf := newInternalHostsProxy(t,
			types.RunPolicySpec{AllowedDomains: []string{allowedHost}},
			publicResolver{}, nil, nil, nil, upstreamAddr(up))

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://"+allowedHost+"/"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if d := lastDecision(t, buf); d.Decision != egress.Allow || d.RuleSource != "policy:allowed" {
			t.Fatalf("decision = %q/%q, want allow/policy:allowed", d.Decision, d.RuleSource)
		}
	})
}

// TestResolvedPrivateAddress_StillAuditedAsPrivateIP is the regression the
// split must not cause: a name that DOES resolve, into private space, is still
// the address-range guard — same rule_source, same site-config advice.
func TestResolvedPrivateAddress_StillAuditedAsPrivateIP(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{allowedHost: ips("10.1.2.3")}}
	p, buf := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{allowedHost}},
		res, nil, nil, nil, "127.0.0.1:1")

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://"+allowedHost+"/"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if d := lastDecision(t, buf); d.RuleSource != "builtin:private-ip" {
		t.Fatalf("rule_source = %q, want builtin:private-ip — a real private-address block must keep "+
			"its own attribution", d.RuleSource)
	}
	detail := rec.Header().Get(egressHeaderDetail)
	if !strings.Contains(detail, "internal_hosts") {
		t.Errorf("%s = %q, want the site-config advice that actually lifts this guard", egressHeaderDetail, detail)
	}
	if detail == resolveFailedDetail {
		t.Errorf("%s carries the resolve-failure sentence for a resolved private address", egressHeaderDetail)
	}
}
