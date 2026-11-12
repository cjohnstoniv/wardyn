// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
// and a re-read shows the record final from another writer. Zero rows alone are never final: the run may
// not be due yet, or the record may still be open. d1 is a caller whose containment decision is made (the
// statement then only re-checks that the run's containment due has passed).
func (s *Server) giveUpFolder(ctx context.Context, run types.AgentRun, d1 bool, st types.RunLocalFolderStatus) (final bool, err error) {
	ss, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return false, errors.New("this server's store cannot keep a folder record")
	}
	back := s.folderGivenUpRecord(st)
	p := store.GiveUpParams{Back: back, Cap: s.pendingCap()}
	if d1 {
		p.D1, p.Lag, p.Unsupported = true, containmentLag, true
	}
	out, err := ss.GiveUpRunLocalFolder(ctx, run.ID, p)
	if err != nil {
		return false, err
	}
	if out.Applied {
		given := types.RunLocalFolderStatus{Gen: out.Gen, Back: &back}
		if err := s.auditGivenUp(ctx, run, given); err != nil {
			slog.WarnContext(ctx, "wardynd: record a given-up folder copy back", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
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

// errGivenUpMarkFailed is auditGivenUp's answer for a row the audit log accepted whose mark failed.
var errGivenUpMarkFailed = errors.New("mark the record audited")
