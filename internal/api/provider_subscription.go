// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Dispatch for a run that chose a model provider, and the per-person Claude
// subscription arm (MP-8). A chosen provider's arm is the only credential
// author for its run: none of the legacy lanes (the host ~/.claude mount, the
// operator's managed token, Bedrock, the operator's api key) credentials it.
//
// The subscription arm serves the run owner's OWN Claude sign-in, stored under
// wardyn-provider-<uid>-oauth in their own namespace, and nobody else's. The
// shared-subscription posture (SubscriptionPostureOK) does not apply to it: that
// posture stops one person's subscription serving others, which this lane
// cannot do. perPersonSubscriptionPosture replaces it for this lane.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending). The states and remedies of the run refusal
// (mpRunRefusal) for a chosen Claude subscription, plus the sink's refusals.
const (
	mpSubNotSignedIn  = "you are not signed in to Claude for it"
	mpSubNoStore      = "this install has no secret store to hold your Claude sign-in"
	mpSubNotPerson    = "the admin token is a shared credential rather than a person, so it has no Claude sign-in of its own"
	mpSubNoImage      = "the Claude sign-in image it needs is not built on this install"
	mpRunRemedySignIn = "connect it from Getting started in the console, or from the banner the console shows on every page."
	mpRunRemedyPerson = "sign in to the console, or use your own wdn_ API token."
	mpRunUnreadable   = "Wardyn couldn't read its model providers just now — nothing was started. Try again in a moment."
	mpSubReadFailed   = "Wardyn couldn't read your Claude sign-in for model provider %s just now — nothing was started. Try again in a moment."

	mpSubSinkNotRecorded = "a per-person Claude sign-in is injected only through the grant Wardyn authors when a run " +
		"launches on its model provider, which records whose sign-in it is; this grant carries no such record"
	mpSubSinkChanged = "this run's model provider was removed, turned off or changed after the run started, " +
		"so its Claude sign-in is no longer injected"
	mpSubSinkHost = "a Claude sign-in may only be injected to its model provider's own host"
)

// chosenProvider is the provider a run chose and whose credential serves it:
// always the run owner's own (runIdentitySubject of the run's creator).
type chosenProvider struct {
	provider types.ModelProvider
	owner    string
}

// perPersonSubscriptionPosture is the per-person posture predicate: "" when
// this install can serve owner a Claude subscription of their own, else the
// state sentence that says why not. Unlike SubscriptionPostureOK it holds on
// Kubernetes and with OIDC, because no credential here serves anyone but its
// owner. Its clauses:
//   - a capture lands in its capturer's own namespace, and the sink resolves the
//     sentinel only from the run owner's: that needs a secret store, and a person
//     to own the namespace (the admin token under OIDC is a mechanism, not a
//     person). The reads themselves are ownSecret's strict List-then-Get, keyed
//     on the owner — never the operator row;
//   - the login image resolves (MP-14), since a sign-in needs it.
func (s *Server) perPersonSubscriptionPosture(ctx context.Context, owner string) string {
	switch {
	case s.cfg.Secrets == nil:
		return mpSubNoStore
	case owner == "" || (s.cfg.OIDC != nil && owner == adminTokenPrincipal):
		return mpSubNotPerson
	case !claudeSignInImageResolves(ctx, s.cfg.AgentImages, s.cfg.Runner):
		return mpSubNoImage
	}
	return ""
}

// providerSubscriptionRefusal is the liveness check for a chosen Claude
// subscription, shared by create, Review and dispatch: "" when owner may run on
// p, else the whole refusal sentence. A store failure is returned, never read
// as "not signed in".
func (s *Server) providerSubscriptionRefusal(ctx context.Context, p types.ModelProvider, owner string) (string, error) {
	if state := s.perPersonSubscriptionPosture(ctx, owner); state != "" {
		remedy := mpRunRemedy
		if state == mpSubNotPerson {
			remedy = mpRunRemedyPerson
		}
		return fmt.Sprintf(mpRunRefusal, p.ID, state, remedy), nil
	}
	_, found, err := s.ownSecret(ctx, owner, providerSecretName(p.UID, providerOAuthPart))
	if err != nil {
		return "", err
	}
	if !found {
		return fmt.Sprintf(mpRunRefusal, p.ID, mpSubNotSignedIn, mpRunRemedySignIn), nil
	}
	return "", nil
}

