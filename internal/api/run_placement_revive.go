// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refreshLocalDeploymentConfig must run before any operator secret resolution.
// Revive reuses the actual-spec classifier and today's owner profile; a stale
// rendered network configuration cannot supply authority. The routing refusal
// remains even for a fully own plan, as at admission and dispatch.
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
	ceiling := dispatchCeiling{}
	if profile != nil {
		ceiling.localSelfDefinedComponents = profile.Limits.LocalSelfDefinedComponents
	}
	p, err := s.localResolvedPlan(ctx, run, ceiling, sc, localStoredSpec(*cfg), nil, llmTransport{}, adoEntraRun{})
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
	return &placement.Refusal{Reason: placement.ReasonPlacementUnavailable, Field: "placement", Detail: "Your own runner is not available: this server cannot place a run on a runner yet."}
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
	}
	for _, in := range c.Injection {
		pc.Injection = append(pc.Injection, runner.InjectionGrant{GrantID: in.GrantID, Rule: in.InjectionRule})
	}
	return runner.SandboxSpec{RunID: c.RunID, ProxyConfig: pc}
}
