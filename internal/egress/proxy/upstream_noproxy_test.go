// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// routingDialer is the seam these tests need that redirectDial cannot give:
// with an upstream configured BOTH the corp-proxy hop and a bypassed direct
// dial go through Proxy.dial, so a single fixed redirect target cannot tell
// them apart. This routes the corp proxy's own pinned address to the fake
// upstream and everything else to a "direct" server, and records every addr.
type routingDialer struct {
	upstreamAddr string // the corp proxy's host:port (dial it for real)
	directAddr   string // where a bypassed/direct dial should actually land
	mu           sync.Mutex
	addrs        []string
}

func (d *routingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	to := d.directAddr
	if addr == d.upstreamAddr {
		to = d.upstreamAddr
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, to)
}

func (d *routingDialer) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// newNoProxyProxy builds a Proxy with a corporate upstream, a bypass list and
// (optionally) internal-host declarations, dialing through routingDialer.
func newNoProxyProxy(t *testing.T, spec types.RunPolicySpec, res resolver, noProxy []string,
	hosts []types.InternalHost, up *upstreamProxy, d *routingDialer) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	return newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(spec),
		Sink:            sink,
		Resolver:        res,
		Dial:            d.dial,
		Upstream:        up,
		UpstreamNoProxy: noProxy,
		InternalHosts:   hosts,
	}), buf
}

func mustUpstream(t *testing.T, addr string) *upstreamProxy {
	t.Helper()
	up, err := parseUpstreamProxy("http://" + addr)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	return up
}

// TestNoProxy_BypassedHostDialsDirect_OthersStillUpstream is gap 1's core
// claim on the FORWARDING TRANSPORT path (handlePlain -> egressDial): a
// declared destination is dialed directly while every other host keeps
// chaining through the corp proxy. It is also the regression for fixing the
// bypass only in egressTarget: the transport CONNECTs everything through the
// upstream independently of that branch, so a target-only change would leave
// the corp proxy still taking the dial.
func TestNoProxy_BypassedHostDialsDirect_OthersStillUpstream(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer direct.Close()
	f := startFakeUpstream(t)
	d := &routingDialer{upstreamAddr: f.addr(), directAddr: upstreamAddr(direct)}
	res := fakeResolver{m: map[string][]net.IP{
		"mirror.corp.internal": ips("93.184.216.34"),
		"api.example.com":      ips("93.184.216.34"),
	}}
	p, _ := newNoProxyProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"mirror.corp.internal", "api.example.com"}},
		res, []string{".corp.internal"}, nil, mustUpstream(t, f.addr()), d)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://mirror.corp.internal/"))
	if rec.Code != http.StatusOK {
		t.Fatalf("bypassed host status = %d, want 200 (it must reach the direct server)", rec.Code)
	}
	if got := atomic.LoadInt32(&f.accepts); got != 0 {
		t.Fatalf("bypassed host went through the corp proxy (accepts=%d) — the bypass did not reach the dial", got)
	}
	// A bypassed dial must carry the RESOLVED, vetted address (no re-resolution).
	if got := d.dialed(); len(got) != 1 || got[0] != "93.184.216.34:80" {
		t.Fatalf("bypassed dial addrs = %v, want [93.184.216.34:80] (the vetted IP, dialed directly)", got)
	}

	// The un-declared host still chains through the corp proxy, by NAME.
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://api.example.com/"))
	if got := atomic.LoadInt32(&f.accepts); got != 1 {
		t.Fatalf("non-bypassed host accepts=%d, want 1 (it must still chain through the corp proxy)", got)
	}
	if gotConnect, _ := f.snapshot(); gotConnect != "CONNECT api.example.com:80" {
		t.Fatalf("corp proxy saw %q, want CONNECT api.example.com:80", gotConnect)
	}
}

