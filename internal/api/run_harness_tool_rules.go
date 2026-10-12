// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// toolgateCredentialEnv is the secret env variable that carries a harness's
// approval credential to its own wardyn-toolgate.
const toolgateCredentialEnv = "WARDYN_TOOLGATE_CREDENTIAL"

// applyHarnessToolRules lays the included harness tools' own rules onto the
// resolved spec (whose tool_rules already are the policy's met with the
// ceiling's by composer.Clamp). With one harness the run keeps the run-wide
// regime, exactly as before per-tool rules existed: the tool's rules and default
// effect are narrowed into spec.ToolRules and nothing per-harness is carried.
// With two or more, each harness gets a set of its own and the proxy tells them
// apart by credential. Either way a rule is compared with the tool's effective
// effect and applies only when stricter, so nothing a person writes loosens the
// policy's or the ceiling's.
func applyHarnessToolRules(spec types.RunPolicySpec, tools []client.IncludedTool) (types.RunPolicySpec, []types.HarnessToolRules) {
	var harnesses []client.IncludedTool
	for _, t := range tools {
		if t.Kind == client.IncludedToolHarness {
			harnesses = append(harnesses, t)
		}
	}
	switch {
	case len(harnesses) == 0:
		return spec, nil
	case len(harnesses) == 1:
		t := harnesses[0]
		if len(t.ToolRules) == 0 && t.DefaultEffect == "" {
			return spec, nil
		}
		spec = spec.Clone()
		for _, r := range t.ToolRules {
			spec.ToolRules, _, _ = narrowToolRule(spec.ToolRules, r)
		}
		if t.DefaultEffect != "" {
			spec.ToolRules, _, _ = narrowToolRule(spec.ToolRules, types.ToolRule{Tool: "*", Effect: t.DefaultEffect})
		}
		return spec, nil
	}
	if len(spec.ToolRules) == 0 && !slices.ContainsFunc(harnesses, func(t client.IncludedTool) bool {
		return len(t.ToolRules) > 0 || t.DefaultEffect != ""
	}) {
		return spec, nil
	}
	var out []types.HarnessToolRules
	for _, t := range harnesses {
		set := types.HarnessToolRules{Harness: t.ID}
		rules := slices.Clone(spec.ToolRules)
		for _, r := range t.ToolRules {
			rules, _, _ = narrowToolRule(rules, r)
		}
		if t.DefaultEffect != "" {
			rules, _, _ = narrowToolRule(rules, types.ToolRule{Tool: "*", Effect: t.DefaultEffect})
		}
		for _, r := range rules {
			if r.Tool == "*" {
				set.Default = r.Effect
				continue
			}
			set.Rules = append(set.Rules, r)
		}
		out = append(out, set)
	}
	return spec, out
}

// narrowToolRule adds nr to rules when it is stricter than the tool's effective
// effect there (its exact rule, else "*"): it replaces the exact rule, or is
// appended. replaced is the exact rule's old effect ("" if none); applied says
// whether anything changed.
func narrowToolRule(rules []types.ToolRule, nr types.ToolRule) (out []types.ToolRule, replaced types.ToolEffect, applied bool) {
	if effectRank(nr.Effect) <= effectRank(effectiveToolEffect(rules, nr.Tool)) {
		return rules, "", false
	}
	out = slices.Clone(rules)
	if i := slices.IndexFunc(out, func(r types.ToolRule) bool { return r.Tool == nr.Tool }); i >= 0 {
		replaced, out[i].Effect = out[i].Effect, nr.Effect
		return out, replaced, true
	}
	return append(out, nr), "", true
}

// effectRank orders no opinion < allow < hold < deny.
func effectRank(e types.ToolEffect) int {
	return map[types.ToolEffect]int{"": 0, types.ToolAllow: 1, types.ToolHold: 2, types.ToolDeny: 3}[e]
}

// effectiveToolEffect is what a rule list says about a tool: its exact rule,
// else the "*" default, else nothing. The lookup order of proxy.Policy.ToolEffectFor
// and composer.ceilingToolEffect.
func effectiveToolEffect(rules []types.ToolRule, tool string) types.ToolEffect {
	var def types.ToolEffect
	for _, r := range rules {
		if r.Tool == tool {
			return r.Effect
		}
		if r.Tool == "*" {
			def = r.Effect
		}
	}
	return def
}

// mintHarnessCredentials makes one random credential per harness that has rules.
// The proxy config gets the digests; the plaintext of the harness the run starts
// (agent) is returned for its own process, and the others' are dropped here: a
// harness that is only included holds no credential until it is started.
func mintHarnessCredentials(sets []types.HarnessToolRules, agent string) (digests []types.HarnessCredential, agentCredential string, err error) {
	for _, s := range sets {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, "", err
		}
		cred := base64.RawURLEncoding.EncodeToString(raw)
		sum := sha256.Sum256([]byte(cred))
		digests = append(digests, types.HarnessCredential{Harness: s.Harness, SHA256: hex.EncodeToString(sum[:])})
		if s.Harness == agent {
			agentCredential = cred
		}
	}
	return digests, agentCredential, nil
}

// stampHarnessCredential mints the harness credentials, puts the started harness's
// plaintext into env under toolgateCredentialEnv, and returns the digests for the
// proxy config and the secret-env keys with that variable added.
func stampHarnessCredential(sets []types.HarnessToolRules, agent string, env map[string]string, secretKeys []string) ([]types.HarnessCredential, []string, error) {
	digests, cred, err := mintHarnessCredentials(sets, agent)
	if err != nil || cred == "" {
		return digests, secretKeys, err
	}
	env[toolgateCredentialEnv] = cred
	return digests, append(slices.Clone(secretKeys), toolgateCredentialEnv), nil
}

// sidecarPolicy is the policy the proxy sidecar is given: the per-entry push rule
// sets and the per-harness tool rules replace the run-wide blocks (the sidecar
// refuses a config that carries both).
func sidecarPolicy(policy types.RunPolicySpec, p dispatchParams) types.RunPolicySpec {
	if len(p.PushRuleSets) > 0 {
		policy.PushRules = nil
	}
	if len(p.HarnessToolRules) > 0 {
		policy.ToolRules = nil
	}
	return policy
}
