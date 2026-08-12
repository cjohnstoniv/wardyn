// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunNeedsModelWarning gates the create-time model-access warning: only a
// non-interactive HARNESS run whose agent needs a model qualifies. exec runs,
// interactive runs, the harness-login box, and non-LLM agents are all exempt.
func TestRunNeedsModelWarning(t *testing.T) {
	cases := []struct {
		name string
		req  createRunRequest
		want bool
	}{
		{"plain codex harness run", createRunRequest{Agent: "codex-cli"}, true},
		{"plain claude harness run", createRunRequest{Agent: "claude-code"}, true},
		{"explicit harness task_mode", createRunRequest{Agent: "codex-cli", TaskMode: "harness"}, true},
		{"exec run (plain command, no harness)", createRunRequest{Agent: "codex-cli", TaskMode: "exec"}, false},
		{"interactive run (operator sees the failure live)", createRunRequest{Agent: "codex-cli", Interactive: true}, false},
		{"harness-login box (mints nothing)", createRunRequest{Agent: "claude-code", Task: harnessLoginTask}, false},
		{"non-LLM agent", createRunRequest{Agent: "oracle"}, false},
		{"empty agent", createRunRequest{Agent: ""}, false},
	}
	for _, c := range cases {
		if got := runNeedsModelWarning(c.req); got != c.want {
			t.Errorf("%s: runNeedsModelWarning = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestResolveRunLLMAccess_ManagedSubDoesNotCoverCodex is the AGT4-2 case: a
// codex-cli run whose ONLY model access is a Wardyn-managed Claude subscription is
// NOT provisioned — managed inject is hard-gated to claude-code, so codex needs its
// own OpenAI credential and would boot then 404 on its first model call.
func TestResolveRunLLMAccess_ManagedSubDoesNotCoverCodex(t *testing.T) {
	s := &Server{cfg: Config{
		Secrets:      &memSecrets{m: map[string][]byte{}}, // no openai-api-key stored
		ManagedToken: fakeSubToken{},                      // a managed Claude subscription IS connected
	}}
	req := createRunRequest{Agent: "codex-cli"}
	spec := types.RunPolicySpec{AllowAllEgress: true} // egress open, but no OpenAI key/grant
	la := s.resolveRunLLMAccess(context.Background(), req, spec, map[string]bool{}, nil)
	if la == nil || la.Provisioned {
		t.Fatalf("codex-cli with only a managed Claude sub: got %+v, want non-nil !Provisioned", la)
	}
}

// TestResolveRunLLMAccess_OpenAIKeyProvisionsCodex is the no-false-warning
// direction: a codex-cli run with a stored openai-api-key, an auto-mint grant, and
// matching egress IS provisioned, so the create path must NOT warn.
func TestResolveRunLLMAccess_OpenAIKeyProvisionsCodex(t *testing.T) {
	s := &Server{cfg: Config{Secrets: &memSecrets{m: map[string][]byte{}}}}
	scope, _ := json.Marshal(map[string]string{"host": "api.openai.com", "secret_name": "openai-api-key"})
	req := createRunRequest{Agent: "codex-cli"}
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"api.openai.com"},
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: scope}},
	}
	la := s.resolveRunLLMAccess(context.Background(), req, spec, map[string]bool{"openai-api-key": true}, nil)
	if la == nil || !la.Provisioned {
		t.Fatalf("codex-cli with a stored OpenAI key + grant + egress: got %+v, want Provisioned", la)
	}
}

// TestNoModelAccessWarning_Copy: the advisory always names the provider host + the
// convention secret; the managed-Claude caveat appears ONLY on a non-claude agent
// when a managed sub is connected (steering to --agent claude-code).
func TestNoModelAccessWarning_Copy(t *testing.T) {
	codex, _ := agentLLMProvider("codex-cli")
	claude, _ := agentLLMProvider("claude-code")

	withManaged := noModelAccessWarning("codex-cli", codex, true)
	for _, want := range []string{"codex-cli", "api.openai.com", "openai-api-key", "claude-code only", "--agent claude-code"} {
		if !strings.Contains(withManaged, want) {
			t.Errorf("codex+managed warning missing %q; got: %s", want, withManaged)
		}
	}
	noManaged := noModelAccessWarning("codex-cli", codex, false)
	if strings.Contains(noManaged, "claude-code only") {
		t.Errorf("codex warning with NO managed sub must not mention the managed caveat; got: %s", noManaged)
	}
	// A claude-code agent must never be told to "use --agent claude-code" even when a
	// managed sub is present (it would be nonsensical self-reference).
	claudeMsg := noModelAccessWarning("claude-code", claude, true)
	if strings.Contains(claudeMsg, "--agent claude-code") {
		t.Errorf("claude-code warning must not steer to itself; got: %s", claudeMsg)
	}
}
