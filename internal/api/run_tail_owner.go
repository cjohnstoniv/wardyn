// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The tail owner gives every ended run that still holds a sandbox its tail, from one owner, under the run's
// lock: the revoke, approvals, output, the computer folder's pass back and the sandbox stop. A forced-marked
// run stays with sweepForcedStops until its mark clears; a runner run whose runner is not connected here is
// not touched at all.
const (
	// folderCleanupEvery is the tail owner's period.
	folderCleanupEvery = 60 * time.Second
	// tailHistoryPerTick is the most runs with nothing owed that one tick gives their full tail.
	tailHistoryPerTick = 50
	// tailGivenUpPerTick is how many given-up records without an accepted audit row one tick works.
	tailGivenUpPerTick = 50
	// tailPage is the rows a listing page reads.
	tailPage = 500
	// tailBackoffMin and tailBackoffMax bound the wait before a folder pass is tried again for a run whose
	// last visit moved nothing: tailBackoffMin, doubling each time, to tailBackoffMax.
	tailBackoffMin = 60 * time.Second
	tailBackoffMax = 10 * time.Minute
)

// tailStep is what the tail owner does for a run.
type tailStep int

const (
	tailNone    tailStep = iota
	tailGiveUp           // the keep of an owed pass back ran out: give it up, then stop
	tailHistory          // nothing owed, past its containment due: the full tail
	tailFolder           // an owed pass back that may still keep: the folder path
)

// terminalSandboxLister is the listing of ended runs that still hold a sandbox (the first method of
// store.RunContainer), which the tail owner reads with fenced runs included.
type terminalSandboxLister interface {
	ListTerminalSandboxPage(ctx context.Context, after *store.TerminalSandboxCursor, limit int, unfencedOnly bool) ([]store.TerminalSandboxRow, *store.TerminalSandboxCursor, error)
}

// tailOwnerState is the tail owner's process state. Zero value is ready to use; none of it survives a restart.
type tailOwnerState struct {
	// auditAfter is where the next listing of given-up records without an accepted audit row starts: nil is the
	// lowest id.
	auditAfter *uuid.UUID
	// accepted holds the given-up records (givenUpKey) whose audit row this process had accepted and whose
	// mark failed: only the mark is tried again for them.
	accepted sync.Map
	// backoff holds, per run, when its folder pass may be tried again (tailBackoff).
	backoff sync.Map
	// finalizeFn replaces finalizeRunEffects, for a test that counts the tails given.
	finalizeFn func(ctx context.Context, run types.AgentRun, step tailStep)
}

// givenUpKey is a given-up record at one generation.
type givenUpKey struct {
	id  uuid.UUID
	gen int
}

// tailBackoff is the wait before a run's next folder pass: its length, and when it ends.
type tailBackoff struct {
	delay time.Duration
	next  time.Time
}

// startTailOwner starts the tail owner on every replica; it stops with the watcher ctx.
func (s *Server) startTailOwner() {
	s.goBackground(func() { s.folderCleanupLoop(s.watcherBaseCtx(), folderCleanupEvery) })
}

// folderCleanupLoop visits the ended runs every `every` until ctx ends. A panic in a tick is contained.
func (s *Server) folderCleanupLoop(ctx context.Context, every time.Duration) {
	if ctx == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.ErrorContext(ctx, "wardynd: PANIC in the tail owner (contained; it continues next tick)", slog.Any("panic", r))
					}
				}()
				s.folderCleanupTick(ctx)
			}()
		}
	}
}

// folderCleanupTick is one tick: the given-up records still missing their audit row, then the walk over the
// listing of ended runs with a sandbox.
func (s *Server) folderCleanupTick(ctx context.Context) {
	s.auditMissingGivenUps(ctx)
	lister, ok := s.cfg.Store.(terminalSandboxLister)
	if !ok {
		return
	}
	now := s.cfg.Now()
	history := 0
	seen := map[uuid.UUID]struct{}{}
	var cur *store.TerminalSandboxCursor
	for {
		rows, next, err := lister.ListTerminalSandboxPage(ctx, cur, tailPage, false)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: the tail owner could not list the ended runs; the next tick tries again", slog.Any("err", err))
			return
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			seen[row.ID] = struct{}{}
			s.tailVisit(ctx, row, now, &history)
		}
		if next == nil {
			break
		}
		cur = next
	}
	s.tail.backoff.Range(func(k, _ any) bool {
		if _, listed := seen[k.(uuid.UUID)]; !listed {
			s.tail.backoff.Delete(k)
		}
		return true
	})
}

