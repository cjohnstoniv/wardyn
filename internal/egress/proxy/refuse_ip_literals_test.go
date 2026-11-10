// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refuse_ip_literals refuses, under allow_all_egress, every IP literal host in any spelling, through
// CONNECT and through absolute-URI HTTP alike. An address listed exactly in allowed_domains is still
// allowed, in its canonical spelling only.

// ipLiteralSpellings are 8.8.8.8 (and a public and a link-local IPv6 address) as a sandbox could spell
// them: host is the bare host, authority its CONNECT and URL form.
var ipLiteralSpellings = []struct{ name, authority string }{
	{"dotted quad", "8.8.8.8"},
	{"dotted triple", "8.8.2056"},
	{"dotted double", "8.526344"},
	{"bare decimal", "134744072"},
	{"hex", "0x08080808"},
	{"octal", "010.010.010.010"},
	{"bracketed v6", "[2001:4860:4860::8888]"},
	{"zoned v6", "[2001:4860:4860::8888%25eth0]"},
	{"zoned link-local v6", "[fe80::1%25eth0]"},
	{"v4-mapped v6", "[::ffff:8.8.8.8]"},
}

// ipLiteralRefused reports whether rows hold the deny a literal earns: policy:ip-literal, or the
// blocked-range deny a link-local address already gets.
func ipLiteralRefused(rows string) bool {
	return strings.Contains(rows, `"rule_source":"`+ruleSourceIPLiteral+`"`) ||
		strings.Contains(rows, `"rule_source":"builtin:private-ip"`)
}

func TestRefuseIPLiterals_EverySpellingOnBothPaths(t *testing.T) {
	spec := types.RunPolicySpec{AllowAllEgress: true, RefuseIPLiterals: true}
	for _, tc := range ipLiteralSpellings {
		t.Run(tc.name+" CONNECT", func(t *testing.T) {
			upstream, dials := countingUpstream(t)
			p, buf := newRuleTestProxy(t, spec, upstream, false, "", nil)
			srv := httptest.NewServer(p)
			defer srv.Close()
			conn, resp := connectThrough(t, srv.URL, tc.authority+":443")
			defer conn.Close()
			if !strings.Contains(resp, " 403 ") || dials.Load() != 0 || !ipLiteralRefused(buf.String()) {
				t.Fatalf("CONNECT %s = %q, %d dials, rows %s; want a 403 literal refusal and no dial", tc.authority, resp, dials.Load(), buf.String())
			}
		})
		t.Run(tc.name+" absolute-URI HTTP", func(t *testing.T) {
			upstream, dials := countingUpstream(t)
			p, buf := newRuleTestProxy(t, spec, upstream, false, "", nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://"+tc.authority+"/x"))
			if rec.Code != http.StatusForbidden || dials.Load() != 0 || !ipLiteralRefused(buf.String()) {
				t.Fatalf("GET http://%s/ = %d, %d dials, rows %s; want a 403 literal refusal and no dial", tc.authority, rec.Code, dials.Load(), buf.String())
			}
		})
	}
}

func TestRefuseIPLiterals_ExactListedAndOff(t *testing.T) {
	eval := func(spec types.RunPolicySpec, host string) (egress.Decision, string) {
		p, _ := newRuleTestProxy(t, spec, "127.0.0.1:1", false, "", nil)
		d, _, log := p.evaluate(context.Background(), host, 443, http.MethodConnect, "")
		src := ""
		if log != nil {
			src = log.RuleSource
		}
		return d, src
	}
	on := types.RunPolicySpec{AllowAllEgress: true, RefuseIPLiterals: true, AllowedDomains: []string{"8.8.8.8"}}
	if d, src := eval(on, "8.8.8.8"); d != egress.Allow {
		t.Errorf("an exactly listed literal = %s %s, want allow", d, src)
	}
	for _, host := range []string{"0x08080808", "134744072", "8.8.4.4"} {
		if d, src := eval(on, host); d != egress.Deny || src != ruleSourceIPLiteral {
			t.Errorf("%s with only 8.8.8.8 listed = %s %s, want deny %s", host, d, src, ruleSourceIPLiteral)
		}
	}
	if d, src := eval(on, "public.test"); d != egress.Allow {
		t.Errorf("a hostname = %s %s, want allow", d, src)
	}
	if d, src := eval(types.RunPolicySpec{AllowAllEgress: true}, "8.8.8.8"); d != egress.Allow {
		t.Errorf("a literal with the switch off = %s %s, want allow", d, src)
	}
}
