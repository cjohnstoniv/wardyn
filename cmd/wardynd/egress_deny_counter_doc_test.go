// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEgressDenyCounterDocNamesTheGatewayVetSite pins the truth of the comment
// that JUSTIFIES an exclusion from wardyn_egress_denies_total.
//
// isPolicyDeny (internal/api/metrics.go) drops builtin:dial-failed from the only
// egress deny counter Wardyn exposes, and the comment above it is the whole
// argument for why that is safe. Three of the four emitting sites are genuine
// dial failures on an ALLOWED request. The fourth is not: internal/egress/proxy/
// llm_routes.go reuses the same rule_source when gatewayTarget returns
// errGatewayVet — vetTrustedHost's GUARD refusal of the configured model gateway
// — so the exclusion also drops a guard refusal out of the counter. A comment
// that claims all four are dial failures turns an accepted residual into an
// invisible one.
//
// Both halves are asserted, in both directions:
//   - if the egress lane ever gives errGatewayVet its own rule_source (the fix
//     this residual is filed for), the first half fails and the comment must be
//     re-derived rather than left claiming a residual that no longer exists;
//   - if the comment drifts back to "all four are dial failures", the second
//     half fails.
func TestEgressDenyCounterDocNamesTheGatewayVetSite(t *testing.T) {
	root := repoRoot(t)

	routes := readGoSource(t, filepath.Join(root, "internal", "egress", "proxy", "llm_routes.go"))
	if !strings.Contains(routes, `if errors.Is(err, errGatewayVet) { source = "builtin:dial-failed" }`) {
		t.Fatalf("llm_routes.go no longer maps errGatewayVet onto builtin:dial-failed — " +
			"re-derive the isPolicyDeny comment in internal/api/metrics.go (and internal/api/internal.go) " +
			"against what the tree now does; the accepted residual F065-gatewayvet may be gone")
	}

	metrics := readGoSource(t, filepath.Join(root, "internal", "api", "metrics.go"))
	for _, must := range []string{
		"errGatewayVet",
		"ACCEPTED RESIDUAL (F065-gatewayvet)",
	} {
		if !strings.Contains(metrics, must) {
			t.Errorf("internal/api/metrics.go's non-policy-deny comment no longer says %q: "+
				"the builtin:dial-failed exclusion also drops llm_routes.go's gateway-vet GUARD refusal, "+
				"and the comment is the only place an operator learns that", must)
		}
	}
}

// readGoSource returns a Go source file with all runs of whitespace collapsed to
// single spaces, so a claim (or a statement) that a human reads as one line is
// one string to Contains regardless of how gofmt wrapped it.
func readGoSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Join(strings.Fields(string(b)), " ")
}
