// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// allowAllCeiling is a deployment whose operator allows every public host and
// lists the entries given.
func allowAllCeiling(domains ...string) types.RunPolicySpec {
	return types.RunPolicySpec{MinConfinementClass: types.CC2, AllowAllEgress: true, AllowedDomains: domains}
}

// TestInlinePolicy_AllowAllCeilingKeepsPublicEntries: under an allow-all ceiling
// a member's own allowlist is a narrowing, and its public entries, names and
// public literal addresses alike, are kept as authored with nothing said.
func TestInlinePolicy_AllowAllCeilingKeepsPublicEntries(t *testing.T) {
	h := capPolicyServer(t, nil, nil)
	h.srv.cfg.DefaultPolicy = allowAllCeiling()
	authored := []string{"pypi.org", "*.npmjs.org", "93.184.216.34", "93.184.216.34:8443", "[2606:2800:220:1::1]:443"}
	got, warns := resolveInline(t, h, types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: slices.Clone(authored)})
	if !slices.Equal(got.AllowedDomains, authored) {
		t.Errorf("allowed_domains = %q, want the authored %q", got.AllowedDomains, authored)
	}
	if d := dropWarns(warns); len(d) != 0 {
		t.Errorf("drop warnings = %q, want none", d)
	}
	if got.AllowAllEgress {
		t.Error("the run is allow-all, and its author never asked for that")
	}
}
