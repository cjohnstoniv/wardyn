// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A pause's compensation, and the settle that finishes what it leaves. Under
// HA a pause can lose its run lock while it undoes its own freeze, and the
// runtime call it makes then cannot be fenced: only the row says what the
// sandbox should be. So every action here is taken on a row just read, under a
// run lock proven held around it, and a run whose sandbox may not match its row
// is recorded (store.PauseSettler) until one does.

// retryTries bounds the tries of one read or runtime call in a compensation, a
// settle or a kill's revocations; what still fails is left to the settle record
// or the kill's audit data.
const retryTries = 3

// retrying calls fn until it succeeds, retryTries times at most, a short
// and growing wait apart, while ctx lasts.
func retrying(ctx context.Context, fn func() error) error {
	err := fn()
	for i := 1; err != nil && i < retryTries; i++ {
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(i) * 100 * time.Millisecond):
		}
		err = fn()
	}
	return err
}

// undoFreeze is pauseRun's compensation: it thaws a freeze no pause marked,
// and never one a newer pause holds. hold is the context of the run lock the
// pause took; ctx outlives the pass and carries no lock.
//
// Whether the run lock is still this replica's is asked of the database, on
// the lock's own connection (db.LockHeld), immediately before and after the
// runtime call. It is never read from hold: once the pass's cancellation has
// ended hold, its cause stays context.Canceled, and a lock lost after that
// never shows there. A thaw without the lock can land on a pause another
// replica froze and marked meanwhile, leaving the run marked paused with its
// agent running. So a lock found lost before the call, or lost while it was in
// flight, sends the compensation round again under the lock taken afresh: it
// re-reads the row and moves the sandbox to it, freezing a run marked paused
// that this compensation may have thawed and thawing one nobody marked.
//
// It fails closed. The pause wrote the settle record before its freeze, or
// found one held (pauseRun), and due is that record's due_at, so a replica
// that dies mid-way still leaves the run to the pause sweep (settlePauses).
// Writing the record again first moves its due past this compensation's own
// deadline. A write that fails leaves the record as it stood, unless the lock
// was lost and another holder has deleted the record since. A row it cannot
// read is never a reason to thaw. A read, a runtime call or a lock it still
// cannot get after a few tries ends the compensation with the sandbox as it is
// and the record kept. The record is deleted only by a round that read the row
// and moved the sandbox to it with its lock held throughout, and only when own
// says the pause wrote it: a record held before the pause may be an earlier
// cut call's, whose late look (pauseLateSettleAfter) is the sweep's to finish.
func (s *Server) undoFreeze(ctx, hold context.Context, f runner.Freezer, run types.AgentRun, reason types.PauseReason, due time.Time, own bool) {
	if d, ok := s.notePauseSettle(ctx, run.ID); ok {
		due = d
	}
	release := func() {}
	defer func() { release() }()
	thawed := false
	for round := 0; ; round++ {
		if round > 0 {
			release()
			slog.WarnContext(ctx, "wardynd: a pause lost its run lock; settling the run under a new one",
				slog.String("run_id", run.ID.String()), slog.Bool("thawed", thawed))
			lctx, unlock, err := s.lockRunOp(ctx, run.ID)
			if err != nil {
				s.pauseUnsettled(ctx, run.ID, "take its run lock again", err)
				return
			}
			hold, release = lctx, unlock
		}
		cur, err := s.readRunRetrying(ctx, run.ID)
		if err != nil {
			s.pauseUnsettled(ctx, run.ID, "read the run", err)
			return
		}
		paused := cur.PausedAt != nil
		if paused && !thawed {
			break // a newer pause holds the run frozen: nothing of this one is left to undo
		}
		if db.LockHeld(hold) != nil {
			continue
		}
		thawed = thawed || !paused
		if err := moveSandbox(ctx, f, run.SandboxRef, paused); err != nil {
			if !paused {
				s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.pause",
					run.ID.String(), "failure", mustJSON(map[string]any{
						"reason": reason, "thaw_error": err.Error(),
					})))
			}
			s.pauseUnsettled(ctx, run.ID, "move the sandbox to its row", err)
			return
		}
		if db.LockHeld(hold) == nil {
			break
		}
	}
	if own {
		s.clearPauseSettle(ctx, run.ID, due)
	}
}

