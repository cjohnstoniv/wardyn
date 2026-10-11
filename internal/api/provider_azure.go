// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// provider_azure.go is the azure_foundry arm of provider dispatch: a run that chose such a provider
// reaches the row's endpoint on its OWNER's own Entra sign-in, redeemed by the control plane
// (injection_azure.go) and attached by the proxy, so no key or token is ever in the sandbox.
//
// What dispatch authors for the lane, and why each piece is its own producer:
//   - one injection grant for the endpoint host, pinned to the row's method-and-path set, whose
//     snapshot names whose sign-in it is, for which row and for which audience;
//   - the endpoint on the exact egress allowlist AND in ProxyConfig.MITMHosts: without the MITM entry
//     the CONNECT would be a blind tunnel and nothing could be gated or injected;
//   - ProxyConfig.AzureGates, the route gate that decides which calls and deployments may carry the
//     token (internal/egress/proxy/azure_gate.go), and ProxyConfig.LLMChannelHosts, so the content
//     scanner reads the host as the dialect it speaks.
//
// The endpoint is NEVER an LLMUpstreams entry. That map also feeds the proxy's gateway table, and a
// gateway is a place /wardyn/llm/* forwards to: Azure there would carry the person's token to a
// path and model nobody gated.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// azureProxyToken is what a harness holds in place of an Azure credential: inert, replaced by the proxy.
	azureProxyToken = "wardyn-proxy-injected"
	// azureHTTPSPort is the one port the endpoint is authored on.
	azureHTTPSPort = "443"
	// azureGrantTTLSeconds bounds the MINT, never the hold: a re-resolve redeems afresh from the stored sign-in.
	azureGrantTTLSeconds = 3600
)

// DRAFT (M2 canon pending). The states and remedies of the run refusal (mpRunRefusal) for a chosen
// azure_foundry provider.
const (
	mpAZNotSignedIn = "you are not signed in to Azure for it"
	mpAZSignInEnded = "your Azure sign-in has ended and needs a new one"
	mpAZNotCovered  = "your Azure sign-in does not cover this provider's audience"
	mpAZNoStore     = "this install has no secret store to hold your Azure sign-in"
	mpAZNotPerson   = "the admin token is a shared credential rather than a person, so it has no Azure sign-in of its own"
	mpAZReadFailed  = "Wardyn couldn't read your Azure sign-in for model provider %s just now — nothing was started. Try again in a moment."

	azureNoCertificateAuthority = "This run was not launched: its Azure credential needs a per-run certificate authority to be attached on the wire, " +
		"and none was provisioned. Without one the proxy tunnels the endpoint blind, so the credential and the route gate would both be skipped."
)

// azureGrantSnapshot is what dispatch records on the lane's grant: whose sign-in it is, for which row, and
// the audience that sign-in is redeemed for. It grants nothing: the sink requires the owner to be the run
// token's own subject and re-derives the row. The audience rides the snapshot, never the live row, so a row
// edit between dispatch and a refresh cannot change what a running sandbox's token can reach (the sink
// refuses on any difference instead). The first two keys are providerGrantSnapshot's, which revive and
// owner-authority read from every provider grant.
type azureGrantSnapshot struct {
	ProviderUID  string `json:"provider_uid"`
	OwnerSubject string `json:"owner_subject"`
	Audience     string `json:"audience"`
}

func (sn azureGrantSnapshot) authored() bool {
	return sn.ProviderUID != "" && sn.OwnerSubject != "" && azureAudienceClosed(sn.Audience)
}

// providerAzureLane is what an azure_foundry provider resolves to for one harness: the endpoint, the one
// audience its route names, and the deployments the route gate pins.
type providerAzureLane struct {
	provider types.ModelProvider
	owner    string // whose sign-in serves it: the run owner's own, always
	harness  string
	endpoint string
	host     string
	route    string
	audience string
	model    string
	fast     string // the Messages harness's small-model deployment, "" when unset
}

