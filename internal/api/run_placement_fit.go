// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

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
// self-hosted runners, the organization's own runners and every remote-hosted runner. A store that
// cannot say answers none, so every candidate is then refused as declaring no limits: a runner is never
// taken as unlimited. A read that fails is an error, so the caller answers a retryable failure, not
// advice to set limits.
func (s *Server) runnerFits(ctx context.Context) (map[string]runnerFit, error) {
	out := map[string]runnerFit{}
	rs, ok := s.cfg.Store.(store.RunnerInventoryStore)
	if !ok {
		return out, nil
	}
	var ids []uuid.UUID
	if owner := poolOwner(ctx); owner != "" {
		if own, ok := s.cfg.Store.(store.RunnerStore); ok {
			runners, err := own.ListRunnersByOwner(ctx, owner)
			if err != nil {
				return nil, fmt.Errorf("read runner limits: %w", err)
			}
			for _, r := range runners {
				ids = append(ids, r.ID)
				out[r.ID.String()] = runnerFit{limits: r.Limits}
			}
		}
	}
	// The organization's own runners, which an organization pool offers (orgRunnerTargets).
	if rr, ok := s.cfg.Store.(store.RunnerStore); ok {
		runners, err := rr.ListRunners(ctx)
		if err != nil {
			return nil, fmt.Errorf("read organization runner limits: %w", err)
		}
		for _, r := range runners {
			if r.OwnerKind == types.RunnerOwnerOrg && r.State == types.RunnerClaimed {
				ids = append(ids, r.ID)
				out[r.ID.String()] = runnerFit{limits: r.Limits}
			}
		}
	}
	var remotes []string
	if rr, ok := s.cfg.Store.(store.RemoteRunnerStore); ok {
		rows, err := rr.ListRemoteRunners(ctx)
		if err != nil {
			return nil, fmt.Errorf("read remote runner limits: %w", err)
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
		return nil, fmt.Errorf("read runner usage: %w", err)
	}
	for k, f := range out {
		f.used = used[k]
		out[k] = f
	}
	return out, nil
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

// runnerSource is the runner limit source's candidate: one real runner among the candidates that
// passed every demand (the pinned one when the request names one), so the size the run is cut to is a
// size that runner takes whole. A runner whose per-run limit holds everything the run asks for (ask; a
// 0 is not asked) is preferred, so an ask one runner can take is never cut for another's sake; among
// those, or among all when none does, the fold takes the largest by CPU, memory, then disk. Placement
// then keeps only candidates whose per-run limit holds the applied size. A runner with room now beats
// a full one, so a run that names no size is not cut to a busy large runner while a smaller one idles.
func runnerSource(fit []poolTarget, pin string, ask types.RunnerAmounts) []runnerpool.RunnerCapacity {
	roomy := slices.ContainsFunc(fit, func(t poolTarget) bool {
		return t.limits != nil && !t.full && (pin == "" || t.id == pin)
	})
	var all, whole []runnerpool.RunnerCapacity
	for _, t := range fit {
		if t.limits == nil || (pin != "" && t.id != pin) || (roomy && t.full) {
			continue
		}
		p := t.limits.PerRun
		c := runnerpool.RunnerCapacity{CPUMillis: p.CPUMillis, MemoryMiB: p.MemoryMiB, DiskMiB: p.DiskMiB}
		all = append(all, c)
		if ask.CPUMillis <= p.CPUMillis && ask.MemoryMiB <= p.MemoryMiB && ask.DiskMiB <= p.DiskMiB {
			whole = append(whole, c)
		}
	}
	if len(whole) > 0 {
		return whole
	}
	return all
}

// unboundSize is the size the run gets with no runner source in the fold: what the deployment, the pool,
// governance and its own ask give it. Its disk is 0 when nothing but a runner sizes the main drive. Only
// a launch that waits (queued) has one.
func (s *Server) unboundSize(ctx context.Context, in placementInput, pool types.RunnerPool, queued bool) (*types.RunnerAmounts, *runRefusal) {
	if !queued {
		return nil, nil
	}
	limits, applied, refusal := s.runSize(ctx, in, &pool, nil)
	if refusal != nil {
		return nil, refusal
	}
	requestDisk := 0
	if in.req.Resources != nil {
		requestDisk = in.req.Resources.DiskMiB
	}
	return &types.RunnerAmounts{CPUMillis: int64(applied.CPUMillis.Applied), MemoryMiB: int64(applied.MemoryMiB.Applied),
		DiskMiB: int64(mainDriveFor(in.policyDiskMiB, requestDisk, limits.DiskMiB).MiB)}, nil
}

// startSizeOn is the size a run of unbound size starts with on a runner with limits l: each amount cut
// to the runner's per-run limit, as the fold cuts it when that runner is the run's source, and an
// amount nothing else sizes (0) is the runner's per-run limit.
func startSizeOn(unbound types.RunnerAmounts, l *types.RunnerLimits) types.RunnerAmounts {
	cut := func(v, limit int64) int64 {
		if v == 0 {
			return limit
		}
		return min(v, limit)
	}
	return types.RunnerAmounts{CPUMillis: cut(unbound.CPUMillis, l.PerRun.CPUMillis), MemoryMiB: cut(unbound.MemoryMiB, l.PerRun.MemoryMiB),
		DiskMiB: cut(unbound.DiskMiB, l.PerRun.DiskMiB)}
}

// insertRoomRefusal answers the insert's own refusal under the runner's row lock, nil for any other
// error: another launch took the runner's last room after placement read it (runner_at_capacity), or
// the runner's limits were lowered under the run's size since, so it could never take it
// (placement_capability).
func insertRoomRefusal(err error, pool types.ResolvedRunnerPool) *runRefusal {
	var full *store.RunnerAtCapacityError
	if !errors.As(err, &full) {
		return nil
	}
	reason := placement.ReasonRunnerAtCapacity
	if full.NeverFits {
		reason = placement.ReasonPlacementCapability
	}
	return targetRefusal(reason, pool.Name, pool.Scope)
}

// reserved is what the run holds on its runner while it holds a sandbox there: the CPU and memory it
// gets and its main drive, as the fold sized them. The insert writes it, and a queued run's start
// writes it again, since the start folds the run afresh.
func (p runPlacement) reserved(drive mainDrive) types.RunnerAmounts {
	return types.RunnerAmounts{CPUMillis: int64(p.applied.CPUMillis.Applied), MemoryMiB: int64(p.applied.MemoryMiB.Applied), DiskMiB: int64(drive.MiB)}
}
