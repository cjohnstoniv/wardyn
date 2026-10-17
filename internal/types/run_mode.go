// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"encoding/hex"
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

// A pool's run-type limit is RunnerPoolLimits.AllowsRunType, and it is called with
// the run's STORED experience, the value every interactive door reads
// (AgentRun.Experience, or CreateRunRequest.Experience exactly as it will be
// stored). Admission and the doors must see one value, so a request with no
// experience (an older client's shape) is stored as "" and never satisfies a
// limit that lists run types, however its legacy fields read. Never pass
// CreateRunRequest.EffectiveExperience: it infers a type the run will not be
// stored with. A place with no run-type limit does not call it, so an older
// client keeps today's behaviour there.

// BackgroundOnly is the one predicate every interactive door of a run calls
// (attach, interactive exec, SSH, the UI gateway, a desktop session, a restored
// session, and the same routes through the API and CLI). It reads the stored
// Experience, never the request and never the legacy Interactive flag.
//
// An empty Experience is a run launched by an older client or before 0.9: it
// keeps today's documented behaviour at every door, so this answers false. A new
// client always carries an experience, so its Background runs answer true.
func (r AgentRun) BackgroundOnly() bool { return r.Experience == ExperienceBackground }

// PushRuleSet is the push content rules of one SCM entry (provider host and
// organisation), wherever the entry came from: a repository or workspace, a
// tool or a component. It is the dispatch-plan carrier the proxy's git route
// reads: the set of the entry that owns the remote applies, and a remote whose
// entry has no set has no content rules. Host empty is the provider's public
// host (github.com, dev.azure.com); a GitHub Enterprise Server or Azure DevOps
// Server names its own. The run-wide RunPolicySpec.PushRules remains only as
// the older-client mapping, which the dispatch plan fans out to one set per entry
// the run brings.
//
// The sets are the product AFTER the ceiling: the dispatch plan lays the
// policy's and the governance ceiling's rules into each entry's set first, and a
// request's rules (RunOverrides.PushRules) may only ADD a deny or a
// require-review path to them, never remove or relax one. A person's rules
// therefore cannot displace an administrator's.
type PushRuleSet struct {
	Provider string `json:"provider"`
	Host     string `json:"host,omitempty"`
	Org      string `json:"org"`
	// BasePath is the path prefix of an Azure DevOps Server provider row (for
	// example "/tfs"), recorded from the row at dispatch. The git route derives
	// a remote's collection with it: without it a remote under a prefix is
	// ambiguous and its push is refused. Empty for every other entry.
	BasePath string        `json:"base_path,omitempty"`
	Rules    PushRulesSpec `json:"rules"`
}

// Key is the entry this set belongs to, "" when the set does not name one
// (ValidatePushRuleSets refuses such a set).
func (s PushRuleSet) Key() string {
	key, _ := PushRuleKey(s.Provider, s.Host, s.Org)
	return key
}

// PushRulesForRemote is the git route's lookup: the set of the entry that owns
// remote. found false means the entry has no content rules; it never falls back
// to another entry's. A remote whose entry cannot be derived is an error, and the
// caller refuses the push rather than treating it as having no rules.
func PushRulesForRemote(sets []PushRuleSet, provider, remote string) (spec PushRulesSpec, found bool, err error) {
	return PushRulesForRemoteAt(sets, provider, remote, "")
}

// PushRulesForRemoteAt is PushRulesForRemote for a provider row whose Azure DevOps
// Server lives under basePath (for example "/tfs").
func PushRulesForRemoteAt(sets []PushRuleSet, provider, remote, basePath string) (spec PushRulesSpec, found bool, err error) {
	host, org, err := PushRuleEntryOfRemoteAt(provider, remote, basePath)
	if err != nil {
		return PushRulesSpec{}, false, err
	}
	key, err := PushRuleKey(provider, host, org)
	if err != nil {
		return PushRulesSpec{}, false, err
	}
	for _, s := range sets {
		if s.Key() == key {
			return s.Rules, true, nil
		}
	}
	return PushRulesSpec{}, false, nil
}

// HarnessToolRules is the tool rules of one included harness and its own
// default effect (the "*" rule). The approval broker's lookup key is Harness:
// the id of the included tool that raised the request, which for a harness is
// its agent name.
//
// The harness id MUST come from an identity the proxy binds itself, never from
// the body of the approval request: every process in the sandbox can post to the
// proxy's local approvals route, so a harness read from the body lets one tool
// borrow a sibling's allow. The binding is a per-included-tool local credential
// minted at dispatch and injected only into that harness's own hook
// configuration (mintHarnessCredentials), so a second harness cannot use a
// sibling's allow.
//
// Like PushRuleSet, the sets are the product after the ceiling: policy and
// ceiling rules are laid into each harness's set first, and a request's per-tool
// rules may only narrow them (hold or deny; a rule that allows is refused), so
// an administrator's deny survives a person's allow.
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
		case s.Key() == "":
			_, err := PushRuleKey(s.Provider, s.Host, s.Org)
			return fmt.Errorf("push_rule_sets[%d]: %w", i, err)
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

// HarnessCredential binds one included harness to the SHA-256 of the random
// credential minted for it at dispatch. The proxy config carries only the digest;
// the plaintext goes to that harness's own process (WARDYN_TOOLGATE_CREDENTIAL)
// and nowhere else, so the approval route can name the harness from a value only
// that harness was given, never from the request body.
type HarnessCredential struct {
	Harness string `json:"harness"`
	// SHA256 is the lower-case hex digest of the credential.
	SHA256 string `json:"sha256"`
}

// ValidateHarnessCredentials is the consumer's structural check: a harness and a
// digest each once, and a digest that is 64 hex digits.
func ValidateHarnessCredentials(creds []HarnessCredential) error {
	harnesses, digests := map[string]bool{}, map[string]bool{}
	for i, c := range creds {
		d, err := hex.DecodeString(c.SHA256)
		switch {
		case c.Harness == "" || harnesses[c.Harness]:
			return fmt.Errorf("harness_credentials[%d]: name each harness once", i)
		case err != nil || len(d) != sha256.Size || c.SHA256 != strings.ToLower(c.SHA256):
			return fmt.Errorf("harness_credentials[%d]: sha256 is not a lower-case SHA-256 hex digest", i)
		case digests[c.SHA256]:
			return fmt.Errorf("harness_credentials[%d]: two harnesses share one credential", i)
		}
		harnesses[c.Harness], digests[c.SHA256] = true, true
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

// RunHarness is one harness a run was launched with and the model provider it
// was granted ("" for none). The rows are the run's authorisation record: a
// renew, revive or queued start re-checks the doors each one needed. Starts
// marks the one harness the sandbox starts; at most one row per run has it.
type RunHarness struct {
	Harness         string `json:"harness"`
	ModelProviderID string `json:"model_provider_id,omitempty"`
	Starts          bool   `json:"starts,omitempty"`
}