// providerAzureLaneFor derives the lane p serves agent on. ok=false when p does not serve agent, or the row
// cannot drive it (a route the harness's dialect cannot use, no deployment, an endpoint with no host).
func providerAzureLaneFor(p types.ModelProvider, agent string) (providerAzureLane, bool) {
	i := slices.IndexFunc(p.Harnesses, func(h types.ProviderHarness) bool { return h.Harness == agent })
	if i < 0 || p.Kind != types.ModelProviderAzureFoundry || p.Azure == nil {
		return providerAzureLane{}, false
	}
	h := p.Harnesses[i]
	audience, known := azureAudienceForRoute(p.Azure.Route)
	u, err := url.Parse(p.Azure.Endpoint)
	pair := (agent == azureMessagesHarness && p.Azure.Route == types.AzureRouteAnthropic) ||
		(agent == azureResponsesHarness && p.Azure.Route == types.AzureRouteOpenAIV1)
	if !known || !pair || err != nil || u.Hostname() == "" || h.Model == "" {
		return providerAzureLane{}, false
	}
	return providerAzureLane{
		provider: p, harness: agent, endpoint: strings.TrimSuffix(p.Azure.Endpoint, "/"), host: strings.ToLower(u.Hostname()),
		route: p.Azure.Route, audience: audience, model: h.Model, fast: h.FastModel,
	}, true
}

// models is the deployments the route gate pins for this run: the main one and the small-model one.
func (l providerAzureLane) models() []string {
	out := []string{l.model}
	if l.fast != "" && l.fast != l.model {
		out = append(out, l.fast)
	}
	return out
}

// env is the sandbox wiring. No key or token variable of any other lane is set: the Messages harness
// without a Foundry token variable would fall back to the Azure SDK's default credential chain, which fails
// closed in the sandbox, and it does NOT take the ANTHROPIC_API_KEY arm.
func (l providerAzureLane) env() map[string]string {
	if l.harness == azureResponsesHarness {
		return map[string]string{
			envCodexBaseURL: l.endpoint + "/openai/v1",
			envCodexModel:   l.model,
			envCodexAPIKey:  azureProxyToken,
		}
	}
	return map[string]string{
		envClaudeUseFoundry:   "1",
		envFoundryBaseURL:     l.endpoint + "/anthropic",
		envFoundryAuthToken:   azureProxyToken,
		envAnthropicModel:     l.model,
		envDefaultOpusModel:   l.model,
		envDefaultSonnetModel: l.model,
		envDefaultHaikuModel:  cmp.Or(l.fast, l.model),
	}
}

// providerAzureRefusal is the liveness check for a chosen azure_foundry provider (providerLiveness's arm,
// so every door's): the zero denial when owner's own sign-in can serve agent. A dry check, never a
// redemption: it spends no refresh token, and a sign-in that only a renewal would find dead surfaces at the
// sidecar's boot. An unreadable sign-in is an error, never "not signed in".
func (s *Server) providerAzureRefusal(ctx context.Context, p types.ModelProvider, agent, owner string) (providerDenial, error) {
	lane, ok := providerAzureLaneFor(p, agent)
	switch {
	case s.cfg.Secrets == nil:
		return stateDenial(p.ID, mpAZNoStore, mpRunRemedy), nil
	case !s.credentialPerson(owner):
		return stateDenial(p.ID, mpAZNotPerson, mpRunRemedyPerson), nil
	case !ok:
		return stateDenial(p.ID, fmt.Sprintf(mpRunStateNotServing, agent), mpRunRemedy), nil
	}
	ec, err := azureFoundryCapture(p.UID, lane.audience)
	if err != nil {
		return stateDenial(p.ID, fmt.Sprintf(mpRunStateNotServing, agent), mpRunRemedy), nil
	}
	blob, found, err := s.readEntraBlob(ctx, owner, ec)
	switch {
	case err != nil:
		return providerDenial{}, err
	case !found:
		return connectDenial(p.ID, mpAZNotSignedIn), nil
	case blob.signInEnded():
		return connectDenial(p.ID, mpAZSignInEnded), nil
	}
	if _, covered := ec.consentCovers(blob.Scopes, ec.scopes); !covered {
		return connectDenial(p.ID, mpAZNotCovered), nil
	}
	return providerDenial{}, nil
}

