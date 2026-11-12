// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The headers a provider's gateway is given belong with its address, which is the organisation's network
// and is never sent: a plan that carries them is refused on a local placement.
func TestLocalEligibilityRefusesGatewayHeaders(t *testing.T) {
	p := LocalPlan{Spec: runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{LLMHeaders: map[string]map[string]string{"gw.corp.example": {"X-Team": "platform"}}}}}
	if _, r := LocalEligibility(p); r == nil || r.Reason != ReasonPlacementCredential || r.Field != "ProxyConfig.LLMHeaders" {
		t.Fatalf("refusal %v, want placement_credential on ProxyConfig.LLMHeaders", r)
	}
	p.Spec.ProxyConfig.LLMHeaders = nil
	if _, r := LocalEligibility(p); r != nil {
		t.Fatalf("a plan with no gateway headers was refused: %v", r)
	}
}
