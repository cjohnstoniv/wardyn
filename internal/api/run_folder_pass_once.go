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
	if folder.RunnerID == nil || !mayStillRun(g) {
		return false
	}
	sub, refusal := s.folderRunner(ctx, run.CreatedBy, *folder.RunnerID)
	return passMayRunBeside(sub, refusal)
}

// passGuard is what a pass back is checked against before it is sent (lastPassOpen): the record before its
// generation, whether that could not be read, and whether its last pass was sent oldPassBound ago or more (past,
// from lastPassPast, read before the pass's own bump).
type passGuard struct {
	last   types.RunLocalFolderStatus
	unread bool
	past   bool
}

// passGuardFor is run's passGuard, with past as lastPassPast read it before the bump. A virtual machine's sandbox
// cannot be kept with its agent stopped (stopForKeep), so its pass is sent as before this release, unguarded: it
// can still run beside one that is stuck, which only a runner that runs one pass per run at a time rules out.
func passGuardFor(run types.AgentRun, prev types.RunLocalFolderStatus, unread, past bool) passGuard {
	if run.MachineKind.IsVM() {
		return passGuard{}
	}
	return passGuard{last: prev, unread: unread, past: past}
}

// mayStillRun is a guard whose last pass the runner may still be running (lastPassUnanswered), or whose
// record could not be read: a read that failed never sends to a runner not known to run one pass per run at
// a time.
func mayStillRun(g passGuard) bool {
	return g.unread || lastPassUnanswered(g.last, g.past)
}

// lastPassUnanswered is a last pass the runner may still be running: unsettled (lastPassUnsettled), and not
// past its bound (past, lastPassPast's answer), which a send time not known never is.
func lastPassUnanswered(rec types.RunLocalFolderStatus, past bool) bool {
	return lastPassUnsettled(rec) && !past
}

