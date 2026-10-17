// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// classifyLocalDispatch is after the final spec writer, before the substrate.
// Missing origins (including an unrecognised late author) fail closed. The
// entry refusal remains until routing and credential authoring are ready.
func (s *Server) classifyLocalDispatch(ctx context.Context, run types.AgentRun, ceiling dispatchCeiling,
	sc types.SiteConfig, spec *runner.SandboxSpec, orgConfigKeys []string, llm llmTransport, trustedOutput bool,
) bool {
	if remotePlacement(run.Placement) {
		return true
	}
	if run.Placement != types.PlacementLocal {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "This run was not launched: its placement is not one this server can classify")
		return false
	}
	p, err := s.localResolvedPlan(ctx, run, ceiling.localSelfDefinedComponents, sc, *spec, orgConfigKeys, llm)
	if err != nil {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "local dispatch provenance could not be read")
		return false
	}
	p.TrustedOutput = p.TrustedOutput || trustedOutput
	corpus := inspectionCorpusOf(spec.ProxyConfig.Policy) // before the laptop copy drops it
	ref, err := s.dispatchTrustRefusal(ctx, run, p)
	if err != nil {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "local dispatch provenance could not be read")
		return false
	}
	var stripped runner.SandboxSpec
	if ref == nil {
		stripped, ref = placement.LocalEligibility(p)
	}
	if ref != nil {
		// A personal component name stays out of the append-only record and the failure hint.
		data, hint := map[string]any{"reason": ref.Reason}, "This run was not launched on your own runner: "+ref.Detail
		if ref.Reason != placement.ReasonPlacementComponentSelfDefine {
			data["field"], hint = ref.Field, "This run was not launched on your own runner: "+ref.Field+": "+ref.Detail
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch", run.ID.String(), "failure", mustJSON(data)))
		s.failAndRevoke(ctx, run.ID, types.RunStarting, hint)
		return false
	}
	*spec = stripped
	// keepRunProxyConfig ran before file-secret completion. Replace its local
	// copy with the classified config; a failed write cannot launch a proxy.
	// The sealed row is the organisation's: it also keeps the dispatch-time
	// inspection corpus the laptop copy drops, for the run's via_org engine
	// (via_org.go). Every path that rebuilds a laptop config from the row
	// re-classifies it (refreshLocalDeploymentConfig), which drops it again.
	if err := s.keepRunProxyConfig(ctx, run.ID, withInspectionCorpus(spec.ProxyConfig, corpus)); err != nil {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "local proxy config could not be stored")
		return false
	}
	return true
}

// localResolvedPlan is the one classification of a local run's spec, at dispatch and at revive. llm is the
// dispatch transport whose resident model secrets the spec carries; the identity lanes are derived here
// (localIdentityLanes) so the two callers cannot classify one stored credential from different inputs.
func (s *Server) localResolvedPlan(ctx context.Context, run types.AgentRun, selfDefinedAllowed bool, sc types.SiteConfig,
	spec runner.SandboxSpec, orgConfigKeys []string, llm llmTransport,
) (placement.LocalPlan, error) {
	if fields := localDispatchUnclassified(); len(fields) != 0 {
		return placement.LocalPlan{}, fmt.Errorf("%s: unclassified dispatch metadata", fields[0])
	}
	rows, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		return placement.LocalPlan{}, err
	}
	comps, err := s.runComponentSnapshot(ctx, run.ID)
	if err != nil {
		return placement.LocalPlan{}, err
	}
	p := placement.LocalPlan{Spec: spec, Origins: map[string]placement.CredentialOrigin{}, OrgConfigKeys: orgConfigKeys,
		UpstreamProxySecretRef: sc.UpstreamProxySecretRef, LocalSelfDefinedComponents: selfDefinedAllowed,
		TrustedOutput: localTrustedOutputRun(run), SelfDefinedComponents: selfDefinedSnapshotNames(comps),
	}
	owner := runIdentitySubject(ctx, run.CreatedBy)
	byID := map[uuid.UUID]types.CredentialGrant{}
	for _, g := range rows {
		if g.RunID != run.ID {
			return placement.LocalPlan{}, fmt.Errorf("grant does not belong to the classified run")
		}
		byID[g.ID] = g
	}
	s.localPolicyGrantOrigins(ctx, owner, rows, &p)
	s.localProxyGrantOrigins(ctx, owner, byID, &p)
	s.localResidentOrigins(ctx, owner, rows, llm, &p)
	ado, azure := localIdentityLanes(ctx, run, sc, spec, owner)
	s.localIdentityOrigins(ctx, owner, ado, azure, &p)
	return p, s.localPlanFacts(ctx, &p, egressBaselineOf(sc), run.RunnerID)
}

