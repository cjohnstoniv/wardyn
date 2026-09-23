// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The lease (long-holds design rev 4, §2.1, RL-3). A run with an end
// (AgentRun.EndsAt) is warned at 24 h, 1 h and 10 min before it, and at the end
// it stops and is KEPT: approvals cancelled, broker credentials revoked, the
// agent stopped and the proxy removed, so it has no network while its files
// stay for Config.EndedRunGrace. The grace running out tears it down. The run
// token is deliberately not revoked: nothing holds it once the proxy is gone,
// it lapses within its TTL, and a later revive mints from it.

// endingSoonThresholds are the warning points before a run's end, smallest
// first so the first one a run is inside is the closest.
var endingSoonThresholds = []time.Duration{10 * time.Minute, time.Hour, 24 * time.Hour}

// dayWarningMinLease is how long a lease must be for the 24-hour warning: a
// one-day run warned a day ahead is warned at its start.
const dayWarningMinLease = 48 * time.Hour

// sweepRunLeases is one pass of the lease over every RUNNING run that has an
// end or is kept. Every write it makes is a conditional UPDATE or state CAS, so
// every replica can run it on its own tick and each warning, end and teardown
// still happens once.
func (s *Server) sweepRunLeases(ctx context.Context) error {
	leaser, ok := s.cfg.Store.(store.RunLeaser)
	if !ok || s.cfg.Runner == nil {
		return nil
	}
	runs, err := leaser.ListLeasedRuns(ctx)
	if err != nil {
		return err
	}
	listed := make(map[uuid.UUID]bool, len(runs))
	for _, run := range runs {
		listed[run.ID] = true
		s.leaseRun(ctx, leaser, run)
	}
	// A run the sweep no longer lists (torn down, killed) needs no entry.
	s.leaseEnded.Range(func(id, _ any) bool {
		if !listed[id.(uuid.UUID)] {
			s.leaseEnded.Delete(id)
		}
		return true
	})
	return nil
}

// leaseRun acts on one run under the same bound reconcileFinalize uses, so one
// wedged sandbox cannot stall the sweep for every other run.
func (s *Server) leaseRun(ctx context.Context, leaser store.RunLeaser, run types.AgentRun) {
	ctx, cancel := context.WithTimeout(ctx, reconcileFinalizeTimeout)
	defer cancel()
	now := s.cfg.Now()
	switch {
	case run.LostAt != nil:
		if run.LostReason != types.LostEnded {
			return
		}
		if !now.Before(run.LostAt.Add(s.cfg.EndedRunGrace)) {
			s.stopEndedRun(ctx, run, "run.ended.expired", map[string]any{
				"ended_at": run.LostAt, "grace_sec": int64(s.cfg.EndedRunGrace.Seconds()),
			})
			return
		}
		// Re-assert the end every pass: a crash between the claim and the end
		// in endRun would otherwise leave a kept run with its agent, proxy and
		// broker credentials up. The broker revoke writes a row per credential,
		// so it runs once per process; a crash shows up as a restart.
		if _, done := s.leaseEnded.LoadOrStore(run.ID, struct{}{}); !done {
			s.revokeRunBroker(ctx, run.ID)
		}
		err := s.endSandbox(ctx, run)
		if errors.Is(err, runner.ErrEndUnsupported) {
			// The substrate cannot keep a sandbox, for good: tear the run down
			// as the first pass would have.
			s.stopEndedRun(ctx, run, "run.ended", map[string]any{
				"kept": false, "end_error": err.Error(), "ended_at": run.LostAt,
			})
			return
		}
		// Any other failure is the daemon failing, and a teardown would fail
		// on the same daemon, so it is retried next pass.
		if err != nil {
			slog.WarnContext(ctx, "wardynd: re-asserting an ended run's stop failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
	case run.EndsAt == nil:
	case !now.Before(*run.EndsAt):
		s.endRun(ctx, leaser, run, now)
	default:
		s.warnRunEnding(ctx, leaser, run, now)
	}
}

// endRun ends a run at its end. The claim (MarkRunEnded) comes first so two
// replicas never both end it, and so the run is already marked kept when its
// agent stops — the completion watcher reads that and leaves the run alone
// instead of finalizing it. Fails closed: when the sandbox cannot be kept (no
// grace, a substrate that cannot keep one, or a failed stop) the run is stopped
// and torn down outright.
func (s *Server) endRun(ctx context.Context, leaser store.RunLeaser, run types.AgentRun, now time.Time) {
	applied, err := leaser.MarkRunEnded(ctx, run.ID, now)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: marking a run ended failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if !applied {
		return
	}
	data := map[string]any{"ends_at": run.EndsAt}
	if _, canKeep := s.cfg.Runner.(runner.SandboxEnder); canKeep && s.cfg.EndedRunGrace > 0 && run.SandboxRef != "" {
		s.cancelRunApprovals(ctx, run.ID)
		s.revokeRunBroker(ctx, run.ID)
		s.leaseEnded.Store(run.ID, struct{}{})
		err := s.endSandbox(ctx, run)
		if err == nil {
			data["kept"] = true
			data["kept_until"] = now.Add(s.cfg.EndedRunGrace)
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.ended",
				run.ID.String(), "success", mustJSON(data)))
			return
		}
		data["end_error"] = err.Error()
	}
	data["kept"] = false
	s.stopEndedRun(ctx, run, "run.ended", data)
}

