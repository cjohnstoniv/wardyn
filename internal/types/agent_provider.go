// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// AgentProviders is the org's AGENT-ENABLEMENT POLICY: which coding agents this
// deployment offers and the model provider each uses by default. A sub-object
// of SiteConfig, shaped like WorkspaceProviders (see that type for the no-DDL /
// MDM-deliverable doctrine).
//
// It exists because availability was an image map with no auth semantics: a
// customer's admin captured one AWS SSO session that silently backed every
// member's runs, with no way for a member to see whose credential was dying.
//
// THE ZERO VALUE IS LEGACY OPEN MODE: with no block, every predicate over it
// answers exactly what it answered before the block existed (see the api
// package's agentProvidersConfigured / agentProviderFor).
//
// VALIDATION LIVES IN internal/api (validateAgentProviders): a row's id is
// checked against the harness catalog / boot image map, and its default
// against the model providers — server state this package can't see. This file
// is types and closed enums only.
type AgentProviders struct {
	// Agents are the per-agent rows; an agent with NO row is not offered at
	// all once the block exists.
	Agents []AgentProvider `json:"agents,omitempty"`
}

// Empty reports whether p carries no policy — the shape a PUT uses to CLEAR
// the block. The write boundary normalizes this back to nil so a later GET
// doesn't render "agent_providers":{} forever.
func (p *AgentProviders) Empty() bool {
	return p == nil || len(p.Agents) == 0
}

// AgentProvider is one agent row: which agent, whether it is offered, and the
// model provider its runs use unless the person chooses another. How a run
// reaches its model, and whose credential that is, live on the model provider
// (ModelProvider); every credential is each person's own.
type AgentProvider struct {
	// ID is the --agent value this row governs: a harness-catalog id OR a
	// WARDYN_AGENT_IMAGES key. The image-map half is deliberate: harnessByID
	// documents an image-map-only custom agent as supported, so a block that
	// refuses agents without a row must let the admin write one for their own.
	ID string `json:"id"`
	// Disabled turns the row off without deleting it — negative-sense, so the
	// zero value is ENABLED. Its runs are refused and the console renders it
	// unavailable, never hidden.
	Disabled bool `json:"disabled,omitempty"`
	// DefaultProvider names the model provider a new run of this agent uses
	// unless the person chooses another; must be enabled for this agent. If
	// later disabled, the default becomes a sentinel whose runs are refused,
	// never silently moved to another provider.
	DefaultProvider string `json:"default_provider,omitempty"`
}