// tailVisit decides one listed row. A forced-marked row is sweepForcedStops'. A runner run whose runner is not
// connected here (a live session, never last-known caps) is not touched: no read, no lock, no tail, no row.
// A record or rule that cannot be read skips the row for the tick: an unreadable answer is never "not owed",
// which would stop a sandbox that owes its changes.
func (s *Server) tailVisit(ctx context.Context, row store.TerminalSandboxRow, now time.Time, history *int) {
	if row.ForcedStopAt != nil {
		return
	}
	if row.RunnerID != nil {
		if _, connected := s.runnerConnected(*row.RunnerID); !connected {
			return
		}
	}
	run := containRunOf(row)
	st, err := s.tailFolderStatus(ctx, run.ID)
	if err != nil {
		tailSkipped(ctx, run.ID, err)
		return
	}
	step, err := s.tailStepFor(ctx, run, st, now)
	if err != nil {
		tailSkipped(ctx, run.ID, err)
		return
	}
	switch step {
	case tailNone:
		return
	case tailHistory:
		if *history >= tailHistoryPerTick {
			return
		}
	case tailFolder:
		if s.tailBackedOff(run.ID, now) || !s.tailFolderReady(ctx, run) {
			return
		}
	}
	s.tailRun(ctx, run.ID, step, history)
}

// tailSkipped logs a row left for the next tick because a read of it failed.
func tailSkipped(ctx context.Context, id uuid.UUID, err error) {
	slog.WarnContext(ctx, "wardynd: the tail owner could not read an ended run's folder state; skipping it until the next tick",
		slog.String("run_id", id.String()), slog.Any("err", err))
}

// tailFolderStatus is run id's folder record. A store that keeps none answers the empty record.
func (s *Server) tailFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return types.RunLocalFolderStatus{}, nil
	}
	return ss.RunLocalFolderStatus(ctx, id)
}

// tailStepFor is the step run is due for at now, given its record st: give up an owed pass whose keep ran out;
// bring back an owed pass whose keep has not; otherwise the full tail once the run is past its containment
// due, which is also the age gate that leaves a fresh end to the ordinary tail. An error is a read that failed.
func (s *Server) tailStepFor(ctx context.Context, run types.AgentRun, st types.RunLocalFolderStatus, now time.Time) (tailStep, error) {
	expired, err := s.folderExpiredOwed(ctx, run, st, now)
	if err != nil {
		return tailNone, err
	}
	if expired {
		return tailGiveUp, nil
	}
	keep, err := s.folderKeepPermitted(ctx, run, st, now)
	if err != nil {
		return tailNone, err
	}
	if keep {
		return tailFolder, nil
	}
	if due, ok := containmentDue(run); ok && !due.After(now) {
		return tailHistory, nil
	}
	return tailNone, nil
}

// tailFolderReady reports whether run's folder runner is connected here, online and sharing folders: the checks
// computerFolderPass makes (folderRunner), with no HTTP answer. Otherwise the run takes no lock and no tail.
func (s *Server) tailFolderReady(ctx context.Context, run types.AgentRun) bool {
	fst, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return false
	}
	folder, _, err := fst.RunLocalFolderSeed(ctx, run.ID)
	if err != nil {
		tailSkipped(ctx, run.ID, err)
		return false
	}
	if folder == nil || folder.RunnerID == nil {
		return false
	}
	full, err := s.cfg.Store.GetRun(ctx, run.ID)
	if err != nil {
		tailSkipped(ctx, run.ID, err)
		return false
	}
	_, refusal := s.folderRunner(ctx, full.CreatedBy, *folder.RunnerID)
	return refusal == nil
}

// tailRun takes run id's lock, reads the run and its record again, and runs step's tail if the same step is
// still due. A lock held elsewhere leaves the run for the next tick.
func (s *Server) tailRun(ctx context.Context, id uuid.UUID, step tailStep, history *int) {
	lctx, unlock, ok := s.tryLockRunOp(ctx, id)
	if !ok {
		return
	}
	defer unlock()
	run, err := s.cfg.Store.GetRun(lctx, id)
	if err != nil {
		tailSkipped(lctx, id, err)
		return
	}
	if !isTerminalRunState(run.State) || run.SandboxRef == "" {
		return
	}
	st, err := s.tailFolderStatus(lctx, id)
	if err != nil {
		tailSkipped(lctx, id, err)
		return
	}
	now := s.cfg.Now()
	again, err := s.tailStepFor(lctx, run, st, now)
	if err != nil {
		tailSkipped(lctx, id, err)
		return
	}
	if again != step {
		return
	}
	switch step {
	case tailGiveUp:
		// Only a final record continues: the sandbox is stopped for a record the give-up made final, or one
		// another writer did, and never for a statement that applied to nothing.
		final, err := s.giveUpFolder(lctx, run, false, st)
		if err != nil {
			slog.WarnContext(lctx, "wardynd: giving up an owed folder copy back failed; the next tick tries again",
				slog.String("run_id", id.String()), slog.Any("err", err))
		}
		if !final {
			return
		}
		s.tailFinalize(lctx, run, step)
	case tailHistory:
		*history++
		s.tailFinalize(lctx, run, step)
	case tailFolder:
		s.tailFinalize(lctx, run, step)
		s.tailNotePass(lctx, id, st, now)
	}
}

