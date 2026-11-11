// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keptEndHoldOffLimit is how long past its due end a kept run may wait on its run's lock (stopKeptRun)
// before forceKeptStop stops it without the lock. A pass back is at most computerFolderBackTimeout, so a
// hold that is bounded in itself is gone long before; a hold found at the first visit past the limit is
// cut whatever its cause, and its pass back is lost with it.
const keptEndHoldOffLimit = 5 * time.Minute

// forceKeptStop is stopKeptRun's answer to a lock it found held (a refusal to take it is not that, and
// does not reach here). Until the run's due end is keptEndHoldOffLimit past, it does nothing, and the next
// pass retries the end. The due end is read from the run's fresh row, so neither a slow sweep, a chain of
// lock holders nor a restart moves it; only a change to the stored end does (an extension, or a revive
// that claimed before expiry). A gone run, or a lost one with no end, is due when it was lost.
//
// Past it, the end is taken without the lock, by the one terminal claim every end competes for: it goes on
// only when the fresh row still carries the listing row's lost mark and end, and StopKeptRunForcedIf then
// wins the run's RUNNING state (and marks the forced stop in the same statement). A loser stops nothing and
// writes nothing: the holder, or whoever else ended the run, owns what follows. The winner publishes the
// terminal state and cuts egress first (the proxy), settles the run's computer folder record as not brought
// back, and runs the terminal tail without a pass back (forcedEnd): the revoke cascade, the approval
// cancel, and the sandbox stop unless forcedKeepsSandbox keeps it. One failure row (sandbox_ref,
// past_due_sec, sandbox_stopped, forced) records it. The mark stays until the sandbox is confirmed stopped;
// sweepForcedStops retries it on every watcher tick.
func (s *Server) forceKeptStop(ctx context.Context, leaser store.RunLeaser, run types.AgentRun, terminal types.RunState, action string, data map[string]any) {
	fs, ok := leaser.(store.ForcedStopper)
	if !ok {
		return
	}
	cur, err := s.cfg.Store.GetRun(ctx, run.ID) // the fresh row decides, never the listing's
	if err != nil || cur.State.IsTerminal() || cur.SandboxRef == "" || cur.LostAt == nil || !sameKeptMark(cur, run) {
		return
	}
	until, ok := s.keptUntil(cur)
	if !ok || cur.LostReason == types.LostGone {
		until = *cur.LostAt
	}
	now := s.cfg.Now()
	if now.Before(until.Add(keptEndHoldOffLimit)) {
		return
	}
	stamp, applied, err := fs.StopKeptRunForcedIf(ctx, run.ID, terminal, run.LostAt, run.LostReason, run.EndsAt)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: forcing a kept run's end failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if !applied {
		return
	}
	s.metrics.runTerminal(terminal)
	fctx, cancel := context.WithTimeout(forcedCtx(ctx), forcedStopBound)
	defer cancel()
	proxyErr := s.stopProxyOrAudit(fctx, run.ID, cur.SandboxRef, action)
	s.settleForcedFolder(fctx, run.ID)
	keep := s.forcedKeepsSandbox(fctx, cur, proxyErr)
	s.finalizeRunTailOrdered(context.WithValue(fctx, forcedEndKey{}, forcedEnd{keep: keep}), run.ID, cur.SandboxRef, action, "success", data, false, terminal == types.RunStopped)
	stopped := false
	settleAfterTeardown(ctx, func(sctx context.Context) { stopped = s.forcedStopConfirmed(sctx, fs, run.ID, stamp) })
	settleAfterTeardown(ctx, func(sctx context.Context) { // its own budget: a revoke that hangs to its deadline cannot expire the row
		s.recordAudit(sctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", action, cur.SandboxRef, "failure",
			mustJSON(map[string]any{"sandbox_ref": cur.SandboxRef, "past_due_sec": int64(now.Sub(until).Seconds()), "sandbox_stopped": stopped, "forced": true})))
	})
}

// forcedStopBound bounds one forced end, and one row of sweepForcedStops: a runner that never answers must
// not hold a watcher tick, and the lease sweep behind it, for good (reconcileFinalizeTimeout's reason).
// It cuts every call that obeys its context. A call that runs apart from it keeps its own budget: the
// Docker driver's stop (stopSandboxBudget, 60 s), the forced end's own teardown (teardownCtxWithin: at
// least 15 s, 90 s for a macOS VM run), settleForcedFolder's record write, and each settle write (15 s). So a
// row can run past forcedStopBound, by at most those budgets, and is still bounded.
var forcedStopBound = reconcileFinalizeTimeout

