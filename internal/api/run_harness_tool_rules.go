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

// harnessToolRulesFor is the run's tool rules per included harness, each the
// product after the ceiling: the resolved spec's rules (the policy's, met with the
// ceiling's by composer.Clamp) are laid into every harness's set first, and the
// tool's own rules and default effect may only narrow them, the stricter effect
// winning (the shape check has already refused an allow). A run with no harness
// tool, or with no rule anywhere, has none and stays on the older run-wide path.
func harnessToolRulesFor(spec types.RunPolicySpec, tools []client.IncludedTool) []types.HarnessToolRules {
	var harnesses []client.IncludedTool
	for _, t := range tools {
		if t.Kind == client.IncludedToolHarness {
			harnesses = append(harnesses, t)
		}
	}
	if len(harnesses) == 0 || len(spec.ToolRules) == 0 && !slices.ContainsFunc(harnesses, func(t client.IncludedTool) bool {
		return len(t.ToolRules) > 0 || t.DefaultEffect != ""
	}) {
		return nil
	}
	var out []types.HarnessToolRules
	for _, t := range harnesses {
		set := types.HarnessToolRules{Harness: t.ID}
		for _, r := range spec.ToolRules {
			if r.Tool == "*" {
				set.Default = stricterEffect(set.Default, r.Effect)
				continue
			}
			set.Rules = append(set.Rules, r)
		}
		set.Default = stricterEffect(set.Default, t.DefaultEffect)
		for _, r := range t.ToolRules {
			if i := slices.IndexFunc(set.Rules, func(o types.ToolRule) bool { return o.Tool == r.Tool }); i >= 0 {
				set.Rules[i].Effect = stricterEffect(set.Rules[i].Effect, r.Effect)
			} else {
				set.Rules = append(set.Rules, r)
			}
		}
		out = append(out, set)
	}
	return out
}

// stricterEffect orders allow < hold < deny; an empty effect is no opinion.
func stricterEffect(a, b types.ToolEffect) types.ToolEffect {
	rank := map[types.ToolEffect]int{"": 0, types.ToolAllow: 1, types.ToolHold: 2, types.ToolDeny: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
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
