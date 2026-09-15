// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"maps"
	"slices"
	"strings"
)

// AgentProviders is the org's AGENT-ENABLEMENT POLICY: which coding agents this
// deployment offers, how each one reaches its model, and whether that credential
// is one for everyone or one per person. It is a sub-object of the SiteConfig
// singleton, shaped byte-for-byte on WorkspaceProviders (see that type for the
// no-DDL / MDM-deliverable doctrine, which applies here word for word).
//
// It exists because availability was an IMAGE MAP with no auth semantics
// (WARDYN_AGENT_IMAGES is boot env; the harness catalog in internal/api/harness.go
// is static code expressing agent x provider POSSIBILITY, not admin choice), so
// "Claude Code is offered here, authenticated via AWS Bedrock SSO, one credential
// per user" was not a statement the product could hold. A customer's admin
// captured one AWS SSO session that silently backed every member's runs, and the
// member had no way to see whose credential was dying.
//
// THE ZERO VALUE IS LEGACY OPEN MODE, and that is load-bearing: with no block,
// every predicate over it answers exactly what it answered before the block
// existed — the image map, the operator credential and the whole
// precedence chain are byte-for-byte 0.7.1 (see the api package's
// agentProvidersConfigured / agentProviderFor).
//
// VALIDATION DELIBERATELY LIVES IN internal/api (validateAgentProviders): a row's
// id is admitted against the harness catalog OR the boot image map, and its
// mechanism against that catalog row's ProviderTypes — server state this package
// cannot see. This file is types and closed enums only.
type AgentProviders struct {
	// Agents are the per-agent rows. A row is the admin saying "this agent is
	// offered here, on this lane"; an agent with NO row is not offered at all
	// once the block exists.
	Agents []AgentProvider `json:"agents,omitempty"`
}

// Empty reports whether p carries no policy at all — the shape a caller PUTs to
// CLEAR the block, and what the write boundary normalizes back to nil so a later
// GET does not render "agent_providers":{} on every read forever
// (SiteConfig.AgentProviders is a pointer for the same reason).
func (p *AgentProviders) Empty() bool {
	return p == nil || len(p.Agents) == 0
}

// AgentMechanism is the ONE model-access lane an agent's runs may use. The set is
// drawn from the lanes that already exist at dispatch (internal/api's
// resolveLLMTransport): the three Anthropic/OpenAI ones, Bedrock's four sub-lanes,
// and none (BYOA — Wardyn wires no model credential).
//
// The four bedrock_* values FOLD to the single coarse "bedrock" the harness
// catalog's ProviderTypes map is keyed by; they are spelled out here because the
// sub-lane is what the admin chose and what a refusal must name — "Bedrock is
// configured" was exactly the sentence that told a customer nothing.
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
	// AgentMechanismBedrockSSO is a captured AWS SSO session — the ONE lane with
	// a per-principal capture path, so the only one CredentialSourcePerUser may
	// name.
	AgentMechanismBedrockSSO AgentMechanism = "bedrock_sso"
	// AgentMechanismBedrockEnv is the daemon's own AWS environment.
	AgentMechanismBedrockEnv AgentMechanism = "bedrock_env"
	// AgentMechanismBedrockAWSDir is the host ~/.aws mount (read-only by
	// contract, never refreshed by Wardyn).
	AgentMechanismBedrockAWSDir AgentMechanism = "bedrock_aws_dir"
	// AgentMechanismNone is BYOA: Wardyn wires no model credential and the image
	// authenticates however it likes. The only mechanism a non-catalog agent
	// (a WARDYN_AGENT_IMAGES key) may name, because no code path can honour
	// another one for it.
	AgentMechanismNone AgentMechanism = "none"
)

// bedrockMechanismPrefix is the fold: every mechanism under it is one sub-lane of
// the coarse "bedrock" provider type the harness catalog is keyed by.
const bedrockMechanismPrefix = "bedrock_"

