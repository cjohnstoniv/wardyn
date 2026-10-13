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
// has no reservation concept (a hard cap only), so its request is the limit it applied. The proxy
// envelope is the one the drivers apply, so an Azure-gated run records runner.AzureProxyMemoryMiB.
func runSizing(kind string, res runner.Resources, azure bool) store.RunSizing {
	sz := runner.EffectiveResources(res)
	z := store.RunSizing{
		RunnerKind:            kind,
		AgentCPURequestMillis: sz.AgentCPURequestMillis,
		AgentCPULimitMillis:   sz.AgentCPULimitMillis,
		AgentMemoryRequestMiB: sz.AgentMemoryRequestMiB,
		AgentMemoryLimitMiB:   sz.AgentMemoryLimitMiB,
	}
	proxyCPU, proxyMem := runner.ProxyLimitsFor(azure)
	z.ProxyMemoryMiB = proxyMem
	if proxyCPU > 0 {
		z.ProxyCPUMillis = &proxyCPU
	}
	if kind != "k8s" {
		z.AgentCPURequestMillis, z.AgentMemoryRequestMiB = z.AgentCPULimitMillis, z.AgentMemoryLimitMiB
	}
	return z
}

// runOnDeployment reports whether run's bound placement is one of this deployment's own substrates, the
// only runs whose dispatch-time sizing is the deployment's to record. A run bound to a registered runner,
// or to an executor this deployment does not run, is not, and a run no pool placed is.
func (s *Server) runOnDeployment(run types.AgentRun) bool {
	if run.RunnerID != nil || run.Placement == types.PlacementLocal {
		return false
	}
	return run.RunnerPoolExecutor == "" || s.remoteRunnerConfigured(run.RunnerPoolExecutor)
}

// recordRunSizing writes the dispatch-time sizing record of a run on this deployment (runOnDeployment). A failure is logged and leaves the
// row NULL; it never blocks dispatch.
func (s *Server) recordRunSizing(ctx context.Context, run types.AgentRun, res runner.Resources, azure bool) {
	setter, ok := s.cfg.Store.(runSizingSetter)
	if !ok || !s.runOnDeployment(run) {
		return
	}
	if err := setter.SetRunSizing(ctx, run.ID, runSizing(s.cfg.RunnerTarget, res, azure)); err != nil {
		slog.WarnContext(ctx, "wardynd: persist run sizing failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
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