// ownerSubscriptionToken is the per-person managed-token provider: the managed
// token provider over the strict read of owner's own wardyn-provider-<uid>-oauth
// blob (a managedCredBlob), in place of the one built at boot over the
// operator's row. Built per resolve, so nothing outlives the request.
func (s *Server) ownerSubscriptionToken(owner, uid string) subscription.Provider {
	name := providerSecretName(uid, providerOAuthPart)
	return &managedCredProvider{provider: "claude", get: func(ctx context.Context) ([]byte, bool, error) {
		return s.ownSecret(ctx, owner, name)
	}}
}

// providerSubscriptionBase is where a subscription provider's runs send
// requests: its route-through gateway, else the vendor host.
func providerSubscriptionBase(p types.ModelProvider) string {
	return cmp.Or(p.BaseURL, "https://"+subscriptionInjectionHost)
}

// resolveProviderTransport is dispatch for a run that chose a model provider.
// It re-reads the provider and refuses, naming it, when it is gone, off, no
// longer serves the run's agent, or its kind has no arm; then the kind's arm
// decides the transport. Fails closed on an unreadable provider block. ok=false
// means the run is already marked FAILED.
func (s *Server) resolveProviderTransport(ctx context.Context, run types.AgentRun, p dispatchParams,
	policy *types.RunPolicySpec, sandboxEnv map[string]string, siteCfg types.SiteConfig, siteCfgOK bool,
) (llmTransport, bool) {
	if !isModelRun(p.TaskMode, run.WorkspaceID, run.SourceID, p.Interactive) || run.Task == harnessLoginTask {
		// A run that calls no model gets no model credential.
		return llmTransport{}, true
	}
	fail := func(kind types.ModelProviderKind, msg string) (llmTransport, bool) {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"error": msg, "provider": run.ModelProviderID, "kind": kind,
			})))
		return llmTransport{}, false
	}
	if !siteCfgOK {
		return fail("", mpRunUnreadable)
	}
	mp, found := modelProviderByID(siteCfg.ModelProviders, run.ModelProviderID)
	switch {
	case !found:
		return fail("", providerRefusal(run.ModelProviderID, mpRunStateMissing).refusal)
	case mp.Disabled:
		return fail(mp.Kind, providerRefusal(mp.ID, mpRunStateOff).refusal)
	case !mp.Serves(run.Agent):
		return fail(mp.Kind, providerRefusal(mp.ID, fmt.Sprintf(mpRunStateNotServing, run.Agent)).refusal)
	}
	// The host ~/.claude mount is the operator's credential: no provider's
	// run carries it beside its own.
	policy.WorkspaceMounts = slices.DeleteFunc(policy.WorkspaceMounts, func(m types.WorkspaceMount) bool {
		return m.Target == claudeCredTarget || m.Target == claudeCredJSONTarget
	})
	switch mp.Kind {
	case types.ModelProviderBedrockSSO, types.ModelProviderBedrockBearer:
		return s.providerBedrockTransport(ctx, run, policy, sandboxEnv, mp, fail)
	case types.ModelProviderAnthropicSubscription:
		owner := runIdentitySubject(ctx, run.CreatedBy)
		refusal, err := s.providerSubscriptionRefusal(ctx, mp, owner)
		if err != nil {
			return fail(mp.Kind, fmt.Sprintf(mpSubReadFailed, mp.ID))
		}
		if refusal != "" {
			return fail(mp.Kind, refusal)
		}
		return s.providerSubscriptionTransport(run, sandboxEnv, chosenProvider{provider: mp, owner: owner}), true
	default:
		return fail(mp.Kind, fmt.Sprintf(mpRunNotYet, mp.ID))
	}
}

// providerSubscriptionTransport wires the sandbox for the run owner's own
// Claude subscription: the managed lane's wire posture (direct to the vendor or
// the provider's route-through, over the tunnel, with a writable config dir and
// inert sentinel creds delivered via env), with the owner's live token injected
// proxy-side.
func (s *Server) providerSubscriptionTransport(run types.AgentRun,
	sandboxEnv map[string]string, c chosenProvider,
) llmTransport {
	sandboxEnv["ANTHROPIC_BASE_URL"] = providerSubscriptionBase(c.provider)
	sandboxEnv["CLAUDE_CONFIG_DIR"] = "/home/agent/.claude-run"
	sandboxEnv["WARDYN_CLAUDE_MANAGED_B64"] = managedSentinelCredsB64()
	for _, h := range c.provider.Harnesses {
		if h.Harness == run.Agent && h.Model != "" {
			sandboxEnv["ANTHROPIC_MODEL"] = h.Model
		}
	}
	return llmTransport{modelRun: true, provider: &c}
}

