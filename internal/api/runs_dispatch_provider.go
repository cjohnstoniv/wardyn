// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Dispatch for a run whose deployment configured model providers: the key and
// endpoint kinds' arm (multi-provider design §2.5, §2.8). Once the provider
// block is non-nil, a model run is credentialed by the provider it chose, from
// its owner's own credential, or by nothing — never by the legacy lane chain,
// which serves the operator's credentials.
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
	mpRunConnectRemedy  = "connect it from Getting started in the console, or from the banner the console shows on every page."
	mpRunCredUnreadable = "Wardyn couldn't read your credential for model provider %s just now, so nothing was started. Try again in a moment."
	mpRunUnreadable     = "Wardyn couldn't read its model providers just now, so nothing was started. Try again in a moment."
	mpRunNoIntegration  = "integration_id no longer chooses a model credential on this deployment: its model providers do — use model_provider instead."
	// mpNoProviderDetail is the brokered-LLM 404's detail for a model run no
	// provider serves: under a provider block nothing else credentials it.
	mpNoProviderDetail = "no model provider serves this agent on this deployment — an admin adds one under Settings → Model providers"
)

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
	if !ok || i < 0 || harnessProviderReason(agent, string(p.Kind)) != "" {
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

// providerCredentialRefusal is the liveness check both doors make for a key or
// endpoint provider: "" when owner's OWN key or token for p is stored, else the
// sentence refusing the run. Read strictly (ownSecret): the operator's row
// never stands in for a person's. The read only grades the key, so it is
// recorded as a status read.
func (s *Server) providerCredentialRefusal(ctx context.Context, owner string, p types.ModelProvider) (string, error) {
	if !s.credentialPerson(owner) {
		return mpcNoPerson, nil
	}
	_, found, err := s.ownSecret(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), owner, providerSecretName(p.UID, providerKeyPart))
	if err != nil || found {
		return "", err
	}
	state := mpRunNoKey
	if p.Kind == types.ModelProviderCustomEndpoint {
		state = mpRunNoToken
	}
	return fmt.Sprintf(mpRunRefusal, p.ID, state, mpRunConnectRemedy), nil
}

// providerCredentialMissing reports whether msg, a providerCredentialRefusal
// answer, is one the person's own stored key or token repairs — the one
// model-provider refusal that carries reason model_credential (#532) at both
// doors. The admin token's (mpcNoPerson) is not: no credential it could store
// would serve the run.
func providerCredentialMissing(msg string) bool { return msg != "" && msg != mpcNoPerson }

// providerGovernsDispatch reports whether this dispatch takes the provider path
// rather than the legacy lane chain: the run chose a provider, or this is a
// model run of an agent Wardyn credentials and the block is set — or could not
// be read. Nothing on the row records that a run was created under a block
// that serves no provider for it, so an unreadable block governs and the
// provider arm refuses the run, as the create door does, rather than hand it
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

