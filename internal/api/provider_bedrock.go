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
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending). The states and remedies of the run refusal
// (mpRunRefusal) for a chosen Bedrock provider, plus the sinks' refusals.
const (
	mpBRNotSignedIn   = "you are not signed in to AWS for it"
	mpBRNoKey         = "you have not added your key for it"
	mpBRNoStore       = "this install has no secret store to hold your AWS credential"
	mpBRNotPerson     = "the admin token is a shared credential rather than a person, so it has no AWS credential of its own"
	mpBRUnset         = "it has no region or model set for %s"
	mpBRPinned        = "your AWS sign-in is for account %s and role %s, and it pins account %s and role %s"
	mpBRPortal        = "your AWS sign-in is for a different AWS access portal than the one it names"
	mpBRRenewing      = "renewing your AWS sign-in did not complete, because AWS did not answer the token request"
	mpBRRemedyRetry   = "your sign-in is still good; launch again in a moment."
	mpBRReadFailed    = "Wardyn couldn't read your AWS credential for model provider %s just now — nothing was started. Try again in a moment."
	mpBRSinkRecorded  = "a model provider's Bedrock key is injected only through the grant Wardyn authors when a run launches on that provider, which records whose key it is; this grant carries no such record"
	mpBRSinkChanged   = "this run's model provider was removed, turned off or changed after the run started, so its AWS credential is no longer injected"
	mpBRSinkHost      = "a model provider's Bedrock key may only be injected to that provider's own Bedrock host"
	mpBRSinkUnreadKey = "Wardyn couldn't read your Bedrock key for this run's model provider just now"
)

// awsScope is the credential scope of a chosen Bedrock provider: always the
// run owner's own namespace, under the provider's UID-keyed names.
func (c chosenProvider) awsScope() awsSSOScope {
	return awsSSOScope{perUser: true, owner: c.owner, bearer: c.provider.Kind == types.ModelProviderBedrockBearer, provider: c.provider.UID}
}

