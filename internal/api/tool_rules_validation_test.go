// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validatePolicySpec is the single chokepoint every policy-entry path funnels
// through — a stored write, an inline run policy, and the boot-time default
// file. A rule that reaches the proxy unvalidated is a rule that reads as
// enforcement and is not.
func TestValidateToolRules(t *testing.T) {
	base := types.RunPolicySpec{
		MinConfinementClass: types.CC1,
		FirstUseApproval:    types.FirstUseAlwaysDeny,
	}
	with := func(rules ...types.ToolRule) types.RunPolicySpec {
		s := base
		s.ToolRules = rules
		return s
	}

	t.Run("a valid rule set passes", func(t *testing.T) {
		if err := validatePolicySpec(with(
			types.ToolRule{Tool: "Read", Effect: types.ToolAllow},
			types.ToolRule{Tool: "Bash", Effect: types.ToolHold},
			types.ToolRule{Tool: "*", Effect: types.ToolDeny},
		)); err != nil {
			t.Fatalf("valid tool_rules rejected: %v", err)
		}
	})

	t.Run("no rules at all passes", func(t *testing.T) {
		if err := validatePolicySpec(base); err != nil {
			t.Fatalf("a policy with no tool_rules must remain valid: %v", err)
		}
	})

	// The most important case: an unrecognised effect must be REFUSED, never
	// ignored. A silently-dropped rule is worse than no rule, because the
	// operator believes the tool is governed.
	t.Run("an unknown effect is refused", func(t *testing.T) {
		err := validatePolicySpec(with(types.ToolRule{Tool: "Bash", Effect: "maybe"}))
		if err == nil {
			t.Fatal("an unknown effect was accepted — a silently-ignored rule reads as enforcement and is not")
		}
		if !strings.Contains(err.Error(), "effect") {
			t.Errorf("the error must name the field: %v", err)
		}
	})

	t.Run("a duplicate tool is refused", func(t *testing.T) {
		err := validatePolicySpec(with(
			types.ToolRule{Tool: "Bash", Effect: types.ToolAllow},
			types.ToolRule{Tool: "Bash", Effect: types.ToolDeny},
		))
		if err == nil {
			t.Fatal("two rules for one tool were accepted — one of them does nothing, and the author believes something untrue about which")
		}
	})

	t.Run("an empty tool name is refused", func(t *testing.T) {
		if err := validatePolicySpec(with(types.ToolRule{Effect: types.ToolAllow})); err == nil {
			t.Fatal("a rule with no tool name was accepted")
		}
	})

	// Whitespace is refused rather than trimmed: the match is exact, so " Bash"
	// would never fire, and silently trimming it hides the author's typo.
	t.Run("a padded tool name is refused, not trimmed", func(t *testing.T) {
		err := validatePolicySpec(with(types.ToolRule{Tool: " Bash", Effect: types.ToolAllow}))
		if err == nil {
			t.Fatal("a tool name with whitespace was accepted — the match is exact, so the rule would never fire")
		}
	})

	t.Run("too many rules is refused", func(t *testing.T) {
		many := make([]types.ToolRule, maxToolRulesPerPolicy+1)
		for i := range many {
			many[i] = types.ToolRule{Tool: strings.Repeat("t", i+1), Effect: types.ToolAllow}
		}
		if err := validatePolicySpec(with(many...)); err == nil {
			t.Fatalf("more than %d rules was accepted; this rides a per-run JSON document into a sidecar", maxToolRulesPerPolicy)
		}
	})

	t.Run("an over-long tool name is refused", func(t *testing.T) {
		if err := validatePolicySpec(with(types.ToolRule{
			Tool: strings.Repeat("t", maxToolRuleNameLen+1), Effect: types.ToolAllow,
		})); err == nil {
			t.Fatal("an over-long tool name was accepted")
		}
	})
}
