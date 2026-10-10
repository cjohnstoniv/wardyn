// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

func TestBackgroundOnlyReadsTheStoredExperience(t *testing.T) {
	for _, tc := range []struct {
		run  AgentRun
		want bool
	}{
		{AgentRun{}, false},
		{AgentRun{Interactive: false}, false},
		{AgentRun{Experience: ExperienceInteractive}, false},
		{AgentRun{Experience: ExperienceBackground}, true},
		{AgentRun{Experience: ExperienceBackground, Interactive: true}, true},
	} {
		if got := tc.run.BackgroundOnly(); got != tc.want {
			t.Errorf("%+v = %v, want %v", tc.run, got, tc.want)
		}
	}
	if ExperienceBackground.Valid() != true || RunExperience("").Valid() || RunExperience("batch").Valid() {
		t.Error("the mode set is exactly background and interactive")
	}
}

func TestPushRulesAreKeyedByProviderAndFoldedOrganisation(t *testing.T) {
	sets := []PushRuleSet{
		{Provider: "github", Org: "Acme", Rules: PushRulesSpec{DenyPaths: []string{"infra/"}}},
		{Provider: "azure_devops", Org: "acme", Rules: PushRulesSpec{RequireReviewPaths: []string{"ci/"}}},
	}
	if got, ok := PushRulesForEntry(sets, "github", "acme"); !ok || got.DenyPaths[0] != "infra/" {
		t.Errorf("github/acme = %+v %v", got, ok)
	}
	if got, ok := PushRulesForEntry(sets, "azure_devops", "ACME"); !ok || got.RequireReviewPaths[0] != "ci/" {
		t.Errorf("azure_devops/ACME = %+v %v", got, ok)
	}
	// An entry with no set has no rules: it never borrows another entry's.
	if _, ok := PushRulesForEntry(sets, "github", "other"); ok {
		t.Error("another organisation borrowed a set")
	}
	if _, ok := PushRulesForEntry(sets, "azure_devops", "other"); ok {
		t.Error("another organisation of the other provider borrowed a set")
	}
	if PushRuleKey("github", "Acme") != "github/acme" {
		t.Error("the key folds the organisation")
	}
}

func TestToolRulesAreEvaluatedPerHarness(t *testing.T) {
	sets := []HarnessToolRules{
		{Harness: "claude-code", Default: ToolAllow, Rules: []ToolRule{{Tool: "Bash", Effect: ToolHold}}},
		{Harness: "codex-cli", Default: ToolDeny},
		{Harness: "custom", Rules: []ToolRule{{Tool: "Read", Effect: ToolAllow}}},
	}
	for _, tc := range []struct {
		harness, tool string
		want          ToolEffect
		ok            bool
	}{
		{"claude-code", "Bash", ToolHold, true},
		{"claude-code", "Read", ToolAllow, true},
		{"codex-cli", "Bash", ToolDeny, true},
		{"codex-cli", "Read", ToolDeny, true},
		{"custom", "Read", ToolAllow, true},
		// No rule and no default: held for a human, never another harness's allow.
		{"custom", "Bash", "", false},
		// A harness with no entry has no rules at all.
		{"unlisted", "Read", "", false},
	} {
		got, ok := ToolEffectForHarness(sets, tc.harness, tc.tool)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s %s = %q %v, want %q %v", tc.harness, tc.tool, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDispatchCarrierValidators(t *testing.T) {
	for name, err := range map[string]error{
		"unknown provider":    ValidatePushRuleSets([]PushRuleSet{{Provider: "gitlab", Org: "a"}}),
		"no organisation":     ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: " "}}),
		"one entry, two sets": ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "Acme"}, {Provider: "github", Org: "acme"}}),
		"bad pattern":         ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "a", Rules: PushRulesSpec{DenyPaths: []string{"a//b"}}}}),
		"no harness":          ValidateHarnessToolRules([]HarnessToolRules{{}}),
		"harness twice":       ValidateHarnessToolRules([]HarnessToolRules{{Harness: "a"}, {Harness: "a"}}),
		"unknown default":     ValidateHarnessToolRules([]HarnessToolRules{{Harness: "a", Default: "maybe"}}),
		"rule named *":        ValidateHarnessToolRules([]HarnessToolRules{{Harness: "a", Rules: []ToolRule{{Tool: "*", Effect: ToolAllow}}}}),
		"tool twice":          ValidateHarnessToolRules([]HarnessToolRules{{Harness: "a", Rules: []ToolRule{{Tool: "Bash", Effect: ToolAllow}, {Tool: "Bash", Effect: ToolDeny}}}}),
		"unknown effect":      ValidateHarnessToolRules([]HarnessToolRules{{Harness: "a", Rules: []ToolRule{{Tool: "Bash", Effect: "maybe"}}}}),
	} {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "a", Rules: PushRulesSpec{DenyPaths: []string{"infra/"}}}, {Provider: "azure_devops", Org: "a"}}); err != nil {
		t.Errorf("two entries of one organisation name: %v", err)
	}
	if err := ValidateHarnessToolRules([]HarnessToolRules{{Harness: "a", Default: ToolHold, Rules: []ToolRule{{Tool: "Bash", Effect: ToolDeny}}}, {Harness: "b"}}); err != nil {
		t.Errorf("valid sets: %v", err)
	}
	if !strings.Contains(ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "a"}, {Provider: "github", Org: "A"}}).Error(), "github/a") {
		t.Error("the duplicate names its entry")
	}
}