// resolveProviderLane is the provider path of the LLM phase. It strips every
// legacy model credential the run's policy carries — the operator's
// ~/.claude mount, and each injection naming a model credential (a provider
// key, a subscription or Bedrock sentinel, or any grant on this agent's model
// hosts) — then, for a run that chose a key or endpoint provider, authors the
// ONE grant that credentials it: the run owner's own key, on the provider's
// host, with that host exactly allowlisted. A run no provider serves is left
// with no model credential at all. A block that could not be read refuses the
// run before anything is stripped or authored.
//
// ok=false means the run was refused and is already marked FAILED.
func (s *Server) resolveProviderLane(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec,
	sandboxEnv map[string]string, injections []runner.InjectionGrant, proxyURL string,
	siteCfg types.SiteConfig, siteCfgOK bool,
) (llmTransport, []runner.InjectionGrant, providerDispatch, bool) {
	llm := llmTransport{modelRun: true}
	if !siteCfgOK {
		s.refuseProviderDispatch(ctx, run, mpRunUnreadable, "", false)
		return llm, injections, providerDispatch{}, false
	}
	// The host-mount subscription path: a policy blessed with the operator's
	// resident ~/.claude must not hand it to a provider run.
	policy.WorkspaceMounts = slices.DeleteFunc(slices.Clone(policy.WorkspaceMounts), func(wm types.WorkspaceMount) bool {
		return wm.Target == claudeCredTarget || wm.Target == claudeCredJSONTarget
	})
	var lane providerKeyLane
	if run.ModelProviderID != "" {
		var refusal string
		var kind types.ModelProviderKind
		var credential bool
		if lane, refusal, kind, credential = s.providerLaneForRun(ctx, run, siteCfg); refusal != "" {
			s.refuseProviderDispatch(ctx, run, refusal, kind, credential)
			return llm, injections, providerDispatch{}, false
		}
	}
	injections = s.dropLegacyModelInjections(ctx, run, injections, lane.host)
	s.applyProviderEnv(run, lane, sandboxEnv, proxyURL)
	if run.ModelProviderID == "" {
		return llm, injections, providerDispatch{detail: mpNoProviderDetail}, true
	}
	grant, ok := s.authorProviderKeyInjection(ctx, run, lane)
	if !ok {
		return llm, injections, providerDispatch{}, false
	}
	injections = append(injections, grant)
	unionAllowedDomains(policy, []string{lane.host})
	var upstreams map[string]string
	if lane.upstream != "" {
		upstreams = map[string]string{lane.vendorHost: lane.upstream}
	}
	return llm, injections, providerDispatch{upstreams: upstreams}, true
}

// providerLaneForRun re-reads the provider the run chose at create and refuses,
// naming it, if it is gone, off, no longer serves the agent, is of a kind this
// arm does not dispatch, or its owner's credential is not stored (§2.4 step 5).
// kind is the provider's, for the refusal's run.create audit row (#532) — ""
// when id names no real provider (mpRunStateMissing), the only case with none
// to name; credential marks a refusal providerCredentialMissing classes.
func (s *Server) providerLaneForRun(ctx context.Context, run types.AgentRun, siteCfg types.SiteConfig) (
	lane providerKeyLane, refusal string, kind types.ModelProviderKind, credential bool,
) {
	id := run.ModelProviderID
	p, found := modelProviderByID(siteCfg.ModelProviders, id)
	switch {
	case !found:
		return providerKeyLane{}, providerRefusal(id, "", mpRunStateMissing).refusal, "", false
	case p.Disabled:
		return providerKeyLane{}, providerRefusal(id, p.Kind, mpRunStateOff).refusal, p.Kind, false
	case !providerKindDispatched[p.Kind]:
		return providerKeyLane{}, fmt.Sprintf(mpRunNotYet, id), p.Kind, false
	}
	lane, ok := providerKeyLaneFor(p, run.Agent)
	if !ok {
		return providerKeyLane{}, providerRefusal(id, p.Kind, fmt.Sprintf(mpRunStateNotServing, run.Agent)).refusal, p.Kind, false
	}
	lane.owner = runIdentitySubject(ctx, run.CreatedBy)
	msg, err := s.providerCredentialRefusal(ctx, lane.owner, p)
	if err != nil {
		return providerKeyLane{}, fmt.Sprintf(mpRunCredUnreadable, id), p.Kind, false
	}
	return lane, msg, p.Kind, providerCredentialMissing(msg)
}

// refuseProviderDispatch fails the run closed (CAS from STARTING, so a
// concurrent kill's KILLED stands) before any credential is authored. kind
// (#532) is the refused provider's kind, "" when it is not known (an
// unreadable block, or a provider id that does not exist) — recorded twice,
// as `kind` and as the legacy `mechanism` field. credential adds `reason:
// model_credential`, the class the console's audit reader (runEndingFromAudit)
// grades a credential ending by; it reads `mechanism` only on such a row, until
// MP-24 moves it off that key.
func (s *Server) refuseProviderDispatch(ctx context.Context, run types.AgentRun, msg string, kind types.ModelProviderKind, credential bool) {
	s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
	data := map[string]any{"error": msg, "provider": run.ModelProviderID}
	if kind != "" {
		data["kind"], data["mechanism"] = kind, string(kind)
	}
	if credential {
		data["reason"] = llmRefusalAuditReason
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(data)))
}

