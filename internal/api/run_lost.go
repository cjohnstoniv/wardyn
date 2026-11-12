// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Lost runs (long-holds design rev 4, §4 rows 2–3, RL-9). A run outlives its
// sandbox: instead of failing and tearing down, an interactive run is KEPT,
// like an ended one (run_lease.go), with its proxy stopped so it has no
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
// its end and grace, or a substrate that cannot keep a sandbox (Kubernetes).
// A proxy stop that fails any other way keeps the run with its containment
// unresolved (containment_error) for the lease sweep to retry (#1060): a
// teardown on the same failing daemon would lose the files and contain nothing.

// runTokenLapseAfter is how long a RUNNING run may go without a renew before
// its identity is dead. The proxy renews at least every 30 minutes and gives
// up an hour after its last good renew (egress/proxy/renew.go
// renewGiveUpAfter, which mirrors the embedded provider's 1 h tokenTTL); the
// five minutes cover the stamp landing a moment after the mint and a small
// clock difference between the database and the minting replica.
const runTokenLapseAfter = identity.RunTokenLapseAfter

// sweepLapsedRunTokens is the boot and sweep check: every RUNNING run whose
// token lapsed is marked lost (outage) with its proxy stopped, or torn down.
func (s *Server) sweepLapsedRunTokens(ctx context.Context) error {
	loser, ok := s.cfg.Store.(store.RunLoser)
	leaser, lok := s.cfg.Store.(store.RunLeaser)
	if !ok || !lok || s.cfg.Runner == nil {
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
			if s.loseRun(ctx, loser, leaser, run, types.LostOutage, types.RunFailed, runTokenLapseAfter, s.cfg.Now()) != loseNotKeepable {
				return
			}
			s.reconcileFinalize(ctx, run.ID, types.RunFailed, run.SandboxRef,
				"run token lapsed: the control plane was unreachable past its renew window")
		}()
	}
	return nil
}

// keepRebootedRunTimeout bounds keepRebootedRun while it holds the run's lock.
// A variable only so a test can shorten it.
var keepRebootedRunTimeout = reconcileFinalizeTimeout

// keepRebootedRun is the watcher's side: an agent it observed terminal whose
// container still exists (the probe carries an exit code; a gone container
// does not) is marked lost (reboot) rather than finalized. false leaves the
// caller's finalize as before. A substrate that cannot keep it (Kubernetes)
// tears it down in the state the exit code says, as the finalize would have.
func (s *Server) keepRebootedRun(ctx context.Context, run types.AgentRun, st runner.Status) bool {
	// A headless run on a runner ends by its runner's generation-bound exit
	// receipt (runner_exits.go): the runner observes the same exit code, a
	// reboot's among them (#3665). A reboot mark here would get that receipt
	// refused (AcceptRunnerExit), so the run would never record its end.
	if !run.Interactive && run.Placement == types.PlacementLocal && run.RunnerID != nil {
		return true
	}
	// A revive already in flight owns this run — a watcher
	// sweep that lands mid-revive (its claim cleared lost_at, but the new proxy
	// or agent is not up yet) must leave it alone, never lose it again.
	if _, busy := s.reviving.Load(run.ID); busy {
		return true
	}
	// The same ownership, for the instant between a revive's mark and its
	// lock, and for a lease pass mid-stop: skip, and the next sweep decides.
	ctx, unlock, locked := s.tryLockRunOp(ctx, run.ID)
	if !locked {
		return true
	}
	defer unlock()
	// The lock must not outlive a wedged daemon: the stop below is bounded as
	// the lapsed-token sweep's is, or a revive of this run would wait for it.
	ctx, cancel := context.WithTimeout(ctx, keepRebootedRunTimeout)
	defer cancel()
	loser, ok := s.cfg.Store.(store.RunLoser)
	leaser, lok := s.cfg.Store.(store.RunLeaser)
	if !ok || !lok || st.ExitCode == nil {
		return false
	}
	terminal := types.RunFailed
	if *st.ExitCode == 0 {
		terminal = types.RunCompleted
	}
	return s.loseRun(ctx, loser, leaser, run, types.LostReboot, terminal, 0, s.cfg.Now()) != loseNotKeepable
}

// loseOutcome is what loseRun did.
type loseOutcome int

const (
	// loseNotKeepable: the run cannot be kept, and the caller's fail-closed arm
	// applies.
	loseNotKeepable loseOutcome = iota
	// loseApplied: the lost mark landed and the run is kept (with its
	// containment unresolved when the stop failed), or torn down because its
	// substrate cannot keep a sandbox.
	loseApplied
	// loseNotApplied: the claim did not land (another replica took it, a renew
	// landed, or the run went terminal), so the run is left alone.
	loseNotApplied
	// loseWriteFailed: the mark could not be written, so nothing was
	// persisted and nothing was contained. The next pass retries.
	loseWriteFailed
)

