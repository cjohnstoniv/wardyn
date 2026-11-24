// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// outageRecovery is what the lapse sweeps do with a lapsed run (#4082): end it under its outage terms, hold it unmarked
// for its proxy to recover, or neither (the caller's existing action). A run its runner holds is never finalized
// here: its strict end is the caller's own loss mark followed by the runner's end action.
type outageRecovery int

const (
	outageRecoveryNone outageRecovery = iota
	outageRecoveryHold
	outageRecoveryEndStrict
	outageRecoveryEndUnattended
	outageRecoveryEndStrictOnRunner
)

// The finalize reasons of the two control-plane ends. The strict one names no outage: a spent strict allowance ends
// the run whether or not an outage was recorded.
const (
	outageEndStrictReason     = "the run's strict outage terms end it: its network was not confirmed within its allowance"
	outageEndUnattendedReason = "control-plane outage: this policy ends unattended runs after an outage"
)

// outageRecoveryFor reads run's recovery facts (store.RunTokenRecoverer.RunRecoveryCheck, with OUT-1's store tracker's
// Recovering) and decides. outage is the pass's verdict (v.outage) or a covering window; serverRun limits the
// unattended end to the server's own runs. A store that is not a store.RunTokenRecoverer, or a read error, answers
// none, which leaves the caller's v0.10.2 action. A server whose identity provider cannot recover (recoverySupported
// false, as with WARDYN_IDENTITY=spire) still takes both ends (strict spent; unattended in an outage), and then answers
// none.
func (s *Server) outageRecoveryFor(ctx context.Context, run types.AgentRun, v lapseVerdict, serverRun bool) outageRecovery {
	rec, ok := s.cfg.Store.(store.RunTokenRecoverer)
	if !ok {
		return outageRecoveryNone
	}
	runnerHeld := run.Placement == types.PlacementLocal && run.RunnerID != nil
	// A runner run never reads the store tracker: the STATE judge cannot (it runs in the store), and the two must agree.
	rc, err := rec.RunRecoveryCheck(ctx, run.ID, s.cfg.StoreHealth.Recovering() && run.RunnerID == nil)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading a lapsed run's recovery facts failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return outageRecoveryNone
	}
	outage := v.outage || rc.Covered
	switch {
	case rc.Cut:
		return outageRecoveryNone
	case rc.StrictSpent && runnerHeld:
		return outageRecoveryEndStrictOnRunner
	case rc.StrictSpent:
		return outageRecoveryEndStrict
	case outage && rc.EndUnattended && serverRun && !run.Interactive:
		return outageRecoveryEndUnattended
	case !s.recoverySupported(), !rc.Held:
		return outageRecoveryNone
	case runnerHeld && !s.runnerAdmissibleForHold(ctx, run):
		return outageRecoveryNone
	}
	return outageRecoveryHold
}

// runnerAdmissibleForHold is whether run's runner could carry its proxy's recovery now (runnerAdmissibleNow):
// claimed, its owner active, runners on. A read error answers false, logged.
func (s *Server) runnerAdmissibleForHold(ctx context.Context, run types.AgentRun) bool {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return false
	}
	admissible, err := s.runnerAdmissibleNow(ctx, st, *run.RunnerID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading whether a held run's runner is admissible failed; not holding the run",
			slog.String("run_id", run.ID.String()), slog.String("runner_id", run.RunnerID.String()), slog.Any("err", err))
	}
	return admissible && err == nil
}

// endStrictRunnerRun ends the sandbox of a runner-held run whose strict allowance is spent, after its loss mark has
// stopped its proxy: the end endRun uses, sent now or queued as the runner's end action while it is away. A runner
// that cannot keep an ended sandbox gets its kill instead. Pending on the runner is success; any other error is
// logged and not retried, since the proxy is already stopped and a revive is refused (reviveStrictSpent).
func (s *Server) endStrictRunnerRun(ctx context.Context, run types.AgentRun) {
	_, err := s.endSandbox(ctx, run)
	if errors.Is(err, runner.ErrEndUnsupported) {
		err = s.cfg.Runner.StopSandbox(ctx, run.SandboxRef)
	}
	if err != nil && !errors.Is(err, runner.ErrPendingOnRunner) {
		slog.WarnContext(ctx, "wardynd: ending a strict run's sandbox on its runner failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
}

// reviveStrictSpent refuses the revive of a run lost to an outage whose strict outage allowance has run out: its
// terms ended it, and a revive's fresh token would restart an allowance it already spent. nil for any other run, and
// on a store that keeps no recovery facts.
func (s *Server) reviveStrictSpent(ctx context.Context, run types.AgentRun) *reviveError {
	rec, ok := s.cfg.Store.(store.RunTokenRecoverer)
	if !ok || run.LostAt == nil || run.LostReason != types.LostOutage {
		return nil
	}
	rc, err := rec.RunRecoveryCheck(ctx, run.ID, false)
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveClaimFailed, loggedMsg(ctx, "read the run's outage terms", err))
	}
	if rc.StrictSpent {
		return reviveRefused(http.StatusConflict, reasonReviveStrictAllowanceSpent,
			"The run's strict outage terms ended it: its network was not confirmed within its allowance. Start a new run.")
	}
	return nil
}
