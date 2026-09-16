// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDomainEntryDotsAndCharset (B10-F7) closes the two ways an operator-authored
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
			if exact, _, _ := classifyDomain(d); exact != "example.com" {
				t.Errorf("classifyDomain(%q) exact = %q, want example.com", d, exact)
			}
		}
		if _, wild, _ := classifyDomain("*.example.com.."); wild != ".example.com" {
			t.Errorf(`classifyDomain("*.example.com..") wild = %q, want ".example.com"`, wild)
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
