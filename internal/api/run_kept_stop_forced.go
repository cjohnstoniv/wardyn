// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

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
	fctx, cancel := context.WithTimeout(ctx, forcedStopBound)
	defer cancel()
	proxyErr := s.stopProxyOrAudit(fctx, run.ID, cur.SandboxRef, action)
	s.settleForcedFolder(fctx, run.ID)
	keep := s.forcedKeepsSandbox(fctx, cur, proxyErr)
	s.finalizeRunTailOrdered(context.WithValue(fctx, forcedEndKey{}, forcedEnd{keep: keep}), run.ID, cur.SandboxRef, action, "success", data, false, terminal == types.RunStopped)
	settleAfterTeardown(ctx, func(sctx context.Context) {
		stopped := s.forcedStopConfirmed(sctx, fs, run.ID, stamp)
		s.recordAudit(sctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", action, cur.SandboxRef, "failure",
			mustJSON(map[string]any{"sandbox_ref": cur.SandboxRef, "past_due_sec": int64(now.Sub(until).Seconds()), "sandbox_stopped": stopped, "forced": true})))
	})
}

// forcedStopBound bounds one forced end, and one row of sweepForcedStops: a runner that never answers must
// not hold a watcher tick, and the lease sweep behind it, for good (reconcileFinalizeTimeout's reason).
var forcedStopBound = reconcileFinalizeTimeout

// forcedNotBroughtBack is the text a forced end records for a computer folder whose changes it did not bring back.
const forcedNotBroughtBack = "not brought back: the run was ended past its end while another operation held it"

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
	next, ok := s.bumpComputerFolderGen(ctx, id, rec.Gen)
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
// by clearing the run's ref only after its own StopSandbox succeeded, and clears the mark when it is.
func (s *Server) forcedStopConfirmed(ctx context.Context, fs store.ForcedStopper, id uuid.UUID, stamp time.Time) bool {
	cur, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil || cur.SandboxRef != "" {
		return false
	}
	s.clearForcedStop(ctx, fs, id, stamp)
	return true
}

func (s *Server) clearForcedStop(ctx context.Context, fs store.ForcedStopper, id uuid.UUID, stamp time.Time) {
	if _, err := fs.ClearForcedStop(ctx, id, stamp); err != nil {
		slog.WarnContext(ctx, "wardynd: clearing a forced stop's mark failed; the next tick checks it again",
			slog.String("run_id", id.String()), slog.Any("err", err))
	}
}

// sweepForcedStops is the forced end's retry, on every watcher tick of every replica: it takes each run
// whose forced stop is not confirmed (the mark, listed through a partial index, so its cost follows
// the marked runs and not the stored ones). Each row has a bound of its own (forcedStopBound), so one
// wedged runner holds neither the tick nor the rows behind it.
//
// Per row: a ref already empty, or a Status probe that answers without error with a state other than
// RUNNING, clears the mark after the revoke cascade. A probe that errors keeps the mark and goes on to the
// stop, unlike the terminal sandbox sweep, which counts it as already gone. With the run's lock free the
// stop is the locked path's (stopTerminalSandbox); with it held, the proxy is stopped and rule R decides
// the sandbox. The mark is cleared once the sandbox is stopped, or kept with its proxy stopped for
// resumeComputerFolderBacks.
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

func (s *Server) retryForcedStop(ctx context.Context, fs store.ForcedStopper, m store.ForcedStop) {
	run, err := s.cfg.Store.GetRun(ctx, m.RunID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading a forced stop's run failed; the next tick retries",
			slog.String("run_id", m.RunID.String()), slog.Any("err", err))
		return
	}
	done := func() {
		_ = s.revokeRunCascade(ctx, run.ID)
		settleAfterTeardown(ctx, func(sctx context.Context) { s.clearForcedStop(sctx, fs, m.RunID, m.At) })
	}
	if run.SandboxRef == "" || s.cfg.Runner == nil {
		done()
		return
	}
	if st, perr := s.cfg.Runner.Status(ctx, run.SandboxRef); perr == nil && st.State != types.RunRunning {
		done()
		return
	}
	const action = "sandbox.sweep"
	lctx, unlock, locked := s.tryLockRunOp(ctx, run.ID)
	if locked {
		defer unlock()
		if cur, gerr := s.cfg.Store.GetRun(lctx, run.ID); gerr == nil {
			run = cur
		}
		left, serr := s.stopTerminalSandbox(lctx, run, action)
		if serr == nil || (left && !errors.Is(serr, errProxyStopUnconfirmed)) {
			if !left {
				settleAfterTeardown(ctx, func(sctx context.Context) { _ = s.clearSandboxRef(sctx, run.ID) })
			}
			done()
		}
		return
	}
	proxyErr := s.stopProxyOrAudit(ctx, run.ID, run.SandboxRef, action)
	if s.forcedKeepsSandbox(ctx, run, proxyErr) {
		_ = s.revokeRunCascade(ctx, run.ID)
		return
	}
	if s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, action) {
		settleAfterTeardown(ctx, func(sctx context.Context) { _ = s.clearSandboxRef(sctx, run.ID) })
		done()
		return
	}
	_ = s.revokeRunCascade(ctx, run.ID)
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
