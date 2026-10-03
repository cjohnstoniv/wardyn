// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"maps"
	"slices"
)

// ModelProviders is the org's model-provider CONFIGURATION: which credential kinds this deployment
// supports, where each sends requests, and which harnesses may use it. Configuration ONLY — no credential
// lives on a record, every person supplies their own. The zero value is load-bearing: with no block, run
// create and dispatch keep the existing lane-resolution path unchanged. Validation lives in internal/api.
type ModelProviders struct {
	Providers []ModelProvider `json:"providers,omitempty"`
}

// Empty reports whether p carries no provider — the shape a PUT uses to CLEAR the block, normalized back
// to nil so a later GET doesn't render "model_providers":{} forever.
func (p *ModelProviders) Empty() bool {
	return p == nil || len(p.Providers) == 0
}

// ModelProviderSecretPrefix starts every per-person credential name: wardyn-provider-<uid>-{key,oauth,sso,entra}.
const ModelProviderSecretPrefix = "wardyn-provider-"

// ModelProviderKind is what kind of credential each person brings to a provider, and so which dispatch
// lane serves it. A closed set.
type ModelProviderKind string

const (
	ModelProviderAnthropicSubscription ModelProviderKind = "anthropic_subscription" // own Claude subscription, via container sign-in
	ModelProviderBedrockSSO            ModelProviderKind = "bedrock_sso"            // own AWS sign-in via admin's start URL + account/role pin
	ModelProviderAnthropicAPIKey       ModelProviderKind = "anthropic_api_key"      // own Anthropic API key
	ModelProviderOpenAIAPIKey          ModelProviderKind = "openai_api_key"         // own OpenAI API key
	ModelProviderBedrockBearer         ModelProviderKind = "bedrock_bearer"         // own Bedrock API key
	// ModelProviderCustomEndpoint: admin's own endpoint, reached with each person's own token/PAT.
	ModelProviderCustomEndpoint ModelProviderKind = "custom_endpoint"
	// ModelProviderAzureFoundry: each person's own Entra sign-in, captured for one Azure resource audience.
	// Writable only while validateModelProviders' activation flag (azureFoundryGateReady) is true.
	ModelProviderAzureFoundry ModelProviderKind = "azure_foundry"
)

// The two inference routes an azure_foundry row serves. One row serves one route, and a route names the one
// Entra audience its sign-in is captured for.
const (
	AzureRouteAnthropic = "anthropic"
	AzureRouteOpenAIV1  = "openai_v1"
)

// ClosedModelProviderKinds is the only kind set a write may name.
var ClosedModelProviderKinds = map[ModelProviderKind]bool{
	ModelProviderAnthropicSubscription: true, ModelProviderBedrockSSO: true,
	ModelProviderAnthropicAPIKey: true, ModelProviderOpenAIAPIKey: true,
	ModelProviderBedrockBearer: true, ModelProviderCustomEndpoint: true,
	ModelProviderAzureFoundry: true,
}

// ClosedModelProviderKindList is ClosedModelProviderKinds in stable order, for a rejected write's error.
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

// IsBedrock reports whether k is one of the two Bedrock kinds (address/sign-in live in ModelProvider.Bedrock).
func (k ModelProviderKind) IsBedrock() bool {
	return k == ModelProviderBedrockSSO || k == ModelProviderBedrockBearer
}

// ModelProvider is one admin-configured provider.
type ModelProvider struct {
	ID string `json:"id"` // admin's slug ("corp-gateway"); unique within the block
	// UID is SERVER-OWNED: minted on first write, never reissued once deleted. Every person's credential is
	// keyed by it, so a submitted value is ignored rather than trusted.
	UID  string            `json:"uid,omitempty"`
	Name string            `json:"name,omitempty"` // shown when people choose it ("Corp gateway")
	Kind ModelProviderKind `json:"kind"`
	// Disabled is negative-sense so the zero value is ENABLED. A disabled provider may remain an agent's
	// default: that default becomes a refused sentinel, never a fall-through to another provider.
	Disabled bool `json:"disabled,omitempty"`
	// BaseURL: required for custom_endpoint, an optional gateway override for Anthropic/OpenAI, never set
	// on Bedrock (its address is Bedrock.BaseURL).
	BaseURL string           `json:"base_url,omitempty"`
	Auth    *ProviderAuth    `json:"auth,omitempty"`    // how each person's token is sent; custom_endpoint only
	Bedrock *BedrockSettings `json:"bedrock,omitempty"` // region, address and sign-in setup for a Bedrock kind
	Azure   *AzureSettings   `json:"azure,omitempty"`   // endpoint and route for azure_foundry
	// Harnesses are the harnesses this provider serves, with per-pairing settings.
	Harnesses []ProviderHarness `json:"harnesses,omitempty"`
}

// Serves reports whether harness is enabled for this provider; says nothing about Disabled.
func (p ModelProvider) Serves(harness string) bool {
	return slices.ContainsFunc(p.Harnesses, func(h ProviderHarness) bool { return h.Harness == harness })
}

// ProviderAuth is how a custom endpoint expects each person's token: header and value format ("Bearer %s").
type ProviderAuth struct {
	Header string `json:"header,omitempty"`
	Format string `json:"format,omitempty"`
}

// BedrockSettings is a Bedrock kind's region, optional data-plane address, and (bedrock_sso only) the
// org-level sign-in setup. The start URL and pin are ADMIN-OWNED: a sign-in proposes, the provider disposes.
type BedrockSettings struct {
	Region       string `json:"region,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	SSOStartURL  string `json:"sso_start_url,omitempty"`
	SSOAccountID string `json:"sso_account_id,omitempty"`
	SSORoleName  string `json:"sso_role_name,omitempty"`
}

// AzureSettings is an azure_foundry row's data-plane endpoint and the one route it serves (AzureRoute*).
type AzureSettings struct {
	Endpoint string `json:"endpoint,omitempty"`
	Route    string `json:"route,omitempty"`
}

// ProviderHarness is one harness a provider is enabled for, and the settings for that pairing.
type ProviderHarness struct {
	Harness string `json:"harness"` // harness-catalog id ("claude-code", "codex-cli")
	// Model: model id this harness uses on this provider; admin-set, no member override. Required
	// (an inference profile) on a Bedrock kind.
	Model string `json:"model,omitempty"`
	// FastModel is the deployment the Messages harness uses for its small-model alias; azure_foundry only,
	// on that harness only. Unset means Model.
	FastModel string `json:"fast_model,omitempty"`
	Path      string `json:"path,omitempty"` // where a custom endpoint serves this harness's API dialect, under BaseURL; may not change the host
	// AuthHeader and AuthFormat override the provider's Auth for this harness alone (custom_endpoint only).
	AuthHeader string `json:"auth_header,omitempty"`
	AuthFormat string `json:"auth_format,omitempty"`
}
