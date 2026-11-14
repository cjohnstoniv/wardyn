// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// folderResumeFor is the resume a pass back that owes changes starts from (folderBackResume), given the one before
// it: the count and names the pass reported, or, when the pass reported no count, the ones before it. Counted
// says a runner counted them. A known count with no names keeps its count and has no list.
func folderResumeFor(prev *types.FolderResume, res *runnerwire.FolderPass) *types.FolderResume {
	next := &types.FolderResume{Unsynced: res.Unsynced}
	switch {
	case res.Unsynced > 0:
		next.Counted, next.Paths = true, res.UnsyncedPaths
	case prev != nil && (prev.Counted || prev.Unsynced > 0):
		next.Counted, next.Paths = true, prev.Paths
	}
	return next
}

// folderLeftText says what a given-up record leaves behind: the count, or that nobody counted. It is the
// text folderBackGivenUp ends its record with.
func folderLeftText(count *int) string {
	if count != nil && *count > 0 {
		return fmt.Sprintf("changed files not brought back: %d", *count)
	}
	return "its changes not brought back yet, uncounted"
}

// folderGivenUpRecord is the final record of an owed copy back whose keep ended, from the latest Resume
// (nil: no pass counted anything): the count when it was counted, up to runnerwire.MaxFolderUnsyncedPaths of
// the names, and how many more the count has. A known count with no names keeps its count and has no list.
// The last pass's file counts and refusals are kept.
func (s *Server) folderGivenUpRecord(st types.RunLocalFolderStatus) types.FolderPassRecord {
	now := s.cfg.Now().UTC()
	rec := types.FolderPassRecord{At: now, Refused: []types.FolderRefusal{}, GivenUpAt: &now}
	if st.Back != nil {
		rec.Files, rec.Bytes = st.Back.Files, st.Back.Bytes
		rec.Refused = append(rec.Refused, st.Back.Refused...)
	}
	var count *int
	if r := st.Resume; r != nil && (r.Counted || r.Unsynced > 0) {
		n := r.Unsynced
		count = &n
		rec.GivenUpPaths = r.Paths[:min(len(r.Paths), runnerwire.MaxFolderUnsyncedPaths)]
		if len(rec.GivenUpPaths) > 0 && n > len(rec.GivenUpPaths) {
			rec.GivenUpPathsMore = n - len(rec.GivenUpPaths)
		}
	}
	rec.GivenUpCount = count
	rec.Error = "the changes not yet back were given up when the sandbox's keep ended: " + folderLeftText(count)
	return rec
}

// giveUpFolder gives up run's owed copy back: one statement drops the Resume, writes the final record and
// moves the record's generation on, so a pass still running for it writes nothing. final says the record is
// final and the sandbox may be stopped: true after this call applied it (the audit row is written and marked
// after, and a failure of either is logged, never waited on), and true when the statement applied nothing
// and found the record already final (the store's Done, read without decoding it, so a record that does not
// decode is still found final), or a re-read shows it final from another writer. Zero rows alone are never
// final: the run may not be due yet, or the record may still be open. d1 is a caller whose containment
// decision is made (the statement then only re-checks that the run's containment due has passed).
func (s *Server) giveUpFolder(ctx context.Context, run types.AgentRun, d1 bool, st types.RunLocalFolderStatus) (final bool, err error) {
	return s.giveUpFolderNoted(ctx, run, d1, st, "")
}

// giveUpFolderNoted is giveUpFolder whose final record's error ends with note: folderRuleUnreadable or
// folderStatusUnreadable when the read that decided the give-up failed (tailStepOf), so the give-up says it did not
// wait on that read, and its given_up row names it; "" for a give-up decided on reads that all answered.
func (s *Server) giveUpFolderNoted(ctx context.Context, run types.AgentRun, d1 bool, st types.RunLocalFolderStatus, note string) (final bool, err error) {
	p := store.GiveUpParams{Cap: s.pendingCap()}
	if d1 {
		p.D1, p.Lag, p.Unsupported = true, containmentLag, true
	}
	return s.giveUpFolderWith(ctx, run, st, note, p)
}

