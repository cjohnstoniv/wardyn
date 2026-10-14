// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDomainEntryDotsAndCharset closes the two ways an operator-authored
// policy entry could be DEAD — accepted at write, never matched at request time —
// which is precisely what ValidDomainEntry exists to prevent.
//
//  1. DOTS. The request side normalises every FQDN-root spelling with
//     TrimRight(".") (splitHostPort), but classifyDomain trimmed at most one
//     suffix per call, so `denied_domains: ["example.com..."]` compiled to the key
//     "example.com." and never matched anything. A DENY that silently protects
//     nothing is the worst shape of this bug, so the fix is at the entry-side
//     funnel — TrimRight, the same normaliser the request side uses — and it
//     fixes allow and deny together.
//
//  2. CHARSET. A non-ASCII entry ("ëxample.com") is equally dead: the wire
//     spelling of an internationalised name is punycode, so no request host can
//     ever equal the literal the operator typed. Rejected at write time now,
//     where the operator is looking at the text they just wrote.
func TestDomainEntryDotsAndCharset(t *testing.T) {
	t.Run("a dotted entry is accepted and normalises to the matchable host", func(t *testing.T) {
		for _, d := range []string{"example.com.", "example.com...", "*.example.com..", "*.example.com..:443"} {
			if err := ValidDomainEntry(d); err != nil {
				t.Errorf("ValidDomainEntry(%q) = %v, want nil", d, err)
			}
		}
		for _, d := range []string{"example.com.", "example.com..."} {
			if exact, _, _ := domainmatch.Classify(d); exact != "example.com" {
				t.Errorf("Classify(%q) exact = %q, want example.com", d, exact)
			}
		}
		if _, wild, _ := domainmatch.Classify("*.example.com.."); wild != ".example.com" {
			t.Errorf(`Classify("*.example.com..") wild = %q, want ".example.com"`, wild)
		}
	})

	// The ALLOW mirror of the same normalisation, pinned because it is the half an
	// operator FEELS on upgrade: a dotted allow entry granted nothing and now
	// grants egress to the host it names. Same one-line root cause, opposite
	// direction, and the changelog says so.
	t.Run("an allow entry with trailing dots now grants egress", func(t *testing.T) {
		p := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"example.com..."}})
		if got := p.evalHost("example.com", 443); got != hostAllow {
			t.Errorf("evalHost(example.com) = %q, want %q", got, hostAllow)
		}
	})

	// The end-to-end consequence, and the reason this is a policy-bypass and not a
	// tidiness item: the deny entry now MATCHES.
	t.Run("a deny entry with trailing dots now matches", func(t *testing.T) {
		p := CompilePolicy(types.RunPolicySpec{
			AllowAllEgress: true,
			DeniedDomains:  []string{"example.com..."},
		})
		if got := p.evalHost("example.com", 443); got != hostDeny {
			t.Errorf("evalHost(example.com) = %q, want %q — the operator's deny entry protects nothing", got, hostDeny)
		}
	})

	t.Run("a non-ASCII entry is refused at write", func(t *testing.T) {
		for _, d := range []string{"ëxample.com", "*.ëxample.com", "ëxample.com:443"} {
			err := ValidDomainEntry(d)
			if err == nil {
				t.Errorf("ValidDomainEntry(%q) = nil; a non-ASCII entry can never match a wire host", d)
				continue
			}
			// Through the constant, never a literal: the refusal sentence is the
			// operator's only signal and is canon-pending.
			if !strings.Contains(err.Error(), charsetWhy) {
				t.Errorf("ValidDomainEntry(%q) error = %v, want it to carry charsetWhy", d, err)
			}
		}
		// The punycode spelling — the one that IS on the wire — is accepted, as are
		// the IP-literal forms the entry shape deliberately supports.
		for _, d := range []string{"xn--xample-9ua.com", "*.xn--xample-9ua.com:443", "::1", "127.0.0.1", "100.64.5.7:8443"} {
			if err := ValidDomainEntry(d); err != nil {
				t.Errorf("ValidDomainEntry(%q) = %v, want nil", d, err)
			}
		}
	})
}

// TestPortQualifiedDottedEntryMatches: the same dots on a ":port" entry. Every
// write gate accepts the entry, so it must compile to a key the request side
// derives: the deny refuses, the allow grants, each on its own port only.
func TestPortQualifiedDottedEntryMatches(t *testing.T) {
	for _, d := range []string{"*.example.com.:443", "example.com..:443"} {
		if err := ValidDomainEntry(d); err != nil {
			t.Fatalf("ValidDomainEntry(%q) = %v, want nil", d, err)
		}
	}
	deny := CompilePolicy(types.RunPolicySpec{
		AllowAllEgress: true,
		DeniedDomains:  []string{"*.example.com.:443", "example.com..:443"},
	})
	for _, h := range []string{"api.example.com", "example.com"} {
		if got := deny.evalHost(h, 443); got != hostDeny {
			t.Errorf("evalHost(%s, 443) = %q, want %q", h, got, hostDeny)
		}
		if got := deny.evalHost(h, 80); got != hostAllow {
			t.Errorf("evalHost(%s, 80) = %q, want %q: the entry names port 443 only", h, got, hostAllow)
		}
	}
	allow := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"*.example.com.:443", "example.com..:443"}})
	for _, h := range []string{"api.example.com", "example.com"} {
		if got := allow.evalHost(h, 443); got != hostAllow {
			t.Errorf("evalHost(%s, 443) = %q, want %q", h, got, hostAllow)
		}
		if got := allow.evalHost(h, 80); got != hostUnknown {
			t.Errorf("evalHost(%s, 80) = %q, want %q", h, got, hostUnknown)
		}
	}
}

// TestPortQualifiedEntryVerdictsUnchanged pins the port-qualified spellings that
// already matched: same verdict on the named port and on another one.
func TestPortQualifiedEntryVerdictsUnchanged(t *testing.T) {
	entries := []string{"*.example.com:443", "example.com:443", "example.com.:443", "*.example.com..", "example.com..."}
	deny := CompilePolicy(types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: entries})
	allow := CompilePolicy(types.RunPolicySpec{AllowedDomains: entries})
	for _, h := range []string{"api.example.com", "example.com"} {
		for _, port := range []int{443, 80} {
			if got := deny.evalHost(h, port); got != hostDeny {
				t.Errorf("deny evalHost(%s, %d) = %q, want %q", h, port, got, hostDeny)
			}
			if got := allow.evalHost(h, port); got != hostAllow {
				t.Errorf("allow evalHost(%s, %d) = %q, want %q", h, port, got, hostAllow)
			}
		}
	}
	if got := deny.evalHost("other.com", 443); got != hostAllow {
		t.Errorf("deny evalHost(other.com) = %q, want %q", got, hostAllow)
	}
}