// forcedNotBroughtBack is the text a forced end records for a computer folder whose changes it did not bring back.
const forcedNotBroughtBack = "Not brought back: the run was ended past its end while another operation held it"

// sameKeptMark reports whether a and b carry the same lost mark and end: the three values StopKeptRunIf
// compares.
func sameKeptMark(a, b types.AgentRun) bool {
	same := func(x, y *time.Time) bool {
		if x == nil || y == nil {
			return x == y
		}
		return x.Equal(*y)
	}
	return same(a.LostAt, b.LostAt) && a.LostReason == b.LostReason && same(a.EndsAt, b.EndsAt)
}

// forcedFolder is run id's computer folder when its pass back would have run (bringComputerFolderBack's own
// preconditions: both ways, seeded, on a runner, and the run's content allowed out), and nil otherwise.
// unreadable is a read that failed.
func (s *Server) forcedFolder(ctx context.Context, id uuid.UUID) (folder *types.RunLocalFolder, unreadable bool) {
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return nil, false
	}
	folder, seed, err := st.RunLocalFolderSeed(ctx, id)
	switch {
	case err != nil:
		return nil, true
	case folder == nil || folder.RunnerID == nil || folder.Direction != types.ComputerFolderBoth || seed.SeededAt == nil:
		return nil, false
	case !s.runDataOutAllowed(ctx, id):
		return nil, false
	}
	return folder, false
}

// settleForcedFolder moves a both-ways seeded computer folder's record to the next generation and settles
// it there with no resume, recording that its changes were not brought back. A record that cannot be read,
// that already holds a completed pass back, or that another owner moved first (the bump is not applied) is
// left as it is: forcedKeepsSandbox then reads it.
func (s *Server) settleForcedFolder(ctx context.Context, id uuid.UUID) {
	folder, _ := s.forcedFolder(ctx, id)
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if folder == nil || !ok {
		return
	}
	rec, err := st.RunLocalFolderStatus(ctx, id)
	if err != nil || folderBroughtBack(rec) {
		return
	}
	next, ok, _ := s.bumpComputerFolderGen(ctx, id, rec.Gen)
	if !ok {
		return
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	s.recordFolderBackAt(rctx, id, *folder, next.Gen, true, nil, runnerwire.FolderPass{}, forcedNotBroughtBack)
}

// forcedKeepsSandbox is rule R: the forced end's sandbox decision without the run's lock. A sandbox is
// kept, with its proxy stopped, only when it is server-hosted and not a virtual machine (the locked path's
// own test, stopTerminalSandbox), its proxy stop was confirmed, and its computer folder's record may be in
// flight. Every other sandbox is stopped, as the locked path stops it: a runner, a virtual machine and
// Kubernetes in server mode cannot stop a proxy alone without that loss.
func (s *Server) forcedKeepsSandbox(ctx context.Context, run types.AgentRun, proxyErr error) bool {
	return proxyErr == nil && run.RunnerID == nil && !run.MachineKind.IsVM() && s.forcedRecordInFlight(ctx, run.ID)
}

// forcedRecordInFlight reports whether run id's computer folder record may belong to a pass that is
// running: it holds a resume, has a generation not settled (without the lock that can be a live pass,
// where under it it can only be a crashed one), or cannot be read. No record, a folder that is not both-ways
// seeded, and a record settled with no resume are clear.
func (s *Server) forcedRecordInFlight(ctx context.Context, id uuid.UUID) bool {
	folder, unreadable := s.forcedFolder(ctx, id)
	if unreadable {
		return true
	}
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if folder == nil || !ok {
		return false
	}
	rec, err := st.RunLocalFolderStatus(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		return true
	}
	return rec.Resume != nil || rec.Gen != rec.Settled
}

// forcedStopConfirmed reports whether the forced end's sandbox is confirmed stopped, which the tail records
// by clearing the run's ref only after its own StopSandbox succeeded, and clears the mark when the revoke also
// succeeds (clearForcedMarkIf). A failed revoke leaves the mark for the retry, whatever the sandbox did.
func (s *Server) forcedStopConfirmed(ctx context.Context, fs store.ForcedStopper, id uuid.UUID, stamp time.Time) bool {
	cur, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil || cur.SandboxRef != "" {
		return false
	}
	s.clearForcedMarkIf(ctx, fs, id, stamp, true, nil)
	return true
}

// clearForcedMarkIf is the one place the forced mark clears, for the initial force and every path of the retry. It
// clears only when both hold: contained (the sandbox stop succeeded, a kept sandbox's proxy stop returned nil, the
// run's ref was already empty, or its runner is gone), and the run's revoke cascade succeeded. The cascade runs
// whatever contained says, and is idempotent, so a retry after a success is harmless; a failed one is written
// once per process per run (revokeRowDue) and keeps the mark until a later tick succeeds. onCleared runs only
// when this call was the one that cleared the mark, so the row it writes is written once across replicas.
func (s *Server) clearForcedMarkIf(ctx context.Context, fs store.ForcedStopper, id uuid.UUID, stamp time.Time, contained bool, onCleared func(context.Context)) bool {
	cleared := false
	settleAfterTeardown(ctx, func(sctx context.Context) { // a budget of its own: the stop may have used the row's
		sctx = forcedCtx(sctx)
		err := s.revokeRunCascade(sctx, id)
		if err == nil && !sctx.Value(forcedCtxKey{}).(*revokeResidue).left.Load() {
			s.forgetTeardownRow(id, "run.revoke") // a clean revoke only: a pending token revoke keeps its one row
		}
		if !contained || err != nil || !s.clearForcedStop(sctx, fs, id, stamp) {
			return
		}
		cleared = true
		if onCleared != nil {
			onCleared(sctx)
		}
	})
	return cleared
}

// forcedCtxKey marks a context as a forced end's or its retry's, so a failed revoke under it is written once per
// process per run (revokeRowDue).
type forcedCtxKey struct{}

func forcedCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, forcedCtxKey{}, &revokeResidue{})
}