// TestNoProxy_EmptyListKeepsEveryDialOnTheUpstream is the absent-config pin:
// with no bypass declared, the very host the previous test bypassed still goes
// through the corp proxy — byte-identical to before the field existed.
func TestNoProxy_EmptyListKeepsEveryDialOnTheUpstream(t *testing.T) {
	f := startFakeUpstream(t)
	d := &routingDialer{upstreamAddr: f.addr(), directAddr: f.addr()}
	res := fakeResolver{m: map[string][]net.IP{"mirror.corp.internal": ips("93.184.216.34")}}
	p, _ := newNoProxyProxy(t, types.RunPolicySpec{AllowedDomains: []string{"mirror.corp.internal"}},
		res, nil, nil, mustUpstream(t, f.addr()), d)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://mirror.corp.internal/"))
	if got := atomic.LoadInt32(&f.accepts); got != 1 {
		t.Fatalf("accepts=%d, want 1 — an empty bypass list must leave every dial on the upstream", got)
	}
}

// TestNoProxy_ConnectTunnelBypasses covers the OPAQUE CONNECT path
// (handleConnect), which never touches the transport and is the path a
// PrivateLink model endpoint actually takes (SigV4/Bedrock is never MITM'd).
func TestNoProxy_ConnectTunnelBypasses(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer direct.Close()
	f := startFakeUpstream(t)
	d := &routingDialer{upstreamAddr: f.addr(), directAddr: upstreamAddr(direct)}
	res := fakeResolver{m: map[string][]net.IP{"vpce-1-bedrock-runtime.us-east-1.vpce.amazonaws.com": ips("100.64.5.7")}}
	p, buf := newNoProxyProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"vpce-1-bedrock-runtime.us-east-1.vpce.amazonaws.com"}},
		res, []string{"vpce.amazonaws.com"},
		[]types.InternalHost{{HostSuffix: "vpce.amazonaws.com", CIDRs: []string{"100.64.0.0/10"}}},
		mustUpstream(t, f.addr()), d)

	srv := httptest.NewServer(p)
	defer srv.Close()
	conn, status := connectThrough(t, srv.URL, "vpce-1-bedrock-runtime.us-east-1.vpce.amazonaws.com:443")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	if got := atomic.LoadInt32(&f.accepts); got != 0 {
		t.Fatalf("CONNECT to a bypassed host went through the corp proxy (accepts=%d)", got)
	}
	if got := d.dialed(); len(got) != 1 || got[0] != "100.64.5.7:443" {
		t.Fatalf("dial addrs = %v, want [100.64.5.7:443]", got)
	}
	if got := lastDecision(t, buf).RuleSource; got != ruleSourceInternalHost {
		t.Fatalf("rule_source = %q, want %q", got, ruleSourceInternalHost)
	}
}

// TestNoProxy_BypassedHostWithoutInternalHostsIsDenied is THE SAFETY PIN. The
// bypass is a ROUTING decision: it must never lift the private/reserved-IP
// SSRF guard. A declared-bypass host whose address is RFC 6598 and that has NO
// SiteConfig.InternalHosts declaration is still denied — and denied HERE, not
// merely refused by a corp proxy that never saw the dial.
func TestNoProxy_BypassedHostWithoutInternalHostsIsDenied(t *testing.T) {
	f := startFakeUpstream(t)
	d := &routingDialer{upstreamAddr: f.addr(), directAddr: f.addr()}
	res := fakeResolver{m: map[string][]net.IP{"vpce-1-bedrock.us-east-1.vpce.amazonaws.com": ips("100.64.5.7")}}
	p, buf := newNoProxyProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"vpce-1-bedrock.us-east-1.vpce.amazonaws.com"}},
		res, []string{"vpce.amazonaws.com"}, nil /* no InternalHosts */, mustUpstream(t, f.addr()), d)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://vpce-1-bedrock.us-east-1.vpce.amazonaws.com/"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — the bypass must not lift the SSRF guard", rec.Code)
	}
	if got := lastDecision(t, buf).RuleSource; got != "builtin:private-ip" {
		t.Fatalf("rule_source = %q, want builtin:private-ip", got)
	}
	if got := len(d.dialed()); got != 0 {
		t.Fatalf("a denied bypassed host must never be dialed at all (dials=%d)", got)
	}
	// And the refusal says which knob fixes it.
	if got := rec.Header().Get(egressHeaderDetail); !strings.Contains(got, "internal_hosts") {
		t.Fatalf("detail header = %q, want it to name internal_hosts", got)
	}
}

