// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pendingCapMin is how long an owed copy back keeps its run's sandbox at least: a run's changes that were not
// brought back are given up this long after it ended, whatever EndedRunGrace says.
const pendingCapMin = 7 * 24 * time.Hour

// pendingCap is how long after a run's end its sandbox is kept for an owed copy back: the longer of
// EndedRunGrace and pendingCapMin. A run's lifetime cap bounds a live run and is no part of it.
func (s *Server) pendingCap() time.Duration { return max(s.cfg.EndedRunGrace, pendingCapMin) }

// folderBackEligible reports whether a pass back can run for run runID at all: it has a folder, on a
// folder runner, both ways, seeded, and its data_out rule lets content out. A seed or rule that cannot
// be read is an error and not an answer: false here stops the sandbox.
func (s *Server) folderBackEligible(ctx context.Context, runID uuid.UUID) (bool, error) {
	st, ok := s.cfg.Store.(store.RunLocalFolders)
	if !ok {
		return false, nil
	}
	folder, seed, err := st.RunLocalFolderSeed(ctx, runID)
	if err != nil {
		return false, err
	}
	if folder == nil || folder.RunnerID == nil || folder.Direction != types.ComputerFolderBoth || seed.SeededAt == nil {
		return false, nil
	}
	rule, _, err := s.runDataOutRule(ctx, runID)
	if err != nil {
		return false, err
	}
	return rule == types.TransferAllowed, nil
}

// folderDone is a record whose pass back ended with its Back written and no Resume left to run.
func folderDone(st types.RunLocalFolderStatus) bool { return st.Back != nil && st.Resume == nil }

// folderGivenUp is a record whose owed changes were given up: it is final, and no pass runs for it again.
func folderGivenUp(st types.RunLocalFolderStatus) bool {
	return st.Back != nil && st.Back.GivenUpAt != nil
}

// folderFinal is a record no pass runs for again: its pass back completed with nothing owed, or its owed changes
// were given up.
func folderFinal(st types.RunLocalFolderStatus) bool {
	return folderBroughtBack(st) || folderGivenUp(st)
}

// folderOwedRecord is the part of "a pass back is owed" that the run and its record answer: the run ended
// (a killed run is never copied back), still holds its sandbox, has a Resume or has had no pass, and is not
// given up.
func folderOwedRecord(run types.AgentRun, st types.RunLocalFolderStatus) bool {
	return run.State.IsTerminal() && run.State != types.RunKilled && run.SandboxRef != "" &&
		(st.Resume != nil || st.Back == nil) && !folderGivenUp(st)
}

// folderOwed reports whether run owes a pass back: folderOwedRecord, and a pass back can run for it
// (folderBackEligible). The caller reads st itself and skips the row for the tick when that read fails;
// an error here is a folder seed or data_out rule that could not be read, and no answer.
func (s *Server) folderOwed(ctx context.Context, run types.AgentRun, st types.RunLocalFolderStatus) (bool, error) {
	if !folderOwedRecord(run, st) {
		return false, nil
	}
	return s.folderBackEligible(ctx, run.ID)
}

// folderExpiredOwed is folderOwed for a run whose keep has run out: its end plus pendingCap has passed, which
// is the database's answer (store.RunDue.CapPassed, against its now()) and never a comparison with a server's clock.
func (s *Server) folderExpiredOwed(ctx context.Context, run types.AgentRun, st types.RunLocalFolderStatus, due store.RunDue) (bool, error) {
	if !due.CapPassed {
		return false, nil
	}
	return s.folderOwed(ctx, run, st)
}

// folderKeepPermitted is folderOwed for a run whose keep has not run out (the database's answer, as for
// folderExpiredOwed).
func (s *Server) folderKeepPermitted(ctx context.Context, run types.AgentRun, st types.RunLocalFolderStatus, due store.RunDue) (bool, error) {
	if due.CapPassed {
		return false, nil
	}
	return s.folderOwed(ctx, run, st)
}
