// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The tail owner gives every ended run that still holds a sandbox its tail, from one owner, under the run's
// lock: the revoke, approvals, output, the computer folder's pass back and the sandbox stop. A forced-marked
// run stays with sweepForcedStops until its mark clears, except for the give-up at the cap on a runner that is
// not connected here; a runner run whose runner is not connected here is otherwise not touched.
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

// terminalSandboxLister is the listing of ended runs that still hold a sandbox, with fenced runs included, and
// the database's due and past-the-cap answers for each (store.TerminalDue).
type terminalSandboxLister interface {
	store.TerminalDue
}

// runDueReader is the one read of store.TerminalDue that a re-read under the run's lock needs (tailReread): one
// run's due and past-the-cap answers, not the listing.
type runDueReader interface {
	RunDueFlags(ctx context.Context, id uuid.UUID, keep time.Duration) (store.RunDue, error)
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
	// historyAfter is where the next tick's history tails start in the listing's order (newest end first): the
	// last row a tick took, nil after a tick that reached the end of the candidates, so the next one starts
	// again from the newest. A row whose tail failed is passed over like any other.
	historyAfter *store.TerminalSandboxCursor
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
	var history []store.TerminalSandboxRow
	seen := map[uuid.UUID]struct{}{}
	var cur *store.TerminalSandboxCursor
	for {
		rows, next, err := lister.ListTerminalSandboxPageDue(ctx, cur, tailPage, false, s.pendingCap())
		if err != nil {
			slog.WarnContext(ctx, "wardynd: the tail owner could not list the ended runs; the next tick tries again", slog.Any("err", err))
			return
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			seen[row.ID] = struct{}{}
			if s.tailVisit(ctx, row, now) {
				history = append(history, row)
			}
		}
		if next == nil {
			break
		}
		cur = next
	}
	s.tailHistoryTails(ctx, history)
	s.tail.backoff.Range(func(k, _ any) bool {
		if _, listed := seen[k.(uuid.UUID)]; !listed {
			s.tail.backoff.Delete(k)
		}
		return true
	})
}

// tailHistoryTails gives the full tail to at most tailHistoryPerTick of the rows that owe nothing (listing
// order, newest end first), from the row after the last one the previous tick took. The position moves past
// every row taken, whether its tail worked or not, so rows whose stop keeps failing cannot hold the quota and
// starve older ones; a tick that reaches the end of the rows starts again from the newest (the cursor shape of
// ListGivenUpUnaudited). Rows are taken in order, so M rows are all reached within ceil(M/tailHistoryPerTick) ticks.
func (s *Server) tailHistoryTails(ctx context.Context, rows []store.TerminalSandboxRow) {
	start := 0
	if cur := s.tail.historyAfter; cur != nil {
		start = len(rows)
		for i, row := range rows {
			if tailRowAfter(row, cur) {
				start = i
				break
			}
		}
		if start == len(rows) { // no row follows the position: wrap to the newest in this same tick
			start = 0
		}
	}
	taken := 0
	var last *store.TerminalSandboxCursor
	for _, row := range rows[start:] {
		if taken >= tailHistoryPerTick || ctx.Err() != nil {
			s.tail.historyAfter = last
			return
		}
		last = &store.TerminalSandboxCursor{At: tailRowEnd(row), ID: row.ID}
		if s.tailRun(ctx, row.ID, tailHistory) {
			taken++
		}
	}
	s.tail.historyAfter = nil
}

// tailRowEnd is when row's run ended: ended_at, else its last update (the listing's key).
func tailRowEnd(row store.TerminalSandboxRow) time.Time {
	if row.EndedAt != nil {
		return *row.EndedAt
	}
	return row.UpdatedAt
}

// tailRowAfter reports whether row comes after cur in the listing's order: ended earlier, or at the same time
// with a smaller id.
func tailRowAfter(row store.TerminalSandboxRow, cur *store.TerminalSandboxCursor) bool {
	at := tailRowEnd(row)
	return at.Before(cur.At) || (at.Equal(cur.At) && bytes.Compare(row.ID[:], cur.ID[:]) < 0)
}

