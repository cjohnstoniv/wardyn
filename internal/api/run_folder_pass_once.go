// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner runs one pass per run at a time when it says so (Caps.FolderPassOnce), and refuses a second
// with folder_pass_busy. Every pass, the seed and each pass back, moves the run's record to a generation
// of its own first, stamped with when (GenAt), and marks it settled (types.RunLocalFolderStatus.Settled)
// once the runner answered for it or nothing was sent. A runner not known to run one pass per run (an
// older one, or one whose caps are not in or that cannot be reached) gets no pass while its last one is
// unsettled and oldPassBound has not passed since it was sent: one it may still be running would run
// beside the new one.

// folderPassBusyMsg is the record of a pass a runner refused while it runs another for the run.
const folderPassBusyMsg = "The runner is still running this run's last pass back; a later sweep tries again"

// oldPassBound is how long after a pass back was sent (GenAt) a runner that may run two passes at once
// gets no other: rc.17 closed that pass's stream by computerFolderBackTimeout + computerFolderAccountTime
// and sent the next on its next resume sweep, an undispatchedGrace later at an on-time sweep. 62 min 5 s.
// It is a delay, not a proof that the old pass ended: past it an older runner's pass can still overlap the
// next, and only a runner that runs one pass per run at a time rules that out.
const oldPassBound = computerFolderBackTimeout + computerFolderAccountTime + undispatchedGrace

// errFolderPassUnconfirmed is a pass not sent, as the runner it would go to may still run the last one
// (lastPassOpen).
var errFolderPassUnconfirmed = errors.New("no pass ran; the runner's last pass for this run was not confirmed, and another could run beside it. Update the Wardyn runner")

// lastPassOpen reports a pass back that must not be sent yet: the last one is not answered, oldPassBound
// has not passed since it was sent (lastPassUnanswered), and the runner is not known to run one pass per
// run at a time: an older runner, or unknown facts (caps not in, a failed lookup, the runner offline).
// computerFolderPass checks the same on the endpoint it sends to.
func (s *Server) lastPassOpen(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, rec types.RunLocalFolderStatus) bool {
	if folder.RunnerID == nil || !s.lastPassUnanswered(rec) {
		return false
	}
	sub, refusal := s.folderRunner(ctx, run.CreatedBy, *folder.RunnerID)
	return passMayRunBeside(sub, refusal)
}

// lastPassUnanswered is a last pass the runner may still be running: unsettled (lastPassUnsettled), and
// sent less than oldPassBound ago, or at a time not known.
func (s *Server) lastPassUnanswered(rec types.RunLocalFolderStatus) bool {
	return lastPassUnsettled(rec) && !s.pastOldPassBound(rec)
}

// lastPassUnsettled is a record whose last pass the runner did not answer for: a generation past the settled
// one, or a record an earlier release wrote, whose passes were never counted. That one is Gen 0 with a seed:
// rc.17 recorded every seed (rc.17 run_computer_folder.go:181) and wrote no generation, while every seed
// since moves the record to a generation before it passes (seedComputerFolder), and a pass back needs the
// folder seeded (bringComputerFolderBack).
func lastPassUnsettled(rec types.RunLocalFolderStatus) bool {
	return rec.Gen != rec.Settled || (rec.Gen == 0 && rec.Seed != nil)
}

// pastOldPassBound is a record whose generation was sent oldPassBound ago or more. One with no send time is
// not: it is stamped at the visit that finds it (bringComputerFolderBack), and the bound runs from there.
func (s *Server) pastOldPassBound(rec types.RunLocalFolderStatus) bool {
	return !rec.GenAt.IsZero() && !s.cfg.Now().Before(rec.GenAt.Add(oldPassBound))
}

// keepForOpenPass keeps run's sandbox while its last pass may still run (lastPassOpen), with no pass sent,
// no generation moved and no stall counted. A record with no send time is stamped now, and the bound runs
// from it. On a resume visit (prev.Resume set) the sandbox was kept already, its agent stopped and its
// egress cut, and stays so. On the first, its agent and egress are stopped first, and it is kept with a
// resume; one where that fails, or that cannot be quiesced, is stopped with the changes not brought back.
func (s *Server) keepForOpenPass(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, prev types.RunLocalFolderStatus, ref string) bool {
	if prev.Resume == nil {
		qctx, qcancel := teardownCtx(ctx)
		qerr := s.quiesceKeptSandbox(qctx, run, ref)
		qcancel()
		if qerr != nil {
			rctx, cancel := teardownCtx(ctx)
			defer cancel()
			errText := folderBackGivenUp("no pass ran; the runner may still be running its last pass back",
				"its agent and egress could not be stopped first: "+qerr.Error(), 0)
			return s.recordFolderBackAt(rctx, run.ID, folder, prev.Gen, false, nil, runnerwire.FolderPass{}, errText)
		}
	}
	return s.keepForResume(ctx, run.ID, folder, prev.Gen, prev)
}