// giveUpFolderStopped gives up the copy back still owed by a terminal run whose sandbox a boot sweep
// (reconcileOrphanedRun), the forced stop's retry (retryForcedStop) or a run's end tail (giveUpFolderEnded)
// stopped, and whose ref the caller is about to clear: with no ref no pass runs again, so the keep ends now
// (a zero cap) and the final record and its given_up row keep what was left behind. final says the ref may be
// cleared: nothing was owed (no resume, and no pass owed that never ran: keepsSandboxForFolder's two halves), or
// the record is final. A record or rule that cannot be read, or a give-up that left the record open, is not final.
func (s *Server) giveUpFolderStopped(ctx context.Context, run types.AgentRun) (final bool, err error) {
	ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return true, nil
	}
	st, err := ss.RunLocalFolderStatus(ctx, run.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	owed := st.Resume != nil
	if !owed {
		if owed, err = s.folderOwed(ctx, run, st); err != nil {
			return false, err
		}
	}
	if !owed {
		return true, nil
	}
	return s.giveUpFolderWith(ctx, run, st, "", store.GiveUpParams{})
}

// giveUpFolderEnded is giveUpFolderStopped for a run's end tail (finalizeRunEffects), which has the run's id
// alone: the run is read here, and only from a store that keeps folder records. A run that is gone is final; a
// run that cannot be read is not. A record that cannot be read is decided as the tail owner decides it
// (tailStepOf): a run that the owed checks needing no record say owes nothing is final, and otherwise (owed, or
// a folder seed or data_out rule that cannot be read either) the copy back is given up once its keep has run out
// (noted folderStatusUnreadable), by the statement that also finds a record already final without decoding it.
// Inside the keep that is not final, so a read that fails for a while throws no count away. Answering not final
// for every such record would keep the ref for good, and the tail owner's give-up past the keep would stop the
// sandbox again on every tick.
func (s *Server) giveUpFolderEnded(ctx context.Context, id uuid.UUID) (final bool, err error) {
	ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return true, nil
	}
	run, err := s.cfg.Store.GetRun(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	if _, serr := ss.RunLocalFolderStatus(ctx, id); serr != nil && !errors.Is(serr, store.ErrNotFound) {
		if owed, oerr := s.folderOwed(ctx, run, types.RunLocalFolderStatus{}); oerr == nil && !owed {
			return true, nil
		}
		return s.giveUpFolderNoted(ctx, run, false, types.RunLocalFolderStatus{}, folderStatusUnreadable)
	}
	return s.giveUpFolderStopped(ctx, run)
}

