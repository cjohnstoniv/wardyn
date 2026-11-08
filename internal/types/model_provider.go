// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// ModelProviders is the org's model-provider CONFIGURATION: which credential kinds this deployment
// supports, where each sends requests, and which harnesses may use it. Configuration ONLY — no credential
// lives on a record, every person supplies their own. The zero value is load-bearing: with no block, run
// create and dispatch keep the existing lane-resolution path unchanged. Validation lives in internal/api.
type ModelProviders struct {
	Providers []ModelProvider `json:"providers,omitempty"`
}

// ModelProvidersRead is GET /api/v1/model-providers' body: the stored block, and ConnectedPeople —
// per provider ID, how many people hold a credential of their own for it (#970). Read-only: PUT's
// strict decode refuses the field, and the ETag covers the stored block alone, so a person
// connecting never stales an admin's edit. Admins only, as the route is: a member never reads this
// list. It carries counts and configuration, never a credential value.
//
// EntraLoginConfigured says whether this install has the console Entra login an azure_foundry
// provider signs people in through; KindsAvailable lists the kinds a write accepts now (azure_foundry
// only while the release has it switched on). Both are server facts, not stored and not in the ETag.
type ModelProvidersRead struct {
	ModelProviders
	ConnectedPeople      map[string]int      `json:"connected_people,omitempty"`
	EntraLoginConfigured bool                `json:"entra_login_configured"`
	KindsAvailable       []ModelProviderKind `json:"kinds_available"`
	// GatewayIdentityIssuer is the iss of every gateway token, and GatewayJWKSPath the key set's path under
	// this deployment's origin, base path included: what an admin tells a gateway.
	GatewayIdentityIssuer string `json:"gateway_identity_issuer,omitempty"`
	GatewayJWKSPath       string `json:"gateway_jwks_path"`
	// GatewayIdentityBedrockSSO says whether a bedrock_sso provider can send run identity on this install:
	// true only while it signs AWS requests at its proxy (WARDYN_AWS_SIGV4_AT_PROXY), which that needs.
	GatewayIdentityBedrockSSO bool `json:"gateway_identity_bedrock_sso"`
}

// Empty reports whether p carries no provider — the shape a PUT uses to CLEAR the block, normalized back
// to nil so a later GET doesn't render "model_providers":{} forever.
func (p *ModelProviders) Empty() bool {
	return p == nil || len(p.Providers) == 0
}

// ModelProviderSecretPrefix starts every per-person credential name: wardyn-provider-<uid>-{key,oauth,sso,entra,oidc}.
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
	// ModelProviderLocalModel: a model server the admin runs on a laptop host or an org node (Ollama,
	// llama.cpp server, LM Studio, vLLM), reached only through the run proxy's brokered route, which
	// holds each request to the provider's model allowlist (LocalModelSettings). It has no per-person
	// credential unless Auth is set; then each person's own token is sent the way Auth says.
	ModelProviderLocalModel ModelProviderKind = "local_model"
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
	ModelProviderAzureFoundry: true, ModelProviderLocalModel: true,
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
	// BaseURL: required for custom_endpoint and local_model, an optional gateway override for
	// Anthropic/OpenAI, never set on Bedrock (its address is Bedrock.BaseURL).
	BaseURL string              `json:"base_url,omitempty"`
	Auth    *ProviderAuth       `json:"auth,omitempty"`    // how each person's token is sent; custom_endpoint, or local_model when its server wants one
	Bedrock *BedrockSettings    `json:"bedrock,omitempty"` // region, address and sign-in setup for a Bedrock kind
	Azure   *AzureSettings      `json:"azure,omitempty"`   // endpoint and route for azure_foundry
	Local   *LocalModelSettings `json:"local,omitempty"`   // model allowlist and request bounds for local_model
	// OIDC makes each person sign in to the endpoint's own authority instead of pasting a token;
	// custom_endpoint only. Absent keeps the pasted token.
	OIDC *ProviderOIDC `json:"oidc,omitempty"`
	// ExtraHeaders are non-secret headers the run proxy sets on every request to this provider's gateway, after
	// the credential (custom_endpoint only; none by default). For a gateway that routes on a header, such as
	// Portkey's x-portkey-provider. The name and value rules refuse anything that could carry a credential or
	// change the request's framing; a change removes every person's stored credential, as a base_url change does.
	ExtraHeaders []ProviderHeader `json:"extra_headers,omitempty"`
	// GatewayIdentity turns on a signed token, naming the person, the run and the runner, on each model call
	// to this provider's gateway. Absent is off, and off changes nothing a run sends.
	GatewayIdentity *ModelProviderGatewayIdentity `json:"gateway_identity,omitempty"`
	// Harnesses are the harnesses this provider serves, with per-pairing settings.
	Harnesses []ProviderHarness `json:"harnesses,omitempty"`
}

