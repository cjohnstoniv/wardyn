// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Dispatch of a run's model credential (multi-provider design §2.5, §2.8): the
// one gate, the one strip, and the per-kind arms. A model run is credentialed
// by the provider it chose, from its owner's own credential, or by nothing. The
// key and endpoint kinds' arm lives here; the subscription arm in
// provider_subscription.go and the Bedrock arms in provider_bedrock.go.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending).
const (
	// Cause-neutral on purpose (#1489): a credential that is not there looks the
	// same whether it was never stored, was deleted when an admin changed the
	// provider's address, or could not be read, so these say only that none is
	// available.
	mpRunNoKey          = "no key is available for it"
	mpRunNoToken        = "no token is available for it"
	mpRunCredUnreadable = "Wardyn couldn't read your credential for model provider %s just now, so nothing was started. Try again in a moment."
	mpRunUnreadable     = "Wardyn couldn't read its model providers just now, so nothing was started. Try again in a moment."
	mpRunNoIntegration  = "integration_id no longer chooses a model credential: a run's model provider does — use model_provider instead."
	// mpNoProviderDetail is the brokered-LLM 404's detail for a model run no
	// provider serves: under a provider block nothing else credentials it.
	mpNoProviderDetail = "no model provider serves this agent on this deployment — an admin adds one under Settings → Model providers"
	// mpNoProviderHarnessDetail is mpNoProviderDetail for a run whose other
	// harnesses each reach their own provider: it names the one that has none.
	mpNoProviderHarnessDetail = "no model provider serves %s on this deployment, though this run's other harnesses have their own — an admin adds one under Settings → Model providers"
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
	case p.Kind == types.ModelProviderAzureFoundry:
		purpose := secretstore.PurposeStatus
		if refresh {
			purpose = secretstore.PurposeDispatch
		}
		d, err := s.providerAzureRefusal(secretstore.WithPurpose(ctx, purpose), p, agent, owner)
		return awsSSOBlob{}, d, err
	case p.Kind.IsBedrock():
		// A status read, unless this pass may renew.
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
	case p.Kind == types.ModelProviderAzureFoundry:
		return fmt.Sprintf(mpAZReadFailed, p.ID)
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
	key    providerKeyLane   // the key and endpoint kinds'
	blob   awsSSOBlob        // bedrock_sso's: the owner's live, renewed session
	azure  providerAzureLane // azure_foundry's
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
	case p.Kind == types.ModelProviderAzureFoundry:
		return []string{l.azure.host}
	}
	return []string{l.key.host}
}