// loseRun marks run lost for reason and stops its proxy. loseNotKeepable means
// the run cannot be kept, and the caller's fail-closed arm applies. Every other
// outcome is the run taken care of or left alone (see loseOutcome); a caller
// that tells a person what became of the run reads loseWriteFailed apart from
// loseApplied, so it never says "lost" about a mark that was not written.
// tokenLife > 0 also requires the run's token to be lapsed by that much still
// (MarkRunLost), so the sweep's mark loses to a renew. at is the mark's time:
// now, except for an ended run put back to ended, whose files grace stays
// counted from when it first ended.
func (s *Server) loseRun(ctx context.Context, loser store.RunLoser, leaser store.RunLeaser, run types.AgentRun, reason types.LostReason, terminal types.RunState, tokenLife time.Duration, at time.Time) loseOutcome {
	o, _ := s.loseRunStopped(ctx, loser, leaser, run, reason, terminal, tokenLife, at, 0)
	return o
}

// loseRunStopped is loseRun, also answering, for loseApplied, the proxy stop's error: nil is a stop
// confirmed, anything else containment still unresolved. fence, when above 0, is the policy generation
// the caller acts for (a revive's, or a failed proxy start's): the lost mark then lands only while it is
// still the run's newest (store.RunLoserAtGeneration), in the mark's own write, so a caller whose lock
// was lost does not mark lost a run a newer revive took over. That miss is loseNotApplied and stops
// nothing. The effects after a mark that landed (approvals, broker) are not fenced, and the stop carries
// fence where generationStopperFor answers a stopper: there it never stops a newer revive's proxy. Elsewhere
// it is stopLostSandbox's unversioned stop. No newer proxy can exist on a VM or a substrate with no replace
// path, nor on a connected runner that revive refuses; a runner read as not connected at selection is not
// refused, so it can hold one, and that stop, sent or queued for its reconnect, can stop it.
// A mark that errors is loseWriteFailed, as unfenced: the caller's fail-closed stop still runs.
func (s *Server) loseRunStopped(ctx context.Context, loser store.RunLoser, leaser store.RunLeaser, run types.AgentRun, reason types.LostReason, terminal types.RunState, tokenLife time.Duration, at time.Time, fence int) (loseOutcome, error) {
	if !s.lostRunKeepable(ctx, run) {
		return loseNotKeepable, nil
	}
	var applied bool
	var err error
	if fl, ok := loser.(store.RunLoserAtGeneration); ok && fence > 0 {
		applied, err = fl.MarkRunLostAtGeneration(ctx, run.ID, fence, reason, at, tokenLife)
	} else {
		applied, err = loser.MarkRunLost(ctx, run.ID, reason, at, tokenLife)
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: marking a run lost failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return loseWriteFailed, nil
	}
	if !applied {
		return loseNotApplied, nil
	}
	run.LostAt, run.LostReason = &at, reason
	s.cancelRunApprovals(ctx, run.ID)
	s.revokeRunBroker(ctx, run.ID)
	s.leaseEnded.Store(run.ID, struct{}{})
	data, outcome := map[string]any{"reason": string(reason)}, "success"
	stopErr := s.stopLostSandboxAt(ctx, run, fence)
	if err := stopErr; errors.Is(err, runner.ErrPendingOnRunner) {
		data["containment"] = "pending_runner"
	} else if err != nil {
		data["lost_error"] = err.Error()
		if errors.Is(err, runner.ErrEndUnsupported) {
			data["kept"] = false
			s.stopKeptRun(ctx, leaser, run, terminal, "run.lost", data)
			return loseApplied, stopErr
		}
		// Any other failure leaves containment unconfirmed, and a teardown
		// on the same failing daemon would only lose the files (#1060): the
		// run stays kept and the lease sweep's re-assert retries the stop.
		data["containment"], outcome = "unresolved", "failure"
		s.noteContainmentFailure(ctx, leaser, run.ID, err)
	}
	data["kept"] = true
	if until, ok := s.keptUntil(run); ok {
		data["kept_until"] = until
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.lost",
		run.ID.String(), outcome, mustJSON(data)))
	return loseApplied, stopErr
}

// lostRunKeepable preserves local headless agents as well as interactive
// sandboxes. Runner lifecycle evidence finalizes a kept headless agent later.
// Whether the run is still inside its end plus the files grace is the database's answer
// (store.RunEndsReader.RunEndsOpen, keepable), never a comparison with this server's clock. A store without
// it, or a read that fails, keeps the run: not keeping it ends the run on the spot, while a kept run has its
// proxy stopped at once (stopLostSandbox) and the lease sweep ends it once its grace has passed.
func (s *Server) lostRunKeepable(ctx context.Context, run types.AgentRun) bool {
	if run.State != types.RunRunning || (!run.Interactive && run.Placement != types.PlacementLocal) || run.SandboxRef == "" || runIsKept(run) {
		return false
	}
	rd, ok := s.cfg.Store.(store.RunEndsReader)
	if !ok {
		return true
	}
	_, keepable, err := rd.RunEndsOpen(ctx, run.ID, s.cfg.EndedRunGrace)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading a run's end failed; the run is kept",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return true
	}
	return keepable
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

// endedFilesKept reports whether an ended run's files grace is still live at
// now: an ended, kept run may be extended and revived only inside it (#1061).
func (s *Server) endedFilesKept(run types.AgentRun, now time.Time) bool {
	return run.LostReason == types.LostEnded && run.LostAt != nil && now.Before(run.LostAt.Add(s.cfg.EndedRunGrace))
}

