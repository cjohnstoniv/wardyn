// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// TestProxyEgressExceptsAzureWireServer: every address-range peer of a proxy's egress (the DNS rule and the
// general rule) carves out Azure's WireServer beside the link-local metadata address, so a compromised
// proxy reaches neither on any port, the host agent plugin port 32526 included.
func TestProxyEgressExceptsAzureWireServer(t *testing.T) {
	rules, _, err := (&Driver{}).proxyEgressPlan(context.Background(), runner.SandboxSpec{})
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for i, r := range rules {
		for _, peer := range r.To {
			if peer.IPBlock == nil {
				continue
			}
			blocks++
			for _, want := range []string{"169.254.169.254/32", "168.63.129.16/32"} {
				if !slices.Contains(peer.IPBlock.Except, want) {
					t.Errorf("proxy egress rule %d ipBlock %+v lacks except %s", i, peer.IPBlock, want)
				}
			}
		}
	}
	if blocks != 2 {
		t.Errorf("proxy egress has %d address-range peers, want 2 (DNS and general): %+v", blocks, rules)
	}
}
