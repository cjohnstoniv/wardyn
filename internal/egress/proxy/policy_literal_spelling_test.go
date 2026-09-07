// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestLiteralIPDenyCoversEverySpellingOfTheSameAddress (F130) pins the deny
// side of the policy matcher against equivalent spellings of one address.
//
// evalHost keyed every lookup on the raw request STRING while AllowsLiteralIP —
// the trusted-literal path egress_target.go takes — keyed the same maps on
// net.IP.String(). One policy, two normalizations: a run that denied the
// literal 93.184.216.34 still allowed "::ffff:93.184.216.34", and vetHostLift's
// literal fast path parsed that straight back to 93.184.216.34 and dialled it.
// Under allow_all_egress (deny-list-only mode) the deny entry was the only
// barrier, so the bypass was total.
//
// The entry side is the same defect from the operator's end: a denied_domains
// entry written in a non-canonical spelling compiled to a key no request could
// ever produce — a dead rule that silently protects nothing, which is precisely
// what ValidDomainEntry exists to refuse at write time.
func TestLiteralIPDenyCoversEverySpellingOfTheSameAddress(t *testing.T) {
	// allow_all_egress is the mode that makes this reachable: every host that
	// survives the deny checks is allowed, so a missed deny key IS the dial.
	for _, tc := range []struct {
		name  string
		entry string
		host  string
	}{
		{"v4-mapped spelling of a denied v4 literal", "93.184.216.34", "::ffff:93.184.216.34"},
		{"hex-group v4-mapped spelling", "93.184.216.34", "::ffff:5db8:d822"},
		{"fully expanded v4-mapped spelling", "93.184.216.34", "0:0:0:0:0:ffff:5db8:d822"},
		{"canonical spelling (control — this always denied)", "93.184.216.34", "93.184.216.34"},
		{"non-canonical ENTRY, canonical request", "::ffff:93.184.216.34", "93.184.216.34"},
		{"zero-padded IPv6 entry, compressed request", "2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::1"},
		{"compressed IPv6 entry, expanded request", "2001:db8::1", "2001:0db8:0000:0000:0000:0000:0000:0001"},
		{"uppercase IPv6 request", "2001:db8::1", "2001:DB8::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := CompilePolicy(types.RunPolicySpec{
				DeniedDomains: []string{tc.entry}, AllowAllEgress: true,
			})
			if got := p.evalHost(tc.host, 443); got != hostDeny {
				t.Errorf("evalHost(%q) with denied_domains %q = %v, want hostDeny — "+
					"the same address under a second spelling must not outrun the deny "+
					"(a deny always beats an allow, and allow_all_egress leaves nothing else)",
					tc.host, tc.entry, got)
			}
		})
	}

	// A port-qualified literal deny keys on the same canonical host, and still
	// binds THAT port only — canonicalizing the host must not turn a
	// port-qualified entry into an any-port one.
	t.Run("port-qualified literal deny", func(t *testing.T) {
		p := CompilePolicy(types.RunPolicySpec{
			DeniedDomains: []string{"93.184.216.34:443"}, AllowAllEgress: true,
		})
		if got := p.evalHost("::ffff:93.184.216.34", 443); got != hostDeny {
			t.Errorf("evalHost(::ffff:93.184.216.34, 443) = %v, want hostDeny", got)
		}
		if got := p.evalHost("::ffff:93.184.216.34", 80); got != hostAllow {
			t.Errorf("evalHost(::ffff:93.184.216.34, 80) = %v, want hostAllow — "+
				"a :443 entry must keep binding that port only", got)
		}
	})

	// Credential injection takes the stricter AllowedExactHost path, and its
	// deny check must read the same canonical key: a denied literal may never
	// become an injection target under a second spelling.
	t.Run("AllowedExactHost honours the canonical deny", func(t *testing.T) {
		p := CompilePolicy(types.RunPolicySpec{
			AllowedDomains: []string{"93.184.216.34"},
			DeniedDomains:  []string{"::ffff:93.184.216.34"},
		})
		if p.AllowedExactHost("93.184.216.34") {
			t.Error("AllowedExactHost(93.184.216.34) = true with a v4-mapped deny entry for the same address, want false")
		}
		if p.AllowsLiteralIP("93.184.216.34", 443) {
			t.Error("AllowsLiteralIP(93.184.216.34) = true with a v4-mapped deny entry for the same address, want false")
		}
	})

	// Hostnames are untouched by the literal canonicalization, and a spelling
	// net.ParseIP cannot read stays verbatim — it matches no allow entry, and
	// the unconditional IP guard still binds the dial.
	t.Run("names and unparseable spellings are unchanged", func(t *testing.T) {
		p := CompilePolicy(types.RunPolicySpec{
			AllowedDomains: []string{"api.github.com", "*.example.com"},
			DeniedDomains:  []string{"evil.example.com"},
		})
		for host, want := range map[string]hostDecision{
			"api.github.com":   hostAllow,
			"a.example.com":    hostAllow,
			"evil.example.com": hostDeny,
			"example.com":      hostUnknown,
			"127.1":            hostUnknown,
			"0x7f000001":       hostUnknown,
		} {
			if got := p.evalHost(host, 443); got != want {
				t.Errorf("evalHost(%q) = %v, want %v", host, got, want)
			}
		}
	})
}
