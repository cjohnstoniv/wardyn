// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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

// TestRunLLMAccess_OnlyAChosenProviderProvisions: a model run's access is its
// chosen model provider and nothing else — no stored convention key, no
// managed subscription — so a codex-cli run with no provider is not
// provisioned and one that chose an OpenAI key provider is.
func TestRunLLMAccess_OnlyAChosenProviderProvisions(t *testing.T) {
	req := createRunRequest{Agent: "codex-cli"}
	if la := runLLMAccess(req, runProviderChoice{}); la == nil || la.Provisioned {
		t.Fatalf("codex-cli with no model provider: got %+v, want non-nil !Provisioned", la)
	}
	chosen := runProviderChoice{chosen: true, provider: types.ModelProvider{ID: "openai", Kind: types.ModelProviderOpenAIAPIKey}}
	if la := runLLMAccess(req, chosen); la == nil || !la.Provisioned {
		t.Fatalf("codex-cli that chose an OpenAI key provider: got %+v, want Provisioned", la)
	}
	if la := runLLMAccess(createRunRequest{Agent: "oracle"}, runProviderChoice{}); la != nil {
		t.Fatalf("a non-LLM agent: got %+v, want nil", la)
	}
}

// TestNoModelAccessWarning_Copy: the advisory names the agent and sends the
// person to Settings → Model providers — never to an integration or a stored
// convention secret, since neither credentials a run (#547).
func TestNoModelAccessWarning_Copy(t *testing.T) {
	for _, agent := range []string{"codex-cli", "claude-code"} {
		got := strings.Join(noModelAccessWarning(createRunRequest{Agent: agent}, runProviderChoice{}), "\n")
		for _, want := range []string{agent, "Settings → Model providers"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s warning missing %q; got: %s", agent, want, got)
			}
		}
		for _, gone := range []string{"integration", "anthropic-api-key", "openai-api-key"} {
			if strings.Contains(got, gone) {
				t.Errorf("%s warning still names %q; got: %s", agent, gone, got)
			}
		}
	}
}
