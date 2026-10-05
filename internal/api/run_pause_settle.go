// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
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

// pauseSettleTries bounds the tries of one read or runtime call in a
// compensation or a settle; what still fails is left to the settle record.
const pauseSettleTries = 3

// retrying calls fn until it succeeds, pauseSettleTries times at most, a short
// and growing wait apart, while ctx lasts.
func retrying(ctx context.Context, fn func() error) error {
	err := fn()
	for i := 1; err != nil && i < pauseSettleTries; i++ {
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
// It fails closed. It writes the settle record before anything else and thaws
// nothing without it, so a replica that dies mid-way still leaves the run to
// the pause sweep (settlePauses). A row it cannot read is never a reason to
// thaw. A read, a runtime call or a lock it still cannot get after a few tries
// ends the compensation with the sandbox as it is and the record kept. The
// record is deleted only by a round that read the row and moved the sandbox to
// it with its lock held throughout.
func (s *Server) undoFreeze(ctx, hold context.Context, f runner.Freezer, run types.AgentRun, reason types.PauseReason) {
	due, recorded := s.notePauseSettle(ctx, run.ID)
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
				s.pauseUnsettled(ctx, run.ID, recorded, "take its run lock again", err)
				return
			}
			hold, release = lctx, unlock
		}
		cur, err := s.readRunRetrying(ctx, run.ID)
		if err != nil {
			s.pauseUnsettled(ctx, run.ID, recorded, "read the run", err)
			return
		}
		paused := cur.PausedAt != nil
		if paused && !thawed {
			break // a newer pause holds the run frozen: nothing of this one is left to undo
		}
		if !paused && !recorded {
			s.pauseUnsettled(ctx, run.ID, false, "record its settle before the thaw", nil)
			return
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
			s.pauseUnsettled(ctx, run.ID, recorded, "move the sandbox to its row", err)
			return
		}
		if db.LockHeld(hold) == nil {
			break
		}
	}
	s.clearPauseSettle(ctx, run.ID, due)
}

// moveSandbox freezes the sandbox when paused, else thaws it, trying a few
// times.
func moveSandbox(ctx context.Context, f runner.Freezer, ref string, paused bool) error {
	act := f.ThawSandbox
	if paused {
		act = f.FreezeSandbox
	}
	return retrying(ctx, func() error { return act(ctx, ref) })
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
		slog.ErrorContext(ctx, "wardynd: a pause could not record its settle; its compensation thaws nothing",
			slog.String("run_id", id.String()), slog.Any("err", err))
		return time.Time{}, false
	}
	return due, true
}

// clearPauseSettle deletes the settle record a compensation wrote, once its
// sandbox is settled. A failure only leaves the sweep a settle that changes
// nothing.
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
func (s *Server) pauseUnsettled(ctx context.Context, id uuid.UUID, recorded bool, what string, err error) {
	msg := "wardynd: a pause compensation could not " + what + "; the pause sweep settles the run once it is due"
	if !recorded {
		msg = "wardynd: a pause compensation could not " + what + "; the sandbox is left as it is and nothing records it"
	}
	slog.ErrorContext(ctx, msg, slog.String("run_id", id.String()), slog.Any("err", err))
}

// settlePauses settles every run whose settle record is due: what a pause
// compensation left (undoFreeze). Under the run's lock it reads the row and
// freezes the sandbox of a run marked paused or thaws one that is not, then
// deletes the record. Like the compensation, it never acts on a row it could
// not read and keeps the record unless its lock was held throughout, so a
// failure is tried again on the next pass. A run that is no longer running has
// no sandbox to settle.
func (s *Server) settlePauses(ctx context.Context, f runner.Freezer) {
	st, ok := s.cfg.Store.(store.PauseSettler)
	if !ok {
		return
	}
	due, err := st.DuePauseSettles(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: listing the runs whose pause is unsettled failed", slog.Any("err", err))
		return
	}
	for _, d := range due {
		if !s.settlePause(ctx, f, d.RunID) {
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
	cur, err := s.readRunRetrying(lctx, id)
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
	if err := moveSandbox(lctx, f, cur.SandboxRef, cur.PausedAt != nil); err != nil {
		slog.WarnContext(ctx, "wardynd: settling a pause could not move the sandbox to its row",
			slog.String("run_id", id.String()), slog.Bool("paused", cur.PausedAt != nil), slog.Any("err", err))
		return false
	}
	return db.LockHeld(lctx) == nil
}
