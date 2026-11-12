// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The stop path of the containment loop. A cut item hands a run to the stop pool in two cases, and only
// once the run is past its containment due: the substrate has no contain call (a virtual machine,
// Kubernetes in server mode), or a second observed cut failure came at least a streak apart. The stop
// pool's item then gives up the owed copy back and stops the sandbox.
const (
	// d1GiveUpPhase is the stop item's give-up phase; d1StopPhase its StopSandbox phase, and d1StopPhaseMac
	// the same for a macOS VM, whose guest shutdown takes longer.
	d1GiveUpPhase  = 5 * time.Second
	d1StopPhase    = 60 * time.Second
	d1StopPhaseMac = macOSVMTeardownBudget
	// containD1 is the stop item that follows a failed cut: pre, a last cut, give-up, StopSandbox, post.
	containD1 = containPre + containCall + d1GiveUpPhase + d1StopPhase + containPost
	// containD1Unsupported is the stop item for a run with no contain call: the last cut is left out.
	containD1Unsupported = containD1 - containCall
	// containServerKeepWait is how long past its containment due a Kubernetes server-mode run with an owed copy
	// back is left to its pass before it is stopped. Its agent is stopped on every visit meanwhile.
	containServerKeepWait = 5 * time.Minute
	// containStreakRunner is the gap two failures of a run on a runner need. A dead link is failed by the
	// keepalive up to (MissedPings+1) pings after the last pong, and a cut is not an observation from then
	// on, so two failures closer than that may both be the dead link's.
	containStreakRunner = max(containTick, (runnerwire.MissedPings+1)*runnerwire.PingInterval)
)

// errAgentNotStoppable is containPreCut's answer for a Kubernetes server-mode run whose agent cannot be
// stopped from the server: no pass can run for it, so the stop does not wait for one.
var errAgentNotStoppable = errors.New("the sandbox's agent cannot be stopped from the server")

// errPassInFlight is containPreCut's answer when the run's lock is held, which is when its copy back is
// running: this visit leaves the agent alone. It is a skipped visit, not a failed cut: d1Due reads it from
// the pre-cut slot and it is never an error of the cut itself, so nothing is stamped for it.
var errPassInFlight = errors.New("a pass back is running for the run; the agent is left alone this visit")

// d1ServerMode reports a hosted Kubernetes run in server mode: it has no contain call.
func (s *Server) d1ServerMode(run types.AgentRun) bool {
	return run.RunnerID == nil && !run.MachineKind.IsVM() && run.SandboxRef != "" &&
		s.cfg.Runner != nil && s.cfg.Runner.Name() == "k8s"
}

// d1NoCall reports a run the stop pool's item does not cut again first: its substrate has no contain call.
func (s *Server) d1NoCall(run types.AgentRun) bool {
	var caps runnerwire.Caps
	if run.RunnerID != nil {
		caps, _ = s.runnerLastCaps(*run.RunnerID)
	}
	call, _ := s.containDispatch(run, caps)
	return !call
}

// d1FolderOwed reads run's folder record and answers whether a copy back is owed. ok is false when a read
// failed: the caller leaves the run for the next round, and a read error never stops a sandbox or gives a
// record up. A store with no folder record owes nothing.
func (s *Server) d1FolderOwed(ctx context.Context, run types.AgentRun) (st types.RunLocalFolderStatus, owed, ok bool) {
	ss, has := s.cfg.Store.(store.RunLocalFolderStatuses)
	if !has {
		return st, false, true
	}
	st, err := ss.RunLocalFolderStatus(ctx, run.ID)
	if err == nil {
		owed, err = s.folderOwed(ctx, run, st)
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading an ended run's folder record for its stop failed; the run is left for the next round",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return st, false, false
	}
	return st, owed, true
}

// d1StreakDue reports a second failure at least a streak after the first, compared by the database on its
// own clock, since the later of the run's due time, its last unobserved round and this process's start.
// A read that fails answers false, for the next round.
func (s *Server) d1StreakDue(ctx context.Context, run types.AgentRun) bool {
	streak, err := s.d1Streak(ctx, run)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: checking an ended run's failure streak failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return false
	}
	return streak
}

// d1Streak is d1StreakDue with its read error apart. A store that cannot answer holds no streak.
func (s *Server) d1Streak(ctx context.Context, run types.AgentRun) (bool, error) {
	st, ok := s.cfg.Store.(store.ContainmentStreaker)
	if !ok {
		return false, nil
	}
	tau := containTick
	if run.RunnerID != nil {
		tau = containStreakRunner
	}
	return st.ContainmentStreakDue(ctx, run.ID, tau, s.streakSince(run))
}

// d1UnsupportedDue is the rule for a run with no contain call: stop it, once its folder record has been
// read. A Kubernetes server-mode run with an owed copy back, whose agent was stopped on this visit, waits
// for containServerKeepWait past its due first: its pass may run in between. held says that wait applies.
func (s *Server) d1UnsupportedDue(ctx context.Context, run types.AgentRun, due time.Time, held bool) bool {
	_, owed, ok := s.d1FolderOwed(ctx, run)
	if !ok {
		return false
	}
	return !(owed && held && s.cfg.Now().Before(due.Add(containServerKeepWait)))
}

