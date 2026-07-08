// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestEvalHost(t *testing.T) {
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"api.github.com", "*.example.com", "Example.ORG"},
		DeniedDomains:  []string{"evil.example.com", "*.blocked.example.com"},
	}
	p := CompilePolicy(spec)

	cases := []struct {
		host string
		want hostDecision
	}{
		{"api.github.com", hostAllow},          // exact
		{"API.GitHub.com", hostAllow},          // case-insensitive
		{"example.org", hostAllow},             // exact, configured mixed-case
		{"a.example.com", hostAllow},           // wildcard label match
		{"x.y.example.com", hostAllow},         // multi-label wildcard
		{"example.com", hostUnknown},           // bare apex NOT matched by *.example.com
		{"notexample.com", hostUnknown},        // suffix must be on label boundary
		{"fooexample.com", hostUnknown},        // no false suffix match
		{"evil.example.com", hostDeny},         // exact deny beats wildcard allow
		{"deep.blocked.example.com", hostDeny}, // wildcard deny beats wildcard allow
		{"github.com", hostUnknown},            // not in allowlist
		{"a.example.com.", hostAllow},          // trailing dot normalized
	}
	for _, c := range cases {
		if got := p.evalHost(c.host, 443); got != c.want {
			t.Errorf("evalHost(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

// TestEvalHostPortQualified asserts an "host:port" allow/deny entry is honored
// (matches ONLY that port) while a bare entry matches any port — ITEM 25. Before
// the fix, "api.test:443" was stored as the exact host "api.test:443" and never
// compared against the bare request host, so a port-qualified allow was silently
// dead (and a port-qualified deny never fired).
func TestEvalHostPortQualified(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"api.test:443", "*.wild.test:8443", "any.test", "zero.test:0", "big.test:99999"},
		DeniedDomains:  []string{"api.test:80", "*.wild.test:80"},
	})
	cases := []struct {
		host string
		port int
		want hostDecision
	}{
		{"api.test", 443, hostAllow},      // port-qualified allow honored (was dead)
		{"api.test", 8080, hostUnknown},   // other ports NOT allowed by :443 entry
		{"api.test", 80, hostDeny},        // port-qualified deny honored (was dead)
		{"any.test", 22, hostAllow},       // bare entry matches ANY port
		{"any.test", 443, hostAllow},      // bare entry matches ANY port
		{"a.wild.test", 8443, hostAllow},  // port-qualified wildcard allow
		{"a.wild.test", 443, hostUnknown}, // wildcard only on its :8443 port
		{"a.wild.test", 80, hostDeny},     // port-qualified wildcard deny beats allow
		// An out-of-range port qualifier ("zero.test:0", "big.test:99999") must
		// leave the entry DEAD — never silently degrade to a bare any-port allow
		// (that would widen egress fail-open on malformed input).
		{"zero.test", 22, hostUnknown},
		{"zero.test", 0, hostUnknown},
		{"big.test", 99999, hostUnknown},
		{"big.test", 443, hostUnknown},
	}
	for _, c := range cases {
		if got := p.evalHost(c.host, c.port); got != c.want {
			t.Errorf("evalHost(%q, %d) = %q, want %q", c.host, c.port, got, c.want)
		}
	}
}

func TestDenyBeatsAllowSameHost(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"dual.example.com"},
		DeniedDomains:  []string{"dual.example.com"},
	})
	if got := p.evalHost("dual.example.com", 443); got != hostDeny {
		t.Fatalf("deny must beat allow: got %q", got)
	}
}

func TestDefaultDeny(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{})
	if got := p.evalHost("anything.com", 443); got != hostUnknown {
		t.Fatalf("empty policy host = %q, want unknown (default-deny at pipeline)", got)
	}
	// And an empty allowlist must not auto-allow via exact-host injection check.
	if p.AllowedExactHost("anything.com") {
		t.Fatalf("empty allowlist must not allow any host for injection")
	}
}

