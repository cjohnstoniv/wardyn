// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestEgressDenyCounterDocClosesTheGatewayVetResidual pins the structure behind
// the comment that justifies isPolicyDeny's (internal/api/metrics.go) exclusion
// from wardyn_egress_denies_total: the gateway-vet refusal is counted, not an
// accepted residual.
//
// When gatewayTarget returns errGatewayVet — vetTrustedHost's guard refusal
// of the configured model gateway — internal/egress/proxy/llm_routes.go
// labels it with its own rule_source (ruleSourceGatewayVetFailed,
// egress_target.go), not builtin:dial-failed. That label is deliberately not
// in isPolicyDeny's exclusion list, so it counts as a denial like any other
// guard refusal; reusing builtin:dial-failed would drop it out of the counter
// alongside the genuine dial failures the exclusion exists for.
//
// If llm_routes.go ever maps errGatewayVet onto builtin:dial-failed, the test
// fails and the metrics.go comment must be re-derived against what the tree
// does at that point. The counting behaviour itself is pinned by
// TestEgressDeniesTotalCountsPolicyDeniesOnly (internal/api).
func TestEgressDenyCounterDocClosesTheGatewayVetResidual(t *testing.T) {
	root := repoRoot(t)

	routes := readGoSource(t, filepath.Join(root, "internal", "egress", "proxy", "llm_routes.go"))
	if strings.Contains(routes, `if errors.Is(err, errGatewayVet) { source = "builtin:dial-failed" }`) {
		t.Fatal("llm_routes.go still maps errGatewayVet onto builtin:dial-failed — " +
			"the gateway-vet residual is not actually closed; re-derive this guard against " +
			"whichever state is real before trusting it")
	}
	if !strings.Contains(routes, `source = ruleSourceGatewayVetFailed`) {
		t.Fatal("llm_routes.go no longer maps errGatewayVet onto ruleSourceGatewayVetFailed — " +
			"re-derive this guard and the isPolicyDeny comment in internal/api/metrics.go " +
			"(and internal/api/internal.go) against what the tree now does")
	}

	egressTarget := readGoSource(t, filepath.Join(root, "internal", "egress", "proxy", "egress_target.go"))
	if !strings.Contains(egressTarget, `ruleSourceGatewayVetFailed = "builtin:gateway-vet-failed"`) {
		t.Fatal("egress_target.go no longer defines ruleSourceGatewayVetFailed as expected — re-derive this guard")
	}
}

// readGoSource returns a Go source file with all runs of whitespace collapsed to
// single spaces, so a claim (or a statement) that a human reads as one line is
// one string to Contains regardless of how gofmt wrapped it.
func readGoSource(t *testing.T, path string) string {
	t.Helper()
	return strings.Join(strings.Fields(readRepo(t, path)), " ")
}