// resolveProviderLane is the LLM phase of every dispatch, for every kind. In
// order: a model run whose site config could not be read is refused; the
// operator's ~/.claude mounts go; each provider the run chose — its own
// agent's, and each other harness's of its set (providerLanePairs) — is
// re-read and its owner's credential checked live, refused naming it on any
// miss; then ONE STRIP over every lane's hosts — every injection that would
// credential the run's model (dropLegacyModelInjections) — and only after it
// each lane's arm: its env, and the key arm's one grant. The subscription and
// Bedrock arms author their grants later in resolveLLMInjections, also after
// the strip, so nothing an arm authors is ever stripped. A run no provider
// serves, or one that makes no model call, is stripped and credentialed by
// nothing; a login box also gets the public vendor host its sign-in dials.
//
// llms holds one transport per lane, pairs[i] naming lane i's harness and
// provider; the run's own agent's lane is first, also when it chose none.
// ok=false means the run was refused and is already marked FAILED.
func (s *Server) resolveProviderLane(ctx context.Context, run types.AgentRun, p dispatchParams, policy *types.RunPolicySpec,
	sandboxEnv map[string]string, injections []runner.InjectionGrant, proxyURL string,
	siteCfg types.SiteConfig, siteCfgOK bool,
) (llms []llmTransport, pairs []types.RunHarness, _ []runner.InjectionGrant, _ providerDispatch, ok bool) {
	// A command that carries harnesses runs them, so it is a model run too.
	modelRun := (isModelRun(p.TaskMode, run.WorkspaceID, run.SourceID, p.Interactive) || (p.TaskMode == "exec" && len(p.Harnesses) > 0)) &&
		run.Task != harnessLoginTask
	pairs = providerLanePairs(run, p.Harnesses)
	needsModel := slices.ContainsFunc(pairs, func(h types.RunHarness) bool { _, ok := agentLLMProvider(h.Harness); return ok })
	chose := slices.ContainsFunc(pairs, func(h types.RunHarness) bool { return h.ModelProviderID != "" })
	// Nothing records that a run was created under a block serving no provider
	// for it, so a model run of an agent Wardyn credentials cannot tell "no
	// provider" from "a provider I could not read" — it is refused, as the
	// create door refuses the same read. A run that makes no model call is
	// stripped with what could be read.
	if !siteCfgOK && (chose || (modelRun && needsModel)) {
		s.refuseProviderDispatch(ctx, run, "", providerDenial{msg: mpRunUnreadable}, nil)
		return nil, nil, injections, providerDispatch{}, false
	}
	// The create door's env_secret refusal again, for a policy that reached
	// dispatch another way (a record session, a stored policy edited since):
	// resolveEnvSecretGrants must never place a model credential beside the
	// provider's, so the run is refused before anything is authored.
	if name, secretName, found := modelEnvSecretGrant(*policy); found && modelRun && needsModel {
		p, _ := modelProviderByID(siteCfg.ModelProviders, run.ModelProviderID)
		s.refuseProviderDispatch(ctx, run, p.Kind, providerDenial{msg: fmt.Sprintf(mpRunModelEnvSecret, secretName, name)},
			map[string]any{"variable": name, "grant": secretName})
		return nil, nil, injections, providerDispatch{}, false
	}
	// The host-mount subscription path: a policy blessed with the operator's
	// resident ~/.claude must not hand it to any run.
	policy.WorkspaceMounts = slices.DeleteFunc(slices.Clone(policy.WorkspaceMounts), func(wm types.WorkspaceMount) bool {
		return wm.Target == claudeCredTarget || wm.Target == claudeCredJSONTarget
	})
	lanes, laneHosts, ok := s.harnessLanes(ctx, run, pairs, siteCfg, modelRun)
	if !ok {
		return nil, nil, injections, providerDispatch{}, false
	}
	injections = s.dropLegacyModelInjections(ctx, run, injections, laneHosts, s.modelServingHosts(siteCfg))
	if run.Task == harnessLoginTask {
		// A Claude sign-in (`claude setup-token`) mints its OAuth token against
		// the public host and must never be redirected to the brokered route.
		sandboxEnv[envAnthropicBaseURL] = "https://api.anthropic.com"
	}
	if !modelRun {
		return nil, nil, injections, providerDispatch{}, true
	}
	var prov providerDispatch
	var lanePairs []types.RunHarness
	if lanes[0].chosen == nil {
		prov.detail = mpNoProviderDetail
		if slices.ContainsFunc(lanes, func(l providerLane) bool { return l.chosen != nil }) {
			prov.detail = fmt.Sprintf(mpNoProviderHarnessDetail, harnessName(run.Agent))
		}
	}
	for i, lane := range lanes {
		if i > 0 && lane.chosen == nil {
			continue
		}
		lr := laneRun(run, pairs[i])
		llm := s.applyProviderEnv(ctx, lr, lane, policy, sandboxEnv, proxyURL)
		if err := llm.bedrock.maskErr; err != nil {
			slog.WarnContext(ctx, "wardynd: the AWS SSO credential could not be recorded for masking", slog.String("run", run.ID.String()), slog.Any("err", err))
			s.refuseProviderDispatch(ctx, lr, lane.chosen.provider.Kind, providerDenial{msg: fmt.Sprintf(mpBRMaskFailed, lane.chosen.provider.ID)}, nil)
			return nil, nil, injections, providerDispatch{}, false
		}
		llms, lanePairs = append(llms, llm), append(lanePairs, pairs[i])
		if lane.chosen == nil || !providerKeyKind(lane.chosen.provider.Kind) {
			continue
		}
		// Two harnesses sharing one key provider share its one grant.
		if !sharesKeyGrant(pairs, lanes, i) {
			grant, ok := s.authorProviderKeyInjection(ctx, lr, lane.key)
			if !ok {
				return nil, nil, injections, providerDispatch{}, false
			}
			injections = append(injections, grant)
			unionAllowedDomains(policy, []string{lane.key.host})
		}
		if lane.key.upstream != "" {
			if prov.upstreams == nil {
				prov.upstreams = map[string]string{}
			}
			prov.upstreams[lane.key.vendorHost] = lane.key.upstream
		}
	}
	return llms, lanePairs, injections, prov, true
}

