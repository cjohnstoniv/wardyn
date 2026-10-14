// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestExceedsLiteralTrust(t *testing.T) {
	for _, tc := range []struct {
		entry     string
		authority []string
		want      bool
	}{
		// The one range an exact entry opens, in every spelling an entry compiles from.
		{"10.0.0.5", nil, true},
		{"10.0.0.5:443", nil, true},
		{" 10.0.0.5. ", nil, true},
		{"172.16.5.5", nil, true},
		{"192.168.1.1:22", nil, true},
		{"100.64.0.9", nil, true},
		{"::ffff:10.0.0.5", nil, true},
		{"[::ffff:10.0.0.5]:443", nil, true},
		{"fd12:3456::1", nil, true},
		{"[FD12:3456:0:0::1]:443", nil, true},
		// Covered only by an exact entry for the same address on every port named.
		{"10.0.0.5", []string{"10.0.0.5"}, false},
		{"10.0.0.5:443", []string{"10.0.0.5"}, false},
		{"10.0.0.5:443", []string{"10.0.0.5:443"}, false},
		{"[::ffff:10.0.0.5]:443", []string{"10.0.0.5:443"}, false},
		{"10.0.0.5:443", []string{"::ffff:10.0.0.5"}, false},
		{"[fd12:3456::1]:22", []string{"FD12:3456:0::1"}, false},
		{"10.0.0.5:8443", []string{"10.0.0.5:443"}, true},
		{"10.0.0.5", []string{"10.0.0.5:443"}, true},
		{"10.0.0.5", []string{"10.0.0.6"}, true},
		{"10.0.0.5", []string{"*.0.0.5", "*.corp.example", "db.corp.internal"}, true},
		// Nothing an entry can open, and nothing the guard refuses, is this rule's to judge.
		{"127.0.0.1", nil, false},
		{"169.254.169.254", nil, false},
		{"64:ff9b::a00:5", nil, false},
		{"::10.0.0.5", nil, false},
		{"93.184.216.34", nil, false},
		{"93.184.216.34:443", nil, false},
		{"db.corp.internal", nil, false},
		{"*.0.0.5", nil, false},
		{"", nil, false},
	} {
		if got := CompilePolicy(types.RunPolicySpec{AllowedDomains: tc.authority}).ExceedsLiteralTrust(tc.entry); got != tc.want {
			t.Errorf("ExceedsLiteralTrust(%q) under %q = %v, want %v", tc.entry, tc.authority, got, tc.want)
		}
	}
}

// memberClamp is the member pipeline's egress half: the entries that exceed the
// ceiling's literal trust go, then the clamp.
func memberClamp(authored []string, ceiling types.RunPolicySpec) types.RunPolicySpec {
	authority := CompilePolicy(types.RunPolicySpec{AllowedDomains: ceiling.AllowedDomains})
	var kept []string
	for _, d := range authored {
		if !authority.ExceedsLiteralTrust(d) {
			kept = append(kept, d)
		}
	}
	got, _ := composer.Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: kept}, ceiling, types.GovernanceLimits{})
	return got
}

// TestMemberLiteralEntryThroughTheClampAndTheGuard runs a member's allowlist
// through the clamp and the proxy's whole decision pipeline: under an allow-all
// ceiling the member's own entry opens a private address only where the ceiling
// lists that address and port, while a public literal they list stays reachable.
func TestMemberLiteralEntryThroughTheClampAndTheGuard(t *testing.T) {
	reach := func(spec types.RunPolicySpec, host string, port int) bool {
		d, _, _ := oracleProxy(t, spec).evaluate(context.Background(), host, port, "CONNECT", "")
		return d == egress.Allow
	}
	allowAll := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowAllEgress: true}

	// The clamp alone keeps the entry and the guard then trusts it: without the
	// bound the address is reachable, so the denials below are the bound's.
	unbounded, _ := composer.Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"10.0.0.5"}}, allowAll, types.GovernanceLimits{})
	if !reach(unbounded, "10.0.0.5", 443) {
		t.Fatal("an exact private entry kept by the clamp is not trusted by the guard; this test's premise is wrong")
	}

	for _, authored := range [][]string{{"10.0.0.5"}, {"10.0.0.5:443"}, {"::ffff:10.0.0.5"}, {"[::ffff:10.0.0.5]:443"}} {
		got := memberClamp(authored, allowAll)
		for _, host := range []string{"10.0.0.5", "::ffff:10.0.0.5"} {
			if reach(got, host, 443) {
				t.Errorf("a member's %q under an allow-all ceiling reaches %s:443", authored, host)
			}
		}
	}

	listed := allowAll
	listed.AllowedDomains = []string{"10.0.0.5:443"}
	if got := memberClamp([]string{"10.0.0.5:443"}, listed); !reach(got, "10.0.0.5", 443) || reach(got, "10.0.0.5", 22) {
		t.Error("an entry the ceiling lists for one port must reach that port and no other")
	}
	for _, authored := range [][]string{{"10.0.0.5"}, {"10.0.0.5:22"}} {
		if got := memberClamp(authored, listed); reach(got, "10.0.0.5", 22) {
			t.Errorf("a member's %q reaches port 22, which the ceiling's 10.0.0.5:443 does not list", authored)
		}
	}

	public := memberClamp([]string{"93.184.216.34", "10.0.0.6"}, allowAll)
	if !reach(public, "93.184.216.34", 443) || reach(public, "10.0.0.6", 443) {
		t.Errorf("a member's public literal must stay reachable and the private one must not: %q", public.AllowedDomains)
	}
	if reach(public, "evil.example", 443) {
		t.Error("a member's own allowlist stopped narrowing the run")
	}
}
