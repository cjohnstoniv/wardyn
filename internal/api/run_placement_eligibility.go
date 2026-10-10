// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) localPlacementRefusal(r *http.Request, req createRunRequest,
	spec types.RunPolicySpec, ceiling governanceCeiling, comps runComponents,
	drive *types.DriveMount, workspaces []types.Workspace,
) *runRefusal {
	if req.Placement != placement.Local {
		return nil
	}
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		var err error
		sc, err = s.cfg.Store.GetSiteConfig(r.Context())
		if err != nil {
			return runServerError("read placement dispatch configuration", err)
		}
	}
	p := placement.LocalPlan{
		Spec:    runner.SandboxSpec{Image: req.Image, Drive: drive, Mounts: buildRunMounts(spec, s.userMountPosture(workspaces)), ProxyConfig: runner.ProxyConfig{Policy: spec.Clone()}},
		Origins: map[string]placement.CredentialOrigin{}, UpstreamProxySecretRef: sc.UpstreamProxySecretRef,
		LocalSelfDefinedComponents: ceiling.Limits.LocalSelfDefinedComponents,
		TrustedOutput:              localTrustedOutputRequest(req),
	}
	for _, c := range comps.attached {
		if c.snapshot.SelfDefined {
			p.SelfDefinedComponents = append(p.SelfDefinedComponents, c.snapshot.Name)
		}
	}
	for i, g := range spec.EligibleGrants {
		origin, ref := s.localGrantOrigin(r.Context(), localRequestOwner(r), g)
		if ref != nil {
			return ref
		}
		p.Origins["ProxyConfig.Policy.EligibleGrants["+strconv.Itoa(i)+"]"] = origin
		if origin.Class == placement.ClassOwn && origin.Stored {
			p.Spec.ProxyConfig.Policy.EligibleGrants[i].OwnerOnly = true
		}
	}
	intents, refusal := s.localCredentialIntents(r, req, spec, workspaces, sc)
	if refusal != nil {
		return refusal
	}
	p.CredentialIntents = intents
	if _, ref := placement.LocalEligibility(p); ref != nil {
		return s.localEligibilityRefusal(r, ceiling, ref)
	}
	// This is deliberately a refusal even when admitted inputs classify as own.
	// Late dispatch can add provider/capture/redirect credentials and files. H7's
	// actual-spec classifier must inspect those before H3/D117 enable execution.
	return runError(placement.ReasonPlacementUnavailable.Status(), string(placement.ReasonPlacementUnavailable),
		"Your own runner is not available: this server cannot place a run on a runner yet.")
}

func localTrustedOutputRequest(req createRunRequest) bool {
	switch req.Task {
	case harnessLoginTask, "workspace record", "workspace verify", "source scan", "workspace scan":
		return true
	default:
		return false
	}
}

// localGrantOrigin proves the owner's actual namespace before any grant write.
// Shared component scopes remain operator material even if the owner happens
// to store a colliding name. No secret value or fallback Get is used as proof.
func (s *Server) localGrantOrigin(ctx context.Context, owner string, g types.GrantSpec) (placement.CredentialOrigin, *runRefusal) {
	origin := placement.CredentialOrigin{GrantKind: g.Kind}
	switch g.Kind {
	case types.GrantGitHubToken:
		origin.Class, origin.Delivery = placement.ClassBrokered, placement.ClassGitHubToken
		return origin, nil
	case types.GrantCloudSTS:
		origin.Class, origin.Delivery = placement.ClassBrokered, placement.ClassCloudSTS
		return origin, nil
	}
	_, name, knownHosts, covered, err := storedSecretGrantPairing(g)
	if err != nil || !covered {
		return origin, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), fmt.Sprintf("eligible_grants: %s credential provenance cannot be resolved", g.Kind))
	}
	origin.Stored = true
	own := owner != "" && s.ownsSecretMemoized(ctx, owner, name) && (knownHosts == "" || s.ownsSecretMemoized(ctx, owner, knownHosts))
	if g.Kind == types.GrantAPIKey && apiKeyScopeShared(g.Scope) {
		own = false
	}
	origin.OwnNamespace, origin.OwnerOnly = own, own
	if own {
		origin.Class = placement.ClassOwn
	} else {
		origin.Class = placement.ClassOperator
	}
	switch g.Kind {
	case types.GrantAPIKey:
		origin.Delivery = placement.ClassAPIKey
	case types.GrantEnvSecret:
		origin.Delivery = placement.ClassEnvSecret
	case types.GrantFileSecret:
		origin.Delivery = placement.ClassFileSecret
	case types.GrantSSHKey:
		origin.Delivery = placement.ClassSSHKey
	case types.GrantGitPAT:
		origin.Delivery = placement.ClassGitPATHelper
	default:
		return origin, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), "eligible_grants: unclassified credential kind")
	}
	return origin, nil
}

func (s *Server) localEligibilityRefusal(r *http.Request, ceiling governanceCeiling, ref *placement.Refusal) *runRefusal {
	if ref.Reason == placement.ReasonPlacementComponentSelfDefine {
		d := authz.Deny(authz.ReasonPlacementComponentSelfDefined, "runs.placement", ref.Error()).WithPolicy(s.ceilingPolicy(r.Context(), ceiling))
		return runDenied(d)
	}
	return runError(ref.Reason.Status(), string(ref.Reason), ref.Error())
}

// ownLocalGrantSpecs verifies the entire set before the first write. Local own
// grant persistence never depends on a caller remembering to stamp OwnerOnly.
func (s *Server) ownLocalGrantSpecs(r *http.Request, spec types.RunPolicySpec) (types.RunPolicySpec, *runRefusal) {
	spec = spec.Clone()
	for i, g := range spec.EligibleGrants {
		origin, ref := s.localGrantOrigin(r.Context(), localRequestOwner(r), g)
		if ref != nil {
			return types.RunPolicySpec{}, ref
		}
		if origin.Class != placement.ClassOwn || !origin.Stored || !origin.OwnNamespace {
			return types.RunPolicySpec{}, runError(placement.ReasonPlacementCredential.Status(), string(placement.ReasonPlacementCredential), fmt.Sprintf("eligible_grants[%d]: credential is not available in its owner's namespace", i))
		}
		spec.EligibleGrants[i].OwnerOnly = true
	}
	return spec, nil
}

func localRequestOwner(r *http.Request) string {
	_, actor := actorFromRequest(r)
	return runIdentitySubject(r.Context(), actor)
}
