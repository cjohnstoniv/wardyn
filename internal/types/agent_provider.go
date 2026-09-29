// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"maps"
	"slices"
	"strings"
)

// AgentProviders is the org's AGENT-ENABLEMENT POLICY: which coding agents this
// deployment offers, how each reaches its model, and whether that credential
// is shared or per-person. A sub-object of SiteConfig, shaped like
// WorkspaceProviders (see that type for the no-DDL / MDM-deliverable doctrine).
//
// It exists because availability was an image map with no auth semantics: a
// customer's admin captured one AWS SSO session that silently backed every
// member's runs, with no way for a member to see whose credential was dying.
//
// THE ZERO VALUE IS LEGACY OPEN MODE: with no block, every predicate over it
// answers exactly what it answered before the block existed (see the api
// package's agentProvidersConfigured / agentProviderFor).
//
// VALIDATION LIVES IN internal/api (validateAgentProviders): a row's id and
// mechanism are checked against the harness catalog / boot image map —
// server state this package can't see. This file is types and closed enums
// only.
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

// AgentMechanism is the ONE model-access lane an agent's runs may use, drawn
// from the lanes dispatch already has: three Anthropic/OpenAI ones, Bedrock's
// four sub-lanes, and none (BYOA).
//
// The four bedrock_* values FOLD to the single coarse "bedrock" the harness
// catalog is keyed by; spelled out here because the sub-lane is what the
// admin chose and what a refusal must name — "Bedrock is configured" told a
// customer nothing.
type AgentMechanism string

const (
	// AgentMechanismAnthropicSubscription is the managed container-login
	// subscription token (setup-token), injected at the proxy.
	AgentMechanismAnthropicSubscription AgentMechanism = "anthropic_subscription"
	// AgentMechanismAnthropicAPIKey is the stored anthropic-api-key lane.
	AgentMechanismAnthropicAPIKey AgentMechanism = "anthropic_api_key"
	// AgentMechanismOpenAIAPIKey is the stored openai-api-key lane.
	AgentMechanismOpenAIAPIKey AgentMechanism = "openai_api_key"
	// AgentMechanismBedrockBearer is the stored bedrock-api-key bearer lane.
	AgentMechanismBedrockBearer AgentMechanism = "bedrock_bearer"
	// AgentMechanismBedrockSSO is a captured AWS SSO session — per-principal,
	// captured by signing in rather than stored. See PerUserMechanisms.
	AgentMechanismBedrockSSO AgentMechanism = "bedrock_sso"
	// AgentMechanismBedrockEnv is the daemon's own AWS environment.
	AgentMechanismBedrockEnv AgentMechanism = "bedrock_env"
	// AgentMechanismBedrockAWSDir is the host ~/.aws mount (read-only by
	// contract, never refreshed by Wardyn).
	AgentMechanismBedrockAWSDir AgentMechanism = "bedrock_aws_dir"
	// AgentMechanismNone is BYOA: Wardyn wires no credential. The only
	// mechanism a non-catalog agent (a WARDYN_AGENT_IMAGES key) may name.
	AgentMechanismNone AgentMechanism = "none"
)

// bedrockMechanismPrefix folds every mechanism under it into the coarse
// "bedrock" provider type the harness catalog is keyed by.
const bedrockMechanismPrefix = "bedrock_"

// AgentProviderTypeBedrock is the COARSE provider type the four bedrock_*
// values fold to — the vocabulary of harnessDef.ProviderTypes.
const AgentProviderTypeBedrock = "bedrock"

// ClosedAgentMechanisms is the closed mechanism set — the only values a write
// may name; validateAgentProviders refuses anything else.
var ClosedAgentMechanisms = map[AgentMechanism]bool{
	AgentMechanismAnthropicSubscription: true, AgentMechanismAnthropicAPIKey: true,
	AgentMechanismOpenAIAPIKey: true, AgentMechanismBedrockBearer: true,
	AgentMechanismBedrockSSO: true, AgentMechanismBedrockEnv: true,
	AgentMechanismBedrockAWSDir: true, AgentMechanismNone: true,
}

// ClosedAgentMechanismList is ClosedAgentMechanisms in a stable order, for a
// rejected write's "want one of: …".
func ClosedAgentMechanismList() []string {
	ms := slices.Sorted(maps.Keys(ClosedAgentMechanisms))
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = string(m)
	}
	return out
}