// lastPassPast is whether prev, the record as read before a pass's own bump, holds an unsettled last pass sent
// oldPassBound ago or more. It is asked once, before the bump (bringComputerFolderBack, seedComputerFolder), and
// carried in the passGuard and to unansweredStallsPast: the database ages only the generation it holds
// (store.FolderGenAges), so once the bump has moved the record on, a record read before it is never past. A settled
// last pass is not asked, since nothing it decides reads the answer.
func (s *Server) lastPassPast(ctx context.Context, id uuid.UUID, prev types.RunLocalFolderStatus) bool {
	return lastPassUnsettled(prev) && s.pastOldPassBound(ctx, id, prev)
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
// not: it is stamped at the visit that finds it (keepForResume), and the bound runs from there. The send time is
// stamped by the database (BumpRunLocalFolderGen, StampFolderGenAt) and aged by it (store.FolderGenAges), so no
// server's clock is compared with it; an age that cannot be read is not past, which keeps the last pass open. The
// database answers only while the record is still at rec.Gen, so it is asked before a bump (lastPassPast). A store
// with no such read (the tests' fakes) falls back to this server's clock.
func (s *Server) pastOldPassBound(ctx context.Context, id uuid.UUID, rec types.RunLocalFolderStatus) bool {
	if rec.GenAt.IsZero() {
		return false
	}
	if ages, ok := s.cfg.Store.(store.FolderGenAges); ok {
		past, err := ages.FolderGenPastBound(ctx, id, rec.Gen, oldPassBound)
		return err == nil && past
	}
	return !s.cfg.Now().Before(rec.GenAt.Add(oldPassBound))
}

// keepForOpenPass keeps run's sandbox while its last pass may still run (lastPassOpen), with no pass sent,
// no generation moved and no stall counted. A record with no send time is stamped now, by the database
// (keepForResume), and the bound runs from it. On a resume visit (prev.Resume set) the sandbox was kept
// already, its agent stopped and its egress cut, and stays so. On the first, its agent and egress are stopped first, and it is kept with a
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

// folderPassAwayMsg is the record of a pass back not sent because the computer's runner was offline.
const folderPassAwayMsg = "Kept: the computer's runner was offline at the end; the copy back is tried again when it is back"

// keepAway keeps the sandbox of a pass back not sent because the computer's runner is offline or not connected
// here, once it is safe to keep (holdUnquiesced), with a resume and nothing counted, so a later sweep tries
// again when the runner is back. One not safe to keep (a virtual machine, an agent that cannot be stopped
// alone, a sandbox on that same offline computer) is stopped with its changes not brought back, as before.
func (s *Server) keepAway(ctx context.Context, run types.AgentRun, folder types.RunLocalFolder, gen int, qerr error) bool {
	if err := s.holdUnquiesced(ctx, run, qerr); err != nil {
		return s.stopNotKept(ctx, run.ID, folder, gen, true, "no pass ran; the computer's runner was offline at the end", err)
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	if s.writeComputerFolderStatus(rctx, run.ID, gen, func(st *types.RunLocalFolderStatus) {
		if st.Resume == nil {
			st.Resume = &types.FolderResume{}
		}
		st.Back = folderPassRecord(s.cfg.Now(), runnerwire.FolderPass{}, folderPassAwayMsg, true)
		st.Settled = gen
	}) {
		s.auditComputerFolderPass(rctx, run.ID, folder, true, runnerwire.FolderPass{}, folderPassAwayMsg)
		return true
	}
	// A resume that did not land leaves the sweeps nothing to resume: kept only for an owner that moved the record on.
	return s.folderGenTaken(rctx, run.ID, gen) || s.stopNotKept(ctx, run.ID, folder, gen, true, "no pass ran; the computer's runner was offline at the end", errors.New("its record could not be written"))
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
// done. The sweeps keep a sandbox whose record holds a resume or owes a pass back not yet run
// (keepsSandboxForFolder), so a resume is written where rec, the record as read, has none. The send time
// becomes genAt, the last pass's, or, with none, the database's now where the record has none
// (StampFolderGenAt), the clock that ages it; only a store that keeps no clock is stamped with this server's. A
// resume that cannot be written stops the sandbox with its changes not brought back; a send time that cannot is
// left as it is, and the next visit stamps it.
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
	ages, dbClock := s.cfg.Store.(store.FolderGenAges)
	wrote := s.writeComputerFolderStatus(rctx, id, gen, func(st *types.RunLocalFolderStatus) {
		if st.Resume == nil {
			st.Resume = &types.FolderResume{}
		}
		switch {
		case !genAt.IsZero():
			st.GenAt = genAt
		case st.GenAt.IsZero() && !dbClock:
			st.GenAt = s.cfg.Now()
		}
	})
	if wrote && genAt.IsZero() && dbClock {
		if _, err := ages.StampFolderGenAt(rctx, id, gen); err != nil {
			slog.WarnContext(ctx, "wardynd: stamp a computer folder pass's send time; the next visit stamps it",
				slog.String("run_id", id.String()), slog.Any("err", err))
		}
	}
	if wrote || rec.Resume != nil {
		return true
	}
	errText := folderBackGivenUp("no pass ran; the runner may still be running its last pass back",
		"the sandbox could not be kept for a later pass", 0)
	return s.recordFolderBackAt(rctx, id, folder, gen, false, nil, runnerwire.FolderPass{}, errText)
}

// unansweredStallsPast is a last pass, unanswered and past its bound (past, lastPassPast's answer before the
// bump), that was a stall in a row of them: the visit gives the changes up without a pass.
func unansweredStallsPast(prev types.RunLocalFolderStatus, past bool) bool {
	return lastPassUnsettled(prev) && prev.Resume != nil && prev.Resume.Stalled >= computerFolderStalls && past
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
	if err != nil {
		slog.WarnContext(ctx, "wardynd: a computer folder's pass record could not be moved before its pass",
			slog.String("run_id", id.String()), slog.Any("err", err))
		return types.RunLocalFolderStatus{}, false, false
	}
	if !ok {
		slog.WarnContext(ctx, "wardynd: a computer folder's pass record moved before its pass",
			slog.String("run_id", id.String()), slog.Bool("moved", ok))
		return types.RunLocalFolderStatus{}, false, true
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
// either way, and this always answers true. The runner's busy reply is proof that another pass for this run
// is live, and stopping the sandbox under it would cut that pass off and lose what it is moving; the 7-day
// cap bounds the keep. So a store fault here does not stop it: it is logged apart, and the next visit tries
// the record again. A record that did not land because another owner moved it on is that owner's to decide.
func (s *Server) recordFolderPassBusy(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int) bool {
	st, ok := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !ok {
		return true
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	cur, err := st.RunLocalFolderStatus(rctx, id)
	landed := false
	if err == nil && cur.Gen == gen {
		if cur.Resume == nil {
			cur.Resume = &types.FolderResume{}
		}
		cur.Settled = gen
		landed, err = st.SetRunLocalFolderStatusIf(rctx, id, store.FolderStatusGuard{Gen: gen}, cur)
	}
	switch {
	case err != nil:
		slog.WarnContext(ctx, "wardynd: recording a busy folder pass failed; the sandbox is kept because the runner is still running a pass for it",
			slog.String("run_id", id.String()), slog.Any("err", err))
	case landed:
		s.auditComputerFolderPass(rctx, id, folder, true, runnerwire.FolderPass{}, folderPassBusyMsg)
	}
	return true
}

// keepAfterBumpMiss answers whether the sandbox stays up after a bump that did not land: only when another
// owner moved the record on, which a fresh read proves; otherwise the miss is recorded and the sandbox stops.
func (s *Server) keepAfterBumpMiss(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int, lost, unread bool) bool {
	if lost && !unread && s.folderGenTaken(ctx, id, gen) {
		return true
	}
	s.recordFolderBumpMissed(ctx, id, folder, gen, lost)
	return false
}

// recordFolderBumpMissed is what a pass back that ran no pass leaves when its record
// could not be moved to the pass's generation (the sandbox is stopped after this, by the caller, and the stop is
// reported on its own path): one failure row, and one try at the record on gen, the
// generation it was read at. lost is a clean miss (another owner moved the record); otherwise the store
// failed, and its error is in bumpComputerFolderGen's log. A write that does not land is logged and changes
// nothing else.
func (s *Server) recordFolderBumpMissed(ctx context.Context, id uuid.UUID, folder types.RunLocalFolder, gen int, lost bool) {
	why := "its store could not write it"
	if lost {
		why = "another owner moved it"
	}
	errText := "no pass ran; the folder record could not be moved before its pass: " + boundReason(why) +
		"; the changes were not copied back by this attempt, uncounted"
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	s.auditComputerFolderPass(rctx, id, folder, true, runnerwire.FolderPass{}, errText)
	if !s.writeComputerFolderStatus(rctx, id, gen, func(st *types.RunLocalFolderStatus) {
		st.Back = folderPassRecord(s.cfg.Now(), runnerwire.FolderPass{}, errText, true)
	}) {
		slog.WarnContext(ctx, "wardynd: a computer folder's pass record could not say why its pass did not run", slog.String("run_id", id.String()))
	}
}

// keepUnreadFolder decides the sandbox of an end that could not read its computer folder's settings or its run's
// data_out rule by the rule the sweeps keep a sandbox by (folderKeepVerdict): kept, with no pass run and nothing
// written, only once the agent's cut is confirmed, or left in place while the run's runner is not here. The record
// then still owes its pass back (folderOwedRecord), which the tail owner runs once those reads answer, or gives up
// past the keep, durably, before its own finalize stops the sandbox with no pass (tailFinalize). A killed run, a run
// that cannot be read and a sandbox that cannot be cut are stopped, as before. A cut that fails is audited as action.
func (s *Server) keepUnreadFolder(ctx context.Context, runID uuid.UUID, ref, action string, readErr error) bool {
	vctx, cancel := teardownCtx(ctx)
	defer cancel()
	run, err := s.cfg.Store.GetRun(vctx, runID)
	if err != nil || run.State == types.RunKilled {
		return false
	}
	run.SandboxRef = ref
	slog.WarnContext(ctx, "wardynd: an ended run's computer folder settings or data_out rule could not be read; its sandbox is kept only on a confirmed cut",
		slog.String("run_id", runID.String()), slog.Any("err", readErr))
	v, _, _ := s.folderKeepVerdict(vctx, run, action)
	return v != folderKeepNone
}
