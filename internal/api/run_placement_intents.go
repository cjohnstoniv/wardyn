// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// localAWSCLIRefusal refuses the AWS CLI on a local placement at every door:
// dispatch authors its sign-in as a credential local placement does not carry,
// so the run would otherwise be accepted and then fail as it starts.
const localAWSCLIRefusal = "This run uses the AWS CLI, which a run on your own runner cannot carry yet. " +
	"Remove the AWS CLI from the run, or run it on the organisation's runners."

// localCredentialIntents reads admitted configuration through the same provider,
// ADO and redirect predicates dispatch reads. It never renews/mints or reads a
// secret value: owner namespace proof is the own List, not fallback Get.
// upstreams is the LLMUpstreams table dispatch would author for every harness's
// provider lane (providerKeyLaneFor), so create judges it as dispatch does.
func (s *Server) localCredentialIntents(r *http.Request, req createRunRequest, spec types.RunPolicySpec,
	comps runComponents, workspaces []types.Workspace, sc types.SiteConfig,
) (_ []placement.CredentialIntent, upstreams map[string]string, _ *runRefusal) {
	if comps.awsCLI() != nil {
		return nil, nil, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), localAWSCLIRefusal)
	}
	_, actor := actorFromRequest(r)
	owner := runIdentitySubject(r.Context(), actor)
	var out []placement.CredentialIntent
	providerLane := func(choice runProviderChoice, agent string) *runRefusal {
		intents, ok := s.localProviderIntent(r, owner, choice.provider)
		if !ok {
			return runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), "model_provider: unclassified credential kind")
		}
		out = append(out, intents...)
		if l, ok := providerKeyLaneFor(choice.provider, agent); ok && l.upstream != "" {
			if upstreams == nil {
				upstreams = map[string]string{}
			}
			upstreams[l.vendorHost] = l.upstream
		}
		return nil
	}
	choice, ref := s.authorizeRunModelProvider(r, req, workspaces, true)
	if ref != nil {
		return nil, nil, ref
	}
	if choice.chosen {
		if ref := providerLane(choice, req.Agent); ref != nil {
			return nil, nil, ref
		}
	}
	// Every other harness's provider, picked as judgeHarnessProviders picks it. A
	// refusal of that pick, or a sign-in that must be the primary's, is left to
	// stepModelProvider, which owns those answers.
	for _, h := range harnessSet(req) {
		if h.Harness == req.Agent {
			continue
		}
		hreq := req
		hreq.Agent, hreq.ModelProvider = h.Harness, h.ModelProviderID
		hc, ref := s.authorizeRunModelProvider(r, hreq, nil, true)
		if ref != nil || !hc.chosen || harnessProviderNeedsPrimary(hc.provider) {
			continue
		}
		if ref := providerLane(hc, h.Harness); ref != nil {
			return nil, nil, ref
		}
	}
	// The one lane dispatch would author, from the same inputs: one intent, and
	// the exact hosts dispatch hands planArtifactRedirect. None when it is off.
	lane, on := resolveADOEntraRun(sc, spec.WorkspaceRepos, req.Repo, adoLaneOwnerOf(r))
	if on {
		out = append(out, placement.CredentialIntent{Field: "ProxyConfig.ADOGrant[" + lane.rowID + "]", Origin: s.localADOOrigin(r.Context(), lane.owner, lane)})
	}
	out = append(out, s.localRedirectIntents(r, spec, sc, resolvedLaneHosts(lane, on))...)
	return out, upstreams, nil
}

