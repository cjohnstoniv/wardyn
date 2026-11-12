// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

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
func (s *Server) stopLostSandboxAt(ctx context.Context, run types.AgentRun, now time.Time, gen int) error {
	if gen <= 0 {
		return s.stopLostSandbox(ctx, run, now)
	}
	gs, ok := s.generationStopperFor(run)
	if !ok {
		return s.stopLostSandbox(ctx, run, now)
	}
	proxyOnly := run.LostReason == types.LostOutage && (run.EndsAt == nil || now.Before(*run.EndsAt))
	var err error
	if proxyOnly {
		if _, ok := s.cfg.Runner.(runner.ProxyStopper); !ok {
			return runner.ErrEndUnsupported
		}
		err = gs.StopProxyAt(ctx, run.SandboxRef, gen)
	} else {
		err = gs.EndSandboxAt(ctx, run.SandboxRef, gen)
		if errors.Is(err, runner.ErrAWSCacheWipeFailed) {
			err = nil
		}
	}
	if errors.Is(err, runner.ErrProxyGenerationNewer) {
		slog.InfoContext(ctx, "wardynd: a newer revive owns the run's proxy; this attempt's stop is not needed",
			slog.String("run_id", run.ID.String()), slog.Int("generation", gen), slog.Any("err", err))
		return nil
	}
	if leaser, ok := s.cfg.Store.(store.RunLeaser); ok && err == nil && !proxyOnly && run.LostReason == types.LostOutage && run.LostAt != nil {
		// A failed write only leaves the run counted; the lease sweep re-asserts this stop every pass.
		if merr := leaser.MarkRunAgentStopped(ctx, run.ID, *run.LostAt); merr != nil {
			slog.WarnContext(ctx, "wardynd: recording a kept run's stopped agent failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", merr))
		}
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
