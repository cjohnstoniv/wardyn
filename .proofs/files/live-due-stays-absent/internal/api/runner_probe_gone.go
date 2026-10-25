// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerProbe is what probeRunnerSandbox did.
type runnerProbe int

const (
	// runnerProbeNone: not a gone case, not known, or left to a later pass. The caller goes on as
	// before.
	runnerProbeNone runnerProbe = iota
	// runnerProbeAbsent: stamped absent and not yet confirmed. The caller keeps watching: the stamp is
	// due within store.RunnerSandboxGoneAfter.
	runnerProbeAbsent
	// runnerProbeEnded: confirmed, and endGoneRun ended the run FAILED (gone).
	runnerProbeEnded
)

// onRunner reports whether ref names a sandbox on a registered runner (placement.RunnerRefPrefix).
func onRunner(ref string) bool { return strings.HasPrefix(ref, "runner:") }

// probeRunnerSandbox is #3667's gone end for a runner that stays connected. A runner run whose agent
// probe (st) reads stopped with no exit code gets one Status call over the runner's live session, and
// the answer goes into the absent stamp the runner's STATE uses (store.PG.NoteRunnerSandboxProbe). Two
// absent reports at least store.RunnerSandboxGoneAfter apart, of either kind, with none between that it
// was there, end the run (endGoneRun). An error records nothing: it is unknown, never absent or present.
func (s *Server) probeRunnerSandbox(ctx context.Context, run types.AgentRun, ref string, st runner.Status) runnerProbe {
	pg, ok := s.cfg.Store.(store.PG)
	if !ok || run.Placement != types.PlacementLocal || run.RunnerID == nil || ref == "" || run.SandboxRef != ref ||
		st.State != types.RunStopped || st.ExitCode != nil {
		return runnerProbeNone
	}
	issued, err := pg.RunnerProbeClock(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading the clock for a runner sandbox check failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return runnerProbeNone
	}
	// Status decides, not the agent probe: AgentStatus can drop an exit code Status keeps (docker
	// driver_helpers.go).
	pctx, cancel := context.WithTimeout(ctx, watcherProbeTimeout)
	cst, err := s.cfg.Runner.Status(pctx, ref)
	cancel()
	if err != nil {
		return runnerProbeNone
	}
	due, stamped, err := pg.NoteRunnerSandboxProbe(ctx, *run.RunnerID, run.ID, ref, cst.State == types.RunStopped && cst.ExitCode == nil, issued)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "wardynd: recording a runner sandbox check failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return runnerProbeNone
	case !stamped:
		return runnerProbeNone
	case due || !due:
		return runnerProbeAbsent
	}
	if s.endGoneRun(ctx, pg, run, func(ctx context.Context, at time.Time) (bool, error) {
		return pg.MarkRunnerRunGoneProbed(ctx, *run.RunnerID, run.ID, ref, issued, at)
	}) {
		return runnerProbeEnded
	}
	return runnerProbeNone
}