// sameScope compares two grant scopes as a typed decode reads them: a stored row's scope has round-tripped
// through jsonb, which reorders keys and drops the request's spacing, so its bytes are never the policy
// copy's. A typed decode matches keys without regard to case (Unicode folding included, so ſ is s) and
// keeps the last match, so a scope naming one key twice up to folding decodes by key order, which jsonb
// changes: such a scope never matches. Numbers are compared exactly rather than as floats.
func sameScope(a, b json.RawMessage) bool {
	x, errx := typedScope(json.NewDecoder(bytes.NewReader(a)))
	y, erry := typedScope(json.NewDecoder(bytes.NewReader(b)))
	return errx == nil && erry == nil && reflect.DeepEqual(x, y)
}

// errScopeKeyFolds: an object names one key twice up to case folding.
var errScopeKeyFolds = errors.New("a scope names one key twice up to case")

// typedScope is the next JSON value as a typed decode sees it: object keys case-folded, numbers as their
// literal. An object that names a key twice up to folding is refused.
func typedScope(dec *json.Decoder) (any, error) {
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		out := map[string]any{}
		var keys []string
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key := tok.(string)
			if slices.ContainsFunc(keys, func(k string) bool { return strings.EqualFold(k, key) }) {
				return nil, errScopeKeyFolds
			}
			keys = append(keys, key)
			v, err := typedScope(dec)
			if err != nil {
				return nil, err
			}
			out[strings.ToLower(strings.ToUpper(key))] = v
		}
		_, err = dec.Token()
		return out, err
	case json.Delim('['):
		out := []any{}
		for dec.More() {
			v, err := typedScope(dec)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		_, err = dec.Token()
		return out, err
	}
	return tok, nil
}

func selfDefinedSnapshotNames(comps []types.RunComponent) []string {
	var names []string
	for _, c := range comps {
		if !c.SelfDefined {
			continue
		}
		name := c.Name
		if name == "" {
			name = "components[" + strconv.Itoa(c.Ordinal) + "]"
		}
		names = append(names, name)
	}
	return names
}

// localPolicyGrantOrigins stamps the policy copy with the persisted OwnerOnly
// value, never a request's unstamped version. Every match is to the exact
// kind+scope of this run.
func (s *Server) localPolicyGrantOrigins(ctx context.Context, owner string, rows []types.CredentialGrant, p *placement.LocalPlan) {
	p.Spec.ProxyConfig.Policy = p.Spec.ProxyConfig.Policy.Clone()
	for i, g := range p.Spec.ProxyConfig.Policy.EligibleGrants {
		for _, row := range rows {
			if row.Spec.Kind != g.Kind || !sameScope(row.Spec.Scope, g.Scope) {
				continue
			}
			origin, ref := s.localGrantOrigin(ctx, owner, row.Spec)
			if ref != nil {
				continue
			}
			origin.OwnerOnly = row.Spec.OwnerOnly
			p.Origins[placementCredentialIndex("ProxyConfig.Policy.EligibleGrants", i)] = origin
			p.Spec.ProxyConfig.Policy.EligibleGrants[i].OwnerOnly = row.Spec.OwnerOnly
			break
		}
	}
}