// providerGrantSnapshot is what dispatch records on a provider grant it
// authors (a Claude sign-in sentinel, a Bedrock key): which provider's
// credential it is and whose. It grants nothing: the sink still requires the
// owner to be the run token's own subject and the provider to be the run's,
// live.
type providerGrantSnapshot struct {
	ProviderUID  string `json:"provider_uid"`
	OwnerSubject string `json:"owner_subject"`
}

// authorProviderSubscriptionInjection authors the per-person subscription
// grant: the sentinel wardyn-provider-<uid>-oauth, host-pinned to the
// provider's host, recording the owner. It replaces every other injection for
// the vendor host and the provider's host, so no other credential rides
// beside the run's own. Returns the per-run MITM host for a route-through
// gateway (the vendor host is a built-in one). ok=false: the run is FAILED.
func (s *Server) authorProviderSubscriptionInjection(ctx context.Context, run types.AgentRun, t llmTransport,
	policy *types.RunPolicySpec, injections []runner.InjectionGrant,
) ([]runner.InjectionGrant, []string, bool) {
	c := t.provider
	base := providerSubscriptionBase(c.provider)
	var mitmHosts []string
	if c.provider.BaseURL != "" {
		mitmHosts = []string{gatewayHostPort(base)}
	}
	injections, ok := s.authorOAuthSentinelGrant(ctx, run, policy, injections, oauthSentinelGrant{
		host:     gatewayHost(base),
		dropHost: subscriptionInjectionHost,
		sentinel: providerSecretName(c.provider.UID, providerOAuthPart),
		source:   "provider",
		detail:   "the run owner's own Claude sign-in injected proxy-side; sandbox holds only an inert sentinel delivered via env",
		provider: c.provider.ID,
		snapshot: providerGrantSnapshot{ProviderUID: c.provider.UID, OwnerSubject: c.owner},
	})
	return injections, mitmHosts, ok
}

// dropUnauthoredProviderInjections removes every injection naming a
// per-person model-provider credential (wardyn-provider-<uid>-oauth / -sso /
// -key), auditing each. Their one author is dispatch, which runs after this; a
// stored, inline or recorded policy's grant carries no record of whose
// credential it is, and the sink refuses it — which would fail the proxy's
// startup.
func (s *Server) dropUnauthoredProviderInjections(ctx context.Context, run types.AgentRun, injections []runner.InjectionGrant) []runner.InjectionGrant {
	return slices.DeleteFunc(injections, func(ig runner.InjectionGrant) bool {
		if !strings.HasPrefix(ig.Rule.SecretName, providerSecretPrefix) {
			return false
		}
		reason := "provider_key_not_dispatch_authored"
		if providerSignInSecret(ig.Rule.SecretName) {
			reason = "provider_signin_not_dispatch_authored"
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.injection.dropped",
			ig.GrantID.String(), "denied", mustJSON(map[string]any{
				"grant_id": ig.GrantID, "secret_name": ig.Rule.SecretName, "host": ig.Rule.Host,
				"reason": reason,
			})))
		return true
	})
}

// providerOAuthUID returns the provider UID a wardyn-provider-<uid>-oauth
// sentinel names.
func providerOAuthUID(name string) (string, bool) {
	uid, ok := strings.CutPrefix(name, providerSecretPrefix)
	if !ok {
		return "", false
	}
	uid, ok = strings.CutSuffix(uid, "-"+providerOAuthPart)
	return uid, ok && uid != ""
}