// providerAWSScope is the chosen provider's credential scope, zero on a run
// that chose none.
func (t llmTransport) providerAWSScope() awsSSOScope {
	if t.provider == nil {
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

// providerBedrockRefusal is the liveness check for a chosen Bedrock provider,
// shared by create, Review and dispatch: "" when owner may run on p, else the
// whole refusal sentence. On the SSO kind it returns the owner's session,
// renewed first when refresh allows (dispatch alone: a dry run never spends a
// one-use refresh token, and an expired-but-renewable session reads live
// there). A store failure is returned, never read as "not connected".
func (s *Server) providerBedrockRefusal(ctx context.Context, p types.ModelProvider, agent, owner string, refresh bool) (awsSSOBlob, string, error) {
	refuse := func(state, remedy string) (awsSSOBlob, string, error) {
		return awsSSOBlob{}, fmt.Sprintf(mpRunRefusal, p.ID, state, remedy), nil
	}
	b := providerBedrockSettings(p)
	switch {
	case s.cfg.Secrets == nil:
		return refuse(mpBRNoStore, mpRunRemedy)
	case owner == "" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal):
		return refuse(mpBRNotPerson, mpRunRemedyPerson)
	case b.Region == "" || providerModel(p, agent) == "":
		return refuse(fmt.Sprintf(mpBRUnset, agent), mpRunRemedy)
	}
	scope := chosenProvider{provider: p, owner: owner}.awsScope()
	if p.Kind == types.ModelProviderBedrockBearer {
		raw, found, err := s.ownSecret(ctx, owner, providerSecretName(p.UID, providerKeyPart))
		if err != nil {
			return awsSSOBlob{}, "", err
		}
		if !found || len(bytes.TrimSpace(raw)) == 0 {
			return refuse(mpBRNoKey, mpRunRemedySignIn)
		}
		return awsSSOBlob{}, "", nil
	}
	blob, found, err := s.readAWSSSOBlob(ctx, scope)
	if err != nil {
		return awsSSOBlob{}, "", err
	}
	if !found {
		return refuse(mpBRNotSignedIn, mpRunRemedySignIn)
	}
	// The pin and the portal before any renewal: a session the provider
	// would refuse is never worth spending a refresh token on.
	if b.SSOAccountID != "" && (blob.AccountID != b.SSOAccountID || blob.RoleName != b.SSORoleName) {
		return refuse(fmt.Sprintf(mpBRPinned, blob.AccountID, blob.RoleName, b.SSOAccountID, b.SSORoleName), mpRunRemedySignIn)
	}
	if !sameStartURL(blob.StartURL, b.SSOStartURL) {
		return refuse(mpBRPortal, mpRunRemedySignIn)
	}
	if refresh {
		var failure string
		if blob, failure = s.refreshAWSSSOBlob(ctx, scope, blob); failure != "" {
			if failure == awsSSORefreshSpentRefusal(true) {
				return refuse(mpBRNotSignedIn, mpRunRemedySignIn)
			}
			return refuse(mpBRRenewing, mpBRRemedyRetry)
		}
	}
	if now := s.cfg.Now(); blob.expired(now) && (refresh || !blob.renewable(now)) {
		return refuse(mpBRNotSignedIn, mpRunRemedySignIn)
	}
	return blob, "", nil
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

// providerBedrockTransport is the Bedrock arm of resolveProviderTransport:
// the owner's own credential, checked live, wired onto the sandbox through
// the same applyBedrockTransport the legacy lanes use, with the region, model
// and base URL the provider names. ok=false: the run is already FAILED.
func (s *Server) providerBedrockTransport(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec,
	sandboxEnv map[string]string, mp types.ModelProvider, fail func(types.ModelProviderKind, string) (llmTransport, bool),
) (llmTransport, bool) {
	c := chosenProvider{provider: mp, owner: runIdentitySubject(ctx, run.CreatedBy)}
	// The same purpose the legacy lanes read and renew under at dispatch
	// (resolveLLMTransport's resolveBedrockAuth).
	blob, refusal, err := s.providerBedrockRefusal(secretstore.WithPurpose(ctx, secretstore.PurposeSSORefresh), mp, run.Agent, c.owner, true)
	if err != nil {
		return fail(mp.Kind, fmt.Sprintf(mpBRReadFailed, mp.ID))
	}
	if refusal != "" {
		return fail(mp.Kind, refusal)
	}
	b := providerBedrockSettings(mp)
	model := providerModel(mp, run.Agent)
	env := bedrockBaseEnv(b.Region, model, b.BaseURL)
	hosts := []string{providerBedrockRuntimeHost(mp), bedrockControlHost(b.Region)}
	var auth bedrockAuth
	if mp.Kind == types.ModelProviderBedrockBearer {
		// A non-empty sentinel so claude-code uses bearer auth; the proxy
		// sets the owner's key on the wire.
		env["AWS_BEARER_TOKEN_BEDROCK"] = "wardyn-proxy-injected"
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
	return t, true
}

// dropForeignModelInjections removes, auditing each, every injection on a
// Bedrock provider run (a no-op on any other) that could carry a model credential other than the
// one its arm authors next: the legacy sentinels (the operator's Claude
// subscription and managed token, the roster's AWS session, bedrock-api-key)
// and anything bound for the Anthropic API, its configured gateway, or this
// run's own Bedrock and access-portal hosts. The chosen provider's arm is the
// only credential author.
func (s *Server) dropForeignModelInjections(ctx context.Context, run types.AgentRun, t llmTransport, injections []runner.InjectionGrant) []runner.InjectionGrant {
	if t.provider == nil || !t.bedrockReady {
		return injections
	}
	hosts := []string{subscriptionInjectionHost, t.bedrock.runtimeHost}
	if h := s.anthropicGatewayHost(); h != "" {
		hosts = append(hosts, h)
	}
	if t.bedrock.ssoInject {
		hosts = append(hosts, ssoPortalHost(t.bedrock.ssoRegion, s.cfg.AWSSSOEndpointOverride))
	}
	legacy := []string{subscriptionOAuthSecret, types.ManagedOAuthSecret, types.AWSSSOAccessTokenSecret, bedrockAPIKeySecret}
	return slices.DeleteFunc(injections, func(ig runner.InjectionGrant) bool {
		if !slices.Contains(legacy, ig.Rule.SecretName) &&
			!slices.ContainsFunc(hosts, func(h string) bool { return hostEqual(h, ig.Rule.Host) }) {
			return false
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.injection.dropped",
			ig.GrantID.String(), "denied", mustJSON(map[string]any{
				"grant_id": ig.GrantID, "secret_name": ig.Rule.SecretName, "host": ig.Rule.Host,
				"reason": "not_the_chosen_provider", "provider": run.ModelProviderID,
			})))
		return true
	})
}

// providerKeyUID returns the provider UID a wardyn-provider-<uid>-key name
// names.
func providerKeyUID(name string) (string, bool) {
	uid, ok := strings.CutPrefix(name, providerSecretPrefix)
	if !ok {
		return "", false
	}
	uid, ok = strings.CutSuffix(uid, "-"+providerKeyPart)
	return uid, ok && uid != ""
}

// runBedrockProvider re-reads the provider run chose and reports it when it
// is still the one uid names, of kind, on and serving the run's agent.
func runBedrockProvider(sc types.SiteConfig, run types.AgentRun, uid string, kind types.ModelProviderKind) (types.ModelProvider, bool) {
	p, found := modelProviderByID(sc.ModelProviders, run.ModelProviderID)
	return p, found && uid != "" && p.UID == uid && p.Kind == kind && !p.Disabled && p.Serves(run.Agent)
}

// resolveProviderBedrockKeyInjection is the wardyn-provider-<uid>-key arm of
// handleInternalInjection. handled=false means the grant names another secret.
// It takes nothing from the grant but the name and its record: the provider is
// re-read by UID and must still be the run's own bedrock_bearer provider, on
// and serving its agent; the host is that provider's Bedrock host; the owner is
// the run token's subject, whose own namespace alone is read (ownSecret —
// never the operator fallback). A key for any other kind has no arm on this
// build and is refused. Every miss fails closed.
func (s *Server) resolveProviderBedrockKeyInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	name := minted.Injection.SecretName
	uid, ok := providerKeyUID(name)
	if !ok {
		return false
	}
	ctx := r.Context()
	// This site records its own secret.read, so its one store read is marked
	// SiteAudited and each record carries the row it read (withStoreRow).
	rctx, row := secretstore.SiteAudited(ctx)
	fail := func(status int, reason, body string) bool {
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", name, "failure",
			mustJSON(withStoreRow(map[string]any{"reason": reason, "grant_id": grantID, "source": "provider"}, row))))
		writeError(w, status, body)
		return true
	}
	var rec providerGrantSnapshot
	if !s.grantSnapshot(ctx, claims.RunID, grantID, &rec) || rec.ProviderUID != uid || rec.OwnerSubject == "" {
		return fail(http.StatusForbidden, "missing_scope_snapshot", mpBRSinkRecorded)
	}
	// The run token, not the grant, is authority for whose run this is.
	if rec.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, "owner_mismatch", mpBRSinkRecorded)
	}
	run, err := s.cfg.Store.GetRun(ctx, claims.RunID)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "run_unreadable", credentialReauthRunUnreadableBody)
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "providers_unreadable", mpRunUnreadable)
	}
	p, live := runBedrockProvider(sc, run, uid, types.ModelProviderBedrockBearer)
	if !live {
		return fail(http.StatusForbidden, "provider_changed", mpBRSinkChanged)
	}
	if !hostEqual(minted.Injection.Host, providerBedrockRuntimeHost(p)) {
		return fail(http.StatusForbidden, "host-not-provider", mpBRSinkHost)
	}
	key, found, err := s.ownSecret(rctx, claims.Sub, name)
	switch {
	case err != nil:
		return fail(http.StatusServiceUnavailable, "store_unreadable", mpBRSinkUnreadKey)
	case !found || len(bytes.TrimSpace(key)) == 0:
		return fail(http.StatusFailedDependency, "own_key_absent", fmt.Sprintf(mpRunRefusal, p.ID, mpBRNoKey, mpRunRemedySignIn))
	}
	// One correct wire shape, whatever the grant says.
	formatted := formatInjectionValue("Bearer %s", key)
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, key)
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(formatted))
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", name, "success",
		mustJSON(withStoreRow(map[string]any{
			"purpose": "proxy-injection", "grant_id": grantID, "jti": minted.JTI,
			"source": "provider", "provider": p.ID, "owner": claims.Sub,
		}, row))))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: minted.Injection.Host, Header: "Authorization", Value: formatted, JTI: minted.JTI,
	})
	return true
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
