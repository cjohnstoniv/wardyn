// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestReconcileLLMAccess_SubHintSurvivesGateway: the "launch this proposal
// from the wizard with your Claude subscription mounted" hint must still
// appear for an Anthropic no-model-access verdict once a gateway is
// configured — it is keyed on the PROVIDER (p.secret ==
// "anthropic-api-key"), not on p.host, which under a gateway is the
// gateway's host, not "api.anthropic.com".
func TestReconcileLLMAccess_SubHintSurvivesGateway(t *testing.T) {
	const wantHint = "launch this proposal from the wizard with your Claude subscription mounted"

	// Baseline: no gateway configured, no secret present -> the hint appears.
	s := &Server{}
	spec := &types.RunPolicySpec{}
	note, provisioned := s.reconcileLLMAccess(spec, "claude-code", map[string]bool{}, false, false)
	if provisioned {
		t.Fatalf("expected no model access, got provisioned")
	}
	if !strings.Contains(note, wantHint) {
		t.Fatalf("expected the subscription hint, got %q", note)
	}

	// Gateway configured: the hint must still appear (this is what the fix
	// covers — keying on p.host == "api.anthropic.com" would silently drop it).
	gw := &Server{cfg: Config{LLMGateways: map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal"}}}
	spec2 := &types.RunPolicySpec{}
	note2, provisioned2 := gw.reconcileLLMAccess(spec2, "claude-code", map[string]bool{}, false, false)
	if provisioned2 {
		t.Fatalf("expected no model access, got provisioned")
	}
	if !strings.Contains(note2, wantHint) {
		t.Fatalf("gateway must not suppress the subscription hint, got %q", note2)
	}

	// Negative control: OpenAI never carries the Claude-only hint, gateway or not.
	noteOpenAI, _ := s.reconcileLLMAccess(&types.RunPolicySpec{}, "codex-cli", map[string]bool{}, false, false)
	if strings.Contains(noteOpenAI, wantHint) {
		t.Fatalf("OpenAI's verdict must never carry the Claude subscription hint, got %q", noteOpenAI)
	}
}