// keepForResume keeps run id's sandbox for a later pass, at gen, with no stall counted and nothing claimed
// done. The sweeps keep only a sandbox whose record holds a resume (computerFolderKept), so one is written
// where rec, the record as read, has none, and a send time where it has none. A resume that cannot be
// written stops the sandbox with its changes not brought back; a send time that cannot is stamped later.
func (s *Server) keepForResume(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int, rec types.RunLocalFolderStatus) bool {
	if rec.Resume != nil && !rec.GenAt.IsZero() {
		return true
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	if s.writeComputerFolderStatus(rctx, id, gen, func(st *types.RunLocalFolderStatus) {
		if st.Resume == nil {
			st.Resume = &types.FolderResume{}
		}
		if st.GenAt.IsZero() {
			st.GenAt = s.cfg.Now()
		}
	}) || rec.Resume != nil {
		return true
	}
	errText := folderBackGivenUp("no pass ran; the runner may still be running its last pass back",
		"the sandbox could not be kept for a later pass", 0)
	return s.recordFolderBackAt(rctx, id, folder, gen, false, nil, runnerwire.FolderPass{}, errText)
}

// unansweredStallsPast is a last pass, unanswered and past its bound, that was a stall in a row of them:
// the visit gives the changes up without a pass.
func (s *Server) unansweredStallsPast(prev types.RunLocalFolderStatus) bool {
	return lastPassUnsettled(prev) && prev.Resume != nil && prev.Resume.Stalled >= computerFolderStalls && s.pastOldPassBound(prev)
}

// keptUnanswered is the verdict on a pass the runner did not answer for, given folderBackVerdict's and the
// resume before the pass (prev): one it may still be running never stops the sandbox at its own visit. It
// counts once as a stall against prev, and a give-up it earns comes past its bound (unansweredStallsPast). A
// sandbox not quiesced (qerr) is not kept.
func keptUnanswered(prev, next *types.FolderResume, errText, passErr string, unsynced int, qerr error) (*types.FolderResume, string) {
	if next != nil || qerr != nil {
		return next, errText
	}
	next = &types.FolderResume{Stalled: 1, Unsynced: unsynced}
	if prev != nil {
		next.Stalled = prev.Stalled + 1
		if unsynced == 0 {
			next.Unsynced = prev.Unsynced
		}
	}
	return next, passErr + "; the runner did not answer for this pass, so the sandbox is kept with its agent stopped and no egress, and the next pass waits until the runner answers for this one or about an hour has passed"
}

// folderBroughtBack is a record whose pass back completed with nothing owed.
func folderBroughtBack(rec types.RunLocalFolderStatus) bool {
	return rec.Back != nil && rec.Back.Error == ""
}

// passMayRunBeside is a runner, as folderRunner answered it, not known to run one pass per run at a time.
func passMayRunBeside(sub *remote.Substrate, refusal *runRefusal) bool {
	return refusal != nil || !runsOnePassPerRun(sub.Caps())
}

// runsOnePassPerRun is a runner whose caps say it runs one pass per run at a time. Caps not read yet count
// as an older runner's.
func runsOnePassPerRun(caps runnerwire.Caps, read bool) bool {
	return read && caps.FolderPassOnce
}

// bumpComputerFolderGen moves run id's record from gen to the next, and answers the record as it then
// stands, its Gen the generation the pass then owns: false when the record moved first or could not be
// written. A store with no records answers gen, settled.
func (s *Server) bumpComputerFolderGen(ctx context.Context, id uuid.UUID, gen int) (types.RunLocalFolderStatus, bool) {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return types.RunLocalFolderStatus{Gen: gen, Settled: gen}, true
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	rec, ok, err := st.BumpRunLocalFolderGen(rctx, id, store.FolderStatusGuard{Gen: gen}, s.cfg.Now())
	if err != nil || !ok {
		slog.WarnContext(ctx, "wardynd: a computer folder's pass record moved before its pass",
			slog.String("run_id", id.String()), slog.Bool("moved", ok), slog.Any("err", err))
		return types.RunLocalFolderStatus{}, false
	}
	return rec, true
}

// recordFolderPassBusy records a pass back the runner refused as busy, at the generation it owned: settled,
// since the runner answered, and kept for a resume so a later sweep tries again. The sandbox stays up
// either way: a record that did not land is another owner's to decide.
func (s *Server) recordFolderPassBusy(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int) bool {
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	if s.writeComputerFolderStatus(rctx, id, gen, func(st *types.RunLocalFolderStatus) {
		if st.Resume == nil {
			st.Resume = &types.FolderResume{}
		}
		st.Settled = gen
	}) {
		s.auditComputerFolderPass(rctx, id, folder, true, runnerwire.FolderPass{}, folderPassBusyMsg)
	}
	return true
}
