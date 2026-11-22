// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"slices"
	"testing"
)

// The laptop copy of a via_org Azure DevOps plan keeps the interception entry production wrote for the host, in its
// own spelling: dispatch writes host:443, which scopes the entry to the TLS port. A bare host beside it would mean
// any port, so a CONNECT to the host on another port would be intercepted and forwarded to the organisation.
func TestLocalCopy_ADOViaOrgKeepsThePortScopedInterceptionEntry(t *testing.T) {
	p, _ := viaOrgADOPlan(adoViaOrgOrigin, viaOrgADOPolicy)
	p.Spec.ProxyConfig.MITMHosts = []string{adoHost + ":443"}
	out, r := LocalEligibility(p)
	if r != nil {
		t.Fatalf("refused: %+v", r)
	}
	if want := []string{adoHost + ":443"}; !slices.Equal(out.ProxyConfig.MITMHosts, want) {
		t.Fatalf("MITMHosts = %v, want %v: no bare host, which would be any port", out.ProxyConfig.MITMHosts, want)
	}
	// A plan that carries no entry for the host gets the port-scoped spelling, never the bare one.
	p, _ = viaOrgADOPlan(adoViaOrgOrigin, viaOrgADOPolicy)
	p.Spec.ProxyConfig.MITMHosts = nil
	out, r = LocalEligibility(p)
	if r != nil {
		t.Fatalf("refused: %+v", r)
	}
	if want := []string{adoHost + ":443"}; !slices.Equal(out.ProxyConfig.MITMHosts, want) {
		t.Fatalf("MITMHosts = %v, want %v", out.ProxyConfig.MITMHosts, want)
	}
}
