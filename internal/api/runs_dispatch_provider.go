// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Dispatch for a run whose deployment configured model providers (multi-provider
// design §2.5, §2.8): the one gate, the one strip, and the per-kind arms. Once
// the provider block is non-nil, a model run is credentialed by the provider it
// chose, from its owner's own credential, or by nothing — never by the legacy
// lane chain, which serves the operator's credentials. The key and endpoint
// kinds' arm lives here; the subscription arm in provider_subscription.go and
// the Bedrock arms in provider_bedrock.go.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending).
const (
	mpRunNoKey          = "you have not added your key for it"
	mpRunNoToken        = "you have not added your token for it"
	mpRunCredUnreadable = "Wardyn couldn't read your credential for model provider %s just now, so nothing was started. Try again in a moment."
	mpRunUnreadable     = "Wardyn couldn't read its model providers just now, so nothing was started. Try again in a moment."
	mpRunNoIntegration  = "integration_id no longer chooses a model credential on this deployment: its model providers do — use model_provider instead."
	// mpNoProviderDetail is the brokered-LLM 404's detail for a model run no
	// provider serves: under a provider block nothing else credentials it.
	mpNoProviderDetail = "no model provider serves this agent on this deployment — an admin adds one under Settings → Model providers"
)

// providerDenial is a model-provider refusal: the sentence, and whether the
// person's own sign-in or stored key is what repairs it — the one class that
// carries reason model_credential (#532) on the create/Review 422 and on
// dispatch's run.create failure row, since the console answers that reason
// with a sign-in and a relaunch. Every other refusal (a provider that is off,
// not available, unset, of no person, or a renewal AWS did not answer) is
// repaired by something a sign-in cannot do, so it carries no reason.
type providerDenial struct {
	msg        string
	credential bool
}

// connectDenial is the one constructor of a credential refusal: its remedy is
// always the connect door, which is what makes it one.
func connectDenial(id, state string) providerDenial {
	return providerDenial{msg: fmt.Sprintf(mpRunRefusal, id, state, mpRunRemedySignIn), credential: true}
}

// stateDenial is a refusal a sign-in does not repair.
func stateDenial(id, state, remedy string) providerDenial {
	return providerDenial{msg: fmt.Sprintf(mpRunRefusal, id, state, remedy)}
}

// providerKeyKind reports whether k is served by the key and endpoint arm.
func providerKeyKind(k types.ModelProviderKind) bool {
	return k == types.ModelProviderAnthropicAPIKey || k == types.ModelProviderOpenAIAPIKey || k == types.ModelProviderCustomEndpoint
}

// providerLiveness is the one liveness check every door makes for a chosen
// provider p — create, Review, a record session and dispatch — dispatched by
// kind: the zero denial when owner's OWN credential for p can serve agent.
// refresh is dispatch's alone: a dry check never spends a Bedrock SSO
// session's one-use refresh token, and the renewed session dispatch reads is
// returned for its arm. An error is a credential that could not be read, never
// "not connected"; callers answer it with providerReadFailed, which is no door.
func (s *Server) providerLiveness(ctx context.Context, p types.ModelProvider, agent, owner string, refresh bool) (awsSSOBlob, providerDenial, error) {
	switch {
	case providerKeyKind(p.Kind):
		d, err := s.providerCredentialRefusal(ctx, owner, p)
		return awsSSOBlob{}, d, err
	case p.Kind == types.ModelProviderAnthropicSubscription:
		purpose := secretstore.PurposeStatus
		if refresh {
			purpose = secretstore.PurposeDispatch
		}
		d, err := s.providerSubscriptionRefusal(secretstore.WithPurpose(ctx, purpose), p, owner)
		return awsSSOBlob{}, d, err
	case p.Kind.IsBedrock():
		// The same purpose the legacy lanes read and renew under at dispatch
		// (resolveLLMTransport's resolveBedrockAuth).
		purpose := secretstore.PurposeStatus
		if refresh {
			purpose = secretstore.PurposeSSORefresh
		}
		return s.providerBedrockRefusal(secretstore.WithPurpose(ctx, purpose), p, agent, owner, refresh)
	}
	// The kind set is closed and validated on write; a kind with no arm is
	// refused, never handed to another arm.
	return awsSSOBlob{}, stateDenial(p.ID, fmt.Sprintf(mpRunStateNotServing, agent), mpRunRemedy), nil
}