// revokeResidue records that a revoke under its forcedCtx left something to report, a failure or a pending
// token revoke, which revokeRunCascade returns nil for.
type revokeResidue struct{ left atomic.Bool }

// revokeRowDue reports whether revokeRunCascade writes its failure row: always, except under forcedCtx, where it
// is written once per process per run until a revoke succeeds.
func (s *Server) revokeRowDue(ctx context.Context, runID uuid.UUID) bool {
	r, ok := ctx.Value(forcedCtxKey{}).(*revokeResidue)
	if !ok {
		return true
	}
	r.left.Store(true) // called only for a revoke with something to report
	_, seen := teardownRowsWritten.LoadOrStore(teardownRowKey{s, runID, "run.revoke"}, struct{}{})
	return !seen
}

// clearForcedStop clears the mark the pass read, and reports whether this call cleared it: false is a mark
// already cleared (another replica's retry won) or one that could not be written, which the next tick checks again.
func (s *Server) clearForcedStop(ctx context.Context, fs store.ForcedStopper, id uuid.UUID, stamp time.Time) bool {
	applied, err := fs.ClearForcedStop(ctx, id, stamp)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: clearing a forced stop's mark failed; the next tick checks it again",
			slog.String("run_id", id.String()), slog.Any("err", err))
	}
	return applied && err == nil
}

// sweepForcedStops is the forced end's retry, on every watcher tick of every replica: it takes each run
// whose forced stop is not confirmed (the mark, listed through a partial index, so its cost follows
// the marked runs and not the stored ones). Each row has a bound of its own (forcedStopBound, plus the
// detached budgets it names), so one wedged runner holds neither the tick nor the rows behind it for good.
//
// Per row, retryForcedStop.
func (s *Server) sweepForcedStops(ctx context.Context) error {
	fs, ok := s.cfg.Store.(store.ForcedStopper)
	if !ok {
		return nil
	}
	marked, err := fs.ListForcedStops(ctx)
	if err != nil {
		return err
	}
	for _, m := range marked {
		rctx, cancel := context.WithTimeout(ctx, forcedStopBound)
		s.retryForcedStop(rctx, fs, m)
		cancel()
	}
	return nil
}

