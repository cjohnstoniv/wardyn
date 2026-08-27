// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tool_rules narrows an autonomous run's tool use from a binary into a policy.
// These pin the matching semantics, which are deliberately dull: exact name,
// then "*", then no match.
func TestToolEffectFor(t *testing.T) {
	spec := types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: "Read", Effect: types.ToolAllow},
		{Tool: "Bash", Effect: types.ToolHold},
		{Tool: "WebFetch", Effect: types.ToolDeny},
	}}
	p := CompilePolicy(spec)

	for _, tc := range []struct {
		tool string
		want types.ToolEffect
		ok   bool
	}{
		{"Read", types.ToolAllow, true},
		{"Bash", types.ToolHold, true},
		{"WebFetch", types.ToolDeny, true},
		// No rule and no "*" default: the caller must fall back to asking a
		// human. This is the case that keeps the field additive.
		{"Edit", "", false},
	} {
		got, ok := p.ToolEffectFor(tc.tool)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ToolEffectFor(%q) = (%q, %v), want (%q, %v)", tc.tool, got, ok, tc.want, tc.ok)
		}
	}

	// Case-sensitive on purpose: "bash" is not "Bash". A case-insensitive match
	// would make a rule fire on a tool the operator did not name.
	if _, ok := p.ToolEffectFor("bash"); ok {
		t.Error(`ToolEffectFor("bash") matched the rule for "Bash" — matching must be case-sensitive, or a rule fires on a tool nobody named`)
	}
}

func TestToolEffectFor_WildcardIsTheDefault(t *testing.T) {
	p := CompilePolicy(types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: "Read", Effect: types.ToolAllow},
		{Tool: "*", Effect: types.ToolDeny},
	}})

	// An exact rule beats the default...
	if got, ok := p.ToolEffectFor("Read"); !ok || got != types.ToolAllow {
		t.Errorf(`ToolEffectFor("Read") = (%q, %v), want (allow, true) — an exact rule must beat "*"`, got, ok)
	}
	// ...and everything else takes it.
	if got, ok := p.ToolEffectFor("Bash"); !ok || got != types.ToolDeny {
		t.Errorf(`ToolEffectFor("Bash") = (%q, %v), want (deny, true) via "*"`, got, ok)
	}
}

// The property that makes this field safe to add: a run with NO rules behaves
// exactly as it did before the field existed. If this regresses, every policy
// written before 0.7 changes meaning silently.
func TestToolEffectFor_NoRulesChangesNothing(t *testing.T) {
	for _, name := range []string{"no rules", "nil policy"} {
		t.Run(name, func(t *testing.T) {
			var p *Policy
			if name == "no rules" {
				p = CompilePolicy(types.RunPolicySpec{})
			}
			if _, ok := p.ToolEffectFor("Bash"); ok {
				t.Error("a policy with no tool_rules reported a rule — every call must still go to a human")
			}
		})
	}
}

// Clone is deep for every slice field, and a miss aliases the backing array
// across concurrent runs — the exact bug its own doc warns about. Nothing else
// would catch a new field being left out.
func TestClone_DeepCopiesToolRules(t *testing.T) {
	orig := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}}
	cp := orig.Clone()
	cp.ToolRules[0].Effect = types.ToolAllow

	if orig.ToolRules[0].Effect != types.ToolHold {
		t.Fatal("mutating the CLONE's tool_rules changed the ORIGINAL — Clone is aliasing the slice, so one run's policy edit would leak into another's")
	}
}