// tailFinalize runs the run's tail on a bound of reconcileFinalizeTimeout, as the sweeps give a whole tail.
func (s *Server) tailFinalize(ctx context.Context, run types.AgentRun, step tailStep) {
	fctx, cancel := context.WithTimeout(ctx, reconcileFinalizeTimeout)
	defer cancel()
	if fn := s.tail.finalizeFn; fn != nil {
		fn(fctx, run, step)
		return
	}
	_ = s.finalizeRunEffects(fctx, run.ID, run.SandboxRef, "sandbox.orphan.sweep", map[string]any{"sandbox_ref": run.SandboxRef}, false, false)
}

// tailNotePass settles run id's backoff after its folder pass was tried: a pass that moved data (the record
// moved on and counted files) clears it; anything else, including no attempt, doubles it.
func (s *Server) tailNotePass(ctx context.Context, id uuid.UUID, before types.RunLocalFolderStatus, now time.Time) {
	cur, err := s.tailFolderStatus(ctx, id)
	moved := err == nil && cur.Gen != before.Gen && cur.Back != nil && cur.Back.Files > 0
	s.tailBackoffNote(id, now, moved)
}

// tailBackedOff reports whether run id's next folder pass is still waiting out its backoff at now.
func (s *Server) tailBackedOff(id uuid.UUID, now time.Time) bool {
	v, ok := s.tail.backoff.Load(id)
	return ok && now.Before(v.(tailBackoff).next)
}

// tailBackoffNote records the outcome of a folder pass tried at now: moved data clears the wait; otherwise it is
// tailBackoffMin the first time and doubles up to tailBackoffMax.
func (s *Server) tailBackoffNote(id uuid.UUID, now time.Time, moved bool) {
	if moved {
		s.tail.backoff.Delete(id)
		return
	}
	d := tailBackoffMin
	if v, ok := s.tail.backoff.Load(id); ok {
		d = min(2*v.(tailBackoff).delay, tailBackoffMax)
	}
	s.tail.backoff.Store(id, tailBackoff{delay: d, next: now.Add(d)})
}

// auditMissingGivenUps works one page of the given-up records whose run.folder.transfer row the audit log never
// accepted, on a cursor that wraps. The cursor is stored before the page is worked, so a row that keeps failing
// never holds back the rows after it; a failed listing keeps the old one. It needs no ref, since the stop after
// a give-up clears it, and it is no runner step, so it runs whether or not the run's runner is connected.
func (s *Server) auditMissingGivenUps(ctx context.Context) {
	ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return
	}
	ids, next, err := ss.ListGivenUpUnaudited(ctx, s.tail.auditAfter, tailGivenUpPerTick)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: listing the given-up folder records without an audit row failed; the next tick tries again", slog.Any("err", err))
		return
	}
	s.tail.auditAfter = next
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.auditGivenUpAgain(ctx, ss, id)
	}
}

// auditGivenUpAgain writes run id's given_up row and marks the record, outside any lock. A row the audit log did
// not take is written again on a later pass; once this process had one accepted, only its mark is tried again.
// A crash can duplicate the row; it can no longer lose it.
func (s *Server) auditGivenUpAgain(ctx context.Context, ss store.RunLocalFolderStatuses, id uuid.UUID) {
	run, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil {
		tailSkipped(ctx, id, err)
		return
	}
	st, err := ss.RunLocalFolderStatus(ctx, id)
	if err != nil {
		tailSkipped(ctx, id, err)
		return
	}
	if !folderGivenUp(st) || st.Back.GivenUpAudited {
		return
	}
	key := givenUpKey{id: id, gen: st.Gen}
	if _, accepted := s.tail.accepted.Load(key); accepted {
		if _, err := ss.MarkGivenUpAudited(ctx, id, st.Gen); err != nil {
			slog.WarnContext(ctx, "wardynd: marking a given-up folder record audited failed; the next pass tries the mark again",
				slog.String("run_id", id.String()), slog.Any("err", err))
			return
		}
		s.tail.accepted.Delete(key)
		return
	}
	if err := s.auditGivenUp(ctx, run, st); err != nil {
		if givenUpMarkFailed(err) {
			s.tail.accepted.Store(key, struct{}{})
		}
		slog.WarnContext(ctx, "wardynd: recording a given-up folder copy back failed; the next pass tries again",
			slog.String("run_id", id.String()), slog.Any("err", err))
	}
}

// givenUpMarkFailed reports whether auditGivenUp failed at the mark, after the audit log had accepted the row.
func givenUpMarkFailed(err error) bool {
	return strings.HasPrefix(err.Error(), "mark the record audited")
}

// revokeOwedSet reports whether run id's revoke is owed to the revoke worker (revoke_owed_at). A store with no
// mark, or a read that failed, answers false.
func (s *Server) revokeOwedSet(ctx context.Context, id uuid.UUID) bool {
	ro, ok := s.cfg.Store.(store.RevokeOwed)
	if !ok {
		return false
	}
	set, err := ro.RevokeOwedSet(ctx, id)
	return err == nil && set
}
