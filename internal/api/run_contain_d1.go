// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
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

// errAgentStopFailed is what containCallPre wraps when a Kubernetes server-mode run's agent was not stopped:
// a plain failure of the visit, stamped and audited like a failed cut, after which d1Due stops the sandbox
// without waiting for the pass. It never wraps a runner's absence: server mode has no runner.
var errAgentStopFailed = errors.New("stopping the ended run's agent for its copy back failed")

// errAgentNotStoppable is containCallPre's answer for a Kubernetes server-mode run whose agent cannot be
// stopped from the server: no pass can run for it, so the stop does not wait for one.
var errAgentNotStoppable = fmt.Errorf("%w: the sandbox's agent cannot be stopped from the server", errAgentStopFailed)

// errPassInFlight is containCallPre's answer when it cannot take the run's lock, which is held while its
// copy back runs: this visit leaves the agent alone. It wraps errContainSkip, so it is a skipped visit, not
// a failed cut: nothing is stamped or audited for it, and d1Due treats it as a visit whose agent was held.
var errPassInFlight = fmt.Errorf("%w: a pass back is running for the run; the agent is left alone this visit", errContainSkip)

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
// own clock, since the latest of the run's due time, its last unobserved round and this process's start.
// A read that fails answers false, for the next round.
func (s *Server) d1StreakDue(ctx context.Context, run types.AgentRun) bool {
	streak, err := s.d1Streak(ctx, run)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: checking an ended run's failure streak failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return false
	}
	return streak
}

// d1Streak is d1StreakDue with its read error apart. A store that cannot answer holds no streak. The
// process start and the last unobserved round go to the store as lengths of time on this process's
// monotonic clock (streakAges), which the store places on the database's clock.
func (s *Server) d1Streak(ctx context.Context, run types.AgentRun) (bool, error) {
	st, ok := s.cfg.Store.(store.ContainmentStreaker)
	if !ok {
		return false, nil
	}
	tau := containTick
	if run.RunnerID != nil {
		tau = containStreakRunner
	}
	startedAgo, unobservedAgo := s.streakAges(run.ID)
	return st.ContainmentStreakDue(ctx, run.ID, tau, startedAgo, unobservedAgo)
}

// d1PastDue reports whether the database's now() is at least wait past run's containment due, with the
// read's error apart. A run that is not terminal is never past it, and a store that cannot answer has no
// run past it.
func (s *Server) d1PastDue(ctx context.Context, run types.AgentRun, wait time.Duration) (bool, error) {
	st, ok := s.cfg.Store.(store.ContainmentStreaker)
	if !ok {
		return false, nil
	}
	return st.ContainmentPastDue(ctx, run.ID, wait)
}

// d1PastDueRead is d1PastDue for d1Due: a read that fails answers false, logged, for the next round.
func (s *Server) d1PastDueRead(ctx context.Context, run types.AgentRun, wait time.Duration) bool {
	past, err := s.d1PastDue(ctx, run, wait)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: checking an ended run's containment due failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return false
	}
	return past
}

// d1UnsupportedDue is the rule for a run with no contain call: stop it, once its folder record has been
// read. A Kubernetes server-mode run with an owed copy back, whose agent was stopped on this visit or whose
// pass was running, waits for containServerKeepWait past its due first, on the database's clock: its pass
// may run in between. held says that wait applies.
func (s *Server) d1UnsupportedDue(ctx context.Context, run types.AgentRun, held bool) bool {
	_, owed, ok := s.d1FolderOwed(ctx, run)
	if !ok {
		return false
	}
	if !owed || !held {
		return true
	}
	return s.d1PastDueRead(ctx, run, containServerKeepWait)
}