// providerAzureTransport wires the sandbox for the lane and marks the run as one the proxy must gate.
func (s *Server) providerAzureTransport(sandboxEnv map[string]string, c chosenProvider, l providerAzureLane) llmTransport {
	maps.Copy(sandboxEnv, l.env())
	return llmTransport{provider: &c, azure: &l}
}

// azureInject reports whether this run's model credential is a per-person Azure sign-in, injected
// proxy-side onto the endpoint host: the run then needs the per-run CA.
func (t llmTransport) azureInject() bool { return t.azure != nil }

// azurePlan is what the lane adds to one run's sidecar configuration.
type azurePlan struct {
	mitmHosts    []string
	gates        []proxy.AzureGateConfig
	channelHosts map[string]string
}

// authorAzureInjection authors the whole lane for one run. A run with no per-run certificate authority is
// REFUSED rather than downgraded, as authorADOEntraInjection does: handleConnect terminates a tunnel only
// when a CA exists, so authoring the host without one would leave a blind tunnel carrying the sandbox's
// own headers. ok=false means the run is already FAILED.
func (s *Server) authorAzureInjection(ctx context.Context, run types.AgentRun, t llmTransport, caCertPEM, caKeyPEM string,
	policy *types.RunPolicySpec, injections []runner.InjectionGrant,
) ([]runner.InjectionGrant, azurePlan, bool) {
	l := *t.azure
	refuse := func(reason, msg string) ([]runner.InjectionGrant, azurePlan, bool) {
		s.refuseProviderDispatch(ctx, run, l.provider.Kind, providerDenial{msg: msg}, map[string]any{"reason": reason})
		return injections, azurePlan{}, false
	}
	if caCertPEM == "" || caKeyPEM == "" {
		return refuse("no_run_certificate_authority", azureNoCertificateAuthority)
	}
	scope := map[string]any{
		"host": l.host, "header": "Authorization", "format": "Bearer %s",
		"secret_name": providerSecretName(l.provider.UID, providerEntraPart),
		// TLS only, and only the route's own calls: a request the pin does not name is forwarded without
		// the token on every door, and the proxy refuses a config whose rule is looser than its gate.
		"require_tls": true,
		"pin_routes":  proxy.AzureRoutePins(l.route),
		"snapshot":    azureGrantSnapshot{ProviderUID: l.provider.UID, OwnerSubject: l.owner, Audience: l.audience},
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return refuse("grant_scope", "could not author the Azure credential injection")
	}
	grantID := uuid.New()
	if _, err := s.createDispatchGrant(ctx, run, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: raw, TTLSeconds: azureGrantTTLSeconds},
	}, false); err != nil {
		return refuse("grant_write", "could not record the Azure credential grant: "+err.Error())
	}
	rule, err := injectionRuleFromScope(raw)
	if err != nil {
		return refuse("grant_scope", "could not compile the Azure credential injection: "+err.Error())
	}
	injections = append(injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
	// Port-qualified on both the allowlist and the MITM entry (a bare MITM entry means any port); the
	// injection rule above is bare, which is what the injector's exact-allowlist binding requires.
	entry := net.JoinHostPort(l.host, azureHTTPSPort)
	if !slices.Contains(policy.AllowedDomains, entry) {
		policy.AllowedDomains = append(policy.AllowedDomains, entry)
	}
	vendor := ""
	if conv, ok := agentLLMProvider(l.harness); ok {
		vendor = conv.host
	}
	return injections, azurePlan{
		mitmHosts:    []string{entry},
		gates:        []proxy.AzureGateConfig{{Host: l.host, Route: l.route, Models: l.models()}},
		channelHosts: map[string]string{l.host: vendor},
	}, true
}

// providerEntraUID returns the provider UID a wardyn-provider-<uid>-entra name names.
func providerEntraUID(name string) (string, bool) {
	uid, ok := strings.CutPrefix(name, providerSecretPrefix)
	if !ok {
		return "", false
	}
	uid, ok = strings.CutSuffix(uid, "-"+providerEntraPart)
	return uid, ok && uid != ""
}