// moveSandbox freezes the sandbox when paused, else thaws it, trying a few
// times. A sandbox that cannot be frozen (a macOS VM) has nothing to move, so
// a settle record an older build left for one clears instead of looping.
func moveSandbox(ctx context.Context, f runner.Freezer, ref string, paused bool) error {
	act := f.ThawSandbox
	if paused {
		act = f.FreezeSandbox
	}
	return retrying(ctx, func() error {
		if err := act(ctx, ref); err != nil && !errors.Is(err, runner.ErrFreezeUnsupported) {
			return err
		}
		return nil
	})
}

// readRunRetrying is GetRun, tried a few times.
func (s *Server) readRunRetrying(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	var run types.AgentRun
	err := retrying(ctx, func() (err error) {
		run, err = s.cfg.Store.GetRun(ctx, id)
		return err
	})
	return run, err
}

// notePauseSettle writes run id's settle record, due once the compensation's
// own deadline has passed. ok is false only when the store keeps such records
// and none could be written; a store with none (no database) has nothing to
// write.
func (s *Server) notePauseSettle(ctx context.Context, id uuid.UUID) (due time.Time, ok bool) {
	st, has := s.cfg.Store.(store.PauseSettler)
	if !has {
		return time.Time{}, true
	}
	err := retrying(ctx, func() (err error) {
		due, err = st.NotePauseSettle(ctx, id, pauseCompensateTimeout)
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: a pause could not write its settle record",
			slog.String("run_id", id.String()), slog.Any("err", err))
		return time.Time{}, false
	}
	return due, true
}

// notePauseIntent writes run id's settle record before a pause's freeze (pauseRun), unless one is already held, on
// a bound of its own: the caller holds the run's lock. The record comes due once the freeze's bound and the mark's
// have passed, so the sweep does not settle it while the pause that wrote it may still be freezing or marking. ok is
// false when the store keeps such records and this one could be neither written nor read; the pause then freezes
// nothing. wrote reports whether this call wrote it.
func (s *Server) notePauseIntent(ctx context.Context, id uuid.UUID) (due time.Time, wrote, ok bool) {
	st, has := s.cfg.Store.(store.PauseSettler)
	if !has {
		return time.Time{}, false, true
	}
	ctx, cancel := context.WithTimeout(ctx, heldStoreCallBound)
	defer cancel()
	err := retrying(ctx, func() (err error) {
		due, wrote, err = st.NotePauseIntent(ctx, id, pauseRuntimeCallTimeout+heldWriteBound)
		return err
	})
	if err != nil {
		slog.WarnContext(ctx, "wardynd: a pause could not record its intent before the freeze; it freezes nothing until a later pass",
			slog.String("run_id", id.String()), slog.Any("err", err))
		return time.Time{}, false, false
	}
	return due, wrote, true
}

// errPauseUnsettled is settleOwedPause's answer for a sandbox it could not move to its row.
var errPauseUnsettled = errors.New("the run's sandbox could not be moved to its pause state")

// settleOwedPause settles run id now when a settle record is held for it, for a person's thaw (thawForExec) under
// the run's lock the caller holds on ctx. A row that is not paused can sit over a frozen sandbox: a pause that died
// between its freeze and its mark leaves only its record to say so, and the sweep acts on it only once it is due. A
// record that cannot be read, or a sandbox that cannot be moved, is an error, so the caller never answers that the
// run is not paused over a frozen agent. The record is kept whatever the outcome: a runtime call still in flight when
// this settle runs (a freeze cut at its bound, or one a pause that lost its lock is still making) can land after it,
// and only the sweep's settle, once the record is due, comes after that call's deadline.
func (s *Server) settleOwedPause(ctx context.Context, id uuid.UUID) error {
	st, ok := s.cfg.Store.(store.PauseSettler)
	f, canFreeze := s.cfg.Runner.(runner.Freezer)
	if !ok || !canFreeze {
		return nil
	}
	due, err := heldRead(ctx, func(ctx context.Context) (time.Time, error) { return st.PauseSettleFor(ctx, id) })
	if err != nil || due.IsZero() {
		return err
	}
	if !s.settlePauseHeld(ctx, ctx, f, id) {
		return errPauseUnsettled
	}
	return nil
}

