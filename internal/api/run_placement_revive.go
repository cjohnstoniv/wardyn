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
	p, err := s.localResolvedPlan(ctx, run, profile != nil && profile.Limits.LocalSelfDefinedComponents, sc, localStoredSpec(*cfg), nil, llmTransport{}, adoEntraRun{})
	if err != nil {
		return err
	}
	stripped, ref := placement.LocalEligibility(p)
	if ref != nil {
		return ref
	}
	cfg.Policy, cfg.MITMHosts = stripped.ProxyConfig.Policy, stripped.ProxyConfig.MITMHosts
	cfg.UpstreamProxyURL, cfg.TrustedCAPEM = "", ""
	cfg.InternalHosts, cfg.UpstreamProxyNoProxy, cfg.LLMUpstreams = nil, nil, nil
	return nil
}

func localStoredSpec(c proxy.Config) runner.SandboxSpec {
	pc := runner.ProxyConfig{
		RunToken: c.RunToken, ControlPlaneURL: c.ControlPlaneURL, ControlPlaneCAPEM: c.ControlPlaneCAPEM,
		Policy: c.Policy, MITMCACertPEM: c.MITMCACertPEM, MITMCAKeyPEM: c.MITMCAKeyPEM,
		MITMHosts: c.MITMHosts, MITMLLM: c.MITMLLM, GitGrants: c.GitGrants, PATGrants: c.PATGrants,
		BrokeredPATGrantIDs: c.BrokeredPATGrantIDs, ADOGrant: c.ADOGrant, AzureGates: c.AzureGates,
		UpstreamProxyURL: c.UpstreamProxyURL, TrustedCAPEM: c.TrustedCAPEM, InternalHosts: c.InternalHosts,
		UpstreamProxyNoProxy: c.UpstreamProxyNoProxy, LLMUpstreams: c.LLMUpstreams,
		LLMChannelHosts: c.LLMChannelHosts, LLMUnavailableDetail: c.LLMUnavailableDetail,
		Unattended: c.Unattended, Attribution: c.Attribution,
		PushRuleSets: c.PushRuleSets, PushRuleBase: c.PushRuleBase, HarnessToolRules: c.HarnessToolRules, HarnessCredentials: c.HarnessCredentials, ViaOrgRoutes: c.ViaOrgRoutes,
	}
	for _, in := range c.Injection {
		pc.Injection = append(pc.Injection, runner.InjectionGrant{GrantID: in.GrantID, Rule: in.InjectionRule})
	}
	return runner.SandboxSpec{RunID: c.RunID, ProxyConfig: pc}
}

// lockRevive takes the run's lock for a revive. For a run on a runner it first takes the runner's one
// revive slot, waiting at most leaseWait: a runner that stops answering holds at most one lock connection
// for its revives, whatever number of its runs are revived. Either one busy is lock_unavailable.
func (s *Server) lockRevive(ctx context.Context, run types.AgentRun) (context.Context, func(), *reviveError) {
	busy := reviveRefused(http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg)
	releaseSlot := func() {}
	if run.RunnerID != nil {
		release, ok := s.runnerEndpoint(*run.RunnerID).acquireRevive(ctx, leaseWait)
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
	return lctx, func() { unlock(); releaseSlot() }, nil
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
