// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An allowed_domains or denied_domains entry that writes an address in a non-standard spelling is refused
// where the policy is written: step 0 refuses every request in that spelling and the standard spelling is
// another key, so a deny written that way binds nothing. Standard spellings and host names pass.
func TestDomainEntryInANonStandardIPSpellingIsRefused(t *testing.T) {
	for _, d := range []string{"134744072", "0x8080808", "010.0.0.1", "8.8.8.008", "0x7f.1", "1.2.3", "0x", "010.0.0.1:443", "1.2.3.4.5"} {
		if err := ValidDomainEntry(d); err == nil || !strings.Contains(err.Error(), ipSpellingWhy) {
			t.Errorf("ValidDomainEntry(%q) = %v, want the spelling refusal", d, err)
		}
	}
	for _, d := range []string{"8.8.8.8", "8.8.8.8:443", "::ffff:1.2.3.4", "[fd00::1]:443", "2001:4860:4860::8888", "0x8.example.com", "123.example.com", "1e3", "*.example.com"} {
		if err := ValidDomainEntry(d); err != nil {
			t.Errorf("ValidDomainEntry(%q) = %v, want nil", d, err)
		}
	}
	// What the refusal is for: a stored deny written 134744072 does not deny 8.8.8.8.
	p, _ := newRuleTestProxy(t, types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: []string{"134744072"}}, "127.0.0.1:1", false, "", nil)
	if d, _, log := p.evaluate(context.Background(), "8.8.8.8", 443, http.MethodConnect, ""); d != egress.Allow || log == nil || log.RuleSource != "policy:allowed" {
		t.Errorf("8.8.8.8 under a deny written 134744072 = %s %v, want allow policy:allowed (the entry is void)", d, log)
	}
}
