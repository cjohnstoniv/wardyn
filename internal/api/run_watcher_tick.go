// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

// runWatcherSweeper re-runs the sweep every `every` until ctx ends (daemon
// shutdown). Panic-safe like the watchers it starts; a contained panic ends the
// sweeper, so it is logged at ERROR — adoption is degraded to boot-only until the
// process restarts. The interval is a parameter only so tests can drive it fast.
func (s *Server) runWatcherSweeper(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	// Zero, so the FIRST tick runs the undispatched pass: it costs one extra
	// ListRuns per process and it retries a boot pass that failed on a transient
	// store error. After that the pass is rate-limited to its own slow cadence —
	// it is an unbounded full-table read whose eligibility only changes on the
	// undispatchedGrace timescale, so running it every sweep tick would be pure
	// waste.
	var lastUndispatched time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Recover PER TICK, not once around the whole loop: a
			// store-driver edge that panics on ONE claimed row must not permanently
			// degrade adoption to boot-only for the process lifetime on the strength
			// of a single log line — the next tick simply tries again.
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.ErrorContext(ctx, "wardynd: PANIC in run watcher sweep tick (contained; adoption continues next tick)",
							slog.Any("panic", r))
					}
				}()
				// One tick, recorded as the run_watcher sweep: the first sub-pass
				// error is the tick's, so an attempt that failed has no success, and a
				// follower's tick, which skips the leader's sub-passes, records its
				// attempt only (sweephealth.NotOwner).
				_ = s.cfg.SweepHealth.Tick(ctx, sweephealth.RunWatcher, func(ctx context.Context) error {
					return s.runWatcherTick(ctx, &lastUndispatched)
				})
			}()
		}
	}
}

// runWatcherTick is one pass of the run watcher sweep: the undispatched-run
// reconcile on its slow cadence, then every sub-pass that rides this tick. Each
// sub-pass runs whatever the others returned, and each error is logged as it
// happens; the first is returned, so one failing sub-pass keeps the tick from
// counting as a success.
func (s *Server) runWatcherTick(ctx context.Context, lastUndispatched *time.Time) error {
	var first error
	pass := func(what string, err error) {
		if err == nil {
			return
		}
		slog.WarnContext(ctx, "wardynd: "+what, slog.Any("err", err))
		if first == nil {
			first = err
		}
	}
	if now := s.cfg.Now(); now.Sub(*lastUndispatched) >= undispatchedGrace {
		*lastUndispatched = now
		pass("undispatched run reconcile", s.finalizeUndispatchedRuns(ctx))
		// Same slow cadence as the undispatched pass (both are unbounded
		// full-table reads whose eligibility only changes on the
		// undispatchedGrace timescale), and AFTER it, so a just-finalized
		// ref-empty run's leaked containers are swept the same tick.
		// ponytail: no separate ticker — piggy-backing this cadence keeps
		// one ContainerList per grace period, free next to what it reclaims.
		pass("orphaned sandbox sweep", s.sweepOrphanedSandboxes(ctx))
		pass("terminal run proxy config purge", s.purgeTerminalRunProxyConfigs(ctx))
		pass("asserted runner subject purge", s.purgeAssertedRunnerSubjects(ctx))
	}
	pass("run watcher sweep", s.sweepRunWatchers(ctx))
	// A tightened profile reaches its live runs on this cadence,
	// before the lease reads their ends.
	pass("run limits re-clamp", s.sweepRunLimits(ctx))
	// The lease rides this cadence too (an end is a minute late at worst); a forced end's unconfirmed stop is retried after it.
	pass("run lease sweep", s.sweepRunLeases(ctx))
	pass("forced stop retry", s.sweepForcedStops(ctx))
	// An elevation past its end already counts for nothing; the sweep deletes its row and audits the end.
	pass("elevation expiry", s.sweepExpiredElevations(ctx))
	// And the pause, after the lease so a run ending this tick is
	// not frozen first; its backstop resumes a run whose request
	// closed without a writer calling approvalClosed.
	pass("run pause sweep", s.sweepRunPauses(ctx))
	// A service's health, read from the state file its supervisor keeps. The read is
	// never presence, so it does not hold a run past an idle wall.
	pass("service health sweep", s.sweepServiceHealth(ctx))
	// A sign-in whose run ended, whose window passed, or whose end failed.
	pass("desktop sign-in sweep", s.sweepDesktopSignins(ctx))
	// A VM's pause whose run ended with the agent still paused writes its row here.
	pass("ended desktop pause sweep", s.sweepEndedDesktopVMPauses(ctx))
	// And the recording hold of a pause whose run ended with no desktop connection left, on every
	// replica: each holds its own.
	pass("ended desktop recording pause sweep", s.sweepEndedDesktopRecordingPauses(ctx))
	// A run whose token lapsed loses its proxy within a tick.
	pass("lapsed run token sweep", s.sweepLapsedRunTokens(ctx))
	// A proxy start whose report did not arrive by its deadline loses its proxy within a tick.
	pass("proxy start deadline sweep", s.sweepOverdueProxyStarts(ctx))
	// Azure DevOps personal access tokens, on their own slower clock.
	pass("run personal access token sweep", s.sweepRunPATs(ctx))
	// Unclaimed runners that outlived their 24h wait.
	pass("lapsed unclaimed runner sweep", s.sweepLapsedRunners(ctx))
	// Ended runs' runner_resident values, erased by the runners this replica
	// holds; on every replica, after the passes above that end runs.
	pass("runner resident erase sweep", s.sweepResidentErase(ctx))
	// Ended runner runs' retirements, one step each; on every replica, as
	// each step is one transaction under the run lock.
	pass("runner retirement producer", s.produceRunnerRetirements(ctx))
	return first
}
