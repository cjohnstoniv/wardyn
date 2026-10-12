// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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
	// ceiling is the governance ceiling policy's own size. untouched is the admin-authored resolved
	// size after the policy step, never a raw inline value (runnerpool.GovernanceSource's contract).
	ceiling, untouched types.ResourceLimits
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
	deploymentSize := types.ResourceLimits{CPUMillis: int(size.CPUMillis), MemoryMiB: int(size.MemoryMiB)}
	dep, err := runnerpool.DeploymentSource(deploymentSize, in.maxAgeSec, in.concurrentRuns)
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
		// A member with no admin-authored size stands at the member default, which a ceiling below the
		// deployment's size lowers, as composer.Clamp's member cap does.
		gov, err := runnerpool.GovernanceSource(in.limits, in.ceiling, in.untouched, composer.MemberDefault(&in.ceiling))
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
// ceiling unless their runs stand outside it, and the deployment's run age and run cap.
func (s *Server) limitSources(ctx context.Context, rt types.RunnerPoolRunType, pool *types.RunnerPool,
	ceiling governanceCeiling, untouched types.ResourceLimits, capacity []runnerpool.RunnerCapacity,
) ([]runnerpool.SourceLimits, *runnerpool.Refusal, error) {
	in := limitSourceInput{
		runType: rt, pool: pool, governed: !s.runUngoverned(ctx), limits: ceiling.Limits, untouched: untouched, capacity: capacity,
		// A negative WARDYN_RUN_MAX_AGE or run cap is off, as 0 is.
		maxAgeSec: max(0, int(s.cfg.RunMaxAge/time.Second)), concurrentRuns: max(0, s.cfg.MaxConcurrentRuns),
	}
	if ceiling.Spec.Resources != nil {
		in.ceiling = *ceiling.Spec.Resources
	}
	return buildLimitSources(in)
}