// harnessLanes is resolveProviderLane's lane per pair: each provider re-read
// and its owner's credential checked live, an AWS sign-in on any harness but
// the primary refused first (harnessProviderNeedsPrimary), then the set held
// to create's one-lane test (laneConflict). hosts is every lane's hosts, for
// the one strip. ok=false means the run was refused and is already FAILED.
func (s *Server) harnessLanes(ctx context.Context, run types.AgentRun, pairs []types.RunHarness, siteCfg types.SiteConfig, modelRun bool,
) (lanes []providerLane, hosts []string, ok bool) {
	lanes = make([]providerLane, len(pairs))
	for i, h := range pairs {
		if h.ModelProviderID == "" || !modelRun {
			continue
		}
		lr := laneRun(run, h)
		if p, found := modelProviderByID(siteCfg.ModelProviders, h.ModelProviderID); i > 0 && found && harnessProviderNeedsPrimary(p) {
			s.refuseProviderDispatch(ctx, lr, p.Kind, providerDenial{msg: fmt.Sprintf(mpRunSignInNotPrimary, p.ID, harnessName(h.Harness))}, nil)
			return nil, nil, false
		}
		lane, kind, d := s.providerLaneForRun(ctx, lr, siteCfg)
		if d.msg != "" {
			s.refuseProviderDispatch(ctx, lr, kind, d, nil)
			return nil, nil, false
		}
		lanes[i] = lane
		hosts = append(hosts, lane.hosts(s)...)
	}
	if i, host := laneConflict(pairs, lanes); i > 0 {
		c := lanes[i].chosen.provider
		s.refuseProviderDispatch(ctx, laneRun(run, pairs[i]), c.Kind, providerDenial{msg: fmt.Sprintf(mpRunLaneConflict, c.ID, host)}, nil)
		return nil, nil, false
	}
	return lanes, hosts, true
}

// mpRunSignInNotPrimary is create's harnessSignInNotPrimaryMsg at dispatch, for
// a provider that became an AWS sign-in after the run was created.
const mpRunSignInNotPrimary = "Model provider %s uses an AWS sign-in, which only the harness a run starts, or else lists first, can use, " +
	"so nothing was started. Launch the run again with %s first, or with another provider for it."

// DRAFT (M2 canon pending).
const mpRunLaneConflict = "Model provider %s would put a second credential on %s, which another harness of this run " +
	"already uses, so nothing was started. An admin gives each harness's provider its own address."

// laneConflict is the first lane, by index into pairs and lanes, whose
// provider cannot be credentialed beside an earlier lane's in one sandbox, and
// the host they would share; 0 when there is none. The test is the create
// door's own (harnessProviderPairsConflict), so dispatch refuses exactly the
// pairs create refuses: a run admitted before a provider changed is caught
// here, before anything is authored.
func laneConflict(pairs []types.RunHarness, lanes []providerLane) (int, string) {
	for j := 1; j < len(lanes); j++ {
		b := lanes[j]
		if b.chosen == nil {
			continue
		}
		for i, a := range lanes[:j] {
			if a.chosen == nil || !harnessProviderPairsConflict(pairs[i].Harness, a.chosen.provider, pairs[j].Harness, b.chosen.provider) {
				continue
			}
			host := b.azure.host
			for _, h := range providerInjectionHosts(b.chosen.provider, pairs[j].Harness) {
				if slices.ContainsFunc(providerInjectionHosts(a.chosen.provider, pairs[i].Harness), func(x string) bool { return hostEqual(x, h) }) {
					host = h
					break
				}
			}
			return j, host
		}
	}
	return 0, ""
}

