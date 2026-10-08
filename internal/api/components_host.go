// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// componentHostBounds is what a component's destinations are held to on one
// request. Every comparison of a component host against another destination is
// types.Destination.OverlapsAtAnyPort: the parse is the proxy's own, so case, a
// trailing dot, a port and a wildcard cannot be read one way here and another
// way at the proxy, and no call site strips a port on its own.
type componentHostBounds struct {
	// deny is the deployment's and the caller's deny lists and every referenced
	// workspace's permanent denies. Create only folds the workspace half into
	// the spec after the run exists, so it is named here rather than read back.
	deny []string
	// model is every destination that serves a model on this deployment.
	model []types.Destination
	// serving is the deployment's own one-host answer to the same question.
	serving func(string) bool
	// credentialed is every host that already carries a proxy-injected
	// credential on this run; the gate adds each header host it admits.
	credentialed []types.Destination
}

func (s *Server) newComponentHostBounds(r *http.Request, sc types.SiteConfig, spec types.RunPolicySpec,
	ceiling governanceCeiling, wsRefs []types.Workspace,
) componentHostBounds {
	return componentHostBounds{
		deny:         slices.Concat(ceiling.Spec.DeniedDomains, spec.DeniedDomains, workspaceDeniedEgress(wsRefs)),
		model:        modelServingDestinations(sc),
		serving:      s.modelServingHosts(sc),
		credentialed: credentialedDestinations(sc, spec, runIdentitySubject(r.Context(), principalFromRequest(r))),
	}
}

// workspaceDeniedEgress is the permanent denies of the workspaces a run
// references — the hosts unionWorkspaceEgress folds into its deny list.
func workspaceDeniedEgress(wsRefs []types.Workspace) []string {
	var out []string
	for _, ws := range wsRefs {
		out = append(out, ws.DeniedEgress...)
	}
	return out
}

// modelServingDestinations is every destination that serves a model on this
// deployment, as a SET a component host is compared against — the vendors'
// hosts and every model provider row's, chosen by the run or not. A set and
// not modelServingHosts' predicate alone, because a predicate over one host
// cannot answer for a wildcard: "*.openai.com" names no host to ask about and
// still covers one that serves a model.
func modelServingDestinations(sc types.SiteConfig) []types.Destination {
	// isModelProviderHost's convention, spelled as destinations.
	hosts := []string{"anthropic.com", "*.anthropic.com", "api.openai.com"}
	for _, h := range harnessCatalog {
		if h.Gateway != nil {
			hosts = append(hosts, h.Gateway.host)
		}
	}
	for _, p := range modelProviderRows(sc) {
		hosts = append(hosts, providerHost(p))
		if b := providerBedrockSettings(p); p.Kind.IsBedrock() && b.Region != "" {
			hosts = append(hosts, providerBedrockRuntimeHost(p), bedrockControlHost(b.Region))
		}
	}
	return parseDestinations(hosts)
}

// credentialedDestinations is every host that carries a proxy-injected
// credential on this run from a source other than a component: a grant already
// on the spec (a policy's, a workspace requirement's), the per-person Azure
// DevOps lane when it resolves for this caller, and every corporate redirect's
// target and the public hosts it stands in for. The proxy keeps one credential
// per bare host and the last one written wins, so a component may never add a
// second.
//
// A model credential's hosts are not listed: no component host may overlap one
// at all (servesModel). A credential dispatch authors from a person's captured
// sign-in on a host no configuration names — the AWS access-portal host of a
// Bedrock sign-in — is not visible here; dispatch compares again.
//
// subject is the run identity's subject, which the Azure DevOps lane resolves
// from. The gate asks with the caller's; dispatch asks again with the run
// owner's, over the site config it reads then (settleCredentialHosts).
func credentialedDestinations(sc types.SiteConfig, spec types.RunPolicySpec, subject string) []types.Destination {
	var hosts []string
	for _, g := range spec.EligibleGrants {
		if g.Kind == types.GrantAPIKey {
			hosts = append(hosts, apiKeyGrantScopeHost(g.Scope))
		}
	}
	// The pair dispatch resolves the lane from (resolveRunAutonomy does too).
	if ado, on := resolveADOEntraRun(sc, repoLocatorsOf(spec.WorkspaceRepos), subject); on {
		hosts = append(hosts, ado.laneHosts()...)
	}
	// Both ends of a redirect: dispatch swaps the public hosts a redirect
	// fronts for its target, so a credential bound to one of them would be
	// bound to a host the run no longer reaches.
	for _, red := range sc.EgressRedirects {
		hosts = append(hosts, hostrules.HostOf(red.To), hostrules.HostOf(red.From))
		hosts = append(hosts, redirectPublicHosts(red)...)
	}
	return parseDestinations(hosts)
}

// parseDestinations parses the entries that name a destination; one that names
// none (an empty host, an unparseable row) reaches nothing to compare against.
func parseDestinations(entries []string) []types.Destination {
	out := make([]types.Destination, 0, len(entries))
	for _, e := range entries {
		if d, err := types.ParseDestination(e); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// servesModel reports whether d reaches, on any port, a destination that
// serves a model here. The set answers for a wildcard; the predicate is asked
// about a single host as well, so the gate never admits a host that dispatch
// would then treat as a model's and strip the credential from
// (dropLegacyModelInjections).
func (b componentHostBounds) servesModel(d types.Destination) bool {
	return !d.Wildcard && b.serving(d.Host) || slices.ContainsFunc(b.model, d.OverlapsAtAnyPort)
}

// denied reports whether a deny list walls d off. ceilingDenies is the
// predicate dispatch re-asserts the same lists with, and it reads a
// port-qualified deny as covering the host: a credential is not port-scoped.
func (b componentHostBounds) denied(d types.Destination) bool {
	return ceilingDenies(b.deny, types.Destination{Host: d.Host, Wildcard: d.Wildcard}.String())
}

// collides reports whether d already carries a credential on this run.
func (b componentHostBounds) collides(d types.Destination) bool {
	return slices.ContainsFunc(b.credentialed, d.OverlapsAtAnyPort)
}
