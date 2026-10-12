// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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
	case run.RunnerID != nil:
		spec.RunnerID = run.RunnerID.String()
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
	}
	return s.cfg.Runner.CreateSandbox(ctx, spec)
}