// pauseSettleRetryEvery is how long notePauseSettleOrRetry's loop waits before each new write of a settle record that
// could not be written; a variable only so a test can shorten it.
var pauseSettleRetryEvery = pauseCompensateTimeout

// pauseSettleRetries holds, per server and run, a notePauseSettleOrRetry loop still writing that run's settle record,
// so one outage starts one loop per run, not one per pass. Package level as teardownRowsWritten is: the key carries
// the server, so two servers in one process do not share an entry.
var pauseSettleRetries sync.Map

type pauseSettleRetryKey struct {
	srv *Server
	run uuid.UUID
}

// notePauseSettleOrRetry is notePauseSettle for a freeze or thaw that may land unrecorded. When the record cannot be
// written, a tracked loop (goBackground) writes it again every pauseSettleRetryEvery until it lands, the run is gone
// or no longer running with a sandbox, or the daemon stops. Without it, a sandbox the cut call moved would not match
// its row with only an error log to show for it; once the record lands, the pause sweep (settlePauses) moves the
// sandbox to its row. due and ok are the first write's.
func (s *Server) notePauseSettleOrRetry(ctx context.Context, id uuid.UUID) (time.Time, bool) {
	due, ok := s.notePauseSettle(ctx, id)
	base := s.cfg.BaseCtx
	if ok || base == nil {
		return due, ok
	}
	key := pauseSettleRetryKey{s, id}
	if _, busy := pauseSettleRetries.LoadOrStore(key, struct{}{}); busy {
		return due, ok
	}
	slog.WarnContext(ctx, "wardynd: a pause's settle record could not be written; it is written again in the background until it lands",
		slog.String("run_id", id.String()))
	s.goBackground(func() {
		defer pauseSettleRetries.Delete(key)
		for {
			select {
			case <-base.Done():
				return
			case <-time.After(pauseSettleRetryEvery):
			}
			if s.retryPauseSettle(base, id) {
				return
			}
		}
	})
	return due, ok
}

// retryPauseSettle is one round of notePauseSettleOrRetry's loop: true once the record is written, or once the run
// needs none (it is gone, or no longer running with a sandbox, as settlePause reads it).
func (s *Server) retryPauseSettle(base context.Context, id uuid.UUID) bool {
	ctx, cancel := context.WithTimeout(base, pauseCompensateTimeout)
	defer cancel()
	run, err := s.cfg.Store.GetRun(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true
	case err != nil:
		return false
	case run.State != types.RunRunning || run.SandboxRef == "":
		return true
	}
	// One write a round, straight to the store: notePauseSettle's own retries and its error log are the first
	// write's, and the loop has already said once that it is writing again.
	st, has := s.cfg.Store.(store.PauseSettler)
	if !has {
		return true
	}
	_, err = st.NotePauseSettle(ctx, id, pauseCompensateTimeout)
	return err == nil
}

// pauseLateSettleAfter is how long the pause sweep keeps a settle record past its due_at. A runtime call its context
// cut can still land after the sweep's first settle (the runtime need not stop at the cut), so every pass until then
// settles the run again, and the first pass after it settles it a last time and deletes the record. The record is in
// the database, so a server that restarts meanwhile still looks.
const pauseLateSettleAfter = 2 * time.Minute

