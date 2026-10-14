// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// remotePlacement is the one test for "keeps today's remote behaviour": an
// empty placement (a run stored before placements existed) or remote. Any
// other value is local or unknown, and a layer that cannot handle it refuses.
func remotePlacement(p types.Placement) bool { return p == "" || p == types.PlacementRemote }

// unsupportedLocalDispatch closes every dispatch lane, before its CAS, renewal,
// grant authoring or mask-manifest writes, to a run create did not bind to a
// runner: a placement this server does not know, or a local run with no runner.
// A bound local run goes on to the actual-spec classifier (classifyLocalDispatch)
// after the final spec writer, and to its runner only (createSandboxAtTarget).
func (s *Server) unsupportedLocalDispatch(ctx context.Context, run types.AgentRun) bool {
	if remotePlacement(run.Placement) || (run.Placement == types.PlacementLocal && run.RunnerID != nil) {
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch", run.ID.String(), "failure", mustJSON(map[string]any{"reason": placement.ReasonPlacementUnavailable})))
	s.failAndRevoke(context.WithoutCancel(ctx), run.ID, types.RunPending, "Your own runner is not available: this run is not bound to one of your runners.")
	return true
}

// dispatchPlacementDenied fails, before anything is claimed or written, a run whose placement its
// dispatch ceiling's deny_placements denies: a lane that did not pass the create doors, or a profile that
// changed between create and dispatch.
func (s *Server) dispatchPlacementDenied(ctx context.Context, run types.AgentRun, c dispatchCeiling) bool {
	p := run.Placement
	if remotePlacement(p) {
		p = types.PlacementRemote
	}
	if !slices.Contains(c.denyPlacements, p) {
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch", run.ID.String(), "failure", mustJSON(map[string]any{"reason": placement.ReasonPlacementDenied})))
	s.failAndRevoke(context.WithoutCancel(ctx), run.ID, types.RunPending, placementDeniedMsg(p, c.profile))
	return true
}

// placementExempt is c for a run Wardyn authors itself, a managed-harness sign-in or a site-config probe,
// which deny_placements does not bind at dispatch, as at create (harnesscred.go): no workspace content,
// and the only way to do what it does. At revive only the sign-in is exempt (revivePlacementDenied): a
// probe run carries no server-set marker, so its revive stays held to the deny list.
func (c dispatchCeiling) placementExempt() dispatchCeiling {
	c.denyPlacements = nil
	return c
}

// createSandboxAtTarget creates the run's sandbox on the exact target it was admitted against, the one
// persisted on its row, and nowhere else. A pool is only the label the run was admitted under: the pool's
// members, a revoked runner and anyone's defaults can change after admission, and none of that reroutes a
// run, a retry or a revive. A pooled run whose row names no target fails; it is never handed to the
// first substrate that can enforce its class.
func (s *Server) createSandboxAtTarget(ctx context.Context, run types.AgentRun, spec runner.SandboxSpec) (runner.Sandbox, error) {
	switch {
	case run.RunnerID != nil && run.Task == harnessLoginTask:
		// A provider sign-in runs on the server's own substrate, never a person's computer: it holds the
		// sign-in's credentials. No code path binds one to a runner, so a bound one is refused outright.
		return runner.Sandbox{}, fmt.Errorf("%w: a provider sign-in is never placed on a runner", runner.ErrExecutorUnavailable)
	case run.RunnerID != nil:
		spec.RunnerID = run.RunnerID.String()
		if rs, ok := s.cfg.Store.(store.RunnerStore); ok {
			row, err := rs.GetRunner(ctx, *run.RunnerID)
			if err != nil {
				return runner.Sandbox{}, fmt.Errorf("read the run's runner: %w", err)
			}
			spec.RunnerMaxRuns = row.MaxRuns
		}
	case run.RunnerPoolID != nil:
		if run.RunnerPoolExecutor == "" {
			return runner.Sandbox{}, fmt.Errorf("%w: the run's pool binding names no executor", runner.ErrExecutorUnavailable)
		}
		if exact, ok := s.cfg.Runner.(runner.ExactExecutor); ok {
			return exact.CreateSandboxOn(ctx, run.RunnerPoolExecutor, spec)
		}
		if s.cfg.Runner.Name() != run.RunnerPoolExecutor {
			return runner.Sandbox{}, fmt.Errorf("%w: %q is not an executor of this deployment", runner.ErrExecutorUnavailable, run.RunnerPoolExecutor)
		}
	default:
		s.stampServerRun(ctx, run, spec)
	}
	return s.cfg.Runner.CreateSandbox(ctx, spec)
}

// serverRunStamper records where a run Wardyn launched itself goes (store.PG.StampServerRun).
type serverRunStamper interface {
	StampServerRun(ctx context.Context, id uuid.UUID, remote string, reserved types.RunnerAmounts) error
}

// stampServerRun counts a run Wardyn launched itself (a sign-in, a probe, a source scan), which no pool
// placed, in the In use of the remote-hosted runner it goes to: the one substrate CreateSandbox picks
// when this deployment runs exactly one. It is exempt from that runner's limits, as it is from
// deny_placements, but never uncounted, so people's runs there are not over-admitted beside it. A
// failed write is logged: the run is not refused for its own accounting.
func (s *Server) stampServerRun(ctx context.Context, run types.AgentRun, spec runner.SandboxSpec) {
	st, ok := s.cfg.Store.(serverRunStamper)
	remote := s.cfg.Runner.Name()
	if !ok || !types.ValidRemoteRunnerID(remote) || !s.remoteRunnerConfigured(remote) {
		return
	}
	size := runner.EffectiveResources(spec.Resources)
	reserved := types.RunnerAmounts{CPUMillis: size.AgentCPULimitMillis, MemoryMiB: size.AgentMemoryLimitMiB, DiskMiB: max(0, spec.Resources.DiskMiB)}
	if err := st.StampServerRun(ctx, run.ID, remote, reserved); err != nil {
		slog.WarnContext(ctx, "wardynd: count a server run on its remote-hosted runner", slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
}