// providerReadFailed is the sentence for a credential liveness could not read:
// the sentence alone at every door (a transient store failure is no door,
// multi-provider §5.8).
func providerReadFailed(p types.ModelProvider) string {
	switch {
	case p.Kind == types.ModelProviderAnthropicSubscription:
		return fmt.Sprintf(mpSubReadFailed, p.ID)
	case p.Kind.IsBedrock():
		return fmt.Sprintf(mpBRReadFailed, p.ID)
	}
	return fmt.Sprintf(mpRunCredUnreadable, p.ID)
}

// providerKeyLane is what the key and endpoint arm sends for one run: the
// brokered route's dialect (vendorHost), the host each request actually goes
// to, how the person's key rides, and where it is read from.
type providerKeyLane struct {
	provider   types.ModelProvider
	vendorHost string // the harness's dialect host: api.anthropic.com or api.openai.com
	host       string // vendorHost, or the host of BaseURL(+Path)
	header     string
	format     string
	upstream   string // BaseURL(+Path) when requests do not go to vendorHost
	model      string
	owner      string // whose namespace the key is read from: the run owner's, always
}

// providerKeyLaneFor derives the lane p serves agent on. ok=false when p does
// not serve agent, or cannot drive it (a kind the harness refuses).
//
// A key kind speaks the vendor's own header convention and may route through
// BaseURL; a custom endpoint always goes to BaseURL+Path and sends the person's
// token the way its Auth (or the harness entry's override) says.
func providerKeyLaneFor(p types.ModelProvider, agent string) (providerKeyLane, bool) {
	conv, ok := agentLLMProvider(agent)
	i := slices.IndexFunc(p.Harnesses, func(h types.ProviderHarness) bool { return h.Harness == agent })
	if !ok || i < 0 || !providerKeyKind(p.Kind) || harnessProviderReason(agent, string(p.Kind)) != "" {
		return providerKeyLane{}, false
	}
	h := p.Harnesses[i]
	l := providerKeyLane{
		provider: p, vendorHost: conv.host, host: conv.host,
		header: conv.header, format: conv.format, model: h.Model,
	}
	base := p.BaseURL
	if p.Kind == types.ModelProviderCustomEndpoint {
		base += h.Path
		var auth types.ProviderAuth
		if p.Auth != nil {
			auth = *p.Auth
		}
		l.header = cmp.Or(h.AuthHeader, auth.Header, defaultEndpointAuthHeader)
		l.format = cmp.Or(h.AuthFormat, auth.Format, defaultEndpointAuthFormat)
	}
	if base != "" {
		l.upstream, l.host = base, gatewayHost(base)
	}
	return l, l.host != ""
}

// credentialPerson reports whether subject can hold a model credential of its
// own. Under OIDC the admin token is a mechanism, not a person.
func (s *Server) credentialPerson(subject string) bool {
	return subject != "" && (s.cfg.OIDC == nil || subject != adminTokenPrincipal)
}

// providerCredentialRefusal is the key and endpoint kinds' liveness: the zero
// denial when owner's OWN key or token for p is stored. Read strictly
// (ownSecret): the operator's row never stands in for a person's. The read
// only grades the key, so it is recorded as a status read. The admin token's
// refusal (mpcNoPerson) is not a credential one: no credential it could store
// would serve the run.
func (s *Server) providerCredentialRefusal(ctx context.Context, owner string, p types.ModelProvider) (providerDenial, error) {
	if !s.credentialPerson(owner) {
		return providerDenial{msg: mpcNoPerson}, nil
	}
	_, found, err := s.ownSecret(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), owner, providerSecretName(p.UID, providerKeyPart))
	if err != nil || found {
		return providerDenial{}, err
	}
	if p.Kind == types.ModelProviderCustomEndpoint {
		return connectDenial(p.ID, mpRunNoToken), nil
	}
	return connectDenial(p.ID, mpRunNoKey), nil
}

// providerGovernsDispatch reports whether this dispatch takes the provider path
// rather than the legacy lane chain: the run chose a provider, or this is a
// model run of an agent Wardyn credentials and the block is set — or could not
// be read. Nothing on the row records that a run was created under a block
// that serves no provider for it, so an unreadable block governs and the
// provider path refuses the run, as the create door does, rather than hand it
// to a chain that serves the operator's credentials.
func providerGovernsDispatch(run types.AgentRun, p dispatchParams, siteCfg types.SiteConfig, siteCfgOK bool) bool {
	if run.ModelProviderID != "" {
		return true
	}
	if run.Task == harnessLoginTask {
		return false
	}
	_, needsModel := agentLLMProvider(run.Agent)
	return needsModel && isModelRun(p.TaskMode, run.WorkspaceID, run.SourceID, p.Interactive) &&
		(!siteCfgOK || siteCfg.ModelProviders != nil)
}