// TestNoProxy_ComposesWithInternalHosts is the composition the whole feature
// is: bypass routes the dial DIRECT, InternalHosts admits the 6598 address it
// lands on. Neither alone suffices — the previous test is the same
// configuration minus the declaration, and it is denied.
func TestNoProxy_ComposesWithInternalHosts(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer direct.Close()
	f := startFakeUpstream(t)
	d := &routingDialer{upstreamAddr: f.addr(), directAddr: upstreamAddr(direct)}
	res := fakeResolver{m: map[string][]net.IP{"vpce-1-bedrock.us-east-1.vpce.amazonaws.com": ips("100.64.5.7")}}
	p, buf := newNoProxyProxy(t,
		types.RunPolicySpec{AllowedDomains: []string{"vpce-1-bedrock.us-east-1.vpce.amazonaws.com"}},
		res, []string{"vpce.amazonaws.com"},
		[]types.InternalHost{{HostSuffix: "vpce.amazonaws.com", CIDRs: []string{"100.64.0.0/10"}}},
		mustUpstream(t, f.addr()), d)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://vpce-1-bedrock.us-east-1.vpce.amazonaws.com/"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — bypass + internal_hosts must reach a 100.64/10 endpoint", rec.Code)
	}
	if got := lastDecision(t, buf).RuleSource; got != ruleSourceInternalHost {
		t.Fatalf("rule_source = %q, want %q", got, ruleSourceInternalHost)
	}
	if got := atomic.LoadInt32(&f.accepts); got != 0 {
		t.Fatalf("the composed dial must not touch the corp proxy (accepts=%d)", got)
	}
}

// TestNoProxy_BypassIsNotAPolicyAllow: a declared bypass changes routing only.
// A host nobody allowed is still refused by policy, before any dial.
func TestNoProxy_BypassIsNotAPolicyAllow(t *testing.T) {
	f := startFakeUpstream(t)
	d := &routingDialer{upstreamAddr: f.addr(), directAddr: f.addr()}
	res := fakeResolver{m: map[string][]net.IP{"mirror.corp.internal": ips("93.184.216.34")}}
	p, buf := newNoProxyProxy(t, types.RunPolicySpec{AllowedDomains: []string{"other.example.com"}},
		res, []string{"corp.internal"}, nil, mustUpstream(t, f.addr()), d)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://mirror.corp.internal/"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := lastDecision(t, buf).RuleSource; got != "policy:default-deny" {
		t.Fatalf("rule_source = %q, want policy:default-deny", got)
	}
	if got := len(d.dialed()); got != 0 {
		t.Fatalf("a policy-denied host must never be dialed (dials=%d)", got)
	}
}

