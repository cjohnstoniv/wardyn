// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// computerFolderRunView's fate values. Fate is what became of the changes not yet back; the sandbox copy is
// whether the sandbox is still there. They are independent: a given-up record whose stop is not yet
// confirmed is given_up with a kept copy.
const (
	folderFateRetained = "retained"
	folderFateGivenUp  = "given_up"
	folderFateComplete = "complete"

	folderCopyKept    = "kept"
	folderCopyDeleted = "deleted"
)

// folderLastReport is the last transfer report of a run's copy back, not confirmed loss: how many changed
// files it had not brought back (null only when no counting cut ever arrived), up to
// runnerwire.MaxFolderUnsyncedPaths of their names, how many more the count has, and when.
type folderLastReport struct {
	Unsynced  *int      `json:"unsynced"`
	Paths     []string  `json:"paths,omitempty"`
	PathsMore int       `json:"paths_more,omitempty"`
	At        time.Time `json:"at"`
}

// folderFate is the fate fields of a run's folder view, each nil when the server cannot tell.
type folderFate struct {
	fate, sandboxCopy *string
	lastReport        *folderLastReport
	giveUpAt          *time.Time
}

func folderString(v string) *string { return &v }

// folderFateOf reads the fate of run's changes from its record. owed is folderOwed's answer for it, and seeded
// says the folder's first sync landed. Fate is absent for a live run, for a record it cannot say anything
// true about (a pass that ended in an error and stopped the sandbox), and where nothing was ever owed. The
// sandbox copy is deleted only for a terminal run whose ref is clear and which had a sandbox with the
// folder in it: every clear of the ref follows a confirmed stop.
func folderFateOf(run types.AgentRun, st types.RunLocalFolderStatus, seeded, owed bool, keep time.Duration) folderFate {
	var f folderFate
	if !run.State.IsTerminal() {
		return f
	}
	switch {
	case run.SandboxRef != "":
		f.sandboxCopy = folderString(folderCopyKept)
	case seeded:
		f.sandboxCopy = folderString(folderCopyDeleted)
	}
	switch {
	case folderGivenUp(st):
		f.fate = folderString(folderFateGivenUp)
		f.lastReport = givenUpReport(st.Back)
	case folderBroughtBack(st):
		f.fate = folderString(folderFateComplete)
	case owed:
		f.fate = folderString(folderFateRetained)
		f.lastReport = resumeReport(st)
		due := runEnded(run).Add(keep)
		f.giveUpAt = &due
	}
	return f
}

// folderResumeCount is a resume's count of the changes not yet back, and whether anybody counted them.
// A resume written before Counted existed has its count in Unsynced alone.
func folderResumeCount(r *types.FolderResume) (int, bool) {
	return r.Unsynced, r.Counted || r.Unsynced > 0
}

// resumeReport is the report of a record still owing its copy back: its Resume's count and names, at the
// last pass's end. None when no pass has run, so nothing was reported.
func resumeReport(st types.RunLocalFolderStatus) *folderLastReport {
	if st.Back == nil {
		return nil
	}
	rep := &folderLastReport{At: st.Back.At}
	if st.Resume == nil {
		return rep
	}
	if n, counted := folderResumeCount(st.Resume); counted {
		rep.Unsynced = &n
		rep.Paths = st.Resume.Paths[:min(len(st.Resume.Paths), runnerwire.MaxFolderUnsyncedPaths)]
		if len(rep.Paths) > 0 && n > len(rep.Paths) {
			rep.PathsMore = n - len(rep.Paths)
		}
	}
	return rep
}

// givenUpReport is the report a given-up record keeps: its count, names and how many more.
func givenUpReport(back *types.FolderPassRecord) *folderLastReport {
	return &folderLastReport{Unsynced: back.GivenUpCount, Paths: back.GivenUpPaths, PathsMore: back.GivenUpPathsMore, At: back.At}
}

// setFolderFate adds the fate fields to v from run and its record. A folder that cannot be judged owed (its seed
// or data_out rule unreadable) leaves the fate off, as a view that cannot tell.
func (s *Server) setFolderFate(ctx context.Context, v *computerFolderRunView, run types.AgentRun, st types.RunLocalFolderStatus, seeded bool) {
	owed, err := s.folderOwed(ctx, run, st)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: judge a computer folder's fate", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	f := folderFateOf(run, st, seeded, owed, s.pendingCap())
	v.Fate, v.SandboxCopy, v.LastReport, v.GiveUpAt = f.fate, f.sandboxCopy, f.lastReport, f.giveUpAt
}
