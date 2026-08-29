// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newInternalHostsProxy builds a Proxy wired with SiteConfig.InternalHosts
// (opts) and no upstream corp proxy (Upstream: nil, per the plan's binding
// resolution for every 6d test).
func newInternalHostsProxy(t *testing.T, spec types.RunPolicySpec, res resolver, hosts []types.InternalHost, localSubnets []*net.IPNet, cpIP net.IP, upstreamAddr string) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	return newProxy(Options{
		RunID:          uuid.New(),
		Policy:         CompilePolicy(spec),
		Sink:           sink,
		Resolver:       res,
		Dial:           redirectDial(upstreamAddr),
		InternalHosts:  hosts,
		LocalSubnets:   localSubnets,
		ControlPlaneIP: cpIP,
	}), buf
}

// TestInternalHost_DeclaredSuffixInCIDR_Allowed_RuleSourceSiteConfig is the
// true end-to-end proof: a hostname resolving inside a declared internal-host
// CIDR is reached (the request actually completes against a live upstream)
// and its decision log names the lift.
func TestInternalHost_DeclaredSuffixInCIDR_Allowed_RuleSourceSiteConfig(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer up.Close()
	res := fakeResolver{m: map[string][]net.IP{"registry.corp.internal": ips("10.40.1.5")}}
	p, buf := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"registry.corp.internal"}},
		res,
		[]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}}},
		nil, nil, upstreamAddr(up))

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://registry.corp.internal/"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (request must actually reach the upstream)", rec.Code)
	}
	if d := lastDecision(t, buf); d.RuleSource != "site-config:internal-host" {
		t.Fatalf("rule_source = %q, want site-config:internal-host", d.RuleSource)
	}
}

// TestInternalHost_UndeclaredHostSameRange_BuiltinPrivateIP: a host in the
// SAME private range, but never declared, keeps the unconditional deny.
func TestInternalHost_UndeclaredHostSameRange_BuiltinPrivateIP(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"other.example.test": ips("10.40.1.5")}}
	p, buf := newInternalHostsProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"other.example.test"}},
		res,
		[]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}}},
		nil, nil, "127.0.0.1:1")

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://other.example.test/"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if d := lastDecision(t, buf); d.RuleSource != "builtin:private-ip" {
		t.Fatalf("rule_source = %q, want builtin:private-ip", d.RuleSource)
	}
}

// TestInternalHost_DeclaredHostOutsideCIDR_Denied: the suffix matches, but the
// resolved address falls outside the entry's declared CIDR.
func TestInternalHost_DeclaredHostOutsideCIDR_Denied(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"registry.corp.internal": ips("10.99.1.5")}}
	p, _ := newInternalHostsProxy(t, types.RunPolicySpec{}, res,
		[]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}}},
		nil, nil, "127.0.0.1:1")
	if guard := p.vetHost("registry.corp.internal"); !guard.Denied {
		t.Fatalf("resolved address outside the declared CIDR must stay denied, got %+v", guard)
	}
}

// TestInternalHost_MixedAnswerWithLoopback_Denied: a declared host resolving
// to BOTH a liftable address and a loopback address must be denied wholesale —
// the DNS-rebinding "any blocked answer denies the whole host" rule is
// unaffected by the lift.
func TestInternalHost_MixedAnswerWithLoopback_Denied(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{
		"registry.corp.internal": ips("10.40.1.5", "127.0.0.1"),
	}}
	p, _ := newInternalHostsProxy(t, types.RunPolicySpec{}, res,
		[]types.InternalHost{{HostSuffix: "corp.internal"}}, // no CIDRs: full liftable set
		nil, nil, "127.0.0.1:1")
	if guard := p.vetHost("registry.corp.internal"); !guard.Denied {
		t.Fatalf("a loopback answer must deny the whole host even when another answer is liftable, got %+v", guard)
	}
}

// TestInternalHost_MetadataAndNAT64Answers_Denied: metadata and NAT64-embedded
// answers are never liftable, declared or not.
func TestInternalHost_MetadataAndNAT64Answers_Denied(t *testing.T) {
	for _, tc := range []struct {
		name string
		ip   string
	}{
		{"metadata", "169.254.169.254"},
		{"nat64-embedded-metadata", "64:ff9b::a9fe:a9fe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := fakeResolver{m: map[string][]net.IP{"gateway.corp.internal": ips(tc.ip)}}
			p, _ := newInternalHostsProxy(t, types.RunPolicySpec{}, res,
				[]types.InternalHost{{HostSuffix: "corp.internal"}},
				nil, nil, "127.0.0.1:1")
			if guard := p.vetHost("gateway.corp.internal"); !guard.Denied {
				t.Fatalf("%s must never be liftable, got %+v", tc.ip, guard)
			}
		})
	}
}