// localProxyGrantOrigins binds each actual proxy route to the stored grant
// that produced it; a route whose grant is missing or differs keeps no
// provenance and so refuses.
func (s *Server) localProxyGrantOrigins(ctx context.Context, owner string, byID map[uuid.UUID]types.CredentialGrant, p *placement.LocalPlan) {
	spec := p.Spec
	stamp := func(path string, row types.CredentialGrant) {
		origin, ref := s.localGrantOrigin(ctx, owner, row.Spec)
		if ref != nil {
			return
		}
		origin.OwnerOnly = row.Spec.OwnerOnly
		p.Origins[path] = origin
	}
	for i, in := range spec.ProxyConfig.Injection {
		row, found := byID[in.GrantID]
		if !found || row.Spec.Kind != types.GrantAPIKey {
			continue
		}
		if rule, e := injectionRuleFromScope(row.Spec.Scope); e == nil && reflect.DeepEqual(rule, in.Rule) {
			stamp(placementCredentialIndex("ProxyConfig.Injection", i), row)
		}
	}
	for host, pat := range spec.ProxyConfig.PATGrants {
		row, found := byID[pat.GrantID]
		if !found || row.Spec.Kind != types.GrantGitPAT {
			continue
		}
		if scope, e := types.DecodeGitPATScope(row.Spec.Scope); e == nil && hostEqual(scope.Host, host) {
			stamp("ProxyConfig.PATGrants["+host+"]", row)
		}
	}
	for repo, id := range spec.ProxyConfig.GitGrants {
		if row, found := byID[id]; found && row.Spec.Kind == types.GrantGitHubToken {
			stamp("ProxyConfig.GitGrants["+repo+"]", row)
		}
	}
	for i, id := range spec.ProxyConfig.BrokeredPATGrantIDs {
		if row, found := byID[id]; found && row.Spec.Kind == types.GrantGitPAT {
			stamp(placementCredentialIndex("ProxyConfig.BrokeredPATGrantIDs", i), row)
		}
	}
}

// localIdentityLanes is the Azure DevOps lane and the Azure Foundry lane a local run's identity
// credentials are classified from, resolved from the run, its spec's repositories and the site
// configuration the way dispatch resolves them (resolveADOEntraRun, providerLaneForRun). run.Repo
// stands in for dispatch's ADOLaneRepo: it only classifies a grant dispatch already stored
// (localIdentityOrigins), which a server-authored lane never has.
func localIdentityLanes(ctx context.Context, run types.AgentRun, sc types.SiteConfig, spec runner.SandboxSpec, owner string) (adoEntraRun, *providerAzureLane) {
	ado, _ := resolveADOEntraRun(sc, spec.ProxyConfig.Policy.WorkspaceRepos, run.Repo, adoLaneOwnerOfRun(ctx, run))
	p, found := modelProviderByID(sc.ModelProviders, run.ModelProviderID)
	if !found || p.Disabled || p.Kind != types.ModelProviderAzureFoundry {
		return ado, nil
	}
	lane, ok := providerAzureLaneFor(p, run.Agent)
	if !ok {
		return ado, nil
	}
	lane.owner = owner
	return ado, &lane
}

// localIdentityOrigins stamps an identity credential only where the lane resolved today still authored
// it: the same organisation, capabilities within the row's ceiling and hosts within the lane for Azure
// DevOps; the same endpoint host, route and deployments for an Azure Foundry gate. Anything else (the
// lane gone, or the provider or its row changed since dispatch) is left without an origin, which refuses.
func (s *Server) localIdentityOrigins(ctx context.Context, owner string, ado adoEntraRun, azure *providerAzureLane, p *placement.LocalPlan) {
	spec := p.Spec
	if g := spec.ProxyConfig.ADOGrant; g != nil && ado.rowID != "" && g.Organization == ado.org &&
		subsetOf(g.Capabilities, ado.ceiling) && subsetOf(g.Hosts, ado.laneHosts()) {
		p.Origins["ProxyConfig.ADOGrant"] = s.localADOOrigin(ctx, owner, ado)
	}
	if azure != nil && azure.owner == owner {
		own := s.ownsSecretMemoized(ctx, owner, providerSecretName(azure.provider.UID, providerEntraPart))
		for i, g := range spec.ProxyConfig.AzureGates {
			if g.Host == azure.host && g.Route == azure.route && subsetOf(g.Models, azure.models()) {
				p.Origins[placementCredentialIndex("ProxyConfig.AzureGates", i)] = placement.CredentialOrigin{Class: placement.ClassOwn, Stored: true, OwnNamespace: own, OwnerOnly: own}
			}
		}
	}
}

