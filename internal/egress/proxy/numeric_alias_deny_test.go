// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// aliasesOf8888 are inet_aton spellings of 8.8.8.8 that net.ParseIP refuses: one 32-bit word in decimal
// and hex, octal and hex quads, and the two- and three-part forms.
var aliasesOf8888 = []string{"134744072", "0x8080808", "010.010.010.010", "0x08.0x08.0x08.0x08", "8.526344", "8.8.2056"}

// A denied address stays denied in every spelling a resolver or a corporate upstream reads as it: the
// policy's denied_domains (bare and port-qualified) and the live deny list. Deny only: an alias never
// inherits an allow, and an alias of an address nothing denies is left to allow_all.
func TestNumericAliasOfADeniedAddressIsDenied(t *testing.T) {
	for _, host := range append(aliasesOf8888, "2001:db8::1%eth0") {
		p := CompilePolicy(types.RunPolicySpec{DeniedDomains: []string{"8.8.8.8", "2001:db8::1"}, AllowAllEgress: true})
		if got := p.evalHost(host, 443); got != hostDeny {
			t.Errorf("denied_domains: evalHost(%q) = %s, want deny", host, got)
		}
		live := CompilePolicy(types.RunPolicySpec{AllowAllEgress: true})
		live.live.set([]string{"8.8.8.8", "2001:db8::1"})
		if got := live.evalHost(host, 443); got != hostDeny {
			t.Errorf("live deny: evalHost(%q) = %s, want deny", host, got)
		}
	}
	port := CompilePolicy(types.RunPolicySpec{DeniedDomains: []string{"8.8.8.8:53"}, AllowAllEgress: true})
	if got := port.evalHost("134744072", 53); got != hostDeny {
		t.Errorf("port-qualified deny: 134744072:53 = %s, want deny", got)
	}
	if got := port.evalHost("134744072", 443); got != hostAllow {
		t.Errorf("port-qualified deny: 134744072:443 = %s, want allow (the entry binds :53 only)", got)
	}
	allow := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"8.8.8.8"}})
	if got := allow.evalHost("134744072", 443); got != hostUnknown {
		t.Errorf("allowed_domains 8.8.8.8: evalHost(134744072) = %s, want unknown (an alias inherits no allow)", got)
	}
	other := CompilePolicy(types.RunPolicySpec{DeniedDomains: []string{"8.8.8.8"}, AllowAllEgress: true})
	if got := other.evalHost("0x8080404", 443); got != hostAllow {
		t.Errorf("an alias of 8.8.4.4 = %s, want allow under allow_all_egress", got)
	}
}

// The refusal comes before the corporate upstream is dialled: the hop that would read the alias as the
// denied address is never handed it, for a denied_domains entry and for a live deny.
func TestNumericAliasOfADeniedAddressNeverReachesTheCorpProxy(t *testing.T) {
	for name, set := range map[string]func(p *Proxy){
		"denied_domains": func(*Proxy) {},
		"live deny":      func(p *Proxy) { p.policy.live.set([]string{"8.8.8.8"}) },
	} {
		t.Run(name, func(t *testing.T) {
			f := startFakeUpstream(t)
			up, err := parseUpstreamProxy("http://" + f.addr())
			if err != nil {
				t.Fatal(err)
			}
			spec := types.RunPolicySpec{AllowAllEgress: true}
			if name == "denied_domains" {
				spec.DeniedDomains = []string{"8.8.8.8"}
			}
			p := newUpstreamProxyPolicy(t, up, spec)
			set(p)
			srv := httptest.NewServer(p)
			defer srv.Close()
			for _, host := range aliasesOf8888 {
				conn, status := connectThrough(t, srv.URL, host+":443")
				_ = conn.Close()
				if !strings.Contains(status, "403") {
					t.Errorf("CONNECT %s:443 = %q, want 403", host, status)
				}
				if got, _ := f.snapshot(); got != "" {
					t.Fatalf("the corp proxy was handed %q for %s, an alias of a denied address", got, host)
				}
			}
			// Control: an alias of an address nothing denies is still chained, so the empty snapshot
			// above means refused, not unseen.
			conn, status := connectThrough(t, srv.URL, "0x8080404:443")
			_ = conn.Close()
			if got, _ := f.snapshot(); !strings.Contains(status, "200") || got != "CONNECT 0x8080404:443" {
				t.Fatalf("control: status %q, corp proxy saw %q", status, got)
			}
		})
	}
}

// A Kubernetes runner's hard deny covers every spelling of its addresses, public ones included (an API
// server endpoint): refused at step 0 like the canonical spelling, by the name check egressTarget and
// the resolver vet run first, with the hard deny's own reason, and never handed to the corp proxy.
func TestHardDenyCoversEverySpellingOfItsAddresses(t *testing.T) {
	hd, err := compileHardDeny(&HardDenyConfig{Hosts: []string{"1.2.3.4"}, CIDRs: []string{"203.0.113.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"16909060", "0x1020304", "1.2.772", "01.02.03.04", "0xcb007109", "203.0.28937"} {
		if !hd.name(host) {
			t.Errorf("hardDeny.name(%q) = false, want true", host)
		}
	}
	if hd.name("16909061") {
		t.Error("hardDeny.name(16909061) = true: 1.2.3.5 is not in the set")
	}

	f := startFakeUpstream(t)
	up, err := parseUpstreamProxy("http://" + f.addr())
	if err != nil {
		t.Fatal(err)
	}
	p := newUpstreamProxyPolicy(t, up, types.RunPolicySpec{AllowAllEgress: true})
	p.hardDeny = hd
	if _, denied := p.literalIPGuard(egress.Request{Host: "16909060", Port: 443}, "16909060", 443); denied == nil {
		t.Error("literalIPGuard(16909060) admitted an alias of a hard-denied address at step 0")
	}
	if kind, _ := p.privateIPBlockKind("16909060", 443); kind != blockHardDeny {
		t.Errorf("privateIPBlockKind(16909060) = %v, want blockHardDeny", kind)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	conn, status := connectThrough(t, srv.URL, "16909060:443")
	_ = conn.Close()
	if !strings.Contains(status, "403") || !strings.Contains(status, "in-cluster") || strings.Contains(status, "write the address as") {
		t.Errorf("CONNECT 16909060:443 = %q, want a 403 naming the hard deny", status)
	}
	if got, _ := f.snapshot(); got != "" {
		t.Fatalf("the corp proxy was handed %q, an alias of a hard-denied address", got)
	}
}
