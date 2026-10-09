// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// github_capabilities is explained as azure_devops_capabilities is: a field
// of its own, listed after it, and a removal on a bound run reads as limits.
func TestExplainRunPolicy_GitHubCapabilities(t *testing.T) {
	base := pvSpec(nil, func(s *types.RunPolicySpec) {
		s.AzureDevOpsCapabilities = []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapPR}
		s.GitHubCapabilities = []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapPR}
	})
	resolved := pvSpec(nil, func(s *types.RunPolicySpec) {
		s.AzureDevOpsCapabilities = []adoscope.Capability{adoscope.CapCodeRead}
		s.GitHubCapabilities = []ghscope.Capability{ghscope.CapCodeRead}
	})
	got := explainRunPolicy(ptr(base), resolved, evidenceOf(func(e *auditEvidence) { e.bounded = true }), types.SiteConfig{}, types.AgentRun{})
	gh := pvChange(got, causeLimits, fieldGHCaps)
	if gh == nil || !slices.Equal(gh.Removed, []string{"pr"}) || len(gh.Added) != 0 {
		t.Fatalf("changes = %+v, want limits removing pr from %s", got, fieldGHCaps)
	}
	ado := slices.IndexFunc(got, func(c runPolicyChange) bool { return c.Field == fieldADOCaps })
	if i := slices.IndexFunc(got, func(c runPolicyChange) bool { return c.Field == fieldGHCaps }); ado < 0 || i < ado {
		t.Errorf("changes = %+v, want %s listed after %s", got, fieldGHCaps, fieldADOCaps)
	}
	if same := explainRunPolicy(ptr(resolved), resolved, evidenceOf(nil), types.SiteConfig{}, types.AgentRun{}); len(same) != 0 {
		t.Errorf("a policy against itself = %+v, want no changes", same)
	}
}