// containStopOwed reports whether run's stop is owed: it is past its containment due, and either its
// substrate has no contain call and its stop is not being held back for its pass (a Kubernetes server-mode
// run with an owed copy back is held until its due plus containServerKeepWait), or a failure the database
// holds is a streak old. These are the reads d1Due makes, with no rule of their own. A read that fails
// answers true: the run is then stopped, never given another attempt at a copy back. A run whose stop is
// owed gets no new pass back; stopping it wins over another attempt.
func (s *Server) containStopOwed(ctx context.Context, run types.AgentRun) bool {
	due, ok := containmentDue(run)
	if !ok || s.cfg.Now().Before(due) {
		return false
	}
	_, owed, readable := s.d1FolderOwed(ctx, run)
	if !readable {
		return true
	}
	switch {
	case s.d1ServerMode(run):
		return !owed || !s.cfg.Now().Before(due.Add(containServerKeepWait))
	case s.d1NoCall(run):
		return true
	}
	streak, err := s.d1Streak(ctx, run)
	return err != nil || streak
}

// d1Reread is the stop item's pre phase: the run as it stands now. An item for a run that is no longer
// terminal, or has lost its sandbox ref, ends.
func (s *Server) d1Reread(base context.Context, run types.AgentRun) (types.AgentRun, bool) {
	ctx, cancel := context.WithTimeout(base, containPre)
	defer cancel()
	cur, err := s.cfg.Store.GetRun(ctx, run.ID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: re-reading an ended run for its stop failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return run, false
	}
	if !isTerminalRunState(cur.State) || cur.SandboxRef == "" {
		return run, false
	}
	return cur, true
}

// d1LastCut is the continuation's last cut, in its own phase. It answers whether the item goes on to the
// give-up and the stop. A cut that held clears the failure and ends the item; an answer that did not
// observe the sandbox ends it too, and restarts the streak; a panic ends it. A cut that failed, or was
// abandoned at the end of its phase, goes on.
func (s *Server) d1LastCut(base context.Context, run types.AgentRun) bool {
	callCtx, cancel := context.WithTimeout(base, containCall)
	defer cancel()
	ans, _ := s.containCallWithin(callCtx, run, func() containAnswer {
		kind, err := s.cutAgent(callCtx, run)
		return containAnswer{kind: kind, err: err}
	})
	switch {
	case errors.Is(ans.err, errContainCallPanicked):
		return false
	case notObserved(ans.err):
		s.containNoteUnobserved(run.ID)
		return false
	case ans.err == nil && ans.kind != cutUnsupported:
		s.d1CutHeld(base, run, ans.kind)
		return false
	}
	return true
}

// d1CutHeld records a last cut that held, on a post phase of its own.
func (s *Server) d1CutHeld(base context.Context, run types.AgentRun, kind cutKind) {
	st, ok := s.cfg.Store.(store.RunContainer)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(base, containPost)
	defer cancel()
	s.containHeld(ctx, st, run, kind)
}

// d1GiveUp is the give-up phase: when a copy back is owed, give it up. It answers whether the sandbox may
// be stopped, which is only when the record is final. A statement that applied nothing is final only when
// a re-read shows the record done; a read error is not final.
func (s *Server) d1GiveUp(base context.Context, run types.AgentRun) bool {
	ctx, cancel := context.WithTimeout(base, d1GiveUpPhase)
	defer cancel()
	st, owed, ok := s.d1FolderOwed(ctx, run)
	if !ok {
		return false
	}
	if !owed {
		return true
	}
	final, err := s.giveUpFolder(ctx, run, true, st)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: giving up an ended run's folder copy back failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return false
	}
	return final
}

// d1Stop is the StopSandbox phase, and the post phase after it. A stop still running at the end of its
// phase is abandoned and counts as failed; its claim is held until the call returns. A confirmed stop
// clears the run's recorded containment failure for that sandbox; the tail owner clears the ref.
func (s *Server) d1Stop(base context.Context, run types.AgentRun) {
	if s.cfg.Runner == nil {
		return
	}
	budget := d1StopPhase
	if run.MachineKind.IsMacOSVM() {
		budget = d1StopPhaseMac
	}
	stopCtx, cancel := context.WithTimeout(base, budget)
	defer cancel()
	ans, _ := s.containCallWithin(stopCtx, run, func() containAnswer {
		return containAnswer{err: s.cfg.Runner.StopSandbox(stopCtx, run.SandboxRef)}
	})
	switch {
	case errors.Is(ans.err, errContainCallPanicked):
		return
	case ans.err == nil:
		s.d1StopConfirmed(base, run)
		return
	case notObserved(ans.err):
		s.containNoteUnobserved(run.ID)
		return
	}
	st, ok := s.cfg.Store.(store.RunContainer)
	if !ok {
		return
	}
	postCtx, cancelPost := context.WithTimeout(base, containPost)
	defer cancelPost()
	s.containFailed(postCtx, st, run, ans.err)
}

// d1StopConfirmed is a StopSandbox that answered nil: the containment failure the run recorded is resolved.
// It is cleared only while the run still holds the sandbox that was stopped (matched on its ref), so a stop
// that comes late cannot clear the failure recorded for a newer sandbox of the same run.
func (s *Server) d1StopConfirmed(base context.Context, run types.AgentRun) {
	st, ok := s.cfg.Store.(store.ContainmentClearer)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(base, containPost)
	defer cancel()
	cleared, err := st.ClearRunContainmentErrorForRef(ctx, run.ID, run.SandboxRef)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: clearing an ended run's containment failure after its confirmed stop failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if cleared {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.containment.reassert", run.ID.String(), "success",
			mustJSON(map[string]any{"reason": "ended", "containment": "resolved", "by": "stop"})))
	}
}