// localProviderIntent is every credential the chosen provider's dispatch arm
// places in a spec: its proxy-side credential, and for bedrock_sso with
// proxy-inject off the real token cache applyBedrockTransport puts in
// SecretEnv, so create refuses what dispatch would refuse after Launch.
func (s *Server) localProviderIntent(r *http.Request, owner string, p types.ModelProvider) ([]placement.CredentialIntent, bool) {
	part, field := providerKeyPart, "ProxyConfig.Injection[model_provider:"+p.ID+"]"
	if providerTakesNoCredential(p) {
		return nil, true // places no credential; its LLMUpstreams entry is what a local runner refuses
	}
	switch p.Kind {
	case types.ModelProviderAnthropicAPIKey, types.ModelProviderOpenAIAPIKey, types.ModelProviderCustomEndpoint, types.ModelProviderBedrockBearer,
		types.ModelProviderLocalModel:
	case types.ModelProviderAnthropicSubscription:
		part = providerOAuthPart
	case types.ModelProviderBedrockSSO:
		part = providerSSOPart
	case types.ModelProviderAzureFoundry:
		part = providerEntraPart
		field = "ProxyConfig.AzureGates[model_provider:" + p.ID + "]"
	default:
		return nil, false
	}
	own := owner != "" && p.UID != "" && s.ownsSecretMemoized(r.Context(), owner, providerSecretName(p.UID, part))
	out := []placement.CredentialIntent{{Field: field, Origin: placement.CredentialOrigin{Class: placement.ClassOwn, Stored: true, OwnNamespace: own, OwnerOnly: own}}}
	if p.Kind == types.ModelProviderBedrockSSO && !s.cfg.AWSSSOProxyInject {
		out = append(out, placement.CredentialIntent{Field: bedrockSSOSecretEnvField, Origin: bedrockSSOSecretEnvOrigin(own)})
	}
	return out, true
}

// bedrockSSOSecretEnvField is the spec path of a captured AWS SSO token cache
// that dispatch placed in SecretEnv (proxy-inject off).
const bedrockSSOSecretEnvField = "SandboxSpec.SecretEnv[" + awsSSOConfigEnvVar + "]"

// bedrockSSOSecretEnvOrigin is that token cache's origin, at create and at
// dispatch: the person's own session, delivered as resident role credentials.
func bedrockSSOSecretEnvOrigin(own bool) placement.CredentialOrigin {
	return placement.CredentialOrigin{Class: placement.ClassOwn, Delivery: placement.ClassBedrockRoleCreds, Stored: true, OwnNamespace: own, OwnerOnly: own}
}

func (s *Server) localRedirectIntents(r *http.Request, spec types.RunPolicySpec, sc types.SiteConfig, laneHosts []string) []placement.CredentialIntent {
	present := map[string]bool{}
	if s.cfg.Secrets != nil {
		if names, err := s.cfg.Secrets.List(r.Context()); err == nil {
			for _, name := range names {
				present[name] = true
			}
		}
	}
	have := artifactRunHostSet(spec.AllowedDomains)
	seen := map[string]bool{}
	var out []placement.CredentialIntent
	for i, row := range sc.EgressRedirects {
		host := strings.ToLower(hostrules.HostOf(row.To))
		if host == "" || seen[host] || !artifactRedirectApplies(row, have) || s.modelServingHosts(sc)(host) || laneCarriesHost(laneHosts, host) {
			continue
		}
		token, why := s.resolveRedirectToken(r.Context(), row, present)
		if why != "" || token.secretName == "" {
			continue
		}
		seen[host] = true
		out = append(out, placement.CredentialIntent{Field: "ProxyConfig.Injection[egress_redirect:" + strconv.Itoa(i) + "]", Origin: placement.CredentialOrigin{Class: placement.ClassOperator, Stored: true, Delivery: placement.ClassAPIKey}})
	}
	return out
}

// localADOOrigin is the one origin of an Azure DevOps lane, at admission and at
// dispatch: own only for the owner's own PAT proven in their namespace; every
// other token mode is minted by the organisation. Only a minted_pat connection's
// opt-in counts toward runner_resident: a bearer token keeps refusing.
func (s *Server) localADOOrigin(ctx context.Context, owner string, a adoEntraRun) placement.CredentialOrigin {
	if a.tokenMode != types.ADOTokenModeOwnPAT || owner == "" || a.owner != owner || !adoEntraValidRowID(a.rowID) {
		return placement.CredentialOrigin{Class: placement.ClassBrokered, Delivery: placement.ClassADOMintedPAT, ResidentOptIn: a.residentOptIn && a.tokenMode == types.ADOTokenModeMintedPAT}
	}
	own := s.ownsSecretMemoized(ctx, owner, adoOwnPATSecretName(a.rowID))
	return placement.CredentialOrigin{Class: placement.ClassOwn, Stored: true, OwnNamespace: own, OwnerOnly: own}
}
