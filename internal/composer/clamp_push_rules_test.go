// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestClamp_PushRulesPassesThroughUnderSilentCeiling pins the "narrows only"
// half of push_rules' clamp contract: unlike llm_inspection, a proposal's own
// push_rules can never WIDEN what a push may touch, so a ceiling that sets no
// opinion leaves it exactly as authored — nil stays nil, and a proposal's own
// rules survive unclamped.
func TestClamp_PushRulesPassesThroughUnderSilentCeiling(t *testing.T) {
	ceiling := operatorCeiling(t) // sets no push_rules opinion

	got, warns := Clamp(types.RunPolicySpec{}, ceiling, 0)
	if got.PushRules != nil {
		t.Errorf("push_rules = %+v, want nil (ceiling sets none)", got.PushRules)
	}
	if hasWarn(warns, "push_rules") {
		t.Errorf("unexpected push_rules warning with no ceiling opinion: %v", warns)
	}

	proposed := types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}, MaxInspectPackMiB: 4}}
	got, warns = Clamp(proposed, ceiling, 0)
	if got.PushRules == nil || got.PushRules.MaxInspectPackMiB != 4 || len(got.PushRules.DenyPaths) != 1 || got.PushRules.DenyPaths[0] != ".github/workflows/**" {
		t.Errorf("push_rules = %+v, want the proposal's own rules untouched", got.PushRules)
	}
	if hasWarn(warns, "push_rules") {
		t.Errorf("unexpected push_rules warning with no ceiling opinion: %v", warns)
	}
}

// TestClamp_PushRulesInheritedWholesaleWhenProposalUnset pins the "floor" half:
// once an operator's ceiling opts push_rules in, a proposal that says nothing
// inherits the ceiling's rules wholesale rather than staying nil.
func TestClamp_PushRulesInheritedWholesaleWhenProposalUnset(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}, MaxInspectPackMiB: 8}

	got, warns := Clamp(types.RunPolicySpec{}, ceiling, 0)
	if got.PushRules == nil || got.PushRules.MaxInspectPackMiB != 8 || len(got.PushRules.DenyPaths) != 1 || got.PushRules.DenyPaths[0] != ".github/workflows/**" {
		t.Errorf("push_rules = %+v, want the ceiling's rules inherited wholesale", got.PushRules)
	}
	if !hasWarn(warns, "push_rules inherited from the operator's policy") {
		t.Errorf("expected a push_rules inherit warning, got %v", warns)
	}

	// Mutating the clamped copy's DenyPaths must not alias the ceiling's own
	// backing array (types.RunPolicySpec.Clone's discipline).
	got.PushRules.DenyPaths[0] = "mutated"
	if ceiling.PushRules.DenyPaths[0] != ".github/workflows/**" {
		t.Error("clamp aliased the ceiling's own PushRules.DenyPaths backing array")
	}
}

// TestClamp_PushRulesMergedWhenBothSet pins the merge half: deny_paths union
// (deny always wins, same stance as denied_domains) and max_inspect_pack_mib
// capped at the ceiling's when the ceiling's is non-zero and stricter.
func TestClamp_PushRulesMergedWhenBothSet(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}, MaxInspectPackMiB: 8}

	proposed := types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{"infra/**"}, MaxInspectPackMiB: 32}}
	got, warns := Clamp(proposed, ceiling, 0)
	if got.PushRules == nil {
		t.Fatal("push_rules = nil, want the merged spec")
	}
	if got.PushRules.MaxInspectPackMiB != 8 {
		t.Errorf("max_inspect_pack_mib = %d, want capped to the ceiling's 8", got.PushRules.MaxInspectPackMiB)
	}
	if !hasWarn(warns, "push_rules.max_inspect_pack_mib capped to operator maximum 8") {
		t.Errorf("expected a max_inspect_pack_mib cap warning, got %v", warns)
	}
	want := map[string]bool{"infra/**": true, ".github/workflows/**": true}
	if len(got.PushRules.DenyPaths) != len(want) {
		t.Fatalf("deny_paths = %v, want the union of both %v", got.PushRules.DenyPaths, want)
	}
	for _, p := range got.PushRules.DenyPaths {
		if !want[p] {
			t.Errorf("deny_paths contains unexpected entry %q", p)
		}
	}

	// A proposal already under the ceiling's max keeps its own (smaller) value.
	proposed.PushRules.MaxInspectPackMiB = 2
	got, _ = Clamp(proposed, ceiling, 0)
	if got.PushRules.MaxInspectPackMiB != 2 {
		t.Errorf("max_inspect_pack_mib = %d, want the proposal's own stricter 2 left alone", got.PushRules.MaxInspectPackMiB)
	}
}

// TestClamp_PushRulesDenyPathsUnionIsCaseSensitive: union() (the
// denied_domains helper) folds case and whitespace, which is correct for a DNS
// name but wrong for a git path — Linux paths are case- and space-sensitive.
// union also seeds its seen-set from its first argument (here, the proposal),
// so a member re-typing the ceiling's own rule in a different case would
// silently displace the ceiling's spelling, leaving the member with strictly
// weaker effective rules than the operator set — the one security property
// this field has. clampPushRules uses unionPaths (exact-string) instead, so
// both spellings survive.
func TestClamp_PushRulesDenyPathsUnionIsCaseSensitive(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}}

	proposed := types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: []string{".GitHub/workflows/**"}}}
	got, _ := Clamp(proposed, ceiling, 0)
	if got.PushRules == nil {
		t.Fatal("push_rules = nil, want the merged spec")
	}
	want := map[string]bool{".GitHub/workflows/**": true, ".github/workflows/**": true}
	if len(got.PushRules.DenyPaths) != len(want) {
		t.Fatalf("deny_paths = %v, want BOTH spellings present (the ceiling's must never be displaced): %v", got.PushRules.DenyPaths, want)
	}
	for _, p := range got.PushRules.DenyPaths {
		if !want[p] {
			t.Errorf("deny_paths contains unexpected entry %q", p)
		}
	}
	foundCeiling := false
	for _, p := range got.PushRules.DenyPaths {
		if p == ".github/workflows/**" {
			foundCeiling = true
		}
	}
	if !foundCeiling {
		t.Error("the ceiling's own exact spelling was displaced by the proposal's differently-cased re-typing")
	}
}

// TestClamp_PushRulesEmptyCeilingSpecReadsAsAbsent pins the other #176 review
// finding: an all-zero-but-non-nil ceiling.PushRules ("push_rules": {} on the
// wire) must behave exactly like a nil one — never inherited wholesale into
// every member's clamped spec.
func TestClamp_PushRulesEmptyCeilingSpecReadsAsAbsent(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.PushRules = &types.PushRulesSpec{} // present, but nothing in it

	got, warns := Clamp(types.RunPolicySpec{}, ceiling, 0)
	if got.PushRules != nil {
		t.Errorf("push_rules = %+v, want nil (an all-zero ceiling spec is absent)", got.PushRules)
	}
	if hasWarn(warns, "push_rules") {
		t.Errorf("unexpected push_rules warning from an all-zero ceiling spec: %v", warns)
	}
}
