// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// host_rules is a field of the run's policy view, one entry per rule set: a resolved spec that adds a set
// shows one change adding that set, listed after allowed_methods, and the run's provenance carries it.
func TestExplainRunPolicy_HostRules(t *testing.T) {
	set := types.HostRuleSet{Rules: []types.HostRule{{Host: "api.example.com", Methods: []string{"GET"}, Paths: []string{"/repos"}, Effect: types.HostRuleAllow}}}
	base := pvSpec(nil, func(s *types.RunPolicySpec) { s.AllowedMethods = []string{"GET"} })
	resolved := pvSpec(nil, func(s *types.RunPolicySpec) {
		s.AllowedMethods = []string{"GET", "POST"}
		s.HostRules = []types.HostRuleSet{set}
	})
	got := explainRunPolicy(ptr(base), resolved, evidenceOf(nil), types.SiteConfig{}, types.AgentRun{})
	i := slices.IndexFunc(got, func(c runPolicyChange) bool { return c.Field == "host_rules" })
	if i < 0 || !slices.Equal(got[i].Added, []string{types.HostRuleSetKey(set)}) || len(got[i].Removed) != 0 {
		t.Fatalf("changes = %+v, want one adding the host rule set", got)
	}
	if m := slices.IndexFunc(got, func(c runPolicyChange) bool { return c.Field == fieldMethods }); m < 0 || m > i && got[m].Cause == got[i].Cause {
		t.Errorf("changes = %+v, want allowed_methods listed before host_rules", got)
	}
	if !slices.Contains(specEntries(resolved), specEntry{"host_rules", types.HostRuleSetKey(set)}) {
		t.Errorf("provenance entries = %+v, want the host rule set", specEntries(resolved))
	}
}
