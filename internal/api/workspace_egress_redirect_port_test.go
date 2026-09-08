// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSubstituteArtifactEgress_LiteralIPToIsPortScoped is the F106 regression.
// substituteArtifactEgress added the redirect's To as a BARE host, and a bare
// allowlist entry matches on every port: Policy.AllowsLiteralIP answers true
// from allowedExact before it ever consults the port-qualified map, so
// egressTarget trusted (and dialled) the mirror's address on 22, 5432 and every
// other port the operator never named — inside the private space the
// unconditional private-IP guard exists to protect. The MITM/token half of the
// same redirect has been port-exact since W13-S1-5 (planArtifactRedirect's
// mitmHosts are net.JoinHostPort(host, redirectPort(r.To))), so the credential
// was scoped to one port while the SSRF trust was not.
func TestSubstituteArtifactEgress_LiteralIPToIsPortScoped(t *testing.T) {
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{{
		From: "https://pypi.org/simple/", To: "https://10.40.2.11:8443/pypi/", Ecosystem: "pip",
	}}}
	got := substituteArtifactEgress([]string{"pypi.org"}, sc)

	if !slices.Contains(got, "10.40.2.11:8443") {
		t.Fatalf("allowlist = %v, want the port-qualified redirect target 10.40.2.11:8443", got)
	}
	if slices.Contains(got, "10.40.2.11") {
		t.Fatalf("allowlist = %v — a BARE literal IP entry trusts the address on EVERY port", got)
	}

	// What the proxy then decides, through the real comparator.
	pol := proxy.CompilePolicy(types.RunPolicySpec{AllowedDomains: got})
	if !pol.AllowsLiteralIP("10.40.2.11", 8443) {
		t.Error("the port the redirect named must stay trusted")
	}
	for _, port := range []int{22, 443, 5432} {
		if pol.AllowsLiteralIP("10.40.2.11", port) {
			t.Errorf("port %d is trusted on the redirect's address, and the redirect named only 8443", port)
		}
	}
}

// TestSubstituteArtifactEgress_PortlessToTakesTheMITMPort: a To that names no
// port is trusted on exactly the port the redirect's MITM half already assumes
// for it — redirectPort's scheme-aware default, 443 here because this To is
// https:// (80 for an explicit http://) — one function, both halves.
func TestSubstituteArtifactEgress_PortlessToTakesTheMITMPort(t *testing.T) {
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{{
		From: "https://pypi.org/simple/", To: "https://10.40.2.11/pypi/", Ecosystem: "pip",
	}}}
	got := substituteArtifactEgress([]string{"pypi.org"}, sc)
	if !slices.Contains(got, "10.40.2.11:443") {
		t.Fatalf("allowlist = %v, want 10.40.2.11:443", got)
	}
	pol := proxy.CompilePolicy(types.RunPolicySpec{AllowedDomains: got})
	if pol.AllowsLiteralIP("10.40.2.11", 22) {
		t.Error("port 22 is trusted on a redirect that named no port at all")
	}
}
