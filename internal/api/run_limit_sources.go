// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner/sizing"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// limitSourceInput is what one run's limit sources are built from.
type limitSourceInput struct {
	runType types.RunnerPoolRunType
	// pool is the run's resolved pool; nil for an unpooled run.
	pool *types.RunnerPool
	// governed is false when governance is skipped for the caller (runUngoverned): the pool, the
	// deployment and the runners still bind them.
	governed bool
	limits   types.GovernanceLimits
	// ceiling is the governance ceiling's AUTHORED size (governanceCeiling.authoredSize). untouched is
	// the admin-authored resolved size after the policy step, never a raw inline value
	// (runnerpool.GovernanceSource's contract); where it is unset the member stands at memberDefault.
	ceiling, untouched, memberDefault types.ResourceLimits
	// deploymentDefault is the deployment's own sandbox size (inheritDeploymentResources); a zero
	// field is sizing.EffectiveLimits'.
	deploymentDefault types.ResourceLimits
	// ephemeral is the organisation's storage.ephemeral block: the main drive's default and maximum.
	ephemeral types.EphemeralProvider
	// capacity is a self-hosted run's candidates' advertised capacity; empty for any other run.
	capacity                  []runnerpool.RunnerCapacity
	maxAgeSec, concurrentRuns int
}

// buildLimitSources is the one sources builder: the deployment always, the pool when there is one,
// governance unless it is skipped, and the runners' capacity for a self-hosted run. The create-time
// end, the fold's resources and the pool list read it, so they cannot disagree about a source. A
// run type the pool does not take is its refusal.
func buildLimitSources(in limitSourceInput) ([]runnerpool.SourceLimits, *runnerpool.Refusal, error) {
	size := sizing.EffectiveLimits()
	deploymentSize := types.ResourceLimits{CPUMillis: cmp.Or(in.deploymentDefault.CPUMillis, int(size.CPUMillis)),
		MemoryMiB: cmp.Or(in.deploymentDefault.MemoryMiB, int(size.MemoryMiB))}
	dep, err := runnerpool.DeploymentSource(deploymentSize, in.ephemeral, in.maxAgeSec, in.concurrentRuns)
	if err != nil {
		return nil, nil, err
	}
	sources := []runnerpool.SourceLimits{runnerpool.WithInteractiveLifetimeDefault(dep, in.runType)}
	if in.pool != nil {
		pool, refusal := runnerpool.PoolSource(*in.pool, in.runType)
		if refusal != nil {
			return nil, refusal, nil
		}
		sources = append(sources, pool)
	}
	if in.governed {
		memberDefault := in.memberDefault
		if memberDefault.CPUMillis < 1 || memberDefault.MemoryMiB < 1 {
			memberDefault = composer.MemberDefault(types.ResourceLimits{}, nil)
		}
		gov, err := runnerpool.GovernanceSource(in.limits, in.ceiling, in.untouched, memberDefault)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, gov)
	}
	if len(in.capacity) > 0 {
		capacity, err := runnerpool.RunnerCapacitySource(in.capacity...)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, capacity)
	}
	return sources, nil, nil
}

// limitSources is buildLimitSources for this request's caller: governance with the caller's REAL
// ceiling unless their runs stand outside it, and the deployment's own size, run age and run cap.
// A member with no admin-authored size stands at the member default, which the resolved ceiling
// (the deployment's size where its profile sets none) lowers, as composer.Clamp's member cap does.
func (s *Server) limitSources(ctx context.Context, rt types.RunnerPoolRunType, pool *types.RunnerPool,
	ceiling governanceCeiling, untouched types.ResourceLimits, capacity []runnerpool.RunnerCapacity,
) ([]runnerpool.SourceLimits, *runnerpool.Refusal, error) {
	in := limitSourceInput{
		runType: rt, pool: pool, governed: !s.runUngoverned(ctx), limits: ceiling.Limits, untouched: untouched, capacity: capacity,
		// A negative WARDYN_RUN_MAX_AGE or run cap is off, as 0 is.
		maxAgeSec: max(0, int(s.cfg.RunMaxAge/time.Second)), concurrentRuns: max(0, s.cfg.MaxConcurrentRuns),
	}
	in.ceiling, in.memberDefault = ceiling.authoredSize(), ceiling.memberSize()
	in.ceiling.DiskMiB = ceiling.authoredDiskMiB()
	in.ephemeral = s.orgEphemeral(ctx)
	if d := s.inheritDeploymentResources(nil); d != nil {
		in.deploymentDefault = types.ResourceLimits{CPUMillis: d.CPUMillis, MemoryMiB: d.MemoryMiB}
	}
	return buildLimitSources(in)
}

// orgEphemeral is the organisation's storage.ephemeral block, the zero value (no default, no
// maximum) when it is absent. A site-config read that fails degrades to that: the dry doors are
// advisory, and dispatch applies the organisation's maximum again.
func (s *Server) orgEphemeral(ctx context.Context) types.EphemeralProvider {
	if s.cfg.Store == nil {
		return types.EphemeralProvider{}
	}
	site, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not read the org storage block; sizing the main drive without it", slog.Any("error", err))
	}
	return orgEphemeralOf(site)
}
