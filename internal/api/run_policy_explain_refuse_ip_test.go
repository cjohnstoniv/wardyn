// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refuse_ip_literals is a field of the run's policy view: a resolved spec that turned it on, against a
// policy without it, shows one change adding it, listed after allow_all_egress.
func TestExplainRunPolicy_RefuseIPLiterals(t *testing.T) {
	base := pvSpec(nil, nil)
	resolved := pvSpec(nil, func(s *types.RunPolicySpec) { s.AllowAllEgress, s.RefuseIPLiterals = true, true })
	got := explainRunPolicy(ptr(base), resolved, evidenceOf(nil), types.SiteConfig{}, types.AgentRun{})
	i := slices.IndexFunc(got, func(c runPolicyChange) bool { return c.Field == "refuse_ip_literals" })
	if i < 0 || !slices.Equal(got[i].Added, []string{"true"}) || len(got[i].Removed) != 0 {
		t.Fatalf("changes = %+v, want one adding refuse_ip_literals", got)
	}
	if all := slices.IndexFunc(got, func(c runPolicyChange) bool { return c.Field == "allow_all_egress" }); all < 0 || all > i && got[all].Cause == got[i].Cause {
		t.Errorf("changes = %+v, want allow_all_egress listed before refuse_ip_literals", got)
	}
}