// tailVisit decides one listed row, and runs its give-up or folder step at once. It reports a row that owes
// nothing and is past its containment due, for the history tails (tailHistoryTails). A forced-marked row is
// sweepForcedStops', except that a marked row on a runner that is not connected here and past the cap is decided
// like any other away row, so a runner that never returns cannot keep an owed pass for ever. A runner run whose
// runner is not connected here (a live session, never last-known caps) gets
// no runner call, no folder pass and no tail; the one step it takes is the give-up at the cap, which needs no
// runner (tailGiveUpAway): the record is read, and an owed pass whose keep ran out is given up, with its audit
// row; inside the cap it takes no lock and writes no row. A record, seed or rule that cannot be read skips the row
// for the tick inside the cap: an unreadable answer is never "not owed", which would stop a sandbox that owes its
// changes. Past the cap the same row is given up (tailStepOf), and its audit row names the read that failed.
func (s *Server) tailVisit(ctx context.Context, row store.TerminalSandboxRow, now time.Time) (history bool) {
	away := false
	if row.RunnerID != nil {
		_, connected := s.runnerConnected(*row.RunnerID)
		away = !connected
	}
	if row.ForcedStopAt != nil && !(away && row.CapPassed) {
		return false
	}
	run := containRunOf(row)
	_, step, _, err := s.tailStepOf(ctx, run, store.RunDue{Due: row.Due, CapPassed: row.CapPassed})
	if err != nil {
		tailSkipped(ctx, run.ID, err)
		return false
	}
	if away {
		if step == tailGiveUp {
			s.tailGiveUpAway(ctx, run.ID)
		}
		return false
	}
	switch step {
	case tailHistory:
		return true
	case tailGiveUp:
		s.tailRun(ctx, run.ID, step)
	case tailFolder:
		if !s.tailBackedOff(run.ID, now) && s.tailFolderVisitable(ctx, run) {
			s.tailRun(ctx, run.ID, step)
		}
	}
	return false
}

// tailSkipped logs a row left for the next tick because a read of it failed.
func tailSkipped(ctx context.Context, id uuid.UUID, err error) {
	slog.WarnContext(ctx, "wardynd: the tail owner could not read an ended run's folder state; skipping it until the next tick",
		slog.String("run_id", id.String()), slog.Any("err", err))
}

// tailStepOf reads run's record and answers the step it is due for (tailStepFor), with the record and the note
// its give-up carries (giveUpFolderNoted). A record that cannot be read is an error inside the cap. Past it the
// step is the give-up, noted folderStatusUnreadable: the statement needs no decoded record, and a record that
// never reads would otherwise keep its sandbox for ever. Before that, the owed checks that need no record are
// asked as the readable path asks them (folderOwed on the empty record, which owes whatever has not run: a run
// that ended, was not killed and holds its ref, with a folder copied back both ways, seeded, and a data_out rule
// that lets it out); a run they say owes nothing takes the readable path's not-owed step, with no give-up. A run
// that is gone (store.ErrNotFound) is only an error.
func (s *Server) tailStepOf(ctx context.Context, run types.AgentRun, due store.RunDue) (types.RunLocalFolderStatus, tailStep, string, error) {
	st, err := s.tailFolderStatus(ctx, run.ID)
	if err != nil {
		if due.CapPassed && !errors.Is(err, store.ErrNotFound) {
			if owed, oerr := s.folderOwed(ctx, run, types.RunLocalFolderStatus{}); oerr == nil && !owed {
				if due.Due {
					return types.RunLocalFolderStatus{}, tailHistory, "", nil
				}
				return types.RunLocalFolderStatus{}, tailNone, "", nil
			}
			slog.WarnContext(ctx, "wardynd: the tail owner could not read an ended run's folder record past its keep; giving its owed changes up",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
			return types.RunLocalFolderStatus{}, tailGiveUp, folderStatusUnreadable, nil
		}
		return st, tailNone, "", err
	}
	step, note, err := s.tailStepFor(ctx, run, st, due)
	return st, step, note, err
}

// tailFolderStatus is run id's folder record. A store that keeps none answers the empty record.
func (s *Server) tailFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return types.RunLocalFolderStatus{}, nil
	}
	return ss.RunLocalFolderStatus(ctx, id)
}

// tailStepFor is the step run is due for, given its record st and the database's own answers due (whether the
// run is past its containment due, and past the keep: store.RunDue, from the listing or a re-read, never a
// server's clock): give up an owed pass whose keep ran out; bring back an owed pass whose keep has not; otherwise
// the full tail once the run is past its containment due, which is also the age gate that leaves a fresh end to
// the ordinary tail. An error is a read that failed, except past the cap, where an owed pass is given up
// whatever its seed or rule reads, noted folderRuleUnreadable (the read that failed is this one).
func (s *Server) tailStepFor(ctx context.Context, run types.AgentRun, st types.RunLocalFolderStatus, due store.RunDue) (tailStep, string, error) {
	expired, err := s.folderExpiredOwed(ctx, run, st, due)
	if err != nil {
		// The cap does not wait on the rest of the record. Past it, an owed pass whose folder or data_out rule
		// cannot be read is given up like any other. Inside the cap the row is skipped for the tick.
		if folderOwedRecord(run, st) && due.CapPassed {
			return tailGiveUp, folderRuleUnreadable, nil
		}
		return tailNone, "", err
	}
	if expired {
		return tailGiveUp, "", nil
	}
	keep, err := s.folderKeepPermitted(ctx, run, st, due)
	if err != nil {
		return tailNone, "", err
	}
	if keep {
		return tailFolder, "", nil
	}
	if due.Due {
		return tailHistory, "", nil
	}
	return tailNone, "", nil
}

