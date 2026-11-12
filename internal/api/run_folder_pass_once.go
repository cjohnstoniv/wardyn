// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
func (s *Server) lastPassOpen(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, g passGuard) bool {
	if folder.RunnerID == nil || !s.mayStillRun(g) {
		return false
	}
	sub, refusal := s.folderRunner(ctx, run.CreatedBy, *folder.RunnerID)
	return passMayRunBeside(sub, refusal)
}

// passGuard is what a pass back is checked against before it is sent (lastPassOpen): the record before its
// generation, and whether that could not be read.
type passGuard struct {
	last   types.RunLocalFolderStatus
	unread bool
}

// passGuardFor is run's passGuard. A virtual machine's sandbox cannot be kept with its agent stopped
// (stopForKeep), so its pass is sent as before this release, unguarded: it can still run beside one that is
// stuck, which only a runner that runs one pass per run at a time rules out.
func passGuardFor(run types.AgentRun, prev types.RunLocalFolderStatus, unread bool) passGuard {
	if run.MachineKind.IsVM() {
		return passGuard{}
	}
	return passGuard{last: prev, unread: unread}
}

// mayStillRun is a guard whose last pass the runner may still be running (lastPassUnanswered), or whose
// record could not be read: a read that failed never sends to a runner not known to run one pass per run at
// a time.
func (s *Server) mayStillRun(g passGuard) bool {
	return g.unread || s.lastPassUnanswered(g.last)
}

// lastPassUnanswered is a last pass the runner may still be running: unsettled (lastPassUnsettled), and
// sent less than oldPassBound ago, or at a time not known.
func (s *Server) lastPassUnanswered(rec types.RunLocalFolderStatus) bool {
	return lastPassUnsettled(rec) && !s.pastOldPassBound(rec)
}

// lastPassUnsettled is a record whose last pass the runner did not answer for: a generation past the settled
// one, or a record an earlier release wrote, whose passes were never counted. That one is Gen 0 with a seed:
// rc.17 recorded every seed (rc.17's seedComputerFolder) and wrote no generation, while every seed
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
func (s *Server) keepForOpenPass(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, g passGuard, ref string) bool {
	prev := g.last
	if prev.Resume == nil || g.unread {
		if err := s.stopForKeep(ctx, run, ref); err != nil {
			// A record never read has no generation to prove another owner moved on (prev.Gen is 0), so a
			// record fault here never keeps a sandbox whose agent could not be stopped.
			kept := s.stopNotKept(ctx, run.ID, folder, prev.Gen, false, "no pass ran; the runner may still be running its last pass back", err)
			return kept && !g.unread
		}
	}
	kept := s.keepForResume(ctx, run.ID, folder, prev.Gen, prev, prev.GenAt)
	if !kept || !g.unread {
		return kept
	}
	// From a record never read, kept only with the resume the sweeps keep it by: a record found past the
	// unknown generation proves no other owner. The read, like the write, outlives a cancelled caller.
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	return s.folderStatus(rctx, run.ID).Resume != nil
}

// stopForKeep makes run's sandbox safe to keep for a later pass with none sent: quiesced, its agent stopped
// and no egress, where its substrate can; else (holdUnquiesced) with its agent stopped alone.
func (s *Server) stopForKeep(ctx context.Context, run types.AgentRun, ref string) error {
	qctx, cancel := teardownCtx(ctx)
	defer cancel()
	return s.holdUnquiesced(qctx, run, s.quiesceKeptSandbox(qctx, run, ref))
}

// holdUnquiesced answers whether a sandbox whose quiesce answered qerr may be kept: one quiesced (qerr nil)
// may; one that cannot be quiesced (notQuiescable) may once its agent is stopped alone, as a bring-back stops
// it (a sandbox on a runner, or on Kubernetes in server mode), with no claim on its egress. A virtual
// machine's sandbox, or one whose quiesce failed, may not.
func (s *Server) holdUnquiesced(ctx context.Context, run types.AgentRun, qerr error) error {
	var cannot notQuiescable
	switch {
	case !errors.As(qerr, &cannot):
		return qerr
	case run.MachineKind.IsVM():
		return errors.New("a virtual machine's agent cannot be stopped alone")
	case !s.agentStoppable(ctx, run):
		return errors.New("its agent is not one that can be stopped alone")
	}
	if err := s.stopAgentForPass(ctx, run.SandboxRef); err != nil {
		return fmt.Errorf("the agent could not be stopped: %w", err)
	}
	return nil
}

