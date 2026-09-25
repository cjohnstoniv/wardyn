// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The Bedrock arms of provider dispatch (MP-9): a run that chose a bedrock_sso
// or bedrock_bearer model provider reaches Bedrock with the region, model and
// base URL the provider record names, on its OWNER's own credential — their
// AWS sign-in (wardyn-provider-<uid>-sso) or their own Bedrock API key
// (wardyn-provider-<uid>-key), read strictly from their namespace. The kind
// names the one lane: resolveBedrockAuth's chain, and its host ~/.aws mount
// and static SigV4 arms (the operator's credentials), are never reached.
package api

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending). The states and remedies of the run refusal
// (mpRunRefusal) for a chosen Bedrock provider.
const (
	mpBRNotSignedIn = "you are not signed in to AWS for it"
	mpBRNoStore     = "this install has no secret store to hold your AWS credential"
	mpBRNotPerson   = "the admin token is a shared credential rather than a person, so it has no AWS credential of its own"
	mpBRUnset       = "it has no region or model set for %s"
	mpBRPinned      = "your AWS sign-in is for account %s and role %s, and it pins account %s and role %s"
	mpBRPortal      = "your AWS sign-in is for a different AWS access portal than the one it names"
	mpBRRenewing    = "renewing your AWS sign-in did not complete, because AWS did not answer the token request"
	mpBRRemedyRetry = "your sign-in is still good; launch again in a moment."
	mpBRReadFailed  = "Wardyn couldn't read your AWS credential for model provider %s just now — nothing was started. Try again in a moment."
)

// awsScope is the credential scope of a chosen Bedrock provider: always the
// run owner's own namespace, under the provider's UID-keyed names.
func (c chosenProvider) awsScope() awsSSOScope {
	return awsSSOScope{perUser: true, owner: c.owner, bearer: c.provider.Kind == types.ModelProviderBedrockBearer, provider: c.provider.UID}
}

// providerAWSScope is the chosen Bedrock provider's credential scope — what
// its session grant records and its reauth hold names — zero on a run that
// chose none or chose another kind.
func (t llmTransport) providerAWSScope() awsSSOScope {
	if t.provider == nil || !t.provider.provider.Kind.IsBedrock() {
		return awsSSOScope{}
	}
	return t.provider.awsScope()
}

// providerModel is the model p names for agent ("" when it names none).
func providerModel(p types.ModelProvider, agent string) string {
	for _, h := range p.Harnesses {
		if h.Harness == agent {
			return h.Model
		}
	}
	return ""
}

// providerBedrockSettings is p's Bedrock block, zero when it has none.
func providerBedrockSettings(p types.ModelProvider) types.BedrockSettings {
	if p.Bedrock == nil {
		return types.BedrockSettings{}
	}
	return *p.Bedrock
}

// providerBedrockRuntimeHost is where p's Bedrock requests go: its base URL's
// host, else the regional data-plane host. The one host its key may reach.
func providerBedrockRuntimeHost(p types.ModelProvider) string {
	b := providerBedrockSettings(p)
	return bedrockDataPlaneHostFor(b.Region, b.BaseURL)
}

// sameStartURL compares two AWS access-portal URLs the way a person reads
// them: case and a trailing slash aside.
func sameStartURL(a, b string) bool {
	norm := func(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }
	return strings.EqualFold(norm(a), norm(b))
}

// providerBedrockRefusal is the liveness check for a chosen Bedrock provider
// (providerLiveness's arm, so every door's): the zero denial when owner may
// run on p. On the SSO kind it returns the owner's session, renewed first when
// refresh allows (dispatch alone: a dry run never spends a one-use refresh
// token, and an expired-but-renewable session reads live there). A refusal a
// fresh sign-in or a stored key repairs — no key, not signed in, a session for
// another pinned account/role or another access portal — is a credential one
// (connectDenial); an install with no store, the admin token, a provider with
// no region or model, and a renewal AWS did not answer are not. A store
// failure is returned, never read as "not connected".
func (s *Server) providerBedrockRefusal(ctx context.Context, p types.ModelProvider, agent, owner string, refresh bool) (awsSSOBlob, providerDenial, error) {
	refuse := func(d providerDenial) (awsSSOBlob, providerDenial, error) { return awsSSOBlob{}, d, nil }
	b := providerBedrockSettings(p)
	switch {
	case s.cfg.Secrets == nil:
		return refuse(stateDenial(p.ID, mpBRNoStore, mpRunRemedy))
	case owner == "" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal):
		return refuse(stateDenial(p.ID, mpBRNotPerson, mpRunRemedyPerson))
	case b.Region == "" || providerModel(p, agent) == "":
		return refuse(stateDenial(p.ID, fmt.Sprintf(mpBRUnset, agent), mpRunRemedy))
	}
	scope := chosenProvider{provider: p, owner: owner}.awsScope()
	if p.Kind == types.ModelProviderBedrockBearer {
		raw, found, err := s.ownSecret(ctx, owner, providerSecretName(p.UID, providerKeyPart))
		if err != nil {
			return awsSSOBlob{}, providerDenial{}, err
		}
		if !found || len(bytes.TrimSpace(raw)) == 0 {
			return refuse(connectDenial(p.ID, mpRunNoKey))
		}
		return awsSSOBlob{}, providerDenial{}, nil
	}
	blob, found, err := s.readAWSSSOBlob(ctx, scope)
	if err != nil {
		return awsSSOBlob{}, providerDenial{}, err
	}
	if !found {
		return refuse(connectDenial(p.ID, mpBRNotSignedIn))
	}
	// The pin and the portal before any renewal: a session the provider
	// would refuse is never worth spending a refresh token on.
	if b.SSOAccountID != "" && (blob.AccountID != b.SSOAccountID || blob.RoleName != b.SSORoleName) {
		return refuse(connectDenial(p.ID, fmt.Sprintf(mpBRPinned, blob.AccountID, blob.RoleName, b.SSOAccountID, b.SSORoleName)))
	}
	if !sameStartURL(blob.StartURL, b.SSOStartURL) {
		return refuse(connectDenial(p.ID, mpBRPortal))
	}
	if refresh {
		var failure string
		if blob, failure = s.refreshAWSSSOBlob(ctx, scope, blob); failure != "" {
			if failure == awsSSORefreshSpentRefusal(true) {
				return refuse(connectDenial(p.ID, mpBRNotSignedIn))
			}
			return refuse(stateDenial(p.ID, mpBRRenewing, mpBRRemedyRetry))
		}
	}
	if now := s.cfg.Now(); blob.expired(now) && (refresh || !blob.renewable(now)) {
		return refuse(connectDenial(p.ID, mpBRNotSignedIn))
	}
	return blob, providerDenial{}, nil
}