// ProviderHeader is one extra header of a custom_endpoint provider.
type ProviderHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Serves reports whether harness is enabled for this provider; says nothing about Disabled.
func (p ModelProvider) Serves(harness string) bool {
	return slices.ContainsFunc(p.Harnesses, func(h ProviderHarness) bool { return h.Harness == harness })
}

// ModelProviderGatewayIdentity is a provider's opt-in to run identity. Audience is the token's aud: the value
// this provider's gateway looks for, and no other destination's. TokenLifetimeSeconds is how long each token
// works: absent (0) is GatewayTokenLifetimeMax, and a set value runs from GatewayTokenLifetimeMin to
// GatewayTokenLifetimeMax. It is not part of the route sealed into a run, so a change applies to the next token.
// Inspection is who checks the prompts sent to this gateway: "" is Wardyn, with the run's policy, and
// GatewayInspectionGateway leaves them to the gateway, except the exact match of the run's own secret
// values, which only Wardyn can run. It is part of the sealed route, so a change refuses the next token.
type ModelProviderGatewayIdentity struct {
	Audience             string `json:"audience"`
	TokenLifetimeSeconds int    `json:"token_lifetime_seconds,omitempty"`
	Inspection           string `json:"inspection,omitempty"`
}

// GatewayInspectionGateway is ModelProviderGatewayIdentity.Inspection when the gateway checks prompts itself.
const GatewayInspectionGateway = "gateway"

// The bounds of ModelProviderGatewayIdentity.TokenLifetimeSeconds.
const (
	GatewayTokenLifetimeMin = 60
	GatewayTokenLifetimeMax = 600
)

// The two ways a sign-in to a custom endpoint's authority is tied to the person who made it.
const (
	// OIDCBindingConsole requires the identity token's issuer to be the one the console signs people in
	// with, and the person it names to be the signed-in one. The default.
	OIDCBindingConsole = "console"
	// OIDCBindingNone compares nothing: the capture records the authority's issuer and subject and
	// stores the sign-in under whoever made it. An admin's choice, audited when set.
	OIDCBindingNone = "none"
)

// ProviderOIDC is a custom_endpoint's sign-in: where each person signs in and as which client. PKCE
// (S256), openid and offline_access are always on and are not settings.
type ProviderOIDC struct {
	// Issuer is required: discovery and the identity token's verification both come from it.
	Issuer string `json:"issuer"`
	// AuthorizeURL and TokenURL override what discovery names.
	AuthorizeURL string `json:"authorize_url,omitempty"`
	TokenURL     string `json:"token_url,omitempty"`
	ClientID     string `json:"client_id"`
	// ClientSecretRef names an operator secret holding the client's secret; empty is a public client.
	ClientSecretRef string `json:"client_secret_ref,omitempty"`
	// Scopes are asked for beside openid and offline_access.
	Scopes []string `json:"scopes"`
	// IdentityBinding is OIDCBindingConsole (also "") or OIDCBindingNone.
	IdentityBinding string `json:"identity_binding,omitempty"`
}

// Binding is IdentityBinding with the default filled in.
func (o *ProviderOIDC) Binding() string {
	if o == nil || o.IdentityBinding == "" {
		return OIDCBindingConsole
	}
	return o.IdentityBinding
}

// Unbound reports whether a person's sign-in on this row is stored without being tied to their console
// sign-in, which a Review line says as "not bound to your sign-in".
func (o *ProviderOIDC) Unbound() bool { return o != nil && o.Binding() == OIDCBindingNone }

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