// tailFolderVisitable reports whether run's owed folder pass is visited now. Its runner is connected here,
// online and sharing folders (the checks computerFolderPass makes, via folderRunner, with no HTTP answer); or
// the runner is refused for good (folders switched off, the runner gone or no longer the creator's), which the
// visit records as a stalled pass, so the owed copy back gives up after computerFolderStalls visits
// (folderBackResume) instead of waiting out the whole cap. A runner that is only away (not connected, offline,
// not yet sharing folders) is not visited: no lock and no tail, and the cap stands.
func (s *Server) tailFolderVisitable(ctx context.Context, run types.AgentRun) bool {
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
	if refusal == nil {
		return true
	}
	switch refusal.body.Reason {
	case reasonComputerFoldersOff:
		return true
	case reasonRunnerNotFound:
		// folderRunner answers this for a failed lookup as well as a missing, revoked or foreign runner: only
		// the lookup's own failure is transient.
		rs, ok := s.cfg.Store.(runnerGetter)
		if !ok {
			return false
		}
		_, err := rs.GetRunner(ctx, *folder.RunnerID)
		return err == nil || errors.Is(err, store.ErrNotFound)
	}
	return false
}

// tailGiveUpAway is the give-up of a run whose runner is not connected here, and only that: under the run's lock,
// with the run and its record read again, the owed pass whose keep ran out is given up (the statement and its
// audit row). No runner is called and no tail runs, so the sandbox stays; the record is then final, the sweeps no
// longer keep the sandbox, and the queued kill or the history tail ends it once the runner is back. A runner that
// never comes back therefore cannot keep a sandbox for ever.
func (s *Server) tailGiveUpAway(ctx context.Context, id uuid.UUID) {
	lctx, unlock, ok := s.tryLockRunOp(ctx, id)
	if !ok {
		return
	}
	defer unlock()
	run, st, note, again, ok := s.tailReread(lctx, id, tailGiveUp)
	if !ok || !again {
		return
	}
	if _, err := s.giveUpFolderNoted(lctx, run, false, st, note); err != nil {
		slog.WarnContext(lctx, "wardynd: giving up an owed folder copy back failed; the next tick tries again",
			slog.String("run_id", id.String()), slog.Any("err", err))
	}
}

// tailReread reads run id and its record again under its lock, and reports whether the run is still ended with its
// ref and step is still its due step, with the note a give-up carries: the reads here decide it (tailStepOf). ok
// false is a read that failed or a run with nothing to tail.
func (s *Server) tailReread(ctx context.Context, id uuid.UUID, step tailStep) (run types.AgentRun, st types.RunLocalFolderStatus, note string, again, ok bool) {
	run, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil {
		tailSkipped(ctx, id, err)
		return run, st, "", false, false
	}
	if !isTerminalRunState(run.State) || run.SandboxRef == "" {
		return run, st, "", false, false
	}
	flags, ok := s.cfg.Store.(runDueReader)
	if !ok {
		return run, st, "", false, false
	}
	due, err := flags.RunDueFlags(ctx, id, s.pendingCap())
	if err != nil {
		tailSkipped(ctx, id, err)
		return run, st, "", false, false
	}
	var next tailStep
	if st, next, note, err = s.tailStepOf(ctx, run, due); err != nil {
		tailSkipped(ctx, id, err)
		return run, st, "", false, false
	}
	return run, st, note, next == step, true
}

// tailRun takes run id's lock, reads the run and its record again, and runs step's tail if the same step is
// still due. A lock held elsewhere leaves the run for the next tick.
func (s *Server) tailRun(ctx context.Context, id uuid.UUID, step tailStep) (ran bool) {
	lctx, unlock, ok := s.tryLockRunOp(ctx, id)
	if !ok {
		return false
	}
	defer unlock()
	run, st, note, again, ok := s.tailReread(lctx, id, step)
	if !ok || !again {
		return false
	}
	now := s.cfg.Now()
	switch step {
	case tailGiveUp:
		// Only a final record continues: the sandbox is stopped for a record the give-up made final, or one
		// another writer did, and never for a statement that applied to nothing.
		final, err := s.giveUpFolderNoted(lctx, run, false, st, note)
		if err != nil {
			slog.WarnContext(lctx, "wardynd: giving up an owed folder copy back failed; the next tick tries again",
				slog.String("run_id", id.String()), slog.Any("err", err))
		}
		if !final {
			return false
		}
		s.tailFinalize(lctx, run, step)
	case tailHistory:
		s.tailFinalize(lctx, run, step)
	case tailFolder:
		s.tailFinalize(lctx, run, step)
		s.tailNotePass(lctx, id, st, now)
	}
	return true
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
	// A new pass record: the generation moved on and the record is not the one before, which a visit that sent
	// nothing leaves as it was.
	moved := err == nil && cur.Gen != before.Gen && cur.Back != nil && cur.Back.Files > 0 &&
		(before.Back == nil || !cur.Back.At.Equal(before.Back.At))
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
		slog.WarnContext(ctx, "wardynd: recording a given-up folder copy back failed; the next pass tries again",
			slog.String("run_id", id.String()), slog.Any("err", err))
	}
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
