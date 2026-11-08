// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AllowlistWithin holds only when the new list is a subset of the old one on the whole entry: host, port and
// literal. An exact entry or literal sits only under the same exact entry, as Leq reads it (an exact entry
// enrols credential injection a wildcard never does, and the meet gives a run's exact entry nothing under a
// wildcard); a wildcard sits under a wider wildcard; a port sits under the same port, or under a bare entry
// only on 80 or 443, the ports a bare entry reaches under the port rule. A reorder or a duplicate changes
// nothing.
func TestAllowlistWithin(t *testing.T) {
	for _, tc := range []struct {
		name      string
		next, old []string
		want      bool
	}{
		{"the same list", []string{"b.example", "c.example"}, []string{"b.example", "c.example"}, true},
		{"reordered with a duplicate", []string{"c.example", "b.example", "b.example"}, []string{"b.example", "c.example"}, true},
		{"a host dropped", []string{"b.example"}, []string{"b.example", "c.example"}, true},
		{"a port entry dropped", []string{"b.example"}, []string{"b.example", "c.example:8443"}, true},
		{"a narrower wildcard", []string{"*.a.example"}, []string{"*.example"}, true},
		{"the same literal", []string{"203.0.113.7"}, []string{"203.0.113.7", "b.example"}, true},
		{"a bare name narrowed to 443", []string{"b.example:443"}, []string{"b.example"}, true},
		{"a wildcard for a name", []string{"*.example"}, []string{"b.example"}, false},
		{"a name for the wildcard over it", []string{"b.example"}, []string{"*.example"}, false},
		{"a name swapped", []string{"evil.example"}, []string{"b.example", "c.example"}, false},
		{"a port swapped", []string{"b.example:9443"}, []string{"b.example:8443"}, false},
		{"a name to a literal", []string{"203.0.113.7"}, []string{"b.example"}, false},
		{"a name to a port", []string{"evil.example:8443"}, []string{"b.example"}, false},
		{"the same name on another port", []string{"b.example:8443"}, []string{"b.example"}, false},
		{"443 widened to a bare name", []string{"b.example"}, []string{"b.example:443"}, false},
		{"a port entry added", []string{"b.example", "c.example", "evil.example:8443"}, []string{"b.example", "c.example"}, false},
		{"a literal added", []string{"b.example", "c.example", "203.0.113.7"}, []string{"b.example", "c.example"}, false},
		{"an out-of-range port stays part of the host", []string{"b.example:99999"}, []string{"b.example"}, false},
	} {
		if got := composer.AllowlistWithin(tc.next, tc.old); got != tc.want {
			t.Errorf("%s: AllowlistWithin(%v, %v) = %v, want %v", tc.name, tc.next, tc.old, got, tc.want)
		}
	}
}

// Leq reads an allowlist's ports as the port rule does: a bare entry reaches 80 and 443 only, so an entry
// naming another port is never within it, and one naming 80 or 443 is.
func TestLeqReadsAllowlistPortsUnderThePortRule(t *testing.T) {
	for _, tc := range []struct {
		a, b []string
		want bool
	}{
		{[]string{"b.example:8443"}, []string{"b.example"}, false},
		{[]string{"*.example:8443"}, []string{"*.example"}, false},
		{[]string{"b.example:443"}, []string{"b.example"}, true},
		{[]string{"b.example:80"}, []string{"b.example"}, true},
		{[]string{"b.example:8443"}, []string{"b.example:8443"}, true},
	} {
		if got := composer.Leq(types.RunPolicySpec{AllowedDomains: tc.a}, types.RunPolicySpec{AllowedDomains: tc.b},
			types.GovernanceLimits{}, types.GovernanceLimits{}); got != tc.want {
			t.Errorf("Leq(%v <= %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