// retryForcedStop retries one marked run's sandbox stop. It never takes a Status probe for a stopped sandbox: a
// Docker agent that was only ended is exited and still present, and still holds the run's files and its network.
// The stop itself is the check for a sandbox that is gone, because Docker and Kubernetes answer a stop of a
// missing sandbox with success. The mark clears (clearForcedMarkIf) only once the run's revoke has also
// succeeded, and then in exactly these cases:
//   - the sandbox stop is confirmed (a success row, once);
//   - the sandbox is kept with its proxy stopped for its computer folder's resume (the locked path, the lock
//     free; a held lock with a record that may be in flight keeps the mark until the lock is free);
//   - the run's ref is already empty;
//   - the run's runner is gone (runnerGoneReason): nothing can confirm the stop, and the failure row says so.
//
// Every other failure, a failed revoke included, keeps the mark, and its first row is the only one this process
// writes. A proxy stop that failed on a kept sandbox leaves a cause (withStopCause) and so keeps it too.
// A record the forced end left unsettled is settled as not brought back before anything is stopped.
func (s *Server) retryForcedStop(ctx context.Context, fs store.ForcedStopper, m store.ForcedStop) {
	ctx = forcedCtx(ctx)
	run, err := s.cfg.Store.GetRun(ctx, m.RunID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading a forced stop's run failed; the next tick retries",
			slog.String("run_id", m.RunID.String()), slog.Any("err", err))
		return
	}
	if run.SandboxRef == "" || s.cfg.Runner == nil {
		s.clearForcedMarkIf(ctx, fs, run.ID, m.At, true, nil)
		return
	}
	s.settleUnsettledForcedFolder(ctx, run.ID)
	ref := run.SandboxRef
	cctx, cause := withStopCause(ctx)
	stopped := false
	lctx, unlock, locked := s.tryLockRunOp(cctx, run.ID)
	if locked {
		defer unlock()
		if cur, gerr := s.cfg.Store.GetRun(lctx, run.ID); gerr == nil {
			run = cur
		}
		left, serr := s.stopTerminalSandbox(lctx, run, sandboxSweepAction)
		switch {
		case serr == nil && !left:
			stopped = true
		case serr == nil || (left && cause.last() == nil):
			// Kept with its proxy stop confirmed: serr is nil, or only the folder record could not be read. A
			// proxy stop that failed leaves a cause, and the mark.
			if s.clearForcedMarkIf(ctx, fs, run.ID, m.At, true, nil) {
				s.forgetTeardownRow(run.ID, sandboxSweepAction) // a later failure is written again
			}
			return
		}
	} else {
		proxyErr := s.stopProxyOrAudit(cctx, run.ID, ref, sandboxSweepAction)
		if s.forcedKeepsSandbox(cctx, run, proxyErr) {
			s.clearForcedMarkIf(ctx, fs, run.ID, m.At, false, nil) // kept with the lock held: the mark waits for it
			return
		}
		stopped = s.stopSandboxOrAudit(cctx, run.ID, ref, sandboxSweepAction)
	}
	if stopped {
		settleAfterTeardown(ctx, func(sctx context.Context) { _ = s.clearSandboxRef(sctx, run.ID) })
		// One success row per confirmed stop, whatever the revoke does: the empty-ref clear that follows a failed
		// revoke writes none.
		settleAfterTeardown(ctx, func(sctx context.Context) {
			s.recordAudit(sctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", sandboxSweepAction, ref, "success",
				mustJSON(map[string]any{"sandbox_ref": ref, "sandbox_stopped": true, "forced": true})))
		})
		s.clearForcedMarkIf(ctx, fs, run.ID, m.At, true, nil)
		s.forgetTeardownRow(run.ID, sandboxSweepAction)
		return
	}
	if reason := s.runnerGoneReason(ctx, run, cause.last()); reason != "" {
		cleared := s.clearForcedMarkIf(ctx, fs, run.ID, m.At, true, func(sctx context.Context) {
			s.recordAudit(sctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", sandboxSweepAction, ref, "failure",
				mustJSON(map[string]any{"sandbox_ref": ref, "sandbox_stopped": false, "forced": true, "runner_gone": true,
					"reason": "its runner is gone; the stop was not confirmed", "cause": reason})))
		})
		if cleared {
			s.forgetTeardownRow(run.ID, sandboxSweepAction)
		}
		return
	}
	s.clearForcedMarkIf(ctx, fs, run.ID, m.At, false, nil)
}

// runnerGoneReason is why a failed stop of run's sandbox can never be confirmed, "" when it still can. It reads
// the stop's error as the orchestrator gives it (Orchestrator.subForRef):
//   - runner.ErrRunnerOffline (the runner is not registered, or its link is down) and runner.ErrPendingOnRunner (the
//     stop is queued for it, remote Substrate.teardown): gone only when the run's runner row is
//     absent or revoked. A runner that is merely offline keeps the mark; the queued stop runs when it reconnects,
//     and a runner cut off from the server contains the sandbox's egress itself when the run's lease or end lapses
//     (the offline manager's CheckLapses);
//   - "no substrate tracked for ref" (subForRef) is NOT gone: the orchestrator gives the same text when
//     its ref lookup failed, so this attempt only could not resolve the owner. It keeps the mark, and the next
//     tick tries again.
//
// Anything else (a daemon that errors, a timeout) can pass, so it keeps the mark.
func (s *Server) runnerGoneReason(ctx context.Context, run types.AgentRun, cause error) string {
	switch {
	case cause == nil:
		return ""
	case errors.Is(cause, runner.ErrRunnerOffline) || errors.Is(cause, runner.ErrPendingOnRunner):
		rs, ok := s.cfg.Store.(runnerGetter)
		if !ok || run.RunnerID == nil {
			return ""
		}
		row, err := rs.GetRunner(ctx, *run.RunnerID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return "the runner's record is gone"
		case err == nil && row.State == types.RunnerRevoked:
			return "the runner is revoked"
		}
	}
	return ""
}

