// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refreshLocalDeploymentConfig must run before any operator secret resolution.
// Revive reuses the actual-spec classifier and today's owner profile; a stale
// rendered network configuration cannot supply authority. The revived run is
// re-leased, and a stopped agent started under a fresh generation, by
// reviveRunProxy (renewRunnerLease, startRunOnRunner).
func (s *Server) refreshLocalDeploymentConfig(ctx context.Context, run types.AgentRun, sc types.SiteConfig, cfg *proxy.Config) error {
	if run.Placement != types.PlacementLocal {
		return &placement.Refusal{Reason: placement.ReasonPlacementUnavailable, Field: "placement", Detail: "unclassified stored run placement"}
	}
	if cfg == nil || cfg.RunID != run.ID {
		return fmt.Errorf("stored proxy config does not belong to the run")
	}
	profile, denied := s.ownerProfile(ctx, run)
	if denied != nil {
		return fmt.Errorf("owner profile does not admit local revive")
	}
	p, err := s.localResolvedPlan(ctx, run, profile != nil && profile.Limits.LocalSelfDefinedComponents, sc, localStoredSpec(*cfg), nil, llmTransport{})
	if err != nil {
		return err
	}
	stripped, ref := placement.LocalEligibility(p)
	if ref != nil {
		return ref
	}
	cfg.Policy, cfg.MITMHosts = stripped.ProxyConfig.Policy, stripped.ProxyConfig.MITMHosts
	cfg.UpstreamProxyURL, cfg.TrustedCAPEM = "", ""
	cfg.UpstreamProxyNoProxy, cfg.LLMUpstreams = nil, nil
	return nil
}

func localStoredSpec(c proxy.Config) runner.SandboxSpec {
	pc := runner.ProxyConfig{
		RunToken: c.RunToken, ControlPlaneURL: c.ControlPlaneURL, ControlPlaneCAPEM: c.ControlPlaneCAPEM,
		Policy: c.Policy, MITMCACertPEM: c.MITMCACertPEM, MITMCAKeyPEM: c.MITMCAKeyPEM,
		MITMHosts: c.MITMHosts, MITMLLM: c.MITMLLM, GitGrants: c.GitGrants, PATGrants: c.PATGrants,
		BrokeredPATGrantIDs: c.BrokeredPATGrantIDs, ADOGrant: c.ADOGrant, AzureGates: c.AzureGates, GitHubAPI: c.GitHubAPI, AzureARM: c.AzureARM,
		UpstreamProxyURL: c.UpstreamProxyURL, TrustedCAPEM: c.TrustedCAPEM, InternalHosts: c.InternalHosts,
		UpstreamProxyNoProxy: c.UpstreamProxyNoProxy, LLMUpstreams: c.LLMUpstreams,
		LLMChannelHosts: c.LLMChannelHosts, LLMUnavailableDetail: c.LLMUnavailableDetail,
		Unattended: c.Unattended, Attribution: c.Attribution,
		PushRuleSets: c.PushRuleSets, PushRuleBase: c.PushRuleBase, HarnessToolRules: c.HarnessToolRules, HarnessCredentials: c.HarnessCredentials, ViaOrgRoutes: c.ViaOrgRoutes,
		MCPServers: c.MCPServers, MCPBindings: c.MCPBindings,
	}
	for _, in := range c.Injection {
		pc.Injection = append(pc.Injection, runner.InjectionGrant{GrantID: in.GrantID, Rule: in.InjectionRule})
	}
	return runner.SandboxSpec{RunID: c.RunID, ProxyConfig: pc}
}

// lockRevive takes the run's lock for a revive. For a run on a runner it first takes the runner owner's
// one revive slot, waiting at most leaseWait: runners that stop answering hold at most one lock connection
// for their owner's revives, whatever number of runners that person claims and of their runs are revived.
// Either one busy is lock_unavailable.
func (s *Server) lockRevive(ctx context.Context, run types.AgentRun) (context.Context, func(), *reviveError) {
	busy := reviveRefused(http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg)
	releaseSlot := func() {}
	if run.RunnerID != nil {
		release, ok := s.runnerOwnerSlots(run.CreatedBy).acquireRevive(ctx, leaseWait)
		if !ok {
			return ctx, nil, busy
		}
		releaseSlot = release
	}
	lctx, unlock, err := s.lockRunOp(ctx, run.ID)
	if err != nil {
		releaseSlot()
		return ctx, nil, busy
	}
	return lctx, func() {
		defer releaseSlot()
		unlock()
	}, nil
}

// reviveCeilingAdmitting is the owner's ceiling, once it still admits the run where it is.
func (s *Server) reviveCeilingAdmitting(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor string) (ownerCeiling, *reviveError) {
	c, rerr := s.reviveCeiling(ctx, run)
	if rerr == nil {
		rerr = s.revivePlacementDenied(ctx, run, c, actorType, actor)
	}
	return c, rerr
}

// revivePlacementDenied refuses a revive of a run whose placement the owner's profile now denies, with
// the create doors' reason and sentence: a profile changed after create stops the run's next revive.
func (s *Server) revivePlacementDenied(ctx context.Context, run types.AgentRun, c ownerCeiling, actorType types.ActorType, actor string) *reviveError {
	p := run.Placement
	if remotePlacement(p) {
		p = types.PlacementRemote
	}
	// A managed-harness sign-in (a server-set task no request can name) is exempt, as at create.
	if run.Task == harnessLoginTask || !slices.Contains(c.denyPlacements, p) {
		return nil
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
		mustJSON(map[string]any{"subject": run.CreatedBy, "reason": placement.ReasonPlacementDenied})))
	return reviveRefused(placement.ReasonPlacementDenied.Status(), string(placement.ReasonPlacementDenied), placementDeniedMsg(p, c.profile))
}

// ownerCeiling is the part of the owner's profile a revive re-asserts: its
// denies, and its name for the audit row.
type ownerCeiling struct {
	deny    []string
	profile string
	// denyPlacements is the profile's deny_placements, judged against where the run already is.
	denyPlacements []types.Placement
}

// reviveCeiling is the OWNER's current ceiling: the profile captured on the
// run at create, as it stands now. It is not a dispatch lane's ceiling (no
// principal is resolved; the run row names the profile). No captured profile
// (an unassigned or super-admin owner) has no profile denies, as at dispatch.
// A captured profile that no longer exists is refused: its walls cannot be
// known.
func (s *Server) reviveCeiling(ctx context.Context, run types.AgentRun) (ownerCeiling, *reviveError) {
	p, ref := s.ownerProfile(ctx, run)
	switch {
	case ref != nil:
		return ownerCeiling{}, reviveRefused(ref.status, ref.reason, ref.msg)
	case p == nil:
		return ownerCeiling{}, nil
	}
	return ownerCeiling{deny: p.Ceiling.DeniedDomains, profile: p.Name, denyPlacements: p.Limits.DenyPlacements}, nil
}
