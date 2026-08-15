// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBuildRunMountsDropsSubscriptionForNonModelRun is the regression proof for
// W5-S1-5: a task-mode=exec (or non-interactive scan) run gets NO LLM
// credential by contract (resolveLLMTransport's modelRun gate), yet
// buildRunMounts used to copy policy.WorkspaceMounts verbatim regardless of
// modelRun — so a resolved policy that happened to carry the resident
// ~/.claude subscription mount (e.g. an operator's subscription-blessed
// default/named policy reused for a plain exec task, with no per-run
// integration consent) landed real OAuth credential files in a sandbox that
// had no model-call reason to see them, contradicting THREAT-MODEL.md 5.1a's
// stated "lands only for a resident_host anthropic_subscription run" bound.
func TestBuildRunMountsDropsSubscriptionForNonModelRun(t *testing.T) {
	policy := types.RunPolicySpec{
		WorkspaceMounts: []types.WorkspaceMount{
			{Source: "/host/repo", Target: "/home/agent/workspace"},
			{Source: "/host/claude", Target: claudeCredTarget},
			{Source: "/host/claude.json", Target: claudeCredJSONTarget},
		},
	}

	t.Run("non-model run: subscription mounts dropped, others kept", func(t *testing.T) {
		mounts := buildRunMounts(policy, llmTransport{modelRun: false})
		if len(mounts) != 1 {
			t.Fatalf("mounts = %+v, want exactly the workspace mount (subscription creds must not reach a non-model run)", mounts)
		}
		if mounts[0].Target != "/home/agent/workspace" {
			t.Fatalf("unexpected surviving mount: %+v", mounts[0])
		}
	})

	t.Run("model run: every mount, including subscription, is kept", func(t *testing.T) {
		mounts := buildRunMounts(policy, llmTransport{modelRun: true})
		if len(mounts) != 3 {
			t.Fatalf("mounts = %+v, want all 3 (a model run is allowed the resident subscription mount)", mounts)
		}
	})
}