// Valid reports whether m is one of the closed mechanisms.
func (m AgentMechanism) Valid() bool { return ClosedAgentMechanisms[m] }

// ProviderType folds m onto the COARSE ai-integration type the harness
// catalog is keyed by: every bedrock_* sub-lane becomes "bedrock"; the three
// Anthropic/OpenAI names map 1:1. AgentMechanismNone folds to itself and is
// decided by the harness row's NoManagedAuth flag instead.
func (m AgentMechanism) ProviderType() string {
	if strings.HasPrefix(string(m), bedrockMechanismPrefix) {
		return AgentProviderTypeBedrock
	}
	return string(m)
}

// CredentialSource says WHOSE credential an agent's runs use.
type CredentialSource string

const (
	// CredentialSourceShared: one credential, captured by an admin, backs
	// every run. The zero value, so an unset field keeps legacy behaviour.
	CredentialSourceShared CredentialSource = "shared"
	// CredentialSourcePerUser: one credential per principal, captured or
	// stored under their own namespace. Permitted for PerUserMechanisms only.
	CredentialSourcePerUser CredentialSource = "per_user"
)

// PerUserMechanisms is the closed set of lanes a per_user row may name: the
// ones whose credential a member can actually hold as their OWN — the
// captured SSO session, or the bearer key written under their own principal.
// SECURITY: every other lane is an operator-namespace read, so declaring it
// per_user would promise one credential per person and silently serve the
// admin's instead — the substitution per_user exists to refuse.
var PerUserMechanisms = map[AgentMechanism]bool{
	AgentMechanismBedrockSSO: true, AgentMechanismBedrockBearer: true,
}

// PerUserMechanismList is PerUserMechanisms in a stable order — see
// ClosedAgentMechanismList.
func PerUserMechanismList() []string {
	ms := slices.Sorted(maps.Keys(PerUserMechanisms))
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = string(m)
	}
	return out
}

// ClosedCredentialSources is the closed source set — see ClosedAgentMechanisms.
var ClosedCredentialSources = map[CredentialSource]bool{
	CredentialSourceShared: true, CredentialSourcePerUser: true,
}

// ClosedCredentialSourceList is ClosedCredentialSources in a stable order, for a
// rejected write's "want one of: …".
func ClosedCredentialSourceList() []string {
	ss := slices.Sorted(maps.Keys(ClosedCredentialSources))
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

// Valid reports whether s is one of the two sources.
func (s CredentialSource) Valid() bool { return ClosedCredentialSources[s] }

// AgentProvider is one agent row: which agent, whether it is offered, the ONE
// lane its runs reach a model on, and whose credential that is.
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
	// Mechanism is the one lane this agent's runs may use. SECURITY: NO
	// cross-mechanism fallback once declared — a run whose lane is dead is
	// refused naming the lane and its state, never silently served by a
	// different vendor's credential.
	Mechanism AgentMechanism `json:"mechanism"`
	// CredentialSource is whose credential that lane uses. Empty reads as
	// CredentialSourceShared.
	CredentialSource CredentialSource `json:"credential_source,omitempty"`
	// SSOStartURL is the AWS access portal every principal signs in against,
	// REQUIRED for bedrock_sso + per_user and forbidden otherwise.
	// SECURITY, ADMIN-OWNED: a member's login launch ignores the request's
	// start URL and uses this one, so a member can't bind their capture to a
	// foreign IdP/account.
	SSOStartURL string `json:"sso_start_url,omitempty"`
	// SSOAccountID and SSORoleName pin WHICH AWS account and role a sign-in
	// for this row may capture. Optional, set together, permitted only
	// alongside SSOStartURL. Admin-owned for the same reason: the sign-in
	// proposes, the roster disposes.
	SSOAccountID string `json:"sso_account_id,omitempty"`
	SSORoleName  string `json:"sso_role_name,omitempty"`
	// DefaultProvider names the model provider a new run of this agent uses
	// unless the person chooses another; must be enabled for this agent. If
	// later disabled, the default becomes a sentinel whose runs are refused,
	// never silently moved to another provider. Empty today; unread by run
	// create/dispatch yet.
	DefaultProvider string `json:"default_provider,omitempty"`
}
