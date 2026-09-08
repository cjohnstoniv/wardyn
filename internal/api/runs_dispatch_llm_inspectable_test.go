// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// inspectableGateStore is the smallest store enforceInspectableLLM's refusal
// path touches: failAndRevoke CASes the run STARTING -> FAILED.
type inspectableGateStore struct {
	store.Store
	failed bool
}

func (s *inspectableGateStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, _, to types.RunState) (bool, error) {
	if to == types.RunFailed {
		s.failed = true
	}
	return true, nil
}

// TestEnforceInspectableLLM_BedrockBearerIsOpaque is the F048 regression.
// require_inspectable_llm is a RUNTIME guarantee (policy.go) and THREAT-MODEL
// 5.1a promises a strict operator that an opaque-transport run fails CLOSED at
// schedule time, with "Bedrock stays opaque regardless". The gate exempted the
// Bedrock BEARER sub-mode on the claim that proxy-injected + MITM'd makes it
// inspectable — but MITM only makes the body READABLE. There is no Bedrock
// extractor (contentscan.Extract covers anthropic.messages / openai.chat /
// generic / mcp.jsonrpc) and channelForHost maps a bedrock-runtime host to
// ChannelGeneric, which classifyLLM treats as not prompt-bearing, so such a run
// was admitted as "inspectable" with ZERO scan coverage.
func TestEnforceInspectableLLM_BedrockBearerIsOpaque(t *testing.T) {
	strict := &types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true, RequireInspectableLLM: true, InterceptTLS: true,
	}}
	cases := map[string]llmTransport{
		"bedrock bearer": {bedrockReady: true, bedrock: bedrockAuth{ready: true, bearer: true}, injectBedrockBearer: true},
		"bedrock sigv4":  {bedrockReady: true, bedrock: bedrockAuth{ready: true}},
	}
	for name, llm := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			st := &inspectableGateStore{}
			srv := New(baseTestConfig(h, st))
			run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", State: types.RunStarting}

			if srv.enforceInspectableLLM(context.Background(), run, strict, llm) {
				t.Fatalf("%s: enforceInspectableLLM admitted an opaque Bedrock transport under require_inspectable_llm", name)
			}
			if !st.failed {
				t.Errorf("%s: the run was not failed closed", name)
			}
		})
	}
}

// TestEnforceInspectableLLM_InspectableStillAdmitted keeps the guard honest:
// the api-key path this gate exists to permit is unaffected.
func TestEnforceInspectableLLM_InspectableStillAdmitted(t *testing.T) {
	h := newHarness(t)
	st := &inspectableGateStore{}
	srv := New(baseTestConfig(h, st))
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", State: types.RunStarting}
	strict := &types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true, RequireInspectableLLM: true, InterceptTLS: true,
	}}

	if !srv.enforceInspectableLLM(context.Background(), run, strict, llmTransport{}) {
		t.Fatal("the inspectable api-key transport must still be admitted")
	}
	if st.failed {
		t.Error("an inspectable transport must not be failed closed")
	}
}