// LocalModelSettings is a local_model provider's model allowlist and the bounds its proxy route puts
// on each request. A run on the provider reaches exactly these models; a zero bound is the default
// (LocalModelBounds).
type LocalModelSettings struct {
	Models           []string `json:"models"`
	MaxContext       int      `json:"max_context,omitempty"`        // cap on a request's options.num_ctx
	MaxOutput        int      `json:"max_output,omitempty"`         // cap on max_tokens and its per-API spellings
	KeepAliveSeconds int      `json:"keep_alive_seconds,omitempty"` // cap on keep_alive, so no run pins a model in memory
	// AllowOnLaptopRunners lets a run on a person's own runner use the server on that runner's own
	// computer (base_url on host.docker.internal), which the runner relays to its loopback at that
	// port alone. Admin-set; false (the default) refuses the run on every person's runner, as before.
	AllowOnLaptopRunners bool `json:"allow_on_laptop_runners,omitempty"`
}

// LaptopModelHost is the one base_url host an AllowOnLaptopRunners provider may name: the runner's own
// computer as its run's proxy reaches it.
const LaptopModelHost = "host.docker.internal"

// LaptopModelPort is the port of a laptop local model's base URL, whose host must be LaptopModelHost: the
// one port a person's runner relays to its own loopback for it, once that person allows it there.
func LaptopModelPort(raw string) (uint16, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, err
	}
	if strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")) != LaptopModelHost {
		return 0, errors.New("a laptop local model must be on " + LaptopModelHost)
	}
	port := map[string]uint64{"http": 80, "https": 443}[u.Scheme]
	if p := u.Port(); p != "" {
		if port, err = strconv.ParseUint(p, 10, 16); err != nil {
			return 0, errors.New("port " + strconv.Quote(p))
		}
	}
	if port == 0 {
		return 0, errors.New("no port")
	}
	return uint16(port), nil
}

// The default bounds: generous enough for an agent's long context and replies, small enough that one
// run cannot hold a model in memory indefinitely.
const (
	DefaultLocalMaxContext       = 32768
	DefaultLocalMaxOutput        = 32768
	DefaultLocalKeepAliveSeconds = 1800
)

// LocalModelBounds is s's bounds with each zero replaced by its default.
func (s LocalModelSettings) LocalModelBounds() LocalModelSettings {
	if s.MaxContext == 0 {
		s.MaxContext = DefaultLocalMaxContext
	}
	if s.MaxOutput == 0 {
		s.MaxOutput = DefaultLocalMaxOutput
	}
	if s.KeepAliveSeconds == 0 {
		s.KeepAliveSeconds = DefaultLocalKeepAliveSeconds
	}
	return s
}

// LocalModelNameRefused reports whether a model name is one no local_model run may name, listed or
// not: a name with a "/" (a registry host or namespace, which makes the model server fetch from a
// registry) or a cloud model (a ":cloud" or "-cloud" tag, which the server forwards off the machine).
func LocalModelNameRefused(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "/") || strings.HasSuffix(n, ":cloud") || strings.HasSuffix(n, "-cloud")
}

// ProviderHarness is one harness a provider is enabled for, and the settings for that pairing.
type ProviderHarness struct {
	Harness string `json:"harness"` // harness-catalog id ("claude-code", "codex-cli")
	// Model: model id this harness uses on this provider; admin-set, no member override. Required
	// (an inference profile) on a Bedrock kind.
	Model string `json:"model,omitempty"`
	// FastModel is the model the Messages harness uses for its small-model alias; azure_foundry and
	// custom_endpoint only, on that harness only. Unset means Model on azure_foundry, and no alias on custom_endpoint.
	FastModel string `json:"fast_model,omitempty"`
	Path      string `json:"path,omitempty"` // where a custom endpoint serves this harness's API dialect, under BaseURL; may not change the host
	// AuthHeader and AuthFormat override the provider's Auth for this harness alone (custom_endpoint only).
	AuthHeader string `json:"auth_header,omitempty"`
	AuthFormat string `json:"auth_format,omitempty"`
}