// dropLegacyModelInjections removes, auditing each, every injection that would
// credential this provider run's model other than the one its arm authors: a
// provider key or sign-in name (only the arm names one), the subscription and
// Bedrock sentinels, and any grant on this agent's vendor host, on the boot
// gateway that re-points it, or on the chosen provider's own host (laneHost;
// "" when none was chosen). Such a grant comes from a stored or default
// policy, a recorded profile or a legacy fold, and it reads the operator's
// credential — or, on the arm's own host, would be a second injection for one
// host, which fails the sidecar at startup.
func (s *Server) dropLegacyModelInjections(ctx context.Context, run types.AgentRun, injections []runner.InjectionGrant, laneHost string) []runner.InjectionGrant {
	conv, _ := agentLLMProvider(run.Agent)
	hosts := []string{conv.host, laneHost, gatewayHost(s.cfg.LLMGateways[conv.host])}
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
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.injection.dropped",
		ig.GrantID.String(), "denied", mustJSON(map[string]any{
			"grant_id": ig.GrantID, "secret_name": ig.Rule.SecretName, "host": ig.Rule.Host, "reason": reason,
		})))
}

// applyProviderEnv points the harness at the proxy's brokered route with a
// placeholder the proxy strips — the only credential the sandbox ever holds —
// and pins the model the admin set for this provider on this harness. With no
// provider chosen the placeholder stays, so the route's 404 explains itself.
func (s *Server) applyProviderEnv(run types.AgentRun, lane providerKeyLane, sandboxEnv map[string]string, proxyURL string) {
	switch run.Agent {
	case "claude-code":
		sandboxEnv["ANTHROPIC_API_KEY"] = "wardyn-proxy-injected"
		if model := cmp.Or(lane.model, s.cfg.AgentAnthropicModel); model != "" {
			sandboxEnv["ANTHROPIC_MODEL"] = model
		}
	case "codex-cli":
		sandboxEnv["OPENAI_BASE_URL"] = proxyURL + "/wardyn/llm/openai"
		sandboxEnv["OPENAI_API_KEY"] = "wardyn-proxy-injected"
	}
}

// providerKeySnapshot is whose key a provider grant resolves and for which
// provider, recorded on the grant dispatch authors. The sink resolves the key
// from exactly this, and only when the owner is the run's own subject.
type providerKeySnapshot struct {
	OwnerSubject string `json:"owner_subject"`
	ProviderUID  string `json:"provider_uid"`
}

// authorProviderKeyInjection writes the run's one model-credential grant: an
// api_key grant for the lane's host naming the owner's own
// wardyn-provider-<uid>-key, with the snapshot the sink requires. ok=false
// means the write failed and the run is already FAILED.
func (s *Server) authorProviderKeyInjection(ctx context.Context, run types.AgentRun, lane providerKeyLane) (runner.InjectionGrant, bool) {
	scope, _ := json.Marshal(map[string]any{
		"host": lane.host, "header": lane.header, "format": lane.format,
		"secret_name": providerSecretName(lane.provider.UID, providerKeyPart),
		"snapshot":    providerKeySnapshot{OwnerSubject: lane.owner, ProviderUID: lane.provider.UID},
	})
	grantID := uuid.New()
	if _, err := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600},
	}); err != nil {
		s.refuseProviderDispatch(ctx, run, "could not author the model provider credential injection: "+err.Error(), lane.provider.Kind, false)
		return runner.InjectionGrant{}, false
	}
	rule, err := injectionRuleFromScope(scope)
	if err != nil {
		s.refuseProviderDispatch(ctx, run, "could not compile the model provider credential injection: "+err.Error(), lane.provider.Kind, false)
		return runner.InjectionGrant{}, false
	}
	return runner.InjectionGrant{GrantID: grantID, Rule: rule}, true
}
