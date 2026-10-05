// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runSizingSetter is the OPTIONAL store capability the dispatch-time sizing record needs,
// kept off store.Store for the reason runStatusDetailSetter states.
type runSizingSetter interface {
	SetRunSizing(ctx context.Context, id uuid.UUID, z store.RunSizing) error
}

// runSizing maps the one runner.EffectiveResources result onto the stored record. Docker
// has no reservation concept (a hard cap only), so its request is the limit it applied.
func runSizing(kind string, res runner.Resources) store.RunSizing {
	sz := runner.EffectiveResources(res)
	z := store.RunSizing{
		RunnerKind:            kind,
		AgentCPURequestMillis: sz.AgentCPURequestMillis,
		AgentCPULimitMillis:   sz.AgentCPULimitMillis,
		AgentMemoryRequestMiB: sz.AgentMemoryRequestMiB,
		AgentMemoryLimitMiB:   sz.AgentMemoryLimitMiB,
		ProxyMemoryMiB:        sz.ProxyMemoryMiB,
	}
	if sz.ProxyCPUMillis > 0 {
		z.ProxyCPUMillis = &sz.ProxyCPUMillis
	}
	if kind != "k8s" {
		z.AgentCPURequestMillis, z.AgentMemoryRequestMiB = z.AgentCPULimitMillis, z.AgentMemoryLimitMiB
	}
	return z
}

// recordRunSizing writes the dispatch-time sizing record. A failure is logged and leaves the
// row NULL; it never blocks dispatch.
func (s *Server) recordRunSizing(ctx context.Context, runID uuid.UUID, res runner.Resources) {
	setter, ok := s.cfg.Store.(runSizingSetter)
	if !ok {
		return
	}
	if err := setter.SetRunSizing(ctx, runID, runSizing(s.cfg.RunnerTarget, res)); err != nil {
		slog.WarnContext(ctx, "wardynd: persist run sizing failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// recordRunDiskCap persists the resolved disk cap for the run page's disk-used reading (RL-13:
// best-effort, like SetRunImage — a failed write must not block dispatch, it just leaves that
// run's cap unknown to the resources widget).
func (s *Server) recordRunDiskCap(ctx context.Context, runID uuid.UUID, res *types.ResourceLimits) {
	effectiveDiskMiB := 0
	if res != nil {
		effectiveDiskMiB = res.DiskMiB
	}
	if err := s.cfg.Store.SetRunDiskMiB(ctx, runID, effectiveDiskMiB); err != nil {
		slog.WarnContext(ctx, "wardynd: persist run disk cap failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}