// settleUnsettledForcedFolder settles run id's computer folder record as not brought back when a forced end
// left it unsettled (its process died between the claim and the settle), and leaves a settled record alone.
func (s *Server) settleUnsettledForcedFolder(ctx context.Context, id uuid.UUID) {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return
	}
	if rec, err := st.RunLocalFolderStatus(ctx, id); err != nil || rec.Gen == rec.Settled {
		return
	}
	s.settleForcedFolder(ctx, id)
}

// reviveExpiryDue refuses a revive whose run reached the end it is kept until (keptUntil) while the
// revive got ready: the claim comes after the slow proxy image pull, and a revive that started inside
// the run's grace must not land past it. It reads the run again, as the claim's own guard would.
func (s *Server) reviveExpiryDue(ctx context.Context, id uuid.UUID) *reviveError {
	cur, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil || cur.LostAt == nil {
		// A failed read, or a run no longer lost, is the claim's to refuse (reasonReviveRunChanged).
		return nil
	}
	until, ok := s.keptUntil(cur)
	if !ok || s.cfg.Now().Before(until) {
		return nil
	}
	if cur.LostReason == types.LostEnded {
		return reviveRefused(http.StatusConflict, reasonReviveEndedFilesGone, "run has ended and its files are no longer kept; start a new run")
	}
	return reviveRefused(http.StatusConflict, reasonRevivePastEnd, "run has passed its end; extend it first")
}

// sandboxSweepAction is the action of the terminal sandbox sweep's rows and of the forced stop retry's
// (sweepForcedStops). Their failed teardowns share one row per process per run.
const sandboxSweepAction = "sandbox.sweep"

// auditFirstTeardownFailure is teardownOrAudit's gate for a failed teardown err: it hands err to a caller that
// asked for the cause (withStopCause), and reports whether the row is to be written. A failure under
// sandboxSweepAction is written once per process per run, the first's, and not on every pass or tick; any other
// action's is written every time.
func (s *Server) auditFirstTeardownFailure(ctx context.Context, runID uuid.UUID, action string, err error) bool {
	if c, ok := ctx.Value(stopCauseKey{}).(*stopCause); ok {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
	}
	if action != sandboxSweepAction {
		return true
	}
	_, seen := teardownRowsWritten.LoadOrStore(teardownRowKey{s, runID, action}, struct{}{})
	return !seen
}

// teardownRowsWritten holds, per server, run and action, a failed teardown this process has audited. In memory
// only: a restart, or another replica, writes its own first row. The bound is per process: the terminal sweep's
// entry for a run (sweepTerminalSandboxRun) lives as long as the process, and the other replicas' rows are
// theirs. It is package level because Server's fields are declared elsewhere; the key carries the server so
// two servers in one process do not share an entry.
var teardownRowsWritten sync.Map

type teardownRowKey struct {
	srv    *Server
	run    uuid.UUID
	action string
}

// forgetTeardownRow drops the run's entry once its sandbox is confirmed stopped, so the map follows the runs
// whose stop is failing and not every run ever stopped.
func (s *Server) forgetTeardownRow(runID uuid.UUID, action string) {
	teardownRowsWritten.Delete(teardownRowKey{s, runID, action})
}

// stopCauseKey carries a *stopCause down to the teardown calls.
type stopCauseKey struct{}

// stopCause is the error of the last teardown that failed under a context from withStopCause.
type stopCause struct {
	mu  sync.Mutex
	err error
}

// withStopCause returns ctx and a stopCause that teardownOrAudit fills with the error of each failed teardown
// run under it, so a caller of stopTerminalSandbox, which answers only that its stop failed, can read why.
func withStopCause(ctx context.Context) (context.Context, *stopCause) {
	c := &stopCause{}
	return context.WithValue(ctx, stopCauseKey{}, c), c
}

// last is the error of the last failed teardown, nil if none failed.
func (c *stopCause) last() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