// clearPauseSettle deletes run id's settle record while it still reads due: a
// pause's intent once its mark lands, or a compensation's once its sandbox is
// settled. A failure only leaves the sweep a settle that changes nothing.
func (s *Server) clearPauseSettle(ctx context.Context, id uuid.UUID, due time.Time) {
	st, has := s.cfg.Store.(store.PauseSettler)
	if !has || due.IsZero() {
		return
	}
	if err := st.ClearPauseSettle(ctx, id, due); err != nil {
		slog.WarnContext(ctx, "wardynd: clearing a settled pause's record failed; the pause sweep settles it again",
			slog.String("run_id", id.String()), slog.Any("err", err))
	}
}

// pauseUnsettled reports a compensation that ends with the run's sandbox
// perhaps not matching its row.
func (s *Server) pauseUnsettled(ctx context.Context, id uuid.UUID, what string, err error) {
	slog.ErrorContext(ctx, "wardynd: a pause compensation could not "+what+"; the pause sweep settles the run if its settle record is still held",
		slog.String("run_id", id.String()), slog.Any("err", err))
}

// settlePauses settles every run whose settle record is due: what a pause, or
// its compensation, left (pauseRun, undoFreeze). Under the run's lock it reads
// the row and freezes the sandbox of a run marked paused or thaws one that is
// not. It deletes the record only on a pass pauseLateSettleAfter or more past
// its due_at; until then each pass settles the run again, so a freeze or thaw
// that lands after its cut is moved back to the row. Like the compensation, it
// never acts on a row it could not read and keeps the record unless its lock
// was held throughout, so a failure is tried again on the next pass. A run
// that is no longer running has no sandbox to settle.
func (s *Server) settlePauses(ctx context.Context, f runner.Freezer) {
	st, ok := s.cfg.Store.(store.PauseSettler)
	if !ok {
		return
	}
	due, err := st.DuePauseSettles(ctx, pauseLateSettleAfter)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: listing the runs whose pause is unsettled failed", slog.Any("err", err))
		return
	}
	for _, d := range due {
		if !s.settlePause(ctx, f, d.RunID) || !d.Last {
			continue
		}
		if err := st.ClearPauseSettle(ctx, d.RunID, d.DueAt); err != nil {
			slog.WarnContext(ctx, "wardynd: clearing a settled pause's record failed",
				slog.String("run_id", d.RunID.String()), slog.Any("err", err))
		}
	}
}

// settlePause is settlePauses for one run: true once its sandbox matches its
// row, or it has none.
func (s *Server) settlePause(ctx context.Context, f runner.Freezer, id uuid.UUID) bool {
	lctx, unlock, ok := s.tryLockRunOp(ctx, id)
	if !ok {
		return false
	}
	defer unlock()
	return s.settlePauseHeld(ctx, lctx, f, id)
}

// settlePauseHeld is settlePause with the run's lock held on lctx; ctx carries its logs.
func (s *Server) settlePauseHeld(ctx, lctx context.Context, f runner.Freezer, id uuid.UUID) bool {
	cur, err := heldRead(lctx, func(ctx context.Context) (types.AgentRun, error) { return s.readRunRetrying(ctx, id) })
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true
	case err != nil:
		slog.WarnContext(ctx, "wardynd: settling a pause could not read the run", slog.String("run_id", id.String()), slog.Any("err", err))
		return false
	case cur.State != types.RunRunning || cur.SandboxRef == "":
		return true
	case cur.LostAt != nil || db.LockHeld(lctx) != nil:
		return false
	}
	mctx, cancel := context.WithTimeout(lctx, pauseRuntimeCallTimeout)
	defer cancel()
	if err := moveSandbox(mctx, f, cur.SandboxRef, cur.PausedAt != nil); err != nil {
		slog.WarnContext(ctx, "wardynd: settling a pause could not move the sandbox to its row",
			slog.String("run_id", id.String()), slog.Bool("paused", cur.PausedAt != nil), slog.Any("err", err))
		return false
	}
	return db.LockHeld(lctx) == nil
}
