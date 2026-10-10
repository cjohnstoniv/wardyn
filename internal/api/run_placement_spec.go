// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// classifyLocalDispatch is after the final spec writer, before the substrate.
// Missing origins (including an unrecognised late author) fail closed. The
// entry refusal remains until routing and credential authoring are ready.
func (s *Server) classifyLocalDispatch(ctx context.Context, run types.AgentRun, ceiling dispatchCeiling,
	sc types.SiteConfig, spec *runner.SandboxSpec, orgConfigKeys []string, llm llmTransport, ado adoEntraRun, trustedOutput bool,
) bool {
	if run.Placement != types.PlacementLocal {
		return true
	}
	p, err := s.localResolvedPlan(ctx, run, ceiling.localSelfDefinedComponents, sc, *spec, orgConfigKeys, llm, ado)
	if err != nil {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "local dispatch provenance could not be read")
		return false
	}
	p.TrustedOutput = p.TrustedOutput || trustedOutput
	stripped, ref := placement.LocalEligibility(p)
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
	if err := s.keepRunProxyConfig(ctx, run.ID, spec.ProxyConfig); err != nil {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "local proxy config could not be stored")
		return false
	}
	return true
}

func (s *Server) localResolvedPlan(ctx context.Context, run types.AgentRun, selfDefinedAllowed bool, sc types.SiteConfig,
	spec runner.SandboxSpec, orgConfigKeys []string, llm llmTransport, ado adoEntraRun,
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
	s.localIdentityOrigins(ctx, owner, ado, llm, &p)
	return p, nil
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
			if row.Spec.Kind != g.Kind || !bytes.Equal(row.Spec.Scope, g.Scope) {
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
	for i, id := range spec.ProxyConfig.BrokeredPATGrantIDs {
		if row, found := byID[id]; found && row.Spec.Kind == types.GrantGitPAT {
			stamp(placementCredentialIndex("ProxyConfig.BrokeredPATGrantIDs", i), row)
		}
	}
}

func (s *Server) localIdentityOrigins(ctx context.Context, owner string, ado adoEntraRun, llm llmTransport, p *placement.LocalPlan) {
	spec := p.Spec
	if spec.ProxyConfig.ADOGrant != nil {
		origin := placement.CredentialOrigin{Class: placement.ClassBrokered, Delivery: placement.ClassADOMintedPAT}
		if ado.tokenMode == types.ADOTokenModeOwnPAT && ado.owner == owner && adoEntraValidRowID(ado.rowID) {
			own := s.ownsSecretMemoized(ctx, owner, adoOwnPATSecretName(ado.rowID))
			origin = placement.CredentialOrigin{Class: placement.ClassOwn, Stored: true, OwnNamespace: own, OwnerOnly: own}
		}
		p.Origins["ProxyConfig.ADOGrant"] = origin
	}
	if llm.azure != nil && llm.azure.owner == owner {
		own := s.ownsSecretMemoized(ctx, owner, providerSecretName(llm.azure.provider.UID, providerEntraPart))
		for i, g := range spec.ProxyConfig.AzureGates {
			if g.Host == llm.azure.host && g.Route == llm.azure.route {
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
	if llm.provider == nil || llm.provider.owner != owner || llm.provider.provider.Kind != types.ModelProviderBedrockSSO {
		return
	}
	own := s.ownsSecretMemoized(ctx, owner, providerSecretName(llm.provider.provider.UID, providerSSOPart))
	for _, key := range llm.secretEnvKeys {
		if _, exists := p.Spec.SecretEnv[key]; exists {
			p.Origins["SandboxSpec.SecretEnv["+key+"]"] = placement.CredentialOrigin{Class: placement.ClassOwn, Stored: true, OwnNamespace: own, OwnerOnly: own}
		}
	}
}

func localTrustedOutputRun(run types.AgentRun) bool {
	// Persisted trusted links are authoring provenance, not a task string a
	// member can use to manufacture evidence. Capture/login remains explicit.
	return run.WorkspaceID != nil || run.SourceID != nil || localTrustedOutputRequest(createRunRequest{Task: run.Task})
}
