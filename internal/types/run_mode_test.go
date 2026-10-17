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

func TestPushRulesAreKeyedByProviderHostAndCanonicalOrganisation(t *testing.T) {
	sets := []PushRuleSet{
		{Provider: "github", Org: "Acme", Rules: PushRulesSpec{DenyPaths: []string{"infra/"}}},
		{Provider: "azure_devops", Org: "acme", Rules: PushRulesSpec{RequireReviewPaths: []string{"ci/"}}},
		{Provider: "github", Host: "ghes.corp.example", Org: "acme", Rules: PushRulesSpec{DenyPaths: []string{"secrets/"}}},
	}
	for _, tc := range []struct {
		name, provider, remote string
		deny, review           string
	}{
		{"github.com/acme", "github", "https://github.com/ACME/x.git", "infra/", ""},
		{"azure devops acme", "azure_devops", "https://dev.azure.com/Acme/p/_git/r", "", "ci/"},
		{"a server's acme is its own entry", "github", "https://ghes.corp.example/acme/x", "secrets/", ""},
	} {
		got, ok, err := PushRulesForRemote(sets, tc.provider, tc.remote)
		if err != nil || !ok || (tc.deny != "" && got.DenyPaths[0] != tc.deny) || (tc.review != "" && got.RequireReviewPaths[0] != tc.review) {
			t.Errorf("%s = %+v %v %v", tc.name, got, ok, err)
		}
	}
	// An entry with no set has no rules: it never borrows another entry's.
	for _, remote := range []string{"https://github.com/other/x", "https://ghes.other.example/acme/x"} {
		if _, ok, err := PushRulesForRemote(sets, "github", remote); ok || err != nil {
			t.Errorf("%s borrowed a set (found %v, err %v)", remote, ok, err)
		}
	}
	if _, ok, _ := PushRulesForRemote(sets, "azure_devops", "https://dev.azure.com/other/p/_git/r"); ok {
		t.Error("another organisation of the other provider borrowed a set")
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
		"trailing space":      ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "acme "}}),
		"trailing dot":        ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "acme."}}),
		"zero-width":          ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "acme\u200b"}}),
		"non-ASCII":           ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "\u0130cme"}}),
		"a host with a port":  ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Host: "ghes.example:8443", Org: "acme"}}),
		"one entry, two sets": ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "Acme"}, {Provider: "github", Host: "GitHub.com", Org: "acme"}}),
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
	if !strings.Contains(ValidatePushRuleSets([]PushRuleSet{{Provider: "github", Org: "a"}, {Provider: "github", Org: "A"}}).Error(), "github.com/a") {
		t.Error("the duplicate names its entry")
	}
}

// TestPoolRunTypeLimitReadsTheStoredExperience pins how a pool's run-type limit
// meets the stored experience: both types, one type, and failing closed for a
// run an older client launched (stored with none).
func TestPoolRunTypeLimitReadsTheStoredExperience(t *testing.T) {
	both := RunnerPoolLimits{Background: &RunnerPoolBackgroundLimits{}, Interactive: &RunnerPoolInteractiveLimits{}}
	backgroundOnly := RunnerPoolLimits{Background: &RunnerPoolBackgroundLimits{}}
	interactiveOnly := RunnerPoolLimits{Interactive: &RunnerPoolInteractiveLimits{}}
	for _, tc := range []struct {
		name   string
		limits RunnerPoolLimits
		stored RunExperience
		want   bool
	}{
		{"both allow background", both, ExperienceBackground, true},
		{"both allow interactive", both, ExperienceInteractive, true},
		{"background only refuses interactive", backgroundOnly, ExperienceInteractive, false},
		{"background only allows background", backgroundOnly, ExperienceBackground, true},
		{"interactive only refuses background", interactiveOnly, ExperienceBackground, false},
		{"a run stored with none is never allowed", both, "", false},
		{"nor background only", backgroundOnly, "", false},
		{"an unknown type is never allowed", both, "batch", false},
	} {
		if got := tc.limits.AllowsRunType(RunnerPoolRunType(tc.stored)); got != tc.want {
			t.Errorf("%s: = %v, want %v", tc.name, got, tc.want)
		}
	}
}