// modelCredential is the create door's model-credential fact for a run that
// chose a provider, which skips enforceCreateLLMMechanism: a Bedrock provider's
// run carries its owner's Bedrock credential, so the autonomy gate grades it
// WITH that credential, as it does every legacy Bedrock lane (#504,
// bedrockCredGradeHolds). Zero for every other choice.
func (c runProviderChoice) modelCredential() modelCredentialFacts {
	if !c.chosen || !c.provider.Kind.IsBedrock() {
		return modelCredentialFacts{}
	}
	return modelCredentialFacts{bedrockHost: providerBedrockRuntimeHost(c.provider)}
}

// providerBedrockTransport is the Bedrock arms' env (applyProviderEnv): the
// owner's own credential, already checked live and, on the SSO kind, renewed
// (blob, providerLaneForRun's), wired onto the sandbox through the same
// applyBedrockTransport the legacy lanes use, with the region, model and base
// URL the provider names. Its grant (the owner's key, or their session) is
// authored later in resolveLLMInjections, after the strip.
func (s *Server) providerBedrockTransport(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec,
	sandboxEnv map[string]string, c chosenProvider, blob awsSSOBlob,
) llmTransport {
	mp := c.provider
	b := providerBedrockSettings(mp)
	model := providerModel(mp, run.Agent)
	env := bedrockBaseEnv(b.Region, model, b.BaseURL)
	hosts := []string{providerBedrockRuntimeHost(mp), bedrockControlHost(b.Region)}
	var auth bedrockAuth
	if mp.Kind == types.ModelProviderBedrockBearer {
		// A non-empty sentinel so claude-code uses bearer auth; the proxy
		// sets the owner's key on the wire.
		env[envBedrockBearer] = "wardyn-proxy-injected"
		auth = bedrockAuth{env: env, egressHosts: hosts, bearer: true, bearerNamespace: c.awsScope()}
	} else {
		auth = s.bedrockSSOAuth(blob, c.awsScope(), env, hosts)
	}
	auth.ready, auth.region, auth.model = true, b.Region, model
	auth.runtimeHost, auth.runtimePort = providerBedrockRuntimeHost(mp), redirectPort(b.BaseURL)
	t := llmTransport{
		modelRun: true, provider: &c, bedrock: auth, bedrockReady: true,
		injectBedrockBearer: auth.bearer, injectBedrockSSO: auth.ssoInject && auth.ssoProxyInject,
	}
	t.secretEnvKeys, t.bedrockAudit = s.applyBedrockTransport(run, auth, policy, sandboxEnv)
	return t
}

// runBedrockProvider re-reads the provider run chose and reports it when it
// is still the one uid names, of kind, on and serving the run's agent.
func runBedrockProvider(sc types.SiteConfig, run types.AgentRun, uid string, kind types.ModelProviderKind) (types.ModelProvider, bool) {
	p, found := modelProviderByID(sc.ModelProviders, run.ModelProviderID)
	return p, found && uid != "" && p.UID == uid && p.Kind == kind && !p.Disabled && p.Serves(run.Agent)
}

// providerSSOScopeAt is the provider arm of resolveAWSSSOInjection's scope
// re-derivation: the run owner's own session for the provider the grant
// recorded, when that provider is still the run's own bedrock_sso provider, on
// and serving its agent, and still pins what the grant recorded. drift names
// the first thing that moved ("" = equal). A grant with no provider on a run
// that chose one — or one naming a provider on a run that did not — drifts.
func providerSSOScopeAt(sc types.SiteConfig, run types.AgentRun, sn awsSSOScopeSnapshot, subject string) (awsSSOScope, string) {
	p, live := runBedrockProvider(sc, run, sn.ProviderUID, types.ModelProviderBedrockSSO)
	switch {
	case !live:
		return awsSSOScope{}, "provider"
	// I2: the owner is the run token's own subject, whatever the grant says.
	case sn.OwnerSubject == "" || sn.OwnerSubject != subject:
		return awsSSOScope{}, "owner_not_caller"
	}
	if b := providerBedrockSettings(p); b.SSOAccountID != "" && (b.SSOAccountID != sn.SSOAccountID || b.SSORoleName != sn.SSORoleName) {
		return awsSSOScope{}, "sso_pin"
	}
	return chosenProvider{provider: p, owner: subject}.awsScope(), ""
}