// AgentProviderTypeBedrock is the COARSE provider type the four bedrock_* values
// fold to — the vocabulary of harnessDef.ProviderTypes and the Settings cards.
const AgentProviderTypeBedrock = "bedrock"

// ClosedAgentMechanisms is the closed mechanism set — the only values a write may
// name (validateAgentProviders refuses anything else), in the
// ClosedGitProviderKinds shape.
var ClosedAgentMechanisms = map[AgentMechanism]bool{
	AgentMechanismAnthropicSubscription: true, AgentMechanismAnthropicAPIKey: true,
	AgentMechanismOpenAIAPIKey: true, AgentMechanismBedrockBearer: true,
	AgentMechanismBedrockSSO: true, AgentMechanismBedrockEnv: true,
	AgentMechanismBedrockAWSDir: true, AgentMechanismNone: true,
}

// ClosedAgentMechanismList is ClosedAgentMechanisms in a stable order, for the
// "want one of: …" half of a rejected write's error.
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

// ProviderType folds m onto the COARSE ai-integration type the harness catalog's
// ProviderTypes map is keyed by: every bedrock_* sub-lane becomes "bedrock", and
// the three Anthropic/OpenAI names map 1:1 (they are already that vocabulary).
// AgentMechanismNone folds to itself and is never looked up in that map — it is
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
	// CredentialSourceShared is today: one credential, captured by an admin,
	// backs every run. The zero value, so an unset field is 0.7.1's behaviour.
	CredentialSourceShared CredentialSource = "shared"
	// CredentialSourcePerUser is one credential per principal, captured by that
	// person through the same device-code login sandbox an admin uses. Permitted
	// for AgentMechanismBedrockSSO only in 0.7.2 — it is the one mechanism with a
	// per-principal capture path.
	CredentialSourcePerUser CredentialSource = "per_user"
)

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
	// ID is the --agent value this row governs: a harness-catalog id
	// ("claude-code", "codex-cli", "none") OR a WARDYN_AGENT_IMAGES key.
	// The image-map half is deliberate — harnessByID documents an
	// image-map-only custom agent as supported, so a block that refuses agents
	// without a row must let the admin write a row for their own image.
	ID string `json:"id"`
	// Disabled turns the row off without deleting it — negative-sense like
	// GitProvider.Disabled, so the zero value is ENABLED. A disabled row is the
	// admin saying "off": its runs are refused and the console renders it
	// unavailable, never hidden.
	Disabled bool `json:"disabled,omitempty"`
	// Mechanism is the one lane this agent's runs may use. There is NO
	// cross-mechanism fallback once it is declared: a run whose lane is dead is
	// refused naming the lane and its state, never silently served by a
	// different vendor's credential (the failure that cost a customer a session).
	Mechanism AgentMechanism `json:"mechanism"`
	// CredentialSource is whose credential that lane uses. Empty reads as
	// CredentialSourceShared — today's behaviour.
	CredentialSource CredentialSource `json:"credential_source,omitempty"`
	// SSOStartURL is the AWS access portal every principal signs in against,
	// REQUIRED when Mechanism is bedrock_sso and CredentialSource is per_user and
	// forbidden otherwise. ADMIN-OWNED on purpose: a member's login launch
	// ignores the request's start URL and uses this one, so a member cannot bind
	// their capture to a foreign IdP/account, and an org URL leaves the member's
	// typing surface.
	SSOStartURL string `json:"sso_start_url,omitempty"`
	// SSOAccountID and SSORoleName pin WHICH AWS account and role a sign-in for
	// this row may capture. Optional, set together, permitted only where
	// SSOStartURL is (bedrock_sso + per_user). Admin-owned for the same reason
	// the start URL is: the sign-in proposes, the roster disposes.
	SSOAccountID string `json:"sso_account_id,omitempty"`
	SSORoleName  string `json:"sso_role_name,omitempty"`
}
