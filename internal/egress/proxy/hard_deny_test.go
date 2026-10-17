// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestHardDenyBeforeInternalHostsLift: on a k8s runner the hard-deny set wins
// over a declared internal_hosts lift, on the name and on every resolved
// address, and refuses a public apiserver address no private guard covers.
func TestHardDenyBeforeInternalHostsLift(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer up.Close()
	res := fakeResolver{m: map[string][]net.IP{
		"registry.corp.internal": ips("10.40.1.5"),  // control: lifted, reached
		"model.corp.internal":    ips("10.96.0.12"), // inside a hard-deny CIDR
		"api.corp.internal":      ips("10.40.1.9"),  // relay IP in hard_deny.hosts
		"kube.example.com":       ips("1.2.3.4"),    // public apiserver address
		"mixed.corp.internal":    ips("10.40.1.6", "10.96.0.1"),
		"gw.default.svc":         ips("10.40.1.7"),
	}}
	p, _ := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"*.corp.internal", "*.svc", "kube.example.com", "1.2.3.4"}},
		res,
		[]types.InternalHost{{HostSuffix: "corp.internal"}, {HostSuffix: "svc"}},
		nil, nil, upstreamAddr(up))
	hd, err := compileHardDeny(&HardDenyConfig{Suffixes: []string{"svc", "cluster.local"}, Hosts: []string{"10.40.1.9", "1.2.3.4"}, CIDRs: []string{"10.96.0.0/12"}})
	if err != nil {
		t.Fatal(err)
	}
	// Control first (a refusal is memoed per host): the public names are
	// refused by the hard deny alone.
	for _, target := range []string{"http://kube.example.com/", "http://1.2.3.4/"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, target))
		if rec.Code != http.StatusOK {
			t.Errorf("%s without hard_deny: status %d, want 200", target, rec.Code)
		}
	}
	p.hardDeny = hd

	for target, want := range map[string]int{
		"http://registry.corp.internal/": http.StatusOK,
		"http://model.corp.internal/":    http.StatusForbidden,
		"http://api.corp.internal/":      http.StatusForbidden,
		"http://kube.example.com/":       http.StatusForbidden,
		"http://1.2.3.4/":                http.StatusForbidden,
		"http://mixed.corp.internal/":    http.StatusForbidden,
		"http://gw.default.svc/":         http.StatusForbidden,
		"http://x.cluster.local./":       http.StatusForbidden,
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, target))
		if rec.Code != want {
			t.Errorf("%s: status %d, want %d", target, rec.Code, want)
		}
	}
	if _, _, err := p.egressTarget("gw.default.svc", 443); blockKindOf(err) != blockHardDeny {
		t.Fatalf("egressTarget(svc) = %v, want a hard-deny refusal", err)
	}
}

// TestHardDenyBeatsExactLiteralTrust: an exactly allow-listed private literal skips egressTarget (the
// operator's literal trust), so the hard deny must refuse it before that trust: the relay ClusterIP or
// runnerd's pod IP typed into allowed_domains stays unreachable. The control literal is still trusted.
func TestHardDenyBeatsExactLiteralTrust(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer up.Close()
	p, _ := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"10.40.1.5", "10.40.1.9", "10.96.0.12"}},
		fakeResolver{m: map[string][]net.IP{}}, nil, nil, nil, upstreamAddr(up))
	hd, err := compileHardDeny(&HardDenyConfig{Suffixes: []string{"svc"}, Hosts: []string{"10.40.1.9"}, CIDRs: []string{"10.96.0.0/12"}})
	if err != nil {
		t.Fatal(err)
	}
	p.hardDeny = hd
	for target, want := range map[string]int{
		"http://10.40.1.5/":       http.StatusOK,        // control: an exact private literal is trusted
		"http://10.40.1.9/":       http.StatusForbidden, // in hard_deny.hosts
		"http://10.96.0.12:8443/": http.StatusForbidden, // inside a hard-deny CIDR
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, target))
		if rec.Code != want {
			t.Errorf("%s: status %d, want %d", target, rec.Code, want)
		}
	}
}

// TestHardDenyAbsentLeavesGuardUnchanged: no hard_deny (Docker, server) keeps
// today's lift.
func TestHardDenyAbsentLeavesGuardUnchanged(t *testing.T) {
	if hd, err := compileHardDeny(nil); hd != nil || err != nil {
		t.Fatalf("compileHardDeny(nil) = %v, %v", hd, err)
	}
	if _, err := compileHardDeny(&HardDenyConfig{CIDRs: []string{"not-a-cidr"}}); err == nil {
		t.Fatal("bad CIDR accepted")
	}
	if _, err := compileHardDeny(&HardDenyConfig{Suffixes: []string{" "}}); err == nil {
		t.Fatal("empty suffix accepted")
	}
}

// TestHardDenyRefusesAnUpstreamProxyInsideIt: an upstream proxy named by an address in the deny set is
// refused at boot; one outside it, a name, or no deny set is accepted.
func TestHardDenyRefusesAnUpstreamProxyInsideIt(t *testing.T) {
	deny := &HardDenyConfig{CIDRs: []string{"10.96.0.0/12"}, Hosts: []string{"10.40.1.9"}}
	for _, tc := range []struct {
		name string
		cfg  Config
		bad  bool
	}{
		{"in a CIDR", Config{HardDeny: deny, UpstreamProxyURL: "http://10.96.4.2:3128"}, true},
		{"a listed host", Config{HardDeny: deny, UpstreamProxyURL: "http://user:pw@10.40.1.9:3128"}, true},
		{"outside the set", Config{HardDeny: deny, UpstreamProxyURL: "http://192.0.2.7:3128"}, false},
		{"a name", Config{HardDeny: deny, UpstreamProxyURL: "http://proxy.corp.example:3128"}, false},
		{"no deny set", Config{UpstreamProxyURL: "http://10.96.4.2:3128"}, false},
	} {
		err := tc.cfg.validateHardDeny()
		if (err != nil) != tc.bad {
			t.Errorf("%s: %v, want refusal %v", tc.name, err, tc.bad)
		}
		if err != nil && strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: the refusal echoed the credential: %v", tc.name, err)
		}
	}
}

// TestHardDenyLiteralDetailNamesHardDeny: a literal refused by the runner's hard deny says so, rather
// than telling the agent its address is merely missing from allowed_domains.
func TestHardDenyLiteralDetailNamesHardDeny(t *testing.T) {
	p, _ := newInternalHostsProxy(t, types.RunPolicySpec{AllowedDomains: []string{"10.40.1.9"}}, fakeResolver{m: map[string][]net.IP{}}, nil, nil, nil, "")
	hd, err := compileHardDeny(&HardDenyConfig{Suffixes: []string{"svc"}, Hosts: []string{"10.40.1.9"}})
	if err != nil {
		t.Fatal(err)
	}
	p.hardDeny = hd
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://10.40.1.9/"))
	d := rec.Header().Get(egressHeaderDetail)
	if rec.Code != http.StatusForbidden || !strings.Contains(d, "in-cluster name or address this runner denies to every run") || strings.Contains(d, "not listed") {
		t.Fatalf("status %d, detail %q", rec.Code, d)
	}
}
