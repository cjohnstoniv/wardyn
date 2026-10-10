// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// agentComponentFact describes the admitted harness and the one chosen model
// lane. No credential value, provider id, access-portal URL or arbitrary
// dispatch settings cross this boundary. Exec runs have no agent process.
func agentComponentFact(f componentFactInputs) (componentFact, bool) {
	req := *f.req
	if req.TaskMode == "exec" {
		return componentFact{}, false
	}
	a := &client.AgentFact{Agent: req.Agent, Hosts: []client.AgentHostFact{}, Secrets: []client.AgentSecretFact{}}
	out := componentFact{Kind: types.ComponentAgent, ID: "agent:" + req.Agent, Reason: "agent", Status: componentUnknown, Requirements: []SetupItem{}, Agent: a}
	if f.mpChoice.chosen {
		p := f.mpChoice.provider
		a.ModelProvider = &client.AgentModelProviderFact{Name: p.Name, Kind: string(p.Kind)}
		addHost := func(host, role string) {
			if host != "" && !slices.Contains(a.Hosts, client.AgentHostFact{Host: host, Role: role}) {
				a.Hosts = append(a.Hosts, client.AgentHostFact{Host: host, Role: role})
			}
		}
		addHost(providerHost(p), "provider")
		if p.Kind.IsBedrock() {
			addHost(bedrockControlHost(providerBedrockSettings(p).Region), "provider")
		}
		addHost(f.mpChoice.loginHost, "login")
		kind := "key"
		switch p.Kind {
		case types.ModelProviderCustomEndpoint, types.ModelProviderAzureFoundry:
			kind = "token"
		case types.ModelProviderAnthropicSubscription:
			kind = "subscription"
		case types.ModelProviderBedrockSSO:
			kind = "aws"
		}
		a.Secrets = append(a.Secrets, client.AgentSecretFact{Kind: kind, Owner: "own", Residency: string(kindResidency(p.Kind))})
		if f.credentialChecked {
			out.Status = componentReady // the gate checked this own credential
		}
	} else if _, needsModel := agentLLMProvider(req.Agent); !needsModel {
		out.Status = componentReady
	}
	// Preview deliberately skips autonomy. Only an unbound run's zero level
	// is provable there; it must not publish a guessed managed document.
	resolved := f.autonomyResolved || (componentAutonomyCap(f.comps) == "" &&
		(f.ceiling.Profile == nil || f.ceiling.Limits.AutonomyRubric == nil))
	if resolved {
		locked := f.ceiling.Profile != nil && f.ceiling.Limits.AutonomyRubric != nil && f.ceiling.Limits.AutonomyRubric.AgentGuardrailLocks
		hold := holdLane(requestIsInteractive(req), req.ToolApprovals, req.TaskMode)
		if path, document, ok := agentpolicy.ForAgent(req.Agent, f.autonomy.Level, hold, locked); ok {
			a.ManagedSettings = &client.ManagedSettingsFact{Path: path, Document: string(document), Locked: agentpolicy.IsLocked(document)}
			if a.ManagedSettings.Locked {
				a.ManagedSettings.LockedBy, a.ManagedSettings.Profile = "profile", f.ceiling.Profile.Name
			}
		}
	}
	env := agentTelemetryEnv()
	a.Telemetry = &client.TelemetryFact{Off: len(env) != 0, Env: sortedKeys(env)}
	return out, true
}

// agentTelemetryEnv is shared with dispatch: facts report names of the exact
// telemetry suppression variables, never arbitrary environment values.
func agentTelemetryEnv() map[string]string {
	if envEnabled(envAllowAgentTelemetry) {
		return nil
	}
	return map[string]string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_TELEMETRY": "1"}
}
