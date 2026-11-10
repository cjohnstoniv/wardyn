// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Run view fates (folderFate.Fate) and sandbox copies (folderFate.SandboxCopy).
const (
	folderFateRetained = "retained"
	folderFateGivenUp  = "given_up"
	folderFateComplete = "complete"
	sandboxCopyKept    = "kept"
	sandboxCopyDeleted = "deleted"
)

// folderFate is what became of an ended run's computer folder changes, for the run's page. Every field is
// absent while the run is live, and where the server cannot tell.
//   - Fate: "retained" while a pass back is owed and the sandbox is kept for it; "given_up" once the
//     changes not yet back were given up; "complete" once the pass back is done.
//   - SandboxCopy: "kept" while the sandbox is recorded as not torn down (its ref set); "deleted" once its
//     teardown was confirmed (every ref clear follows a confirmed stop).
//   - LastReport: the last transfer report's count of changes not brought back, and their names. It is
//     what the runner last reported, not a confirmed loss.
//   - GiveUpAt: while retained, when the changes not yet back are given up.
type folderFate struct {
	Fate        *string           `json:"fate,omitempty"`
	SandboxCopy *string           `json:"sandbox_copy,omitempty"`
	LastReport  *folderLastReport `json:"last_report,omitempty"`
	GiveUpAt    *time.Time        `json:"give_up_at,omitempty"`
}

// folderLastReport is a transfer report's changes not brought back. Unsynced is null only when no pass
// ever counted them; a count with no names (a runner that does not list them) keeps its count.
type folderLastReport struct {
	Unsynced  *int      `json:"unsynced"`
	Paths     []string  `json:"paths,omitempty"`
	PathsMore int       `json:"paths_more,omitempty"`
	At        time.Time `json:"at"`
}

// readFolderFate reads run id's folderFate.
func (s *Server) readFolderFate(ctx context.Context, id uuid.UUID) folderFate {
	run, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil || !isTerminalRunState(run.State) {
		return folderFate{}
	}
	f := s.readFolderPassFacts(ctx, run)
	if f.err != nil || f.folder == nil {
		return folderFate{}
	}
	var v folderFate
	switch {
	case run.SandboxRef != "":
		v.SandboxCopy = ptrTo(sandboxCopyKept)
	case f.seeded:
		v.SandboxCopy = ptrTo(sandboxCopyDeleted)
	}
	back, resume := f.status.Back, f.status.Resume
	switch {
	case back != nil && back.GivenUpAt != nil:
		v.Fate = ptrTo(folderFateGivenUp)
		v.LastReport = &folderLastReport{Unsynced: back.GivenUpCount, Paths: back.GivenUpPaths, PathsMore: back.GivenUpPathsMore, At: back.At}
	case run.SandboxRef != "" && f.owed():
		v.Fate = ptrTo(folderFateRetained)
		ended := run.UpdatedAt
		if run.EndedAt != nil {
			ended = *run.EndedAt
		}
		v.GiveUpAt = ptrTo(ended.Add(s.pendingCap()))
		if resume != nil {
			r := &folderLastReport{Paths: resume.Paths}
			if back != nil {
				r.At = back.At
			}
			if resume.Counted {
				r.Unsynced = ptrTo(resume.Unsynced)
				r.PathsMore = max(resume.Unsynced-len(resume.Paths), 0)
			}
			v.LastReport = r
		}
	case folderStatusDone(f.status):
		v.Fate = ptrTo(folderFateComplete)
	}
	return v
}

func ptrTo[T any](v T) *T { return &v }
