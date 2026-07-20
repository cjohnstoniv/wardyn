// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// applyWorkspaceCreds folds a workspace/container's operator-owned model/harness
// cred binding into a run's policy at create. These pin the four modes.
func credServer(t *testing.T, secretNames ...string) *Server {
	t.Helper()
	m := &memSecrets{m: map[string][]byte{}}
	for _, n := range secretNames {
		_ = m.Put(context.Background(), n, []byte("x"))
	}
	return New(Config{Secrets: m})
}

func TestApplyWorkspaceCreds_APIKey_AppendsGrantAndEgress(t *testing.T) {
	s := credServer(t, "acme-anthropic-key")
	spec := &types.RunPolicySpec{}
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{
		Mode: types.WorkspaceLLMCredAPIKey, APIKeySecret: "acme-anthropic-key",
	}}
	if mode := s.applyWorkspaceCreds(context.Background(), spec, ws, "claude-code"); mode != types.WorkspaceLLMCredAPIKey {
		t.Fatalf("mode = %q, want api_key", mode)
	}
	g, ok := apiKeyGrantForHost(spec, "api.anthropic.com")
	if !ok {
		t.Fatal("expected an api_key grant for api.anthropic.com")
	}
	var scope map[string]string
	_ = json.Unmarshal(g.Scope, &scope)
	if scope["secret_name"] != "acme-anthropic-key" {
		t.Fatalf("grant secret = %q, want the workspace's secret", scope["secret_name"])
	}
	if !domainAllowedExact(spec.AllowedDomains, "api.anthropic.com") {
		t.Fatal("expected api.anthropic.com egress coupled to the grant")
	}
}

func TestApplyWorkspaceCreds_APIKey_AbsentSecretFallsBack(t *testing.T) {
	s := credServer(t) // the referenced secret is NOT stored
	spec := &types.RunPolicySpec{}
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{
		Mode: types.WorkspaceLLMCredAPIKey, APIKeySecret: "missing-key",
	}}
	if mode := s.applyWorkspaceCreds(context.Background(), spec, ws, "claude-code"); mode != "" {
		t.Fatalf("expected fall-back (no binding) for an absent secret, got %q", mode)
	}
	if len(spec.EligibleGrants) != 0 {
		t.Fatal("must not append a grant whose secret would fail the proxy closed")
	}
}

func TestApplyWorkspaceCreds_Managed_EnsuresEgressDropsCompetingAPIKey(t *testing.T) {
	s := credServer(t)
	scope, _ := json.Marshal(map[string]string{"host": "api.anthropic.com", "secret_name": "stray"})
	spec := &types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: scope}}}
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{Mode: types.WorkspaceLLMCredManaged}}
	if mode := s.applyWorkspaceCreds(context.Background(), spec, ws, "claude-code"); mode != types.WorkspaceLLMCredManaged {
		t.Fatalf("mode = %q, want managed", mode)
	}
	if _, ok := apiKeyGrantForHost(spec, "api.anthropic.com"); ok {
		t.Fatal("managed must drop a competing api-key grant so the managed token injects")
	}
	if !domainAllowedExact(spec.AllowedDomains, "api.anthropic.com") {
		t.Fatal("managed must ensure api.anthropic.com egress")
	}
}

func TestApplyWorkspaceCreds_NoBindingIsNoOp(t *testing.T) {
	s := credServer(t)
	spec := &types.RunPolicySpec{}
	// nil binding
	if mode := s.applyWorkspaceCreds(context.Background(), spec, &types.Workspace{}, "claude-code"); mode != "" {
		t.Fatalf("nil binding: mode = %q, want empty", mode)
	}
	// explicit none
	ws := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{Mode: types.WorkspaceLLMCredNone}}
	if mode := s.applyWorkspaceCreds(context.Background(), spec, ws, "claude-code"); mode != "" {
		t.Fatalf("none binding: mode = %q, want empty", mode)
	}
	// non-LLM agent: nothing to bind even with a mode set
	ws2 := &types.Workspace{LLMCred: &types.WorkspaceLLMCred{Mode: types.WorkspaceLLMCredManaged}}
	if mode := s.applyWorkspaceCreds(context.Background(), spec, ws2, "some-other-agent"); mode != "" {
		t.Fatalf("non-LLM agent: mode = %q, want empty", mode)
	}
	if len(spec.EligibleGrants) != 0 || len(spec.AllowedDomains) != 0 {
		t.Fatal("no-op cases must not mutate the spec")
	}
}