// endedKept is the condition a write on an ended, kept run lands under (see
// store.EndedKept), for run as read and the grace counted at now; nil for a run
// its own end did not stop.
func (s *Server) endedKept(run types.AgentRun, now time.Time) *store.EndedKept {
	if run.LostReason != types.LostEnded || run.LostAt == nil {
		return nil
	}
	return &store.EndedKept{LostAt: *run.LostAt, KeptAfter: now.Add(-s.cfg.EndedRunGrace), Now: now}
}

// lostEnd is what the database says of a lost run's end, for its stop.
type lostEnd int

const (
	lostEndPast       lostEnd = iota // not an outage, or an outage past its end: the agent is stopped
	lostEndOpen                      // an outage inside its end: only the proxy is stopped
	lostEndReadFailed                // an outage whose end could not be read: the proxy is cut, nothing else is stopped
	lostEndNoReader                  // an outage on a store that cannot read it: the proxy is cut, then the agent stopped
)

// outageEnd says which stop a lost run gets. Whether an outage run is still inside its end is the database's
// answer (store.RunEndsReader.RunEndsOpen, endsOpen), never a comparison with this server's clock. Any other
// lost run is lostEndPast, with no read. An end that cannot be read is never taken as open or as past: the
// proxy is cut first, because the stop that follows can send nothing (an end the substrate cannot do,
// runner.ErrEndUnsupported) or be held off while every end hold is taken. A failed read then stops nothing
// else and returns the read error, so the run stays kept and the lease sweep reads again on its next pass; a
// store with no reader never will, so there the agent is stopped as well, where the substrate can end it.
// The read's error comes back with lostEndReadFailed.
func (s *Server) outageEnd(ctx context.Context, run types.AgentRun) (lostEnd, error) {
	if run.LostReason != types.LostOutage {
		return lostEndPast, nil
	}
	rd, ok := s.cfg.Store.(store.RunEndsReader)
	if !ok {
		return lostEndNoReader, nil
	}
	open, _, err := rd.RunEndsOpen(ctx, run.ID, s.cfg.EndedRunGrace)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading a lost run's end failed; cutting its proxy, the stop is retried",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return lostEndReadFailed, err
	}
	if open {
		return lostEndOpen, nil
	}
	return lostEndPast, nil
}

// lostReadFailure is a stop's answer when a lost run's end could not be read: the cut's error when the cut
// failed, else the read's error, wrapped. Neither runner.ErrEndUnsupported nor runner.ErrPendingOnRunner is
// let through from the cut: the first would tear the run down through stopKeptRun, and a read that keeps failing
// is retried by the lease sweep instead.
func lostReadFailure(readErr, cutErr error) error {
	wrapped := fmt.Errorf("read the lost run's end: %w", readErr)
	switch {
	case cutErr == nil:
		return wrapped
	case errors.Is(cutErr, runner.ErrEndUnsupported) || errors.Is(cutErr, runner.ErrPendingOnRunner):
		return fmt.Errorf("%w (its proxy stop answered: %v)", wrapped, cutErr)
	}
	return cutErr
}

// stopLostSandbox stops a lost run's proxy, and its agent where outageEnd says so. An outage run inside its
// lease keeps its agent running (lostEndOpen); any other lost run (a reboot, or an outage past its end) has
// its agent stopped as at the end. A run whose end could not be read has its proxy cut and returns the read
// error with no end call; one on a store with no reader has its proxy cut and then its agent ended. If that end
// is unsupported and the cut failed, the cut's error is the answer: containment is unresolved. An outage run's
// stopped agent is recorded (store.RunLeaser.MarkRunAgentStopped), so extending the run later does not make it
// hold a slot again.
func (s *Server) stopLostSandbox(ctx context.Context, run types.AgentRun) error {
	end, readErr := s.outageEnd(ctx, run)
	var cutErr error
	if end != lostEndPast {
		stopper, ok := s.cfg.Runner.(runner.ProxyStopper)
		if ok {
			cutErr = stopper.StopProxy(ctx, run.SandboxRef)
		}
		switch end {
		case lostEndOpen:
			if !ok {
				return runner.ErrEndUnsupported
			}
			return cutErr
		case lostEndReadFailed:
			return lostReadFailure(readErr, cutErr)
		}
	}
	_, err := s.endSandbox(ctx, run)
	if leaser, ok := s.cfg.Store.(store.RunLeaser); ok && err == nil && run.LostReason == types.LostOutage && run.LostAt != nil {
		// A failed write only leaves the run counted; the lease sweep re-asserts
		// this stop every pass and retries it.
		if merr := leaser.MarkRunAgentStopped(ctx, run.ID, *run.LostAt); merr != nil {
			slog.WarnContext(ctx, "wardynd: recording a kept run's stopped agent failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", merr))
		}
	}
	if errors.Is(err, runner.ErrEndUnsupported) && cutErr != nil {
		return cutErr // the cut failed too: containment unresolved, retried by the lease sweep
	}
	return err
}