// TestBypassUpstreamMatching pins the NO_PROXY spelling rules: label-suffix
// host matching (never mid-label), leading-dot equivalence, CIDR matching for
// literal-IP destinations, and no wildcard.
func TestBypassUpstreamMatching(t *testing.T) {
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		UpstreamNoProxy: []string{".corp.internal", "vpce.amazonaws.com", "100.64.0.0/10", "*", "not a host"},
	})
	cases := []struct {
		host string
		want bool
	}{
		{"corp.internal", true},
		{"mirror.corp.internal", true},
		{"MIRROR.CORP.INTERNAL.", true},
		{"notcorp.internal", false},       // never a mid-label/substring match
		{"corp.internal.evil.com", false}, // suffix, not prefix
		{"vpce-1-bedrock.vpce.amazonaws.com", true},
		{"100.64.5.7", true},            // CIDR entry, literal destination
		{"10.0.0.5", false},             // private but undeclared
		{"anything.example.com", false}, // "*" must never have compiled
		{"", false},
	}
	for _, c := range cases {
		if got := p.bypassUpstream(c.host); got != c.want {
			t.Errorf("bypassUpstream(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestValidNoProxyEntry pins the one shared spelling rule the write-time
// validator and the compiler both use (no dual matcher).
func TestValidNoProxyEntry(t *testing.T) {
	ok := []string{"corp.internal", ".corp.internal", "CORP.INTERNAL", "registry", "100.64.0.0/10", "fd00::/8"}
	bad := []string{"", "  ", "*", "*.corp.internal", "http://proxy", "corp internal", "100.64.0.0/99", ":8080"}
	for _, e := range ok {
		if !ValidNoProxyEntry(e) {
			t.Errorf("ValidNoProxyEntry(%q) = false, want true", e)
		}
	}
	for _, e := range bad {
		if ValidNoProxyEntry(e) {
			t.Errorf("ValidNoProxyEntry(%q) = true, want false", e)
		}
	}
	// Whatever the validator refuses, the compiler must drop — never widen.
	if got := compileNoProxy(bad); len(got) != 0 {
		t.Fatalf("compileNoProxy(bad) = %v, want none", got)
	}
}

// TestRedirectLiteralIP_TrustedForItsRunOnly is gap 3: an egress redirect's
// literal-IP To is reachable on EVERY vet path for the run whose allowlist the
// redirect substitution actually wrote it into (egressTarget is the path
// serveMITMRequest and both brokers re-vet through, and it used to re-deny
// what evaluate() had already trusted), and is refused for an unrelated run.
func TestRedirectLiteralIP_TrustedForItsRunOnly(t *testing.T) {
	mk := func(spec types.RunPolicySpec) *Proxy {
		return newProxy(Options{
			RunID:    uuid.New(),
			Policy:   CompilePolicy(spec),
			Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
			Resolver: publicResolver{},
		})
	}
	// The run the redirect covers: substituteArtifactEgress put the To host on
	// its allowlist.
	inScope := mk(types.RunPolicySpec{AllowedDomains: []string{"100.64.5.7"}})
	target, src, err := inScope.egressTarget("100.64.5.7", 443)
	if err != nil {
		t.Fatalf("redirect To must be reachable for the run it covers: %v", err)
	}
	if target != "100.64.5.7:443" {
		t.Fatalf("target = %q, want 100.64.5.7:443", target)
	}
	if src != ruleSourceEgressRedirect {
		t.Fatalf("rule_source = %q, want %q", src, ruleSourceEgressRedirect)
	}

	// An unrelated run — same address, no allowlist entry — is refused.
	if _, _, err := mk(types.RunPolicySpec{AllowedDomains: []string{"api.example.com"}}).
		egressTarget("100.64.5.7", 443); err == nil {
		t.Fatal("a run whose redirect does not target this literal must be denied")
	}

	// A DENY always wins, even with the allow entry present.
	if _, _, err := mk(types.RunPolicySpec{
		AllowedDomains: []string{"100.64.5.7"},
		DeniedDomains:  []string{"100.64.5.7"},
	}).egressTarget("100.64.5.7", 443); err == nil {
		t.Fatal("a denied literal must stay denied")
	}
}

// TestRedirectLiteralIP_AuditedAsItsOwnGrant: the end-to-end decision log
// attributes reaching a private address to the grant that allowed it, not to a
// generic policy:allowed.
func TestRedirectLiteralIP_AuditedAsItsOwnGrant(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer direct.Close()
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"100.64.5.7"}}),
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
		Resolver: publicResolver{},
		Dial:     redirectDial(upstreamAddr(direct)),
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://100.64.5.7/"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := lastDecision(t, buf).RuleSource; got != ruleSourceEgressRedirect {
		t.Fatalf("rule_source = %q, want %q", got, ruleSourceEgressRedirect)
	}
}

// TestLiteralIPDenialNamesTheCause: every other literal-IP refusal says WHAT
// was refused and WHY, so a correct private-endpoint configuration is never
// read as an entitlement problem.
func TestLiteralIPDenialNamesTheCause(t *testing.T) {
	pol := CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"10.0.0.9", "priv.example.test"},
		DeniedDomains:  []string{"10.0.0.9"},
	})
	cases := []struct {
		name string
		host string
		want []string
	}{
		{"explicitly denied", "10.0.0.9", []string{"10.0.0.9", "denied_domains"}},
		{"unlisted literal", "100.64.5.7", []string{"100.64.5.7", "allowed_domains", "egress_redirects"}},
		{"hostname resolving private", "priv.example.test", []string{"internal_hosts"}},
	}
	for _, c := range cases {
		got := literalIPDenialDetail(c.host, 443, pol)
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: detail %q must name %q", c.name, got, want)
			}
		}
	}
	if got := literalIPDenialDetail("", 443, pol); got != "" {
		t.Errorf("empty host detail = %q, want \"\"", got)
	}
}
