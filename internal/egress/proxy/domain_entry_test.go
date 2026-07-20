// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strings"
	"testing"
)

// TestValidDomainEntry pins the shape rule every operator-supplied policy
// ingest point enforces: the two matchable forms (exact host, leading "*."
// wildcard), each optionally ":port"-qualified — and nothing else. The
// motivating bug is "oidc.*.amazonaws.com", which compiles to an exact
// hostname no request can equal.
func TestValidDomainEntry(t *testing.T) {
	good := []string{
		"api.anthropic.com", "*.amazonaws.com", "example.com:443",
		"*.example.com:443", "EXAMPLE.com.", "::1", "127.0.0.1",
	}
	for _, d := range good {
		if err := ValidDomainEntry(d); err != nil {
			t.Errorf("ValidDomainEntry(%q) = %v, want nil", d, err)
		}
	}
	bad := []string{
		"oidc.*.amazonaws.com", "*.*.example.com", "*", "", "  ",
		"https://example.com", "example.com/path", "example.com:0",
		"example.com:abc", "example.com:99999", "a b.com",
		// The wildcard branch was unguarded while the exact branch above was
		// checked, so these compiled to suffixes like ".example.com:0" that no
		// request host can end with (evalHost gets host and port separately) —
		// the same "policy that lies" the exact cases reject.
		"*.example.com:0", "*.example.com:abc", "*.example.com:99999",
		"*.example.com/path", "*.exa mple.com",
	}
	for _, d := range bad {
		if err := ValidDomainEntry(d); err == nil {
			t.Errorf("ValidDomainEntry(%q) = nil, want an error", d)
		}
	}
	// The error names the offending entry (actionable for the operator).
	if err := ValidDomainEntry("oidc.*.amazonaws.com"); err == nil ||
		!strings.Contains(err.Error(), "oidc.*.amazonaws.com") {
		t.Errorf("error must name the entry, got %v", err)
	}
}
