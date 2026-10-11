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

// localCredentialIntents reads admitted configuration through the same provider,
// ADO and redirect predicates dispatch reads. It never renews/mints or reads a
// secret value: owner namespace proof is the own List, not fallback Get.
func (s *Server) localCredentialIntents(r *http.Request, req createRunRequest, spec types.RunPolicySpec,
	workspaces []types.Workspace, sc types.SiteConfig,
) ([]placement.CredentialIntent, *runRefusal) {
	_, actor := actorFromRequest(r)
	owner := runIdentitySubject(r.Context(), actor)
	var out []placement.CredentialIntent
	choice, ref := s.authorizeRunModelProvider(r, req, workspaces, true)
	if ref != nil {
		return nil, ref
	}
	if choice.chosen {
		intent, ok := s.localProviderIntent(r, owner, choice.provider)
		if !ok {
			return nil, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), "model_provider: unclassified credential kind")
		}
		out = append(out, intent)
	}
	var laneHosts []string
	for _, repo := range append(repoLocatorsOf(spec.WorkspaceRepos), req.Repo) {
		a, on := adoEntraRunForRepo(sc, repo, owner)
		if !on {
			continue
		}
		laneHosts = append(laneHosts, adoEntraEgressEntries(a.org)...)
		out = append(out, placement.CredentialIntent{Field: "ProxyConfig.ADOGrant[" + a.rowID + "]", Origin: s.localADOOrigin(r.Context(), owner, a)})
	}
	out = append(out, s.localRedirectIntents(r, spec, sc, laneHosts)...)
	return out, nil
}

func (s *Server) localProviderIntent(r *http.Request, owner string, p types.ModelProvider) (placement.CredentialIntent, bool) {
	part, field := providerKeyPart, "ProxyConfig.Injection[model_provider:"+p.ID+"]"
	switch p.Kind {
	case types.ModelProviderAnthropicAPIKey, types.ModelProviderOpenAIAPIKey, types.ModelProviderCustomEndpoint, types.ModelProviderBedrockBearer:
	case types.ModelProviderAnthropicSubscription:
		part = providerOAuthPart
	case types.ModelProviderBedrockSSO:
		part = providerSSOPart
	case types.ModelProviderAzureFoundry:
		part = providerEntraPart
		field = "ProxyConfig.AzureGates[model_provider:" + p.ID + "]"
	default:
		return placement.CredentialIntent{}, false
	}
	own := owner != "" && p.UID != "" && s.ownsSecretMemoized(r.Context(), owner, providerSecretName(p.UID, part))
	return placement.CredentialIntent{Field: field, Origin: placement.CredentialOrigin{Class: placement.ClassOwn, Stored: true, OwnNamespace: own, OwnerOnly: own}}, true
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
// other token mode is minted by the organisation.
func (s *Server) localADOOrigin(ctx context.Context, owner string, a adoEntraRun) placement.CredentialOrigin {
	if a.tokenMode != types.ADOTokenModeOwnPAT || owner == "" || a.owner != owner || !adoEntraValidRowID(a.rowID) {
		return placement.CredentialOrigin{Class: placement.ClassBrokered, Delivery: placement.ClassADOMintedPAT}
	}
	own := s.ownsSecretMemoized(ctx, owner, adoOwnPATSecretName(a.rowID))
	return placement.CredentialOrigin{Class: placement.ClassOwn, Stored: true, OwnNamespace: own, OwnerOnly: own}
}
