// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// applyWorkspaceCreds folds a workspace/container's operator-owned model/harness
// cred binding (types.WorkspaceLLMCred.IntegrationRef) into a run's policy at
// create. The old api_key/managed/bedrock Mode-keyed branches this file used to
// pin (TestApplyWorkspaceCreds_APIKey_*, TestApplyWorkspaceCreds_Managed_*) are
// GONE — WorkspaceLLMCred carries only IntegrationRef now, and
// resolveWorkspaceIntegration (llmcred.go) is an explicit W5 stub that always
// reports itself unresolvable, so EVERY binding is currently a no-op (falls back
// to the run's global provider config). Nothing today exercises a real
// resolution, so the only honest behavior left to pin is the no-op contract
// itself, including the still-live pre-checks (nil binding, empty ref, and the
// non-LLM-agent short-circuit) that run before resolution would even be
// attempted.
func TestApplyWorkspaceCreds_NoBindingIsNoOp(t *testing.T) {
	s := New(Config{})
	spec := &types.RunPolicySpec{}

	// nil binding
	if mode := s.applyWorkspaceCreds(context.Background(), spec, &types.Workspace{}, "claude-code"); mode != "" {
		t.Fatalf("nil binding: mode = %q, want empty", mode)
	}
	// explicit empty ref (the IntegrationRef equivalent of "no binding")
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: ""}}
	if mode := s.applyWorkspaceCreds(context.Background(), spec, ws, "claude-code"); mode != "" {
		t.Fatalf("empty ref: mode = %q, want empty", mode)
	}
	// non-LLM agent: nothing to bind even with a ref set
	ws2 := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-anthropic"}}
	if mode := s.applyWorkspaceCreds(context.Background(), spec, ws2, "some-other-agent"); mode != "" {
		t.Fatalf("non-LLM agent: mode = %q, want empty", mode)
	}
	// LLM agent WITH a ref set: still a no-op today (W5: resolveWorkspaceIntegration
	// is unimplemented). This assertion is expected to start failing the moment W5
	// lands real resolution — that's the point: it flags this test for an update
	// instead of silently going stale.
	ws3 := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{IntegrationRef: "acme-anthropic"}}
	if mode := s.applyWorkspaceCreds(context.Background(), spec, ws3, "claude-code"); mode != "" {
		t.Fatalf("unresolvable ref: mode = %q, want empty (W5 resolution not wired)", mode)
	}
	if len(spec.EligibleGrants) != 0 || len(spec.AllowedDomains) != 0 {
		t.Fatal("no-op cases must not mutate the spec")
	}
}