// providerDispatch is what the provider path hands the rest of dispatch.
type providerDispatch struct {
	upstreams map[string]string // ProxyConfig.LLMUpstreams: {vendor host → BaseURL+Path}, nil for the vendor itself
	detail    string            // the brokered-LLM 404's detail
}

// providerLane is what the chosen provider's arm resolved for one run before
// anything is stripped or authored: the provider and whose credential serves
// it, and what its kind's arm needs. The zero lane is "no provider serves this
// run": nothing credentials it.
type providerLane struct {
	chosen *chosenProvider
	key    providerKeyLane // the key and endpoint kinds'
	blob   awsSSOBlob      // bedrock_sso's: the owner's live, renewed session
}

// hosts is every host the lane's arm credentials or reaches its model on: the
// strip drops any other injection bound for one of them, so no second
// credential rides beside the arm's own (two injections for one host also
// fail the sidecar at startup).
func (l providerLane) hosts(s *Server) []string {
	if l.chosen == nil {
		return nil
	}
	p := l.chosen.provider
	switch {
	case p.Kind == types.ModelProviderAnthropicSubscription:
		return []string{gatewayHost(providerSubscriptionBase(p))}
	case p.Kind.IsBedrock():
		b := providerBedrockSettings(p)
		hosts := []string{providerBedrockRuntimeHost(p), bedrockControlHost(b.Region)}
		if p.Kind == types.ModelProviderBedrockSSO {
			hosts = append(hosts, ssoPortalHost(l.blob.Region, s.cfg.AWSSSOEndpointOverride))
		}
		return hosts
	}
	return []string{l.key.host}
}

// resolveProviderLane is the provider path of the LLM phase, for every kind.
// In order: a block that could not be read refuses the run; the operator's
// ~/.claude mounts go; a run that chose a provider has it re-read and its
// owner's credential checked live, refused naming it on any miss; then the
// STRIP — every injection that would credential the run's model
// (dropLegacyModelInjections) — and only after it the kind's arm: its env,
// and the key arm's one grant. The subscription and Bedrock arms author their
// grants later in resolveLLMInjections, also after the strip, so nothing an
// arm authors is ever stripped. A run no provider serves, or one that makes no
// model call, is stripped and credentialed by nothing.
//
// ok=false means the run was refused and is already marked FAILED.
func (s *Server) resolveProviderLane(ctx context.Context, run types.AgentRun, p dispatchParams, policy *types.RunPolicySpec,
	sandboxEnv map[string]string, injections []runner.InjectionGrant, proxyURL string,
	siteCfg types.SiteConfig, siteCfgOK bool,
) (llmTransport, []runner.InjectionGrant, providerDispatch, bool) {
	if !siteCfgOK {
		s.refuseProviderDispatch(ctx, run, "", providerDenial{msg: mpRunUnreadable})
		return llmTransport{}, injections, providerDispatch{}, false
	}
	// The host-mount subscription path: a policy blessed with the operator's
	// resident ~/.claude must not hand it to a provider run.
	policy.WorkspaceMounts = slices.DeleteFunc(slices.Clone(policy.WorkspaceMounts), func(wm types.WorkspaceMount) bool {
		return wm.Target == claudeCredTarget || wm.Target == claudeCredJSONTarget
	})
	modelRun := isModelRun(p.TaskMode, run.WorkspaceID, run.SourceID, p.Interactive) && run.Task != harnessLoginTask
	var lane providerLane
	if run.ModelProviderID != "" && modelRun {
		var kind types.ModelProviderKind
		var d providerDenial
		if lane, kind, d = s.providerLaneForRun(ctx, run, siteCfg); d.msg != "" {
			s.refuseProviderDispatch(ctx, run, kind, d)
			return llmTransport{}, injections, providerDispatch{}, false
		}
	}
	injections = s.dropLegacyModelInjections(ctx, run, injections, lane.hosts(s))
	if !modelRun {
		return llmTransport{}, injections, providerDispatch{}, true
	}
	llm := s.applyProviderEnv(ctx, run, lane, policy, sandboxEnv, proxyURL)
	if lane.chosen == nil {
		return llm, injections, providerDispatch{detail: mpNoProviderDetail}, true
	}
	if !providerKeyKind(lane.chosen.provider.Kind) {
		return llm, injections, providerDispatch{}, true
	}
	grant, ok := s.authorProviderKeyInjection(ctx, run, lane.key)
	if !ok {
		return llm, injections, providerDispatch{}, false
	}
	injections = append(injections, grant)
	unionAllowedDomains(policy, []string{lane.key.host})
	var upstreams map[string]string
	if lane.key.upstream != "" {
		upstreams = map[string]string{lane.key.vendorHost: lane.key.upstream}
	}
	return llm, injections, providerDispatch{upstreams: upstreams}, true
}

