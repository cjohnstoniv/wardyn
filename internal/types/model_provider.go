// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"maps"
	"slices"
)

// ModelProviders is the org's model-provider CONFIGURATION: which kinds of model
// credential this deployment supports, where each one sends requests, and which
// harnesses may use it, with settings per harness. It is a sub-object of the
// SiteConfig singleton on the WorkspaceProviders/AgentProviders doctrine (no
// DDL, MDM-deliverable), and it is configuration ONLY: no credential lives on a
// record. Every person, admins included, supplies their own.
//
// THE ZERO VALUE IS TODAY, and that is load-bearing: with no block, run create
// and dispatch keep the existing lane-resolution path byte for byte, so an
// upgraded install behaves exactly as it did before the block existed.
//
// Validation lives in internal/api (validateModelProviders): which harness a
// kind can drive is the harness catalog's knowledge, which this package cannot
// see. This file is types and closed enums only.
type ModelProviders struct {
	Providers []ModelProvider `json:"providers,omitempty"`
}

// Empty reports whether p carries no provider at all — the shape a caller PUTs
// to CLEAR the block, which the write boundary normalizes back to nil so a later
// GET does not render "model_providers":{} forever.
func (p *ModelProviders) Empty() bool {
	return p == nil || len(p.Providers) == 0
}

// ModelProviderSecretPrefix starts every per-person model-provider credential
// name: wardyn-provider-<uid>-{key,oauth,sso}, stored in its owner's own
// namespace (internal/api owns the scheme).
const ModelProviderSecretPrefix = "wardyn-provider-"

// ModelProviderKind is what kind of credential each person brings to a
// provider, and so which dispatch lane serves it. A closed set.
type ModelProviderKind string

const (
	// ModelProviderAnthropicSubscription is each person's own Claude
	// subscription, captured by a container sign-in.
	ModelProviderAnthropicSubscription ModelProviderKind = "anthropic_subscription"
	// ModelProviderBedrockSSO is each person's own AWS sign-in, one click from
	// the admin's start URL and account/role pin.
	ModelProviderBedrockSSO ModelProviderKind = "bedrock_sso"
	// ModelProviderAnthropicAPIKey is each person's own Anthropic API key.
	ModelProviderAnthropicAPIKey ModelProviderKind = "anthropic_api_key"
	// ModelProviderOpenAIAPIKey is each person's own OpenAI API key.
	ModelProviderOpenAIAPIKey ModelProviderKind = "openai_api_key"
	// ModelProviderBedrockBearer is each person's own Bedrock API key.
	ModelProviderBedrockBearer ModelProviderKind = "bedrock_bearer"
	// ModelProviderCustomEndpoint is the admin's own endpoint, reached with each
	// person's own token or PAT, sent the way the provider's Auth says.
	ModelProviderCustomEndpoint ModelProviderKind = "custom_endpoint"
)

// ClosedModelProviderKinds is the closed kind set — the only kinds a write may
// name.
var ClosedModelProviderKinds = map[ModelProviderKind]bool{
	ModelProviderAnthropicSubscription: true, ModelProviderBedrockSSO: true,
	ModelProviderAnthropicAPIKey: true, ModelProviderOpenAIAPIKey: true,
	ModelProviderBedrockBearer: true, ModelProviderCustomEndpoint: true,
}

// ClosedModelProviderKindList is ClosedModelProviderKinds in a stable order, for
// the "want one of: …" half of a rejected write's error.
func ClosedModelProviderKindList() []string {
	ks := slices.Sorted(maps.Keys(ClosedModelProviderKinds))
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return out
}

// Valid reports whether k is one of the closed kinds.
func (k ModelProviderKind) Valid() bool { return ClosedModelProviderKinds[k] }

// IsBedrock reports whether k is one of the two Bedrock kinds, the ones whose
// address and sign-in setup live in ModelProvider.Bedrock.
func (k ModelProviderKind) IsBedrock() bool {
	return k == ModelProviderBedrockSSO || k == ModelProviderBedrockBearer
}

// ModelProvider is one admin-configured provider.
type ModelProvider struct {
	// ID is the admin's slug ("corp-gateway") — what the CLI, a workspace pin
	// and a run name. Unique within the block.
	ID string `json:"id"`
	// UID is SERVER-OWNED: minted on the provider's first write and carried by
	// ID after that, never reissued once the provider is deleted. Every person's
	// own credential for this provider is keyed by it, so a submitted value is
	// ignored rather than trusted.
	UID string `json:"uid,omitempty"`
	// Name is what people see when they choose it ("Corp gateway").
	Name string            `json:"name,omitempty"`
	Kind ModelProviderKind `json:"kind"`
	// Disabled turns the provider off without deleting it, negative-sense so the
	// zero value is ENABLED. A disabled provider may remain an agent's default:
	// that default is then a sentinel whose runs are refused, never a fall
	// through to another provider.
	Disabled bool `json:"disabled,omitempty"`
	// BaseURL is where requests go: required for custom_endpoint, an optional
	// route-through gateway for the Anthropic/OpenAI kinds, and never set on a
	// Bedrock kind (its address is Bedrock.BaseURL).
	BaseURL string `json:"base_url,omitempty"`
	// Auth is HOW each person's token is sent to a custom_endpoint. The only
	// kind it applies to.
	Auth *ProviderAuth `json:"auth,omitempty"`
	// Bedrock is the region, address and sign-in setup of a Bedrock kind.
	Bedrock *BedrockSettings `json:"bedrock,omitempty"`
	// Harnesses are the harnesses this provider may serve, with the settings for
	// each — the provider-to-harness join lives here, on the provider.
	Harnesses []ProviderHarness `json:"harnesses,omitempty"`
}

// Serves reports whether harness is one this provider is enabled for. It says
// nothing about Disabled: a disabled provider still names the harnesses it
// serves when it is switched back on.
func (p ModelProvider) Serves(harness string) bool {
	return slices.ContainsFunc(p.Harnesses, func(h ProviderHarness) bool { return h.Harness == harness })
}

// ProviderAuth is how a custom endpoint expects each person's token: the header
// it rides in and the value format ("Bearer %s").
type ProviderAuth struct {
	Header string `json:"header,omitempty"`
	Format string `json:"format,omitempty"`
}

// BedrockSettings is a Bedrock kind's region, optional data-plane address, and
// (bedrock_sso only) the org-level sign-in setup that makes each person's AWS
// sign-in one click. The start URL and the pin are ADMIN-OWNED: a sign-in
// proposes, the provider disposes.
type BedrockSettings struct {
	Region       string `json:"region,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	SSOStartURL  string `json:"sso_start_url,omitempty"`
	SSOAccountID string `json:"sso_account_id,omitempty"`
	SSORoleName  string `json:"sso_role_name,omitempty"`
}

// ProviderHarness is one harness a provider is enabled for, and the settings for
// that pairing.
type ProviderHarness struct {
	// Harness is a harness-catalog id ("claude-code", "codex-cli").
	Harness string `json:"harness"`
	// Model is the model id runs of this harness use on this provider; admin-set,
	// with no member override. Required on a Bedrock kind (an inference profile).
	Model string `json:"model,omitempty"`
	// Path is where a custom endpoint serves this harness's API dialect, under
	// BaseURL. It may not change the host.
	Path string `json:"path,omitempty"`
	// AuthHeader and AuthFormat override the provider's Auth for this harness
	// alone (custom_endpoint only).
	AuthHeader string `json:"auth_header,omitempty"`
	AuthFormat string `json:"auth_format,omitempty"`
}
