// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// fakeResolver returns a fixed set of addresses (or an error) for every host, so
// the guard can be exercised without touching real DNS or the network.
type fakeResolver struct {
	ips []net.IP
	err error
}

func (f fakeResolver) LookupIP(_ context.Context, _ string) ([]net.IP, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ips, nil
}

func TestVet_RejectsHostResolvingToBlockedAddress(t *testing.T) {
	tests := []struct {
		name string
		ip   string
	}{
		{"loopback 127.0.0.1", "127.0.0.1"},
		{"cloud metadata 169.254.169.254", "169.254.169.254"},
		{"rfc1918 10.x", "10.1.2.3"},
		{"rfc1918 192.168.x", "192.168.0.5"},
		{"ipv6 loopback ::1", "::1"},
		{"ipv4-mapped loopback", "::ffff:127.0.0.1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test IP %q", tc.ip)
			}
			// The host IS on the allowlist; it must still be refused because it
			// resolves to a blocked address (DNS-rebinding / SSRF guard).
			g := NewGuard([]string{"api.example.com"}, false, fakeResolver{ips: []net.IP{ip}})
			if _, err := g.Vet(context.Background(), "api.example.com"); err == nil {
				t.Fatalf("Vet() error = nil, want refusal for host resolving to %s", tc.ip)
			}
		})
	}
}

func TestVet_RejectsHostNotOnAllowlist(t *testing.T) {
	// Resolves to a perfectly public address, but the host is off-list.
	g := NewGuard([]string{"api.openai.com"}, false, fakeResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}})
	_, err := g.Vet(context.Background(), "evil.example.com")
	if err == nil {
		t.Fatal("Vet() error = nil, want refusal for off-allowlist host")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error = %q, want it to mention the allowlist", err)
	}
}

func TestVet_AllowsOnListPublicHost(t *testing.T) {
	want := net.ParseIP("93.184.216.34")
	g := NewGuard([]string{"api.anthropic.com"}, false, fakeResolver{ips: []net.IP{want}})
	got, err := g.Vet(context.Background(), "api.anthropic.com")
	if err != nil {
		t.Fatalf("Vet() unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("Vet() = %s, want %s", got, want)
	}
}

func TestVet_FailsClosedOnResolveError(t *testing.T) {
	g := NewGuard([]string{"api.anthropic.com"}, false, fakeResolver{err: errors.New("nxdomain")})
	if _, err := g.Vet(context.Background(), "api.anthropic.com"); err == nil {
		t.Fatal("Vet() error = nil, want fail-closed on resolver error")
	}
}

func TestVet_RejectsMixedPublicAndPrivateAnswers(t *testing.T) {
	// One public + one private answer is the rebinding shape; refuse wholesale.
	g := NewGuard([]string{"api.anthropic.com"}, false, fakeResolver{ips: []net.IP{
		net.ParseIP("93.184.216.34"),
		net.ParseIP("10.0.0.7"),
	}})
	if _, err := g.Vet(context.Background(), "api.anthropic.com"); err == nil {
		t.Fatal("Vet() error = nil, want refusal when any resolved address is blocked")
	}
}

func TestVet_AllowPrivatePermitsLoopbackButNotMetadata(t *testing.T) {
	// The openai "compatible" (local BYOM) case: an operator-configured loopback
	// endpoint is permitted under allowPrivate...
	g := NewGuard([]string{"127.0.0.1"}, true, nil) // literal IP host, no DNS needed
	if _, err := g.Vet(context.Background(), "127.0.0.1"); err != nil {
		t.Errorf("Vet(127.0.0.1, allowPrivate) error = %v, want nil", err)
	}
	// ...but the cloud-metadata address stays blocked even under allowPrivate.
	gm := NewGuard([]string{"169.254.169.254"}, true, nil)
	if _, err := gm.Vet(context.Background(), "169.254.169.254"); err == nil {
		t.Error("Vet(169.254.169.254, allowPrivate) error = nil, want metadata still blocked")
	}
}

func TestIsBlocked(t *testing.T) {
	tests := []struct {
		ip           string
		allowPrivate bool
		want         bool
	}{
		{"8.8.8.8", false, false},
		{"93.184.216.34", false, false},
		{"127.0.0.1", false, true},
		{"10.0.0.1", false, true},
		{"172.16.5.4", false, true},
		{"192.168.1.1", false, true},
		{"169.254.169.254", false, true},
		{"100.64.0.1", false, true}, // CGNAT
		{"::1", false, true},
		{"fc00::1", false, true}, // ULA
		{"fe80::1", false, true}, // link-local v6
		{"::ffff:10.0.0.1", false, true},
		// allowPrivate relaxes loopback/RFC1918 but NOT metadata/link-local.
		{"127.0.0.1", true, false},
		{"10.0.0.1", true, false},
		{"192.168.1.1", true, false},
		{"169.254.169.254", true, true},
		{"fe80::1", true, true},
		{"8.8.8.8", true, false},
		// NAT64-smuggled targets (U050): NAT64 literals are blocked WHOLESALE in
		// both modes (fail closed, matching the egress proxy) — a NAT64 literal
		// never legitimately appears on a model-provider egress path, and the
		// embedded-v4 recheck only enriches the denial reason.
		{"64:ff9b::a9fe:a9fe", false, true},  // -> 169.254.169.254 metadata
		{"64:ff9b::a9fe:a9fe", true, true},   // still blocked under allowPrivate
		{"64:ff9b::0a00:0001", false, true},  // -> 10.0.0.1
		{"64:ff9b::0808:0808", false, true},  // -> 8.8.8.8, still blocked (wholesale)
		{"64:ff9b:1::a9fe:a9fe", false, true}, // local-use NAT64 prefix (RFC 8215)
	}
	for _, tc := range tests {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", tc.ip)
		}
		if got, why := IsBlocked(ip, tc.allowPrivate); got != tc.want {
			t.Errorf("IsBlocked(%s, allowPrivate=%v) = %v (%s), want %v", tc.ip, tc.allowPrivate, got, why, tc.want)
		}
	}
}

func TestHostOf(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://api.openai.com/v1/", "api.openai.com"},
		{"https://api.anthropic.com", "api.anthropic.com"},
		{"https://res.openai.azure.com/openai/v1", "res.openai.azure.com"},
		{"http://127.0.0.1:11434", "127.0.0.1"},
		{"https://API.OpenAI.com.", "api.openai.com"},
		{"", ""},
		{"::not a url::", ""},
	}
	for _, tc := range tests {
		if got := HostOf(tc.in); got != tc.want {
			t.Errorf("HostOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	g := NewGuard([]string{"api.openai.com", "API.Anthropic.com."}, false, nil)
	for _, h := range []string{"api.openai.com", "API.OPENAI.COM", "api.anthropic.com"} {
		if !g.HostAllowed(h) {
			t.Errorf("HostAllowed(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"evil.com", "api.openai.com.evil.com", ""} {
		if g.HostAllowed(h) {
			t.Errorf("HostAllowed(%q) = true, want false", h)
		}
	}
}

// TestNewClient_BlocksLoopbackEndToEnd exercises the assembled client: a real Do
// against a loopback host (on the allowlist) must be refused by the DialContext
// guard before any connection is made.
func TestNewClient_BlocksLoopbackEndToEnd(t *testing.T) {
	c := NewClient([]string{"127.0.0.1"}, false, 0)
	_, err := c.Get("http://127.0.0.1:0/")
	if err == nil {
		t.Fatal("Get(loopback) error = nil, want the dial guard to refuse it")
	}
	if !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error = %v, want it to come from the dial guard", err)
	}
}