// resolveProviderSubscriptionInjection is the per-person Claude subscription
// arm of handleInternalInjection. handled=false means the grant names another
// secret. It takes nothing from the grant but the name: the provider is
// re-read by UID and must still be the run's own, on and serving its agent;
// the host is the provider's; the owner is the run token's subject, whose own
// namespace alone is read. The shared-subscription posture 403 does not apply:
// keyed by sentinel name, it stays on the two legacy sentinels only.
func (s *Server) resolveProviderSubscriptionInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	name := minted.Injection.SecretName
	uid, ok := providerOAuthUID(name)
	if !ok {
		return false
	}
	ctx := r.Context()
	fail := func(status int, reason, body string) bool {
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", name, "failure",
			mustJSON(map[string]any{"reason": reason, "grant_id": grantID, "source": "provider"})))
		writeError(w, status, body)
		return true
	}
	var rec providerGrantSnapshot
	if !s.grantSnapshot(ctx, claims.RunID, grantID, &rec) || rec.ProviderUID != uid || rec.OwnerSubject == "" {
		return fail(http.StatusForbidden, "missing_scope_snapshot", mpSubSinkNotRecorded)
	}
	// The run token, not the grant, is authority for whose run this is.
	if rec.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, "owner_mismatch", mpSubSinkNotRecorded)
	}
	run, err := s.cfg.Store.GetRun(ctx, claims.RunID)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "run_unreadable", credentialReauthRunUnreadableBody)
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "providers_unreadable", mpRunUnreadable)
	}
	p, found := modelProviderByID(sc.ModelProviders, run.ModelProviderID)
	if !found || p.UID != uid || p.Kind != types.ModelProviderAnthropicSubscription || p.Disabled || !p.Serves(run.Agent) {
		return fail(http.StatusForbidden, "provider_changed", mpSubSinkChanged)
	}
	if !hostEqual(minted.Injection.Host, gatewayHost(providerSubscriptionBase(p))) {
		return fail(http.StatusForbidden, "oauth-host-not-provider", mpSubSinkHost)
	}
	tok, err := s.ownerSubscriptionToken(claims.Sub, uid).Current(ctx)
	if err != nil {
		return fail(http.StatusFailedDependency, "resolve-failed", fmt.Sprintf(mpRunRefusal, p.ID, mpSubNotSignedIn, mpRunRemedySignIn))
	}
	// One correct wire shape, whatever the grant says (see the legacy arm).
	formatted := formatInjectionValue("Bearer %s", []byte(tok.Value))
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(tok.Value))
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(formatted))
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", name, "success",
		mustJSON(map[string]any{
			"purpose": "proxy-injection-subscription", "grant_id": grantID, "jti": minted.JTI,
			"source": "provider", "provider": p.ID, "owner": claims.Sub,
		})))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: minted.Injection.Host, Header: "Authorization", Value: formatted, JTI: minted.JTI,
	})
	return true
}

// oauthSentinelGrant is one OAuth sentinel grant for authorOAuthSentinelGrant.
type oauthSentinelGrant struct {
	host     string // where the token goes; also joins the allowlist
	dropHost string // a second host whose injections it replaces ("" = none)
	sentinel string
	source   string
	detail   string
	provider string // the chosen provider's id, audited; "" on the legacy lanes
	snapshot any    // recorded under the scope's "snapshot" key; nil on the legacy lanes
}

// authorOAuthSentinelGrant writes one OAuth sentinel grant for run and
// appends its injection, replacing any other injection for the same hosts —
// two injections for one host crash the sidecar when the second's secret is
// absent. ok=false: the grant write failed and the run is FAILED.
func (s *Server) authorOAuthSentinelGrant(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec,
	injections []runner.InjectionGrant, g oauthSentinelGrant,
) ([]runner.InjectionGrant, bool) {
	injections = slices.DeleteFunc(injections, func(ig runner.InjectionGrant) bool {
		return hostEqual(ig.Rule.Host, g.host) || (g.dropHost != "" && hostEqual(ig.Rule.Host, g.dropHost))
	})
	scope := map[string]any{"host": g.host, "header": "Authorization", "format": "Bearer %s", "secret_name": g.sentinel}
	if g.snapshot != nil {
		scope["snapshot"] = g.snapshot
	}
	raw, _ := json.Marshal(scope)
	grantID := uuid.New()
	if _, err := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: raw, TTLSeconds: 3600},
	}); err != nil {
		// CAS from STARTING (claimed at dispatch entry) so a concurrent kill's
		// KILLED state is preserved rather than clobbered back to FAILED.
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "could not author the "+g.source+" credential injection: "+err.Error())
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": g.source + " inject grant: " + err.Error()})))
		return injections, false
	}
	if rule, err := injectionRuleFromScope(raw); err == nil {
		injections = append(injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
	}
	unionAllowedDomains(policy, []string{g.host})
	data := map[string]any{"host": g.host, "tls_mitm": true, "source": g.source, "detail": g.detail}
	if g.provider != "" {
		data["provider"] = g.provider
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm.subscription_inject",
		run.ID.String(), "success", mustJSON(data)))
	return injections, true
}