// sharesKeyGrant is whether key lane i shares an earlier key lane's grant: the
// same host, which laneConflict has already let through only when the two
// would author the same grant (harnessProviderPairsConflict).
func sharesKeyGrant(pairs []types.RunHarness, lanes []providerLane, i int) bool {
	b := lanes[i]
	for k, a := range lanes[:i] {
		if a.chosen != nil && providerKeyKind(a.chosen.provider.Kind) && hostEqual(a.key.host, b.key.host) &&
			!harnessProviderPairsConflict(pairs[k].Harness, a.chosen.provider, pairs[i].Harness, b.chosen.provider) {
			return true
		}
	}
	return false
}

// providerLanePairs is the (harness, model provider) pairs dispatch gives a
// lane: the run's own agent and provider first, exactly as a run launched
// without the harness set, then each other harness of the set that was granted
// a provider.
func providerLanePairs(run types.AgentRun, harnesses []types.RunHarness) []types.RunHarness {
	pairs := []types.RunHarness{{Harness: run.Agent, ModelProviderID: run.ModelProviderID}}
	for _, h := range harnesses {
		if h.Harness != run.Agent && h.ModelProviderID != "" {
			pairs = append(pairs, h)
		}
	}
	return pairs
}

// laneRun is run as the lane of harness h sees it: h's harness as its agent and
// h's provider as its choice. Every per-provider check, env arm, grant and audit
// row of that lane reads it, so each names its own harness and provider.
func laneRun(run types.AgentRun, h types.RunHarness) types.AgentRun {
	run.Agent, run.ModelProviderID = h.Harness, h.ModelProviderID
	return run
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
	if p.Kind == types.ModelProviderAzureFoundry {
		var ok bool
		if lane.azure, ok = providerAzureLaneFor(p, run.Agent); !ok {
			return providerLane{}, p.Kind, notServing
		}
		lane.azure.owner = lane.chosen.owner
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
// ("" when it chose none); `kind` is omitted when it is not known (an
// unreadable block, or a provider id that does not exist); a credential refusal adds `reason:
// model_credential`, the class the console's audit reader (runEndingFromAudit)
// grades a credential ending by. extra adds what else the refusal names (an
// env_secret refusal's `variable` and `grant`).
func (s *Server) refuseProviderDispatch(ctx context.Context, run types.AgentRun, kind types.ModelProviderKind, d providerDenial, extra map[string]any) {
	s.failAndRevoke(ctx, run.ID, types.RunStarting, d.msg)
	data := map[string]any{"error": d.msg, "provider": run.ModelProviderID}
	maps.Copy(data, extra)
	if kind != "" {
		data["kind"] = kind
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
// bedrock-api-key, and any grant on a model vendor's host (every harness's,
// not only this run's: an OpenAI key on a claude-code run is still a model
// credential), on the boot gateway that re-points one, on a host the chosen
// provider's arm credentials (laneHosts), or on any other host that serves a
// model here — every provider row's, chosen or not (serving, modelServingHosts).
// Such a grant comes from a stored or default policy, a recorded profile, a
// workspace requirement or a legacy fold, and it reads the operator's
// credential — or, on the arm's own host, would be a second injection for one
// host, which fails the sidecar at startup. It must run before every arm
// authors: it deletes the very names the arms write.
func (s *Server) dropLegacyModelInjections(ctx context.Context, run types.AgentRun, injections []runner.InjectionGrant, laneHosts []string, serving func(string) bool) []runner.InjectionGrant {
	model := s.modelCredentialInjection(laneHosts, serving)
	return slices.DeleteFunc(injections, func(ig runner.InjectionGrant) bool {
		if model(ig.Rule) {
			s.auditDroppedInjection(ctx, run, ig, "model_credential_not_provider_authored")
			return true
		}
		return false
	})
}

// modelCredentialInjection is the strip's test: whether an injection rule
// would credential a model, by the secret it names (a provider's, a retired
// operator-lane name or sentinel) or the host it is bound for
// (dropLegacyModelInjections' list). A revive applies it to a stored
// config (stripRevivedModelInjections).
func (s *Server) modelCredentialInjection(laneHosts []string, serving func(string) bool) func(egress.InjectionRule) bool {
	hosts := slices.Clone(laneHosts)
	for _, h := range harnessCatalog {
		if h.Gateway != nil {
			hosts = append(hosts, h.Gateway.host)
		}
	}
	return func(r egress.InjectionRule) bool {
		name := r.SecretName
		return strings.HasPrefix(name, providerSecretPrefix) || slices.Contains(retiredModelCredentialNames, name) ||
			name == types.SubscriptionOAuthSecret || name == types.ManagedOAuthSecret ||
			name == types.AWSSSOAccessTokenSecret || name == bedrockAPIKeySecret ||
			slices.ContainsFunc(hosts, func(h string) bool { return h != "" && hostEqual(h, r.Host) }) ||
			serving(r.Host)
	}
}

func (s *Server) auditDroppedInjection(ctx context.Context, run types.AgentRun, ig runner.InjectionGrant, reason string) {
	data := map[string]any{"grant_id": ig.GrantID, "secret_name": ig.Rule.SecretName, "host": ig.Rule.Host, "reason": reason}
	if run.ModelProviderID != "" {
		data["provider"] = run.ModelProviderID
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.injection.drop",
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
		case c.provider.Kind == types.ModelProviderAzureFoundry:
			return s.providerAzureTransport(sandboxEnv, *c, lane.azure)
		}
	}
	switch run.Agent {
	case "claude-code":
		sandboxEnv[envAnthropicAPIKey] = "wardyn-proxy-injected"
		if model := lane.key.model; model != "" {
			sandboxEnv[envAnthropicModel] = model
		}
	case "codex-cli":
		// codex 0.149.1 reads neither OPENAI_* variable: its base URL comes
		// from config.toml and its key from CODEX_API_KEY (task mode) or
		// auth.json (interactive), both of which the image's prep writes
		// from these two. The OPENAI_* pair is unused by codex and deprecated: only the
		// pinned codex is supported.
		sandboxEnv[envCodexBaseURL] = codexBaseURL(proxyURL, lane.key.upstream)
		sandboxEnv[envCodexAPIKey] = "wardyn-proxy-injected"
		sandboxEnv[envOpenAIBaseURL] = proxyURL + "/wardyn/llm/openai"
		sandboxEnv[envOpenAIAPIKey] = "wardyn-proxy-injected"
	}
	return llmTransport{provider: lane.chosen}
}

// codexBaseURL is the base URL codex is configured with: the brokered openai
// route plus the path its final upstream needs. codex appends `responses`, the
// proxy strips /wardyn/llm/openai and prepends a configured gateway's own path
// prefix, so the vendor host (no gateway) and a gateway whose base URL has an
// empty path both need the `/v1` here, and a gateway that carries a prefix
// must not get a second one.
func codexBaseURL(proxyURL, gateway string) string {
	base := proxyURL + "/wardyn/llm/openai"
	if u, err := url.Parse(gateway); gateway != "" && err == nil && strings.TrimSuffix(u.Path, "/") != "" {
		return base
	}
	return base + "/v1"
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
		"snapshot":    providerGrantSnapshot{OwnerSubject: lane.owner, ProviderUID: lane.provider.UID, Harness: run.Agent},
	})
	grantID := uuid.New()
	if _, err := s.createDispatchGrant(ctx, run, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600},
	}, false); err != nil {
		s.refuseProviderDispatch(ctx, run, lane.provider.Kind, providerDenial{msg: "could not author the model provider credential injection: " + err.Error()}, nil)
		return runner.InjectionGrant{}, false
	}
	rule, err := injectionRuleFromScope(scope)
	if err != nil {
		s.refuseProviderDispatch(ctx, run, lane.provider.Kind, providerDenial{msg: "could not compile the model provider credential injection: " + err.Error()}, nil)
		return runner.InjectionGrant{}, false
	}
	return runner.InjectionGrant{GrantID: grantID, Rule: rule}, true
}