func TestMethodAllowed(t *testing.T) {
	none := CompilePolicy(types.RunPolicySpec{})
	if !none.methodAllowed("GET") || !none.methodAllowed("CONNECT") {
		t.Fatalf("empty method restriction must allow all methods")
	}
	restricted := CompilePolicy(types.RunPolicySpec{AllowedMethods: []string{"get", "POST"}})
	if !restricted.methodAllowed("GET") {
		t.Errorf("GET should be allowed (case-insensitive)")
	}
	if !restricted.methodAllowed("post") {
		t.Errorf("post should be allowed (case-insensitive)")
	}
	if restricted.methodAllowed("CONNECT") {
		t.Errorf("CONNECT must be denied when not in allowed methods")
	}
	if restricted.methodAllowed("DELETE") {
		t.Errorf("DELETE must be denied")
	}
}

func TestAllowedExactHostExcludesWildcard(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{
		AllowedDomains: []string{"exact.example.com", "*.wild.example.com"},
		DeniedDomains:  []string{"bad.example.com"},
	})
	if !p.AllowedExactHost("exact.example.com") {
		t.Errorf("exact host must qualify for injection")
	}
	// Injection must NOT apply to a wildcard-matched host (no secret leak).
	if p.AllowedExactHost("a.wild.example.com") {
		t.Errorf("wildcard-matched host must NOT qualify for exact injection")
	}
	if p.AllowedExactHost("bad.example.com") {
		t.Errorf("denied host must never qualify")
	}
}

// TestAllowAllEgress exercises the "allow all (deny-list only)" mode: any
// non-denied PUBLIC host is allowed even when it is absent from
// allowed_domains, denied_domains STILL wins, and credential injection is NOT
// widened (AllowedExactHost stays false for an allow-all-only host).
func TestAllowAllEgress(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{
		AllowAllEgress: true,
		// allowed_domains may be EMPTY under allow-all; the one exact entry
		// below exists only to prove injection is gated on it, not allow-all.
		AllowedDomains: []string{"inject.example.com"},
		DeniedDomains:  []string{"blocked.example.com", "*.deny.example.com"},
	})

	// Arbitrary host NOT in allowed_domains is allowed under allow-all.
	if got := p.evalHost("random.example.com", 443); got != hostAllow {
		t.Errorf("allow-all: random.example.com = %q, want hostAllow", got)
	}
	if got := p.evalHost("some-other-host.net", 443); got != hostAllow {
		t.Errorf("allow-all: arbitrary host = %q, want hostAllow", got)
	}

	// denied_domains STILL wins under allow-all (exact and wildcard).
	if got := p.evalHost("blocked.example.com", 443); got != hostDeny {
		t.Errorf("allow-all: exact denied host = %q, want hostDeny", got)
	}
	if got := p.evalHost("a.deny.example.com", 443); got != hostDeny {
		t.Errorf("allow-all: wildcard denied host = %q, want hostDeny", got)
	}

	// Credential injection must NOT be widened by allow-all: a host reachable
	// ONLY via allow-all does not qualify for exact-host injection. A secret
	// must never leak to an arbitrary host.
	if p.AllowedExactHost("random.example.com") {
		t.Errorf("allow-all must NOT widen injection: random.example.com qualified for exact injection")
	}
	// The explicit exact allowlist entry STILL qualifies for injection.
	if !p.AllowedExactHost("inject.example.com") {
		t.Errorf("explicit exact entry must still qualify for injection under allow-all")
	}
	// A denied host never qualifies, even under allow-all.
	if p.AllowedExactHost("blocked.example.com") {
		t.Errorf("denied host must never qualify for injection")
	}
}

// TestAllowAllEgressDoesNotBypassIPGuard asserts the SSRF/private-IP guard is
// unaffected by allow-all: VetHost still denies private/metadata IPs regardless
// of policy mode (allow-all reaches PUBLIC hosts only).
func TestAllowAllEgressDoesNotBypassIPGuard(t *testing.T) {
	// VetHost is policy-independent, but assert it here to lock the invariant
	// that allow-all is "public hosts only".
	for _, host := range []string{"169.254.169.254", "127.0.0.1", "10.1.2.3", "::1"} {
		if got := VetHost(host, nil); !got.Denied {
			t.Errorf("VetHost(%q) must stay denied under allow-all (public hosts only)", host)
		}
	}
	// A host that RESOLVES to the metadata address is denied even though
	// evalHost would allow it under allow-all.
	p := CompilePolicy(types.RunPolicySpec{AllowAllEgress: true})
	if got := p.evalHost("metadata.example.com", 443); got != hostAllow {
		t.Fatalf("precondition: allow-all should allow the name; got %q", got)
	}
	res := fakeResolver{m: map[string][]net.IP{"metadata.example.com": ips("169.254.169.254")}}
	if got := VetHost("metadata.example.com", res); !got.Denied {
		t.Errorf("allow-all host resolving to metadata IP must be denied by VetHost")
	}
}