// giveUpKeptUnquiesced gives up the copy back of a terminal run whose sandbox was kept for a resume (rec, its
// record at the generation the pass back owns, holds one) and that its pass back could not quiesce: gone, or its
// agent or egress would not stop. The sandbox is stopped after this, so its keep ends here, and the record keeps
// what the resume counted, with its given_up row, as when a stop ends any keep (giveUpFolderStopped). A give-up
// not made final leaves the resume and its count in place, with the generation settled (nothing was sent), so the
// end's give-up after the stop (giveUpFolderEnded) tries again, and a ref kept while that fails keeps it owed.
// true says the pass back is decided here and the sandbox is stopped; false is no resume, or a run not ended, and
// the pass back then records its own outcome.
func (s *Server) giveUpKeptUnquiesced(ctx context.Context, run types.AgentRun, rec types.RunLocalFolderStatus) bool {
	if rec.Resume == nil || !isTerminalRunState(run.State) {
		return false
	}
	gctx, cancel := teardownCtx(ctx)
	defer cancel()
	if final, err := s.giveUpFolderWith(gctx, run, rec, "", store.GiveUpParams{}); !final {
		slog.WarnContext(gctx, "wardynd: giving up a kept sandbox's copy back failed; its resume stays for the give-up after the stop",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		s.writeComputerFolderStatus(gctx, run.ID, rec.Gen, func(st *types.RunLocalFolderStatus) { st.Settled = rec.Gen })
	}
	return true
}

// folderUnquiescedText is the record of a pass back that did not run because its sandbox's agent and egress could
// not be stopped first (qerr): a sandbox that had already stopped says so, and a person's bring-back says its agent
// is still running.
func folderUnquiescedText(ctx context.Context, qerr error) string {
	if errors.Is(qerr, errSandboxStopped) {
		return "no pass ran; the sandbox had already stopped, with its changes not brought back yet, uncounted"
	}
	if _, ok := bringBackBy(ctx); ok {
		return bringBackAgentRunningMsg
	}
	return folderBackGivenUp("no pass ran", "its agent and egress could not be stopped first: "+qerr.Error(), 0)
}

// giveUpFolderWith is giveUpFolderNoted with the statement's dues in p; the final record it writes is built here.
func (s *Server) giveUpFolderWith(ctx context.Context, run types.AgentRun, st types.RunLocalFolderStatus, note string, p store.GiveUpParams) (final bool, err error) {
	ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return false, errors.New("this server's store cannot keep a folder record")
	}
	// The statement, its audit row and the re-read below run under the run's lock on one bound of their own: a
	// holder's ctx may have none (the tail owner's loop, the boot pass). A give-up cut there is not final, and the
	// next visit reads the record again; a statement that landed after its cut is then found final (Done).
	ctx, cancel := context.WithTimeout(ctx, heldStoreCallBound)
	defer cancel()
	back := s.folderGivenUpRecord(st)
	back.Error += note
	p.Back = back
	out, err := ss.GiveUpRunLocalFolder(ctx, run.ID, p)
	if err != nil {
		return false, err
	}
	if out.Applied {
		given := types.RunLocalFolderStatus{Gen: out.Gen, Back: &back}
		if err := s.auditGivenUp(ctx, run, given); errors.Is(err, errGivenUpMarkFailed) {
			slog.WarnContext(ctx, "wardynd: the given-up row was accepted and its mark failed; the tail owner retries the mark",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
		} else if err != nil {
			slog.WarnContext(ctx, "wardynd: record a given-up folder copy back", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
		return true, nil
	}
	if out.Done {
		return true, nil
	}
	cur, err := ss.RunLocalFolderStatus(ctx, run.ID)
	if err != nil {
		return false, err
	}
	return folderDone(cur), nil
}

// auditGivenUp writes the run.folder.transfer failure row for a record given up (st, as it stands at its Gen),
// and, only once the audit log has accepted it (written, or held by the audit spool), marks the record so
// (MarkGivenUpAudited, at st.Gen). A row not accepted is an error and no mark: the row is written again later.
func (s *Server) auditGivenUp(ctx context.Context, run types.AgentRun, st types.RunLocalFolderStatus) error {
	if st.Back == nil || st.Back.GivenUpAt == nil {
		return errors.New("the record is not given up")
	}
	fst, ok := s.cfg.Store.(store.RunLocalFolders)
	ss, ok2 := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok || !ok2 {
		return errors.New("this server's store cannot keep a folder record")
	}
	folder, _, err := fst.RunLocalFolderSeed(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("read the folder: %w", err)
	}
	if folder == nil {
		return errors.New("the run has no folder")
	}
	data := map[string]any{"folder": folder.Name, "remote_dir": folder.RemoteDir, "direction": "back",
		"files": st.Back.Files, "bytes": st.Back.Bytes, "error": st.Back.Error}
	switch {
	case strings.HasSuffix(st.Back.Error, folderRuleUnreadable):
		data["rule_unreadable"] = true
	case strings.HasSuffix(st.Back.Error, folderStatusUnreadable):
		data["status_unreadable"] = true
	}
	ev := s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.folder.transfer", run.ID.String(), "failure", mustJSON(data))
	if !s.recordAuditDurably(ctx, ev) {
		return errors.New("the audit log did not take the row")
	}
	if _, err := ss.MarkGivenUpAudited(ctx, run.ID, st.Gen); err != nil {
		// The row is accepted: only its mark is tried again (the tail owner), so the row is never written twice.
		s.tail.accepted.Store(givenUpKey{id: run.ID, gen: st.Gen}, struct{}{})
		return fmt.Errorf("%w: %w", errGivenUpMarkFailed, err)
	}
	s.tail.accepted.Delete(givenUpKey{id: run.ID, gen: st.Gen})
	return nil
}

// folderRuleUnreadable ends the error of a give-up made while the folder's seed or data_out rule could not be read
// (giveUpFolderNoted); the given_up row carries it as rule_unreadable.
const folderRuleUnreadable = "; its folder seed or data_out rule could not be read"

// folderStatusUnreadable ends the error of a give-up made while the folder's record could not be read
// (giveUpFolderNoted); the given_up row carries it as status_unreadable.
const folderStatusUnreadable = "; its folder status could not be read"

// errGivenUpMarkFailed is auditGivenUp's answer for a row the audit log accepted whose mark failed.
var errGivenUpMarkFailed = errors.New("mark the record audited")
