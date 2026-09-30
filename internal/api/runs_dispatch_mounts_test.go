// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBuildRunMounts_DropsTheRetiredClaudeMountForEveryRun: the host
// ~/.claude subscription mount is retired, so a policy row stored before 0.8.2
// that still carries it lands in no sandbox, model run or not; every other
// mount passes through.
func TestBuildRunMounts_DropsTheRetiredClaudeMountForEveryRun(t *testing.T) {
	policy := types.RunPolicySpec{
		WorkspaceMounts: []types.WorkspaceMount{
			{Source: "/host/repo", Target: "/home/agent/workspace"},
			{Source: "/host/claude", Target: claudeCredTarget},
			{Source: "/host/claude.json", Target: claudeCredJSONTarget},
		},
	}
	mounts := buildRunMounts(policy, userMountPosture{})
	if len(mounts) != 1 || mounts[0].Target != "/home/agent/workspace" {
		t.Fatalf("mounts = %+v, want only the workspace mount", mounts)
	}
}

// TestValidatePolicySpec_RefusesTheRetiredClaudeMount: a policy (the boot
// default policy included, through LoadPolicySpec) naming the retired mount
// target is refused, naming where model access lives now.
func TestValidatePolicySpec_RefusesTheRetiredClaudeMount(t *testing.T) {
	for _, target := range []string{claudeCredTarget, claudeCredJSONTarget} {
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2,
			WorkspaceMounts: []types.WorkspaceMount{{Source: "/home/op/.wardyn/claude-creds/.claude", Target: target}}}
		err := validatePolicySpec(spec)
		if err == nil || !strings.Contains(err.Error(), "Settings → Model providers") {
			t.Errorf("target %s: err = %v, want a refusal naming Settings → Model providers", target, err)
		}
	}
}
