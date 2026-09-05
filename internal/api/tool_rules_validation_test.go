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

// TestValidateAllowedDomainsCount is F061's residue: allowed_domains was the
// last per-spec list with NO count cap, so one request body could carry ~52,425
// entries (what fits under maxJSONBody) and every one of them became work — the
// proxy matches against each per request, and on the member path
// narrowMemberInlinePolicy asks the capability seam about each. POST
// /runs/preflight is on the member router, persists nothing, and is therefore
// repeatable for free.
//
// The literals here are deliberate: 256 is the contract a member can hit, so
// this test states it rather than restating the constant to itself.
func TestValidateAllowedDomainsCount(t *testing.T) {
	base := types.RunPolicySpec{
		MinConfinementClass: types.CC1,
		FirstUseApproval:    types.FirstUseAlwaysDeny,
	}
	domains := func(n int) types.RunPolicySpec {
		s := base
		s.AllowedDomains = make([]string, n)
		for i := range s.AllowedDomains {
			s.AllowedDomains[i] = "api.anthropic.com"
		}
		return s
	}

	t.Run("exactly 256 entries is accepted", func(t *testing.T) {
		if err := validatePolicySpec(domains(256)); err != nil {
			t.Fatalf("256 entries was refused (%v) — the cap is a hostile-input ceiling, not a sizing of a real allowlist", err)
		}
	})

	t.Run("257 entries is refused, naming the cap", func(t *testing.T) {
		err := validatePolicySpec(domains(257))
		if err == nil {
			t.Fatal("257 allowed_domains entries was accepted; every entry is per-request proxy work and a per-entry capability question on the member path")
		}
		if want := "allowed_domains: at most 256 entries"; !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	})

	// A REFUSAL, never a truncation: a silently shortened allowlist is a policy
	// that reads as permission and denies.
	t.Run("the spec is not truncated", func(t *testing.T) {
		spec := domains(257)
		_ = validatePolicySpec(spec)
		if len(spec.AllowedDomains) != 257 {
			t.Errorf("allowed_domains was mutated to %d entries — validation must refuse, not edit", len(spec.AllowedDomains))
		}
	})
}
