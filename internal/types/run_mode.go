// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"
)

// RunExperience is the canonical run mode a person chose when the run was
// launched: a Background task or an Interactive environment (0.9 New Run).
type RunExperience string

const (
	// ExperienceBackground is a run with no interactive terminal, SSH,
	// interactive exec or attach, web application gateway or graphical desktop.
	// Logs, audit, status and the authorised lifecycle controls stay.
	ExperienceBackground RunExperience = "background"
	// ExperienceInteractive is general sandboxed work through whatever
	// interactive surfaces the placement actually supports.
	ExperienceInteractive RunExperience = "interactive"
)

// Valid reports whether e is one of the two modes. The empty value is not a mode.
func (e RunExperience) Valid() bool {
	return e == ExperienceBackground || e == ExperienceInteractive
}

// BackgroundOnly is the one predicate every interactive door of a run calls
// (attach, interactive exec, SSH, the UI gateway, a desktop session, a restored
// session, and the same routes through the API and CLI). It reads the stored
// Experience, never the request and never the legacy Interactive flag.
//
// An empty Experience is a run launched by an older client or before 0.9: it
// keeps today's documented behaviour at every door, so this answers false. A new
// client always carries an experience, so its Background runs answer true.
func (r AgentRun) BackgroundOnly() bool { return r.Experience == ExperienceBackground }

// PushRuleKey is the identity of an SCM entry for push rules: the provider and
// the organisation, the organisation folded to lower case because GitHub owners
// and Azure DevOps organisations are case-insensitive. Rules are carried per
// entry, so two spellings of one organisation must meet one key.
func PushRuleKey(provider, org string) string {
	return provider + "/" + strings.ToLower(org)
}

// PushRuleSet is the push content rules of one SCM entry (provider and
// organisation), wherever the entry came from: a repository or workspace, a
// tool or a component. It is the dispatch-plan carrier the proxy's git route
// reads: the set of the entry that owns the remote applies, and a remote whose
// entry has no set has no content rules. The run-wide RunPolicySpec.PushRules
// remains only as the older-client mapping, which the dispatch plan fans out to
// one set per entry the run brings.
type PushRuleSet struct {
	Provider string        `json:"provider"`
	Org      string        `json:"org"`
	Rules    PushRulesSpec `json:"rules"`
}

// Key is the entry this set belongs to.
func (s PushRuleSet) Key() string { return PushRuleKey(s.Provider, s.Org) }

// PushRulesForEntry finds the set of one SCM entry. Not found means the entry
// has no content rules; it never falls back to another entry's.
func PushRulesForEntry(sets []PushRuleSet, provider, org string) (PushRulesSpec, bool) {
	key := PushRuleKey(provider, org)
	for _, s := range sets {
		if s.Key() == key {
			return s.Rules, true
		}
	}
	return PushRulesSpec{}, false
}

// HarnessToolRules is the tool rules of one included harness and its own
// default effect (the "*" rule). The approval broker's lookup key is Harness:
// the id of the included tool that raised the request, which for a harness is
// its agent name.
type HarnessToolRules struct {
	Harness string `json:"harness"`
	// Default is the effect for a tool no rule names. Empty means the harness
	// has no default, and an unmatched tool is held for a human.
	Default ToolEffect `json:"default,omitempty"`
	Rules   []ToolRule `json:"rules,omitempty"`
}

// ToolEffectForHarness reports what the rules of ONE harness say about a tool
// call, and whether any rule decided it. Match order: the exact tool, then that
// harness's own default. Another harness's rules never apply, and a harness with
// no entry has no rules (ok=false), so its call is held for a human exactly as a
// run with no rules is today; it never inherits a sibling's allow.
func ToolEffectForHarness(sets []HarnessToolRules, harness, tool string) (ToolEffect, bool) {
	for _, s := range sets {
		if s.Harness != harness {
			continue
		}
		for _, r := range s.Rules {
			if r.Tool == tool {
				return r.Effect, true
			}
		}
		return s.Default, s.Default != ""
	}
	return "", false
}

// ValidatePushRuleSets is the structural check a consumer of the carrier makes
// before trusting it: a known provider, an organisation, one set per entry, and
// path patterns the broker can match. Numeric bounds are the authoring door's
// (validatePushRules in internal/api).
func ValidatePushRuleSets(sets []PushRuleSet) error {
	seen := map[string]bool{}
	for i, s := range sets {
		switch {
		case s.Provider != string(GitProviderGitHub) && s.Provider != string(GitProviderAzureDevOps):
			return fmt.Errorf("push_rule_sets[%d]: provider %q is not one of github, azure_devops", i, s.Provider)
		case strings.TrimSpace(s.Org) == "":
			return fmt.Errorf("push_rule_sets[%d]: org is required", i)
		case seen[s.Key()]:
			return fmt.Errorf("push_rule_sets[%d]: %s has two sets; rules are carried once per entry", i, s.Key())
		}
		seen[s.Key()] = true
		for _, p := range append(append([]string(nil), s.Rules.DenyPaths...), s.Rules.RequireReviewPaths...) {
			if _, err := DenyPathSegments(p); err != nil {
				return fmt.Errorf("push_rule_sets[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// ValidateHarnessToolRules is the structural check a consumer of the carrier
// makes: a harness once, a closed effect, no rule named "*" (the default has its
// own field) and no tool named twice.
func ValidateHarnessToolRules(sets []HarnessToolRules) error {
	seen := map[string]bool{}
	for i, s := range sets {
		if s.Harness == "" || seen[s.Harness] {
			return fmt.Errorf("harness_tool_rules[%d]: name each harness once", i)
		}
		seen[s.Harness] = true
		if s.Default != "" && !ValidToolEffect(s.Default) {
			return fmt.Errorf("harness_tool_rules[%d]: default %q is not allow, hold or deny", i, s.Default)
		}
		tools := map[string]bool{}
		for j, r := range s.Rules {
			switch {
			case r.Tool == "" || r.Tool == "*" || tools[r.Tool]:
				return fmt.Errorf("harness_tool_rules[%d].rules[%d]: name a tool once; the default is its own field", i, j)
			case !ValidToolEffect(r.Effect):
				return fmt.Errorf("harness_tool_rules[%d].rules[%d]: effect %q is not allow, hold or deny", i, j, r.Effect)
			}
			tools[r.Tool] = true
		}
	}
	return nil
}
