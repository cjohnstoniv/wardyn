// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Lost runs (long-holds design rev 4, §4 rows 2–3, RL-9). A run outlives its
// sandbox: instead of failing and tearing down, an interactive run is KEPT,
// like an ended one (run_lease.go), with its proxy removed so it has no
// egress, its approvals cancelled and its broker credentials revoked. Its run
// identity is kept for a later revive to mint from; nothing holds its token.
//
//   - reboot: the watcher saw the agent container exited but still there (a
//     host reboot, a Docker Desktop restart, a long suspend). The agent stays
//     stopped and its writable layer is the kept files.
//   - outage: the run's token lapsed because the control plane was unreachable
//     past the proxy's renew window. The proxy then gives up renewing but keeps
//     forwarding allowlisted egress on a dead identity, audit-dark
//     (egress/proxy/renew.go), so it is removed; the agent keeps running.
//
// A lost run is kept while its lease or its ended-run grace is live: until its
// end plus the grace, or until someone kills it when it has no end. Everything
// that cannot be kept fails closed and is torn down as before: a headless run
// (the completion watcher that would finish it skips kept runs), a run past
// its end and grace, a substrate that cannot keep a sandbox (Kubernetes), or a
// proxy removal that fails.

// runTokenLapseAfter is how long a RUNNING run may go without a renew before
// its identity is dead. The proxy renews at least every 30 minutes and gives
// up an hour after its last good renew (egress/proxy/renew.go
// renewGiveUpAfter, which mirrors the embedded provider's 1 h tokenTTL); the
// five minutes cover the stamp landing a moment after the mint and a small
// clock difference between the database and the minting replica.
const runTokenLapseAfter = time.Hour + 5*time.Minute

// sweepLapsedRunTokens is the boot and sweep check: every RUNNING run whose
// token lapsed is marked lost (outage) with its proxy removed, or torn down.
func (s *Server) sweepLapsedRunTokens(ctx context.Context) error {
	loser, ok := s.cfg.Store.(store.RunLoser)
	if !ok || s.cfg.Runner == nil {
		return nil
	}
	runs, err := loser.ListLapsedTokenRuns(ctx, runTokenLapseAfter)
	if err != nil {
		return err
	}
	for _, run := range runs {
		func() {
			ctx, cancel := context.WithTimeout(ctx, reconcileFinalizeTimeout)
			defer cancel()
			if s.loseRun(ctx, loser, run, types.LostOutage, types.RunFailed) {
				return
			}
			s.reconcileFinalize(ctx, run.ID, types.RunFailed, run.SandboxRef,
				"run token lapsed: the control plane was unreachable past its renew window")
		}()
	}
	return nil
}

// keepRebootedRun is the watcher's side: an agent it observed terminal whose
// container still exists (the probe carries an exit code; a gone container
// does not) is marked lost (reboot) rather than finalized. false leaves the
// caller's finalize as before. A substrate that cannot keep it (Kubernetes)
// tears it down in the state the exit code says, as the finalize would have.
func (s *Server) keepRebootedRun(ctx context.Context, run types.AgentRun, st runner.Status) bool {
	loser, ok := s.cfg.Store.(store.RunLoser)
	if !ok || st.ExitCode == nil {
		return false
	}
	terminal := types.RunFailed
	if *st.ExitCode == 0 {
		terminal = types.RunCompleted
	}
	return s.loseRun(ctx, loser, run, types.LostReboot, terminal)
}

// loseRun marks run lost for reason and removes its proxy. false means the run
// cannot be kept, and the caller's fail-closed arm applies. true means it is
// taken care of: kept, torn down (as terminal) because its proxy could not be
// removed, or left alone because the claim did not land (another replica took
// it, a renew landed, or it went terminal) or could not be written (the next
// pass retries).
func (s *Server) loseRun(ctx context.Context, loser store.RunLoser, run types.AgentRun, reason types.LostReason, terminal types.RunState) bool {
	now := s.cfg.Now()
	if !s.lostRunKeepable(run, now) {
		return false
	}
	var tokenLife time.Duration
	if reason == types.LostOutage {
		tokenLife = runTokenLapseAfter
	}
	applied, err := loser.MarkRunLost(ctx, run.ID, reason, now, tokenLife)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: marking a run lost failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return true
	}
	if !applied {
		return true
	}
	run.LostAt, run.LostReason = &now, reason
	s.cancelRunApprovals(ctx, run.ID)
	s.revokeRunBroker(ctx, run.ID)
	s.leaseEnded.Store(run.ID, struct{}{})
	data := map[string]any{"reason": string(reason)}
	if err := s.stopLostSandbox(ctx, run, now); err != nil {
		data["kept"] = false
		data["lost_error"] = err.Error()
		s.stopKeptRun(ctx, run, terminal, "run.lost", data)
		return true
	}
	data["kept"] = true
	if until, ok := s.keptUntil(run); ok {
		data["kept_until"] = until
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.lost",
		run.ID.String(), "success", mustJSON(data)))
	return true
}

// lostRunKeepable: an interactive RUNNING run with a sandbox, not already
// kept, whose lease or ended-run grace is still live.
func (s *Server) lostRunKeepable(run types.AgentRun, now time.Time) bool {
	if run.State != types.RunRunning || !run.Interactive || run.SandboxRef == "" || runIsKept(run) {
		return false
	}
	return run.EndsAt == nil || now.Before(run.EndsAt.Add(s.cfg.EndedRunGrace))
}

// keptUntil is when a kept run is torn down: an ended run at its end plus the
// grace counted from when it ended, a lost one at its end plus the grace, and
// a lost run with no end only when someone kills it (ok false).
func (s *Server) keptUntil(run types.AgentRun) (time.Time, bool) {
	switch {
	case run.LostReason == types.LostEnded:
		return run.LostAt.Add(s.cfg.EndedRunGrace), true
	case run.EndsAt != nil:
		return run.EndsAt.Add(s.cfg.EndedRunGrace), true
	default:
		return time.Time{}, false
	}
}

// stopLostSandbox removes a lost run's proxy. An outage run inside its lease
// keeps its agent running; any other lost run (a reboot, or an outage past
// its end) has its agent stopped as at the end.
func (s *Server) stopLostSandbox(ctx context.Context, run types.AgentRun, now time.Time) error {
	if run.LostReason == types.LostOutage && (run.EndsAt == nil || now.Before(*run.EndsAt)) {
		stopper, ok := s.cfg.Runner.(runner.ProxyStopper)
		if !ok {
			return runner.ErrEndUnsupported
		}
		return stopper.StopProxy(ctx, run.SandboxRef)
	}
	return s.endSandbox(ctx, run)
}
