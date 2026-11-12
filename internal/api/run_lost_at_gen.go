// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// generationStopperFor is the runner's generation-fenced stops for run, when they can be used: a container
// (a VM has no proxy generation), a runner that implements them, and, for a run on a runner, a runner whose
// session is live with the ProxyFence cap (last-known caps never count: an unconnected runner is stopped
// the plain way, which queues).
func (s *Server) generationStopperFor(run types.AgentRun) (runner.ProxyGenerationStopper, bool) {
	gs, ok := s.cfg.Runner.(runner.ProxyGenerationStopper)
	if !ok || run.MachineKind.IsVM() {
		return nil, false
	}
	if run.RunnerID != nil {
		if caps, ok := s.runnerConnected(*run.RunnerID); !ok || !caps.ProxyFence {
			return nil, false
		}
	}
	return gs, true
}

// stopLostSandboxAt is stopLostSandbox for a caller that acts for policy generation gen and may run after
// its lock was lost: where the runner honours generations it never stops the proxy or sandbox of a newer
// revive (runner.ErrProxyGenerationNewer is not a failure: that revive owns the run now). gen 0, a VM, or a
// runner that is not connected with ProxyFence stops the plain way.
func (s *Server) stopLostSandboxAt(ctx context.Context, run types.AgentRun, gen int) error {
	if gen <= 0 {
		return s.stopLostSandbox(ctx, run)
	}
	gs, ok := s.generationStopperFor(run)
	if !ok {
		return s.stopLostSandbox(ctx, run)
	}
	newer := func(err error) bool {
		if !errors.Is(err, runner.ErrProxyGenerationNewer) {
			return false
		}
		slog.InfoContext(ctx, "wardynd: a newer revive owns the run's proxy; this attempt's stop is not needed",
			slog.String("run_id", run.ID.String()), slog.Int("generation", gen), slog.Any("err", err))
		return true
	}
	end, readErr := s.outageEnd(ctx, run)
	var cutErr error
	if end != lostEndPast {
		_, canCut := s.cfg.Runner.(runner.ProxyStopper)
		if !canCut && end == lostEndOpen {
			return runner.ErrEndUnsupported
		}
		if canCut {
			cutErr = gs.StopProxyAt(ctx, run.SandboxRef, gen)
		}
		if newer(cutErr) {
			return nil // a newer revive owns the run: no end call, no stop mark
		}
		switch end {
		case lostEndOpen:
			return cutErr
		case lostEndReadFailed:
			return lostReadFailure(readErr, cutErr)
		}
	}
	err := gs.EndSandboxAt(ctx, run.SandboxRef, gen)
	if errors.Is(err, runner.ErrAWSCacheWipeFailed) {
		err = nil
	}
	if newer(err) {
		return nil
	}
	if leaser, ok := s.cfg.Store.(store.RunLeaser); ok && err == nil && run.LostReason == types.LostOutage && run.LostAt != nil {
		// A failed write only leaves the run counted; the lease sweep re-asserts this stop every pass.
		if merr := leaser.MarkRunAgentStopped(ctx, run.ID, *run.LostAt); merr != nil {
			slog.WarnContext(ctx, "wardynd: recording a kept run's stopped agent failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", merr))
		}
	}
	if errors.Is(err, runner.ErrEndUnsupported) && cutErr != nil {
		return cutErr // the cut failed too: containment unresolved, retried by the lease sweep
	}
	return err
}

// replaceProxyHashedAt is h.ReplaceProxyHashed that names the start's generation when h honours it.
func replaceProxyHashedAt(ctx context.Context, h runner.ProxyReviverHashed, ref string, cfgJSON []byte, gen int) (string, error) {
	if at, ok := h.(runner.ProxyReviverAt); ok && gen > 0 {
		return at.ReplaceProxyHashedAt(ctx, ref, cfgJSON, gen)
	}
	return h.ReplaceProxyHashed(ctx, ref, cfgJSON)
}

// runnerNeedsUpdateToRevive is the caps of the connected runner a container run is on when that runner is a
// Docker one without ProxyFence, which cannot replace a proxy under a generation (every Kubernetes runner
// sets EndsUnkept and is not refused; a VM and an unconnected runner never are).
func (s *Server) runnerNeedsUpdateToRevive(run types.AgentRun) (runnerwire.Caps, bool) {
	if run.RunnerID == nil || run.MachineKind.IsVM() {
		return runnerwire.Caps{}, false
	}
	caps, ok := s.runnerConnected(*run.RunnerID)
	return caps, ok && !caps.ProxyFence && !caps.EndsUnkept
}