// providerLaneForRun re-reads the provider the run chose at create and checks
// its owner's credential live (§2.4 step 5), refusing, naming it, when it is
// gone, off, no longer serves the agent, or the owner's credential is not
// there (providerLiveness). kind is the provider's, for the refusal's
// run.create row (#532) — "" when id names no real provider
// (mpRunStateMissing), the only case with none to name. Nothing is authored.
func (s *Server) providerLaneForRun(ctx context.Context, run types.AgentRun, siteCfg types.SiteConfig) (providerLane, types.ModelProviderKind, providerDenial) {
	id := run.ModelProviderID
	p, found := modelProviderByID(siteCfg.ModelProviders, id)
	notServing := stateDenial(id, fmt.Sprintf(mpRunStateNotServing, run.Agent), mpRunRemedy)
	switch {
	case !found:
		return providerLane{}, "", stateDenial(id, mpRunStateMissing, mpRunRemedy)
	case p.Disabled:
		return providerLane{}, p.Kind, stateDenial(id, mpRunStateOff, mpRunRemedy)
	case !p.Serves(run.Agent):
		return providerLane{}, p.Kind, notServing
	}
	lane := providerLane{chosen: &chosenProvider{provider: p, owner: runIdentitySubject(ctx, run.CreatedBy)}}
	if providerKeyKind(p.Kind) {
		var ok bool
		if lane.key, ok = providerKeyLaneFor(p, run.Agent); !ok {
			return providerLane{}, p.Kind, notServing
		}
		lane.key.owner = lane.chosen.owner
	}
	blob, d, err := s.providerLiveness(ctx, p, run.Agent, lane.chosen.owner, true)
	if err != nil {
		return providerLane{}, p.Kind, providerDenial{msg: providerReadFailed(p)}
	}
	lane.blob = blob
	return lane, p.Kind, d
}

// refuseProviderDispatch fails the run closed (CAS from STARTING, so a
// concurrent kill's KILLED stands) before any credential is authored, and
// writes its run.create failure row (#532): `provider` is the id the run chose
// ("" when it chose none); kind, "" when it is not known (an unreadable block,
// or a provider id that does not exist), is recorded twice, as `kind` and as
// the legacy `mechanism` field; a credential refusal adds `reason:
// model_credential`, the class the console's audit reader (runEndingFromAudit)
// grades a credential ending by.
func (s *Server) refuseProviderDispatch(ctx context.Context, run types.AgentRun, kind types.ModelProviderKind, d providerDenial) {
	s.failAndRevoke(ctx, run.ID, types.RunStarting, d.msg)
	data := map[string]any{"error": d.msg, "provider": run.ModelProviderID}
	if kind != "" {
		data["kind"], data["mechanism"] = kind, string(kind)
	}
	if d.credential {
		data["reason"] = llmRefusalAuditReason
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(data)))
}

// dropLegacyModelInjections is the provider path's one strip: it removes,
// auditing each, every injection that would credential this run's model other
// than the ones its arm authors after it — a provider key or sign-in name
// (only an arm names one), the subscription, managed and AWS SSO sentinels,
// bedrock-api-key, and any grant on this agent's vendor host, on the boot
// gateway that re-points it, or on a host the chosen provider's arm
// credentials (laneHosts). Such a grant comes from a stored or default policy,
// a recorded profile or a legacy fold, and it reads the operator's
// credential — or, on the arm's own host, would be a second injection for one
// host, which fails the sidecar at startup. It must run before every arm
// authors: it deletes the very names the arms write.
func (s *Server) dropLegacyModelInjections(ctx context.Context, run types.AgentRun, injections []runner.InjectionGrant, laneHosts []string) []runner.InjectionGrant {
	conv, _ := agentLLMProvider(run.Agent)
	hosts := append([]string{conv.host, gatewayHost(s.cfg.LLMGateways[conv.host])}, laneHosts...)
	return slices.DeleteFunc(injections, func(ig runner.InjectionGrant) bool {
		name := ig.Rule.SecretName
		model := strings.HasPrefix(name, providerSecretPrefix) ||
			name == types.SubscriptionOAuthSecret || name == types.ManagedOAuthSecret ||
			name == types.AWSSSOAccessTokenSecret || name == bedrockAPIKeySecret ||
			slices.ContainsFunc(hosts, func(h string) bool { return h != "" && hostEqual(h, ig.Rule.Host) })
		if model {
			s.auditDroppedInjection(ctx, run, ig, "model_credential_not_provider_authored")
		}
		return model
	})
}