// TestInternalHost_OwnSubnetOrControlPlane_Denied: a declared, in-CIDR address
// is still refused when it is the proxy's own interface subnet or its
// resolved control-plane host.
func TestInternalHost_OwnSubnetOrControlPlane_Denied(t *testing.T) {
	_, ownSubnet, err := net.ParseCIDR("10.40.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("own subnet", func(t *testing.T) {
		res := fakeResolver{m: map[string][]net.IP{"registry.corp.internal": ips("10.40.0.5")}}
		p, _ := newInternalHostsProxy(t, types.RunPolicySpec{}, res,
			[]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}}},
			[]*net.IPNet{ownSubnet}, nil, "127.0.0.1:1")
		if guard := p.vetHost("registry.corp.internal"); !guard.Denied {
			t.Fatalf("an address on the proxy's own subnet must never be lifted, got %+v", guard)
		}
	})
	t.Run("control plane host", func(t *testing.T) {
		cpIP := net.ParseIP("10.40.0.9")
		res := fakeResolver{m: map[string][]net.IP{"registry.corp.internal": ips("10.40.0.9")}}
		p, _ := newInternalHostsProxy(t, types.RunPolicySpec{}, res,
			[]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}}},
			nil, cpIP, "127.0.0.1:1")
		if guard := p.vetHost("registry.corp.internal"); !guard.Denied {
			t.Fatalf("the resolved control-plane host must never be lifted, got %+v", guard)
		}
	})
}

// TestInternalHost_LiteralIPFastPath_Lifted: vetHostLift's literal-IP branch
// (host is already the dotted-decimal address, no DNS involved) honors the
// lift exactly like the resolved-hostname path.
func TestInternalHost_LiteralIPFastPath_Lifted(t *testing.T) {
	p, _ := newInternalHostsProxy(t, types.RunPolicySpec{}, nil,
		[]types.InternalHost{{HostSuffix: "10.40.1.5", CIDRs: []string{"10.40.0.0/16"}}},
		nil, nil, "127.0.0.1:1")
	guard := p.vetHost("10.40.1.5")
	if guard.Denied {
		t.Fatalf("declared literal IP must be lifted, got %+v", guard)
	}
	if !guard.Lifted {
		t.Fatalf("guard.Lifted must be true for the internal-host path")
	}
}

// TestInternalHost_BrokeredLLMRoute_Lifted proves egressTarget itself carries
// the lift — the SAME function every forward-egress caller uses (evaluate,
// proxyLLMRequest, the git/PAT brokers), so a brokered route benefits from a
// declared internal host exactly like evaluate() does.
func TestInternalHost_BrokeredLLMRoute_Lifted(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"gateway.corp.internal": ips("10.40.1.5")}}
	p, _ := newInternalHostsProxy(t, types.RunPolicySpec{}, res,
		[]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}}},
		nil, nil, "127.0.0.1:1")
	target, ruleSource, err := p.egressTarget("gateway.corp.internal", 443)
	if err != nil {
		t.Fatalf("egressTarget: %v", err)
	}
	if ruleSource != "site-config:internal-host" {
		t.Fatalf("ruleSource = %q, want site-config:internal-host", ruleSource)
	}
	if target != "10.40.1.5:443" {
		t.Fatalf("target = %q", target)
	}
}

// TestInternalHost_EmptyCIDRs_FullLiftableSet_NotReservedOther: an entry with
// no CIDRs lifts the full RFC1918/ULA/CGNAT set for a matching host, but NEVER
// the non-liftable ReservedV4 entries (198.18.0.0/15 benchmarking) — those
// stay blockReservedOther, which the lift predicate is never even offered.
func TestInternalHost_EmptyCIDRs_FullLiftableSet_NotReservedOther(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"bench.corp.internal": ips("198.18.0.1")}}
	p, _ := newInternalHostsProxy(t, types.RunPolicySpec{}, res,
		[]types.InternalHost{{HostSuffix: "corp.internal"}}, // no CIDRs
		nil, nil, "127.0.0.1:1")
	if guard := p.vetHost("bench.corp.internal"); !guard.Denied {
		t.Fatalf("198.18.0.1 (benchmarking, not in ipguard.Liftable) must stay denied, got %+v", guard)
	}
}

// TestInternalHost_UnparseableCIDR_DropsWholeEntry: an entry with one good
// CIDR and one unparseable CIDR must be dropped ENTIRELY, not admitted with
// only the good CIDR — liftInternalHost treats zero CIDRs as "no CIDRs
// declared" and lifts the FULL liftable set for the suffix, so silently
// dropping only the bad CIDR out of an entry meant to be narrow would widen
// it into that full-set default instead of narrowing it.
func TestInternalHost_UnparseableCIDR_DropsWholeEntry(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"gateway.corp.internal": ips("10.40.1.5")}}
	p, _ := newInternalHostsProxy(t, types.RunPolicySpec{}, res,
		[]types.InternalHost{{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16", "not-a-cidr"}}},
		nil, nil, "127.0.0.1:1")
	if guard := p.vetHost("gateway.corp.internal"); !guard.Denied {
		t.Fatalf("an entry carrying one unparseable CIDR must be dropped whole (fail closed), got %+v", guard)
	}
}