func placementCredentialIndex(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

func (s *Server) localResidentOrigins(ctx context.Context, owner string, rows []types.CredentialGrant, llm llmTransport, p *placement.LocalPlan) {
	for _, row := range rows {
		name, _, _, covered, e := storedSecretGrantPairing(row.Spec)
		if e != nil || !covered {
			continue
		}
		origin, ref := s.localGrantOrigin(ctx, owner, row.Spec)
		if ref != nil {
			continue
		}
		origin.OwnerOnly = row.Spec.OwnerOnly
		if row.Spec.Kind == types.GrantEnvSecret {
			if _, exists := p.Spec.SecretEnv[name]; exists {
				p.Origins["SandboxSpec.SecretEnv["+name+"]"] = origin
			}
		}
		if row.Spec.Kind == types.GrantFileSecret {
			for i, f := range p.Spec.ManagedFiles {
				if f.AgentOwned && f.Path == runner.ComponentSecretDir+"/"+name {
					p.Origins[placementCredentialIndex("SandboxSpec.ManagedFiles", i)] = origin
				}
			}
		}
	}
	// Only dispatchMCP writes this key: env_secret and component names refuse WARDYN_*.
	if _, exists := p.Spec.SecretEnv[codexMCPEnv]; exists {
		p.Origins["SandboxSpec.SecretEnv["+codexMCPEnv+"]"] = placement.CredentialOrigin{Class: placement.ClassPlatform}
	}
	if llm.provider == nil || llm.provider.owner != owner || llm.provider.provider.Kind != types.ModelProviderBedrockSSO {
		return
	}
	own := s.ownsSecretMemoized(ctx, owner, providerSecretName(llm.provider.provider.UID, providerSSOPart))
	for _, key := range llm.secretEnvKeys {
		if _, exists := p.Spec.SecretEnv[key]; exists {
			p.Origins["SandboxSpec.SecretEnv["+key+"]"] = bedrockSSOSecretEnvOrigin(own)
		}
	}
}

// dispatchTrustRefusal is dispatch's trust door (grantModesRefusal) over the run's stored grants, at the
// bound runner's trust class that localPlanFacts read into p.
func (s *Server) dispatchTrustRefusal(ctx context.Context, run types.AgentRun, p placement.LocalPlan) (*placement.Refusal, error) {
	rows, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	owner := runIdentitySubject(ctx, run.CreatedBy)
	return grantModesRefusal(p, rows, func(g types.CredentialGrant) (string, bool) {
		origin, ref := s.localGrantOrigin(ctx, owner, g.Spec)
		return origin.Delivery, ref == nil
	}), nil
}

func localTrustedOutputRun(run types.AgentRun) bool {
	// Persisted trusted links are authoring provenance, not a task string a
	// member can use to manufacture evidence. Capture/login remains explicit.
	return run.WorkspaceID != nil || run.SourceID != nil || localTrustedOutputRequest(createRunRequest{Task: run.Task})
}

// inspectionCorpus is a run's dispatch-time workspace-secret corpus: the names its policy
// declared and the values dispatch resolved for them.
type inspectionCorpus struct{ names, values []string }

func inspectionCorpusOf(pol types.RunPolicySpec) inspectionCorpus {
	if li := pol.LLMInspection; li != nil {
		return inspectionCorpus{slices.Clone(li.WorkspaceSecretNames), slices.Clone(li.WorkspaceSecretValues)}
	}
	return inspectionCorpus{}
}

// withInspectionCorpus is pc with c restored on a copy of its inspection settings: the org-only
// form of a laptop copy.
func withInspectionCorpus(pc runner.ProxyConfig, c inspectionCorpus) runner.ProxyConfig {
	if pc.Policy.LLMInspection == nil || len(c.names)+len(c.values) == 0 {
		return pc
	}
	pc.Policy = pc.Policy.Clone()
	pc.Policy.LLMInspection.WorkspaceSecretNames, pc.Policy.LLMInspection.WorkspaceSecretValues = c.names, c.values
	return pc
}