// dropUnauthoredProviderInjections is the legacy path's half of "only the
// provider arm names a provider credential": with no provider chosen, every
// injection naming one came from a policy, never from dispatch.
func (s *Server) dropUnauthoredProviderInjections(ctx context.Context, run types.AgentRun, injections []runner.InjectionGrant) []runner.InjectionGrant {
	return slices.DeleteFunc(injections, func(ig runner.InjectionGrant) bool {
		if !strings.HasPrefix(ig.Rule.SecretName, providerSecretPrefix) {
			return false
		}
		s.auditDroppedInjection(ctx, run, ig, "model_provider_not_dispatch_authored")
		return true
	})
}

func (s *Server) auditDroppedInjection(ctx context.Context, run types.AgentRun, ig runner.InjectionGrant, reason string) {
	data := map[string]any{"grant_id": ig.GrantID, "secret_name": ig.Rule.SecretName, "host": ig.Rule.Host, "reason": reason}
	if run.ModelProviderID != "" {
		data["provider"] = run.ModelProviderID
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.injection.dropped",
		ig.GrantID.String(), "denied", mustJSON(data)))
}

// applyProviderEnv wires the sandbox for the lane's kind, and only after the
// strip. The subscription and Bedrock arms set their own env (the managed
// sentinel and route; the Bedrock switch, region, model and, for bearer, its
// placeholder) and carry no API-key placeholder. The key and endpoint arm, and
// a run no provider serves, point the harness at the proxy's brokered route
// with a placeholder the proxy strips — the only credential the sandbox ever
// holds — so with no provider the route's 404 explains itself; the key arm
// also pins the model the admin set for this provider on this harness.
func (s *Server) applyProviderEnv(ctx context.Context, run types.AgentRun, lane providerLane,
	policy *types.RunPolicySpec, sandboxEnv map[string]string, proxyURL string,
) llmTransport {
	if c := lane.chosen; c != nil {
		switch {
		case c.provider.Kind == types.ModelProviderAnthropicSubscription:
			return s.providerSubscriptionTransport(run, sandboxEnv, *c)
		case c.provider.Kind.IsBedrock():
			return s.providerBedrockTransport(ctx, run, policy, sandboxEnv, *c, lane.blob)
		}
	}
	switch run.Agent {
	case "claude-code":
		sandboxEnv["ANTHROPIC_API_KEY"] = "wardyn-proxy-injected"
		if model := cmp.Or(lane.key.model, s.cfg.AgentAnthropicModel); model != "" {
			sandboxEnv["ANTHROPIC_MODEL"] = model
		}
	case "codex-cli":
		sandboxEnv["OPENAI_BASE_URL"] = proxyURL + "/wardyn/llm/openai"
		sandboxEnv["OPENAI_API_KEY"] = "wardyn-proxy-injected"
	}
	return llmTransport{modelRun: true, provider: lane.chosen}
}

// authorProviderKeyInjection writes the key arm's one model-credential grant:
// an api_key grant for the lane's host naming the owner's own
// wardyn-provider-<uid>-key, with the record the sink requires
// (providerGrantSnapshot). ok=false means the write failed and the run is
// already FAILED.
func (s *Server) authorProviderKeyInjection(ctx context.Context, run types.AgentRun, lane providerKeyLane) (runner.InjectionGrant, bool) {
	scope, _ := json.Marshal(map[string]any{
		"host": lane.host, "header": lane.header, "format": lane.format,
		"secret_name": providerSecretName(lane.provider.UID, providerKeyPart),
		"snapshot":    providerGrantSnapshot{OwnerSubject: lane.owner, ProviderUID: lane.provider.UID},
	})
	grantID := uuid.New()
	if _, err := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600},
	}); err != nil {
		s.refuseProviderDispatch(ctx, run, lane.provider.Kind, providerDenial{msg: "could not author the model provider credential injection: " + err.Error()})
		return runner.InjectionGrant{}, false
	}
	rule, err := injectionRuleFromScope(scope)
	if err != nil {
		s.refuseProviderDispatch(ctx, run, lane.provider.Kind, providerDenial{msg: "could not compile the model provider credential injection: " + err.Error()})
		return runner.InjectionGrant{}, false
	}
	return runner.InjectionGrant{GrantID: grantID, Rule: rule}, true
}