// keepBusy keeps the sandbox of a pass back the runner refused as busy (recordFolderPassBusy), once it is
// safe to keep (holdUnquiesced); one that is not is stopped with its changes not brought back.
func (s *Server) keepBusy(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, gen int, qerr error) bool {
	if err := s.holdUnquiesced(ctx, run, qerr); err != nil {
		return s.stopNotKept(ctx, run.ID, folder, gen, true, "no pass ran; the runner is still running this run's last pass back", err)
	}
	return s.recordFolderPassBusy(ctx, run.ID, folder, gen)
}

// keepUnconfirmed keeps the sandbox of a pass back not sent because the runner may still run the last
// (errFolderPassUnconfirmed), once it is safe to keep (holdUnquiesced), with the last pass's send time
// (genAt) kept, so the wait does not start again. One not safe to keep is stopped with its changes not
// brought back.
func (s *Server) keepUnconfirmed(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, gen int, rec types.RunLocalFolderStatus, genAt time.Time, qerr error) bool {
	if err := s.holdUnquiesced(ctx, run, qerr); err != nil {
		return s.stopNotKept(ctx, run.ID, folder, gen, false, "no pass ran; the runner may still be running its last pass back", err)
	}
	return s.keepForResume(ctx, run.ID, folder, gen, rec, genAt)
}

// stopNotKept records, at gen, a pass back that did not run on a sandbox that cannot be kept for why, and
// lets it be stopped with its changes not brought back.
func (s *Server) stopNotKept(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int, settled bool, head string, why error) bool {
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	errText := folderBackGivenUp(head, "the sandbox could not be kept with its agent stopped: "+why.Error(), 0)
	return s.recordFolderBackAt(rctx, id, folder, gen, settled, nil, runnerwire.FolderPass{}, errText)
}

// keepForResume keeps run id's sandbox for a later pass, at gen, with no stall counted and nothing claimed
// done. The sweeps keep only a sandbox whose record holds a resume (computerFolderKept), so one is written
// where rec, the record as read, has none. The send time becomes genAt, the last pass's, or, with none,
// now where rec has none. A resume that cannot be written stops the sandbox with its changes not brought
// back; a send time that cannot is left as it is.
func (s *Server) keepForResume(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int, rec types.RunLocalFolderStatus, genAt time.Time) bool {
	want := genAt
	if want.IsZero() {
		want = rec.GenAt
	}
	if rec.Resume != nil && !want.IsZero() && rec.GenAt.Equal(want) {
		return true
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	if s.writeComputerFolderStatus(rctx, id, gen, func(st *types.RunLocalFolderStatus) {
		if st.Resume == nil {
			st.Resume = &types.FolderResume{}
		}
		switch {
		case !genAt.IsZero():
			st.GenAt = genAt
		case st.GenAt.IsZero():
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
// written. lost is only the first, a clean miss; a store error is never lost, since its bump may have
// landed. A store with no records answers gen, settled.
func (s *Server) bumpComputerFolderGen(ctx context.Context, id uuid.UUID, gen int) (rec types.RunLocalFolderStatus, ok, lost bool) {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return types.RunLocalFolderStatus{Gen: gen, Settled: gen}, true, false
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	rec, ok, err := st.BumpRunLocalFolderGen(rctx, id, store.FolderStatusGuard{Gen: gen}, s.cfg.Now())
	if err != nil || !ok {
		slog.WarnContext(ctx, "wardynd: a computer folder's pass record moved before its pass",
			slog.String("run_id", id.String()), slog.Bool("moved", ok), slog.Any("err", err))
		return types.RunLocalFolderStatus{}, false, err == nil
	}
	return rec, true, false
}

// folderGenTaken reports whether a fresh read finds run id's computer folder record past gen: proof that
// another owner moved it on. A read that fails, or a record still at gen, proves nothing.
func (s *Server) folderGenTaken(ctx context.Context, id uuid.UUID, gen int) bool {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return false
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	cur, err := st.RunLocalFolderStatus(rctx, id)
	return err == nil && cur.Gen > gen
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