// fakeResolver returns canned addresses for VetHost tests.
type fakeResolver struct {
	m   map[string][]net.IP
	err error
}

func (f fakeResolver) LookupIP(host string) ([]net.IP, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.m[host], nil
}

func ips(ss ...string) []net.IP {
	out := make([]net.IP, 0, len(ss))
	for _, s := range ss {
		out = append(out, net.ParseIP(s))
	}
	return out
}

func TestVetHostBlocksPrivateAndMetadata(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		resolved   []net.IP
		wantDenied bool
	}{
		{"public literal", "93.184.216.34", nil, false},
		{"loopback literal", "127.0.0.1", nil, true},
		{"rfc1918 10", "10.1.2.3", nil, true},
		{"rfc1918 172", "172.16.5.5", nil, true},
		{"rfc1918 192", "192.168.1.1", nil, true},
		{"metadata", "169.254.169.254", nil, true},
		{"link-local", "169.254.1.1", nil, true},
		{"cgnat", "100.64.0.1", nil, true},
		{"ipv6 loopback", "::1", nil, true},
		{"ipv6 ula", "fc00::1", nil, true},
		{"ipv6 link-local", "fe80::1", nil, true},
		{"ipv4-mapped loopback", "::ffff:127.0.0.1", nil, true},
		{"nat64 metadata", "64:ff9b::a9fe:a9fe", nil, true}, // E2: 169.254.169.254 embedded
		{"nat64 rfc1918", "64:ff9b::0a00:0005", nil, true},  // E2: 10.0.0.5 embedded
		{"nat64 localuse metadata", "64:ff9b:1::a9fe:a9fe", nil, true},
		{"resolved public", "good.example.com", ips("93.184.216.34"), false},
		{"resolved private", "rebind.example.com", ips("10.0.0.5"), true},
		{"resolved mixed pub+priv", "mixed.example.com", ips("93.184.216.34", "127.0.0.1"), true},
		{"resolved metadata", "meta.example.com", ips("169.254.169.254"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := fakeResolver{m: map[string][]net.IP{c.host: c.resolved}}
			got := VetHost(c.host, res)
			if got.Denied != c.wantDenied {
				t.Fatalf("VetHost(%q) Denied=%v (reason=%q), want %v", c.host, got.Denied, got.Reason, c.wantDenied)
			}
			if !got.Denied && got.IP == nil {
				t.Fatalf("allowed host must return a dial IP")
			}
		})
	}
}

// TestNAT64EmbeddedNotOverblocking guards the E2 fix against over-blocking: the
// embedded-v4 check is scoped to NAT64 prefixes, so a legit public IPv6 whose
// low 32 bits happen to look like a reserved v4 (2606:4700:4700::1111 ends in
// 0.0.17.17, inside 0.0.0.0/8) must NOT be denied.
func TestNAT64EmbeddedNotOverblocking(t *testing.T) {
	if blocked, why := isBlockedIP(net.ParseIP("2606:4700:4700::1111")); blocked {
		t.Fatalf("public IPv6 outside NAT64 prefixes must not be blocked (reason=%q)", why)
	}
	// And a NAT64 address embedding a PUBLIC v4 is still blocked wholesale (the
	// prefix is a by-IP bypass of hostname allowlisting): fail closed.
	if blocked, _ := isBlockedIP(net.ParseIP("64:ff9b::5db8:d822")); !blocked { // 93.184.216.34
		t.Fatalf("NAT64 prefix must be blocked wholesale (fail closed)")
	}
}

func TestVetHostResolveFailureFailsClosed(t *testing.T) {
	res := fakeResolver{err: net.UnknownNetworkError("boom")}
	got := VetHost("whatever.example.com", res)
	if !got.Denied {
		t.Fatalf("resolve failure must fail closed (deny)")
	}
}

func TestVetHostEmptyAddressesFailsClosed(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"empty.example.com": {}}}
	got := VetHost("empty.example.com", res)
	if !got.Denied {
		t.Fatalf("no addresses must fail closed (deny)")
	}
}