// endSandbox is the runner half of the end. The caller has checked the runner
// is a SandboxEnder, or is re-asserting an end that already passed that check.
func (s *Server) endSandbox(ctx context.Context, run types.AgentRun) error {
	ender, ok := s.cfg.Runner.(runner.SandboxEnder)
	if !ok {
		return runner.ErrEndUnsupported
	}
	return ender.EndSandbox(ctx, run.SandboxRef)
}

// stopEndedRun makes an ended run terminal (STOPPED) through the shared
// terminal tail: the full revoke cascade, the approval cancel and the sandbox
// teardown. The CAS keeps a concurrent kill's outcome.
func (s *Server) stopEndedRun(ctx context.Context, run types.AgentRun, action string, data map[string]any) {
	applied, err := s.casRunState(ctx, run.ID, types.RunRunning, types.RunStopped)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: stopping an ended run failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if applied {
		s.finalizeRunTail(ctx, run.ID, run.SandboxRef, action, "success", data)
	}
}

// revokeRunBroker is revokeRunCascade's broker half alone, for the end: a kept
// run keeps its run identity so a revive can mint from it.
func (s *Server) revokeRunBroker(ctx context.Context, runID uuid.UUID) {
	if s.cfg.Broker == nil {
		return
	}
	if err := s.cfg.Broker.RevokeRun(ctx, runID); err != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.revoke",
			runID.String(), "failure", mustJSON(map[string]any{"broker_error": err.Error()})))
	}
}

// warnRunEnding emits run.ending_soon once per threshold per end. A pass that
// finds a run already inside several thresholds (a late sweep, a short lease)
// sends only the closest; MarkRunEndingSoon then refuses the wider ones.
func (s *Server) warnRunEnding(ctx context.Context, leaser store.RunLeaser, run types.AgentRun, now time.Time) {
	left := run.EndsAt.Sub(now)
	for _, threshold := range endingSoonThresholds {
		if left > threshold {
			continue
		}
		if threshold == 24*time.Hour && run.EndsAt.Sub(run.CreatedAt) <= dayWarningMinLease {
			return
		}
		applied, err := leaser.MarkRunEndingSoon(ctx, run.ID, *run.EndsAt, int(threshold.Seconds()))
		if err != nil {
			slog.WarnContext(ctx, "wardynd: recording a run's end warning failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
			return
		}
		if applied {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.ending_soon",
				run.ID.String(), "success", mustJSON(map[string]any{
					"ends_at":        run.EndsAt,
					"threshold_sec":  int64(threshold.Seconds()),
					"left_sec":       int64(left.Seconds()),
					"files_kept_sec": int64(s.cfg.EndedRunGrace.Seconds()),
				})))
		}
		return
	}
}

// runIsKept reports whether a run the lease ended (or, later, one lost to a
// reboot) is being kept. Its agent is stopped on purpose, so an agent-exit
// watcher must not finalize it: that would tear down the files it is kept for.
func runIsKept(run types.AgentRun) bool { return run.LostAt != nil }