// containStopOwed reports whether run's stop is owed: it is past its containment due, and either its
// substrate has no contain call and its stop is not being held back for its pass (a Kubernetes server-mode
// run with an owed copy back is held until its due plus containServerKeepWait), or a failure the database
// holds is a streak old. These are the reads d1Due makes, with no rule of their own, and every time they
// compare is the database's. A read that fails answers true, the due read included: the run is then
// stopped, never given another attempt at a copy back. A run whose stop is owed gets no new pass back;
// stopping it wins over another attempt.
func (s *Server) containStopOwed(ctx context.Context, run types.AgentRun) bool {
	past, err := s.d1PastDue(ctx, run, 0)
	if err != nil {
		return true
	}
	if !past {
		return false
	}
	_, owed, readable := s.d1FolderOwed(ctx, run)
	if !readable {
		return true
	}
	switch {
	case s.d1ServerMode(run):
		if !owed {
			return true
		}
		waited, err := s.d1PastDue(ctx, run, containServerKeepWait)
		return err != nil || waited
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
// abandoned at the end of its phase, goes on. The call is made under the stop item's claim, generation gen.
func (s *Server) d1LastCut(base context.Context, run types.AgentRun, gen uint64) bool {
	callCtx, cancel := context.WithTimeout(base, containCall)
	defer cancel()
	ans, _ := s.containCallWithin(callCtx, run, gen, func() containAnswer {
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
	if ctx.Err() != nil {
		slog.DebugContext(base, "wardynd: an ended run's lock was lost; nothing given up", slog.String("run_id", run.ID.String()))
		return false
	}
	final, err := s.giveUpFolder(ctx, run, true, st)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: giving up an ended run's folder copy back failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return false
	}
	return final
}

// errStopNotBegunLockLost is a stop withheld because the run lock was lost before StopSandbox began. It is
// no stop attempt: no confirmation, no unobserved mark, no failure stamp; the next visit tries again.
var errStopNotBegunLockLost = errors.New("the run lock was lost before the stop began")

// d1Stop is the StopSandbox phase, and the post phase after it. A stop still running at the end of its
// phase is abandoned and counts as failed; its claim is held until the call returns. A confirmed stop
// clears the run's recorded containment failure for that sandbox; the tail owner clears the ref. The call is
// made under the stop item's claim, generation gen, and only while lctx, the run lock's context, is live:
// the stop's own phase derives from lctx, so a lost lock ends it. The lock is checked twice, before the
// phase and inside the call just before StopSandbox, so a lock lost while the call waits to start begins no
// stop; a call abandoned on a lost lock before it began is withheld from beginning. Either is no stop
// attempt (errStopNotBegunLockLost). A stop already sent is not recalled. What follows a call that was made
// (the confirmation, the failure stamp) stays on base, under the same gen.
func (s *Server) d1Stop(base, lctx context.Context, run types.AgentRun, gen uint64) {
	if s.cfg.Runner == nil {
		return
	}
	budget := d1StopPhase
	if run.MachineKind.IsMacOSVM() {
		budget = d1StopPhaseMac
	}
	stopCtx, cancel := context.WithTimeout(lctx, budget)
	defer cancel()
	if lctx.Err() != nil {
		slog.DebugContext(base, "wardynd: an ended run's lock was lost; nothing stopped, the next visit tries again", slog.String("run_id", run.ID.String()))
		return
	}
	// begun is 0 until the call begins StopSandbox (1) or the item withholds it after abandoning it (2).
	var begun atomic.Int32
	notBegun := func() containAnswer {
		return containAnswer{err: fmt.Errorf("%w: %w", errStopNotBegunLockLost, context.Cause(lctx))}
	}
	ans, _ := s.containCallWithin(stopCtx, run, gen, func() containAnswer {
		if lctx.Err() != nil || !begun.CompareAndSwap(0, 1) {
			return notBegun()
		}
		return containAnswer{err: s.cfg.Runner.StopSandbox(stopCtx, run.SandboxRef)}
	})
	if errors.Is(ans.err, errContainCallAbandoned) && lctx.Err() != nil && begun.CompareAndSwap(0, 2) {
		ans = notBegun()
	}
	switch {
	case errors.Is(ans.err, errStopNotBegunLockLost):
		slog.DebugContext(base, "wardynd: an ended run's lock was lost before its stop began; nothing stopped, the next visit tries again", slog.String("run_id", run.ID.String()))
		return
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
