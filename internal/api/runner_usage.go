// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Usage as every read shows it (0.9, "usage always shown"): In use is the store's one In use query
// (store.RunnerUsage, the predicate admission counts under the runner rows' locks), Available is
// runnerpool.Available of the declared limits. Numbers only: no read names the runs, their owners or a
// per-person split.

// runnersInUse is the In use of the runners named, by key. A store that cannot count answers none.
func (s *Server) runnersInUse(ctx context.Context, runners []uuid.UUID, remotes []string) (map[string]types.RunnerTally, error) {
	rs, ok := s.cfg.Store.(store.RunnerInventoryStore)
	if !ok || (len(runners) == 0 && len(remotes) == 0) {
		return map[string]types.RunnerTally{}, nil
	}
	return rs.RunnerUsage(ctx, runners, remotes, uuid.Nil)
}

// diskCapped reports whether a target's disk limits are enforced, not just counted.
func diskCapped(e types.StorageEnforcement) bool {
	return e == types.StorageEnforcementFilesystem || e == types.StorageEnforcementEviction
}

// selfHostedUsage is a self-hosted runner's usage: online is the hub's live view, and what it reports
// is its last caps event on this replica.
func (s *Server) selfHostedUsage(row types.Runner, used types.RunnerTally) types.RunnerUsage {
	online := row.State == types.RunnerClaimed && s.runnerOnline(row.ID)
	facts, _ := s.runnerFacts(row.ID)
	var reported *types.RunnerAmounts
	if caps, _, _ := s.runnerReportedCaps(row.ID); caps != nil {
		reported = &types.RunnerAmounts{CPUMillis: caps.CPUMillisMax, MemoryMiB: caps.MemoryMiBMax}
	}
	return runnerpool.Usage(row.Limits, used, online, diskCapped(facts.EphemeralDiskEnforcement), reported)
}

// remoteUsage is a remote-hosted runner's usage: online is its last health read, and what it reports
// is what its substrate measures now. A runner this server does not run is offline and reports nothing.
func (s *Server) remoteUsage(ctx context.Context, rr types.RemoteRunner, used types.RunnerTally) types.RunnerUsage {
	if !s.remoteRunnerConfigured(rr.ID) {
		return runnerpool.Usage(rr.Limits, used, false, false, nil)
	}
	facts, online := s.executorFacts(ctx, rr.ID)
	var reported *types.RunnerAmounts
	m, why := s.measureRemote(ctx, rr.ID)
	if m != (types.RunnerAmounts{}) {
		reported = &m
	}
	u := runnerpool.Usage(rr.Limits, used, online, diskCapped(facts.EphemeralDiskEnforcement), reported)
	u.ReportedWhy = why
	return u
}

// poolCapacity adds up a pool's runners the caller's catalogue row may show: its organization runners,
// or a user pool's own runners and remote-hosted runners. Total and In use count every member that
// declares limits; Available counts the online ones, and none in a pool that is switched off; the
// largest per-run limit is the biggest run a member takes. Numbers only.
func (s *Server) poolCapacity(row store.RunnerPoolCatalogueRow, fits map[string]runnerFit, online func(key string) bool) *client.PoolCapacity {
	keys := slices.Clone(row.ExecutorIDs)
	for _, id := range row.OwnRunnerIDs {
		keys = append(keys, id.String())
	}
	out := &client.PoolCapacity{Runners: len(keys)}
	active := row.Pool.State == types.RunnerPoolActive
	for _, k := range keys {
		f, up := fits[k], online(k)
		if up {
			out.RunnersOnline++
		}
		if f.limits == nil {
			continue
		}
		l := f.limits
		out.Total = addTally(out.Total, types.RunnerTally{Runs: l.RunsAtOnce, CPUMillis: l.Total.CPUMillis, MemoryMiB: l.Total.MemoryMiB, DiskMiB: l.Total.DiskMiB})
		out.InUse = addTally(out.InUse, f.used)
		if active {
			out.Available = addTally(out.Available, runnerpool.Available(l, f.used, up))
		}
		out.LargestPerRun = types.RunnerAmounts{CPUMillis: max(out.LargestPerRun.CPUMillis, l.PerRun.CPUMillis),
			MemoryMiB: max(out.LargestPerRun.MemoryMiB, l.PerRun.MemoryMiB), DiskMiB: max(out.LargestPerRun.DiskMiB, l.PerRun.DiskMiB)}
	}
	return out
}

func addTally(a, b types.RunnerTally) types.RunnerTally {
	return types.RunnerTally{Runs: a.Runs + b.Runs, CPUMillis: a.CPUMillis + b.CPUMillis, MemoryMiB: a.MemoryMiB + b.MemoryMiB, DiskMiB: a.DiskMiB + b.DiskMiB}
}
