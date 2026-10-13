// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Runner limits at placement (0.9). Every candidate, either kind, is held to the limits its runner
// declared, read from the store (never from what a runner reports, which is per replica and live
// only): a runner with none declared is never a candidate, a run past its per-run limit never goes
// there, and one with no room (its Runs at once, or a total) is passed over or waited for. The
// figures are the store's In use, read without a lock; the insert, the promotion and the start
// count again under the runner rows' locks (store.lockRunnerRooms), with the same runnerpool.Fits.

// runnerFit is one runner's declared limits and what its runs hold now.
type runnerFit struct {
	limits *types.RunnerLimits
	used   types.RunnerTally
}

// runnerFits is every runner the caller's runs could be placed on, by target id: the caller's own
// self-hosted runners and every remote-hosted runner. A store that cannot say, or a read that fails,
// answers none, so every candidate is then refused as declaring no limits: a runner is never taken
// as unlimited.
func (s *Server) runnerFits(ctx context.Context) map[string]runnerFit {
	out := map[string]runnerFit{}
	rs, ok := s.cfg.Store.(store.RunnerInventoryStore)
	if !ok {
		return out
	}
	var ids []uuid.UUID
	if owner := poolOwner(ctx); owner != "" {
		if own, ok := s.cfg.Store.(store.RunnerStore); ok {
			runners, err := own.ListRunnersByOwner(ctx, owner)
			if err != nil {
				slog.WarnContext(ctx, "wardynd: read runner limits; no runner of yours is a candidate", slog.Any("err", err))
				return map[string]runnerFit{}
			}
			for _, r := range runners {
				ids = append(ids, r.ID)
				out[r.ID.String()] = runnerFit{limits: r.Limits}
			}
		}
	}
	var remotes []string
	if rr, ok := s.cfg.Store.(store.RemoteRunnerStore); ok {
		rows, err := rr.ListRemoteRunners(ctx)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: read remote runner limits; no remote-hosted runner is a candidate", slog.Any("err", err))
			return map[string]runnerFit{}
		}
		for _, row := range rows {
			remotes = append(remotes, row.Runner.ID)
			out[row.Runner.ID] = runnerFit{limits: row.Runner.Limits}
		}
	}
	// A promoted run's start finds the run itself holding its place on its runner: that place, and what
	// it reserves there, are this run's own, so they never count against it.
	used, err := rs.RunnerUsage(ctx, ids, remotes, promotedRun(ctx))
	if err != nil {
		slog.WarnContext(ctx, "wardynd: read runner usage; no runner is a candidate", slog.Any("err", err))
		return map[string]runnerFit{}
	}
	for k, f := range out {
		f.used = used[k]
		out[k] = f
	}
	return out
}

// need is what the run reserves on its runner, as far as the fold knows it here.
func (d placementDemand) need() types.RunnerAmounts {
	return types.RunnerAmounts{CPUMillis: d.cpu, MemoryMiB: d.mem, DiskMiB: d.disk}
}

// fitRefusal is t's answer under its runner's declared limits: runner_limits_unset when it declares
// none, placement_capability when the run is past its per-run limit, and runner_at_capacity when it
// has no room and the run would start at once (a run that waits in the pool's queue is held to room
// by the promotion instead).
func (d placementDemand) fitRefusal(t poolTarget) placement.Reason {
	f := d.fits[t.id]
	switch runnerpool.Fits(f.limits, f.used, d.need()) {
	case runnerpool.FitUnset:
		return placement.ReasonRunnerLimitsUnset
	case runnerpool.FitTooBig:
		return placement.ReasonPlacementCapability
	case runnerpool.FitNoRoom:
		if d.holdRunnerCapacity {
			return placement.ReasonRunnerAtCapacity
		}
	}
	return ""
}

// withFit is t with its runner's declared limits and whether it has room for the run now.
func (d placementDemand) withFit(t poolTarget) poolTarget {
	f := d.fits[t.id]
	t.limits = f.limits
	t.full = t.full || runnerpool.Fits(f.limits, f.used, d.need()) == runnerpool.FitNoRoom
	return t
}

// runnerSource is the runner limit source's candidates: the per-run limits of the candidates that
// passed every demand, narrowed to the pinned one when the request names one. The fold takes the
// largest of them, so a dry door shows the biggest run the pool can take, and placement then keeps only
// candidates whose per-run limit holds the applied size.
func runnerSource(fit []poolTarget, pin string) []runnerpool.RunnerCapacity {
	var out []runnerpool.RunnerCapacity
	for _, t := range fit {
		if t.limits != nil && (pin == "" || t.id == pin) {
			out = append(out, runnerpool.RunnerCapacity{CPUMillis: t.limits.PerRun.CPUMillis, MemoryMiB: t.limits.PerRun.MemoryMiB, DiskMiB: t.limits.PerRun.DiskMiB})
		}
	}
	return out
}
