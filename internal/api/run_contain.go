// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The containment loop owns an ended run that still holds a sandbox: it cuts the agent off its networks,
// every round, until a cut succeeds and is recorded. It never stops a sandbox (the stop pool's item
// does) and never writes a run state.
const (
	// containTick is the loop's period (tau).
	containTick = 10 * time.Second
	// containWorkers is the number of cut workers (K).
	containWorkers = 4
	// containStopWorkers is the number of stop workers (K_s).
	containStopWorkers = 4
	// containPre, containCall and containPost are an item's three phases; containItem (T) is their sum.
	// The pre budget is held back for the work the stop path adds before a cut.
	containPre  = 5 * time.Second
	containCall = 20 * time.Second
	containPost = 5 * time.Second
	containItem = containPre + containCall + containPost
	// containPage is the rows a tick reads per page.
	containPage = 500
	// containQueueCap is the cut queue's room: more than two pages, so a full tick never blocks.
	containQueueCap = 4 * containPage
)

// containPool says which pool holds a run in the in-flight set.
type containPool int

const (
	containPoolCut containPool = iota
	containPoolStop
)

// errContainCallAbandoned is a cut call still running when its phase ended: the worker moved on.
var errContainCallAbandoned = errors.New("the cut did not answer within its phase")

// errContainCallPanicked is a cut call that panicked. It is not an observation of the sandbox.
var errContainCallPanicked = errors.New("the cut panicked")

// containAnswer is what the call phase produced: cutAgent's answer, and containPreCut's error.
type containAnswer struct {
	kind   cutKind
	err    error
	preErr error
}

// lateErr is the answer's error for the late rule: containPreCut's if any, else cutAgent's.
func (a containAnswer) lateErr() error {
	if a.preErr != nil {
		return a.preErr
	}
	return a.err
}

// containEntry is one run in the in-flight set: the pool that has it, whether an item holds it, and how
// many calls are still out for it.
type containEntry struct {
	pool  containPool
	held  bool
	calls int
}

// containFlight is one call run through containCallWithin. Its fields are guarded by containState.mu.
type containFlight struct {
	run       types.AgentRun
	counted   bool // the call is on its run's entry
	abandoned bool // the worker moved on without the answer
	done      bool // the call has returned
	ans       containAnswer
	dueKnown  bool // post has made d1Due's answer
	due       bool
}

// containState is the containment loop's process state. The zero value is ready to use.
type containState struct {
	once sync.Once
	mu   sync.Mutex

	inFlight   map[uuid.UUID]*containEntry // claimed runs, shared by the cut and stop pools
	unobserved map[uuid.UUID]time.Time     // run id -> when its sandbox was last not observed
	seen       map[uuid.UUID]struct{}      // ids listed since the cursor last wrapped
	started    time.Time                   // when this process started the loop
	cursor     *store.TerminalSandboxCursor

	cutQ       chan types.AgentRun
	stopFIFO   []types.AgentRun
	stopQueued map[uuid.UUID]bool
	stopWake   chan struct{}

	// Tests only: zero means the constants; d1DueFn stands in for d1Due.
	callPhase, postPhase time.Duration
	d1DueFn              func(ctx context.Context, run types.AgentRun, kind cutKind, err, preErr error) bool
}

func (c *containState) init() {
	c.once.Do(func() {
		c.inFlight = map[uuid.UUID]*containEntry{}
		c.unobserved = map[uuid.UUID]time.Time{}
		c.seen = map[uuid.UUID]struct{}{}
		c.stopQueued = map[uuid.UUID]bool{}
		c.cutQ = make(chan types.AgentRun, containQueueCap)
		c.stopWake = make(chan struct{}, 1)
	})
}

// startContainLoop starts the loop on every replica, with its K cut workers and K_s stop workers. All of
// them stop with the watcher ctx.
func (s *Server) startContainLoop() {
	ctx := s.watcherBaseCtx()
	if ctx == nil {
		return
	}
	c := &s.contain
	c.init()
	c.mu.Lock()
	c.started = s.cfg.Now()
	c.mu.Unlock()
	s.goBackground(func() { s.containLoop(ctx, containTick) })
	for range containWorkers {
		s.goBackground(func() { s.containCutWorker(ctx) })
	}
	for range containStopWorkers {
		s.goBackground(func() { s.containStopWorker(ctx) })
	}
}

// containLoop reads the listing every tick until ctx ends.
func (s *Server) containLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.containTickOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// containTickOnce reads the first page of the unfenced listing and one rolling page whose cursor walks
// on and wraps, and queues a cut for every due run in them. No running run is read. A listing that
// fails is logged and contributes nothing.
func (s *Server) containTickOnce(ctx context.Context) {
	st, ok := s.cfg.Store.(store.RunContainer)
	if !ok {
		return
	}
	c := &s.contain
	c.init()
	first, next, err := st.ListTerminalSandboxPage(ctx, nil, containPage, true)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: listing ended runs for containment failed", slog.Any("err", err))
		return
	}
	s.containVisit(first)
	c.mu.Lock()
	cur := c.cursor
	c.mu.Unlock()
	if cur != nil {
		more, after, err := st.ListTerminalSandboxPage(ctx, cur, containPage, true)
		if err != nil {
			slog.ErrorContext(ctx, "wardynd: listing ended runs for containment failed", slog.Any("err", err))
			return
		}
		s.containVisit(more)
		next = after
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursor = next
	if next != nil {
		return
	}
	// The cursor wrapped: an unobserved mark for a run not listed in the whole walk is dropped.
	for id := range c.unobserved {
		if _, listed := c.seen[id]; !listed {
			delete(c.unobserved, id)
		}
	}
	c.seen = map[uuid.UUID]struct{}{}
}

// containVisit queues a cut for each row that is due: skip a run already claimed, a runner run whose
// runner is not connected here (not counted, no call), and one not yet due.
func (s *Server) containVisit(rows []store.TerminalSandboxRow) {
	c := &s.contain
	now := s.cfg.Now()
	c.mu.Lock()
	for _, row := range rows {
		c.seen[row.ID] = struct{}{}
	}
	c.mu.Unlock()
	for _, row := range rows {
		if s.containBusy(row.ID) {
			continue
		}
		run := containRunOf(row)
		if run.RunnerID != nil {
			if _, connected := s.runnerConnected(*run.RunnerID); !connected {
				s.containNoteUnobserved(row.ID)
				continue
			}
		}
		if due, ok := containmentDue(run); !ok || due.After(now) {
			continue
		}
		if !s.containClaim(row.ID, containPoolCut) {
			continue
		}
		select {
		case c.cutQ <- run:
		default:
			s.containRelease(row.ID)
		}
	}
}

// containRunOf is the run the listing row stands for: what cutAgent and containmentDue read.
func containRunOf(row store.TerminalSandboxRow) types.AgentRun {
	return types.AgentRun{
		ID: row.ID, State: row.State, SandboxRef: row.SandboxRef, RunnerID: row.RunnerID, MachineKind: row.MachineKind,
		EndedAt: row.EndedAt, UpdatedAt: row.UpdatedAt, ContainmentErrorAt: row.ContainmentErrorAt,
	}
}

// containBusy reports a run claimed by either pool; neither pool dispatches it again.
func (s *Server) containBusy(id uuid.UUID) bool {
	c := &s.contain
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	_, busy := c.inFlight[id]
	return busy
}

// containClaim claims id for pool; false when it is already claimed.
func (s *Server) containClaim(id uuid.UUID, pool containPool) bool {
	c := &s.contain
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, busy := c.inFlight[id]; busy {
		return false
	}
	c.inFlight[id] = &containEntry{pool: pool, held: true}
	return true
}

// containRelease ends an item's hold on id. The entry goes once no item holds it and no call is out.
func (s *Server) containRelease(id uuid.UUID) {
	c := &s.contain
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.inFlight[id]; e != nil {
		e.held = false
		if e.calls == 0 {
			delete(c.inFlight, id)
		}
	}
}

// containHandOffLocked moves a cut run to the stop pool with no moment unclaimed: a run held by a cut
// item, or kept for that item's unanswered call, becomes the stop item's. False when it is neither.
func (c *containState) containHandOffLocked(id uuid.UUID) bool {
	e := c.inFlight[id]
	if e == nil || e.pool != containPoolCut || (!e.held && e.calls == 0) {
		return false
	}
	e.pool, e.held = containPoolStop, true
	return true
}

// containHandOff is the hand-off from the cut pool to the stop pool; true when this call made it, in
// which case the caller no longer holds the claim and must not release it.
func (s *Server) containHandOff(id uuid.UUID) bool {
	c := &s.contain
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.containHandOffLocked(id)
}

// enqueueStop hands the run to the stop pool and adds it to the stop FIFO, once. A cut item calls it while
// it still holds its claim, and releases only when it answered false.
func (s *Server) enqueueStop(run types.AgentRun) bool {
	if !s.containHandOff(run.ID) {
		return false
	}
	s.pushStop(run)
	return true
}

// pushStop adds run to the stop FIFO unless it is already in it, and wakes a stop worker.
func (s *Server) pushStop(run types.AgentRun) {
	c := &s.contain
	c.init()
	c.mu.Lock()
	if c.stopQueued[run.ID] {
		c.mu.Unlock()
		return
	}
	c.stopQueued[run.ID] = true
	c.stopFIFO = append(c.stopFIFO, run)
	c.mu.Unlock()
	select {
	case c.stopWake <- struct{}{}:
	default:
	}
}

// popStop takes the oldest run in the stop FIFO.
func (s *Server) popStop() (types.AgentRun, bool) {
	c := &s.contain
	c.mu.Lock()
	if len(c.stopFIFO) == 0 {
		c.mu.Unlock()
		return types.AgentRun{}, false
	}
	run := c.stopFIFO[0]
	c.stopFIFO = c.stopFIFO[1:]
	delete(c.stopQueued, run.ID)
	more := len(c.stopFIFO) > 0
	c.mu.Unlock()
	if more {
		select {
		case c.stopWake <- struct{}{}:
		default:
		}
	}
	return run, true
}

// containStopWorker takes the stop FIFO in order and runs the stop item for each run.
func (s *Server) containStopWorker(ctx context.Context) {
	c := &s.contain
	for ctx.Err() == nil {
		run, ok := s.popStop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-c.stopWake:
			}
			continue
		}
		s.containStopItem(ctx, run)
	}
}

// containStopItem runs one stop item. The claim is released when it ends, a panic included.
func (s *Server) containStopItem(ctx context.Context, run types.AgentRun) {
	defer s.containRelease(run.ID)
	defer containRecover(run.ID)
	s.runStopItem(ctx, run)
}

// containRecover logs a panic in an item so it never leaves a run claimed until a restart.
func containRecover(id uuid.UUID) {
	if r := recover(); r != nil {
		slog.Error("wardynd: a containment item panicked", slog.String("run_id", id.String()), slog.Any("panic", r))
	}
}

// containCutWorker takes cut items off the queue until ctx ends.
func (s *Server) containCutWorker(ctx context.Context) {
	q := s.contain.cutQ
	for {
		select {
		case <-ctx.Done():
			return
		case run := <-q:
			s.containCutItem(ctx, run)
		}
	}
}

// containPhases are the call and post phase lengths: the constants, unless a test set them.
func (s *Server) containPhases() (call, post time.Duration) {
	call, post = containCall, containPost
	if s.contain.callPhase > 0 {
		call = s.contain.callPhase
	}
	if s.contain.postPhase > 0 {
		post = s.contain.postPhase
	}
	return call, post
}

// containCutItem cuts one ended run's agent. The call and the writes never share a deadline: a hung cut
// must still stamp its failure, or D1 never sees a streak. Each phase runs on its own ctx made from the
// parent without its cancel, so a shutdown does not cut a phase short.
func (s *Server) containCutItem(ctx context.Context, run types.AgentRun) {
	handed := false
	defer func() {
		if !handed {
			s.containRelease(run.ID)
		}
	}()
	defer containRecover(run.ID)
	callLen, postLen := s.containPhases()
	base := context.WithoutCancel(ctx)
	callCtx, cancelCall := context.WithTimeout(base, callLen)
	defer cancelCall()
	ans, flight := s.containCallWithin(callCtx, run, func() containAnswer {
		preErr := s.containPreCut(callCtx, run)
		if err := callCtx.Err(); err != nil {
			return containAnswer{preErr: preErr, err: fmt.Errorf("the call phase ended before the cut: %w", err)}
		}
		kind, err := s.cutAgent(callCtx, run)
		return containAnswer{kind: kind, err: err, preErr: preErr}
	})
	if errors.Is(ans.err, errContainCallPanicked) {
		return
	}
	postCtx, cancelPost := context.WithTimeout(base, postLen)
	defer cancelPost()
	s.containOutcome(postCtx, run, ans)
	due := s.containD1Due(postCtx, run, ans)
	switch {
	case flight != nil:
		handed = s.containDueKnown(flight, due)
	case due:
		handed = s.enqueueStop(run)
	}
}

// containD1Due asks d1Due, through the test stand-in when one is set.
func (s *Server) containD1Due(ctx context.Context, run types.AgentRun, ans containAnswer) bool {
	if f := s.contain.d1DueFn; f != nil {
		return f(ctx, run, ans.kind, ans.err, ans.preErr)
	}
	return s.d1Due(ctx, run, ans.kind, ans.err, ans.preErr)
}

// containCallWithin runs the call phase's work on its own goroutine and waits for its answer or for
// ctx's end, whichever is first. A runner call can outlive its ctx (it writes under the link's ctx), so
// the worker never waits past its phase: a call still running at the phase's end is abandoned, and the
// item's answer is errContainCallAbandoned. The run stays claimed until the call returns; its answer then
// goes through the late rule (containCallDone). The returned flight is non-nil only for an abandoned call.
// An abandoned call still counts: its stop waits for the answer, never the worker.
func (s *Server) containCallWithin(ctx context.Context, run types.AgentRun, work func() containAnswer) (containAnswer, *containFlight) {
	c := &s.contain
	c.init()
	f := &containFlight{run: run}
	c.mu.Lock()
	if e := c.inFlight[run.ID]; e != nil {
		e.calls++
		f.counted = true
	}
	c.mu.Unlock()
	answer := make(chan containAnswer, 1)
	s.goBackground(func() {
		var ans containAnswer
		defer func() {
			answer <- ans
			s.containCallDone(f, ans)
		}()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("wardynd: a containment cut panicked", slog.String("run_id", run.ID.String()), slog.Any("panic", r))
				ans = containAnswer{err: fmt.Errorf("%w: %v", errContainCallPanicked, r)}
			}
		}()
		ans = work()
	})
	select {
	case ans := <-answer:
		return ans, nil
	case <-ctx.Done():
	}
	c.mu.Lock()
	if f.done {
		c.mu.Unlock()
		return <-answer, nil
	}
	f.abandoned = true
	c.mu.Unlock()
	return containAnswer{err: fmt.Errorf("%w: %w", errContainCallAbandoned, ctx.Err())}, f
}

// containCallDone is a call's return. It takes the call off its run's entry. For an abandoned call it
// also records an unobserved answer and applies the late rule, if post has already made d1Due's answer.
func (s *Server) containCallDone(f *containFlight, ans containAnswer) {
	c := &s.contain
	c.mu.Lock()
	f.done, f.ans = true, ans
	push := false
	if f.abandoned {
		if notObserved(ans.lateErr()) {
			c.unobserved[f.run.ID] = s.cfg.Now()
		}
		push = c.containLateStopLocked(f)
	}
	if f.counted {
		if e := c.inFlight[f.run.ID]; e != nil {
			e.calls--
			if e.calls == 0 && !e.held {
				delete(c.inFlight, f.run.ID)
			}
		}
	}
	c.mu.Unlock()
	if push {
		s.pushStop(f.run)
	}
}

// containDueKnown records post's d1Due answer for an abandoned call, and applies the late rule if the call
// has already returned. True when the run went to the stop pool, which the item then does not release.
func (s *Server) containDueKnown(f *containFlight, due bool) bool {
	c := &s.contain
	c.mu.Lock()
	f.dueKnown, f.due = true, due
	push := c.containLateStopLocked(f)
	c.mu.Unlock()
	if push {
		s.pushStop(f.run)
	}
	return push
}

// containLateStopLocked is the rule for an abandoned call, applied by whichever of the call's return and
// post's d1Due answer comes second. A stop that d1Due called for is dropped only if the answer shows the
// cut held (the next round records it), is unobserved, or was a panic; any other answer, an error or
// cutUnsupported with nil, hands the run to the stop pool.
func (c *containState) containLateStopLocked(f *containFlight) bool {
	if !f.done || !f.dueKnown || !f.due {
		return false
	}
	err := f.ans.lateErr()
	switch {
	case errors.Is(err, errContainCallPanicked), notObserved(err):
		return false
	case err == nil && (f.ans.kind == cutFenced || f.ans.kind == cutProxyStopped):
		return false
	}
	return c.containHandOffLocked(f.run.ID)
}

// containOutcome records the call's outcome on ctx, the post phase's: a cut that held clears the run's
// failure (and fences it, when it was a fence), an unobserved answer only marks the round, any other
// error stamps a failure. It checks ctx before each write, and a failed write is not an observation.
func (s *Server) containOutcome(ctx context.Context, run types.AgentRun, ans containAnswer) {
	st, ok := s.cfg.Store.(store.RunContainer)
	if !ok {
		return
	}
	switch {
	case ans.err == nil && ans.kind == cutUnsupported:
	case ans.err == nil:
		s.containHeld(ctx, st, run, ans.kind)
	case notObserved(ans.err):
		s.containNoteUnobserved(run.ID)
	default:
		s.containFailed(ctx, st, run, ans.err)
	}
}

// containHeld is a cut that held. A fence is recorded (fenced_at), and the run's recorded containment
// failure is cleared with one success row when it cleared one. A stop of the proxy alone records nothing
// beyond that, so the next round cuts again.
func (s *Server) containHeld(ctx context.Context, st store.RunContainer, run types.AgentRun, kind cutKind) {
	c := &s.contain
	c.mu.Lock()
	delete(c.unobserved, run.ID)
	c.mu.Unlock()
	if kind == cutFenced && ctx.Err() == nil {
		if err := st.MarkRunFenced(ctx, run.ID); err != nil {
			slog.WarnContext(ctx, "wardynd: recording a fenced run failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
	}
	if ctx.Err() != nil {
		return
	}
	cleared, err := st.ClearRunContainmentError(ctx, run.ID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: clearing an ended run's resolved containment failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if cleared {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.containment.reassert", run.ID.String(), "success",
			mustJSON(map[string]any{"reason": "ended", "containment": "resolved"})))
	}
}

// containFailed stamps a cut failure on an ended run. Only the call that starts the streak writes the
// failure row.
func (s *Server) containFailed(ctx context.Context, st store.RunContainer, run types.AgentRun, cause error) {
	if ctx.Err() != nil {
		return
	}
	started, err := st.SetRunContainmentErrorSince(ctx, run.ID, cause.Error(), s.cfg.Now(), s.streakSince(run))
	if err != nil {
		slog.WarnContext(ctx, "wardynd: recording an ended run's containment failure failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if started {
		slog.WarnContext(ctx, "wardynd: cutting an ended run's agent failed", slog.String("run_id", run.ID.String()), slog.Any("err", cause))
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.containment.reassert", run.ID.String(), "failure",
			mustJSON(map[string]any{"reason": "ended", "containment": "unresolved", "error": cause.Error()})))
	}
}

// containNoteUnobserved marks that this round did not observe the run's sandbox. A failure stamped before
// the mark never counts toward a streak. Nothing is written to the database: a replica that never sees
// the runner would otherwise reset the streak of the one that does.
func (s *Server) containNoteUnobserved(id uuid.UUID) {
	c := &s.contain
	c.init()
	now := s.cfg.Now()
	c.mu.Lock()
	c.unobserved[id] = now
	c.mu.Unlock()
}

// streakSince is the earliest a failure stamp may be and still count toward a streak for run: the latest of
// the run's containment due time, the last round that did not observe it, and when this process started.
func (s *Server) streakSince(run types.AgentRun) time.Time {
	c := &s.contain
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	since := c.started
	if due, ok := containmentDue(run); ok && due.After(since) {
		since = due
	}
	if t := c.unobserved[run.ID]; t.After(since) {
		since = t
	}
	return since
}

// d1Due reports whether the cut item should hand run to the stop pool, in its post phase. It is true only
// once the run is past its containment due, and then in two cases. The run's substrate has no contain call
// (cutUnsupported with no error: a virtual machine, Kubernetes in server mode), or the cut failed and the
// database holds a failure at least a streak older than now that is not older than the run's due, its last
// unobserved round or this process's start. An answer that did not observe the sandbox is never a failure.
// A Kubernetes server-mode run with an owed copy back, whose agent was stopped on this visit or whose pass
// was running, waits for containServerKeepWait past its due; a stop of its agent that failed makes it due at
// once. A folder read that fails makes it false for the tick. It reads the clock when it is called, not
// when the item began.
func (s *Server) d1Due(ctx context.Context, run types.AgentRun, kind cutKind, err, preErr error) bool {
	due, ok := containmentDue(run)
	if !ok || s.cfg.Now().Before(due) {
		return false
	}
	failed := err != nil && !notObserved(err)
	serverMode := s.d1ServerMode(run)
	skipped := errors.Is(preErr, errPassInFlight)
	// A server-mode run has no cut to fail: a pre-cut that failed or ran out its phase fails closed. A visit
	// skipped for a running pass is neither.
	stalled := serverMode && ((preErr != nil && !skipped) || errors.Is(err, errContainCallAbandoned) || errors.Is(err, context.DeadlineExceeded))
	switch {
	case stalled, err == nil && kind == cutUnsupported:
		return s.d1UnsupportedDue(ctx, run, due, serverMode && (preErr == nil || skipped) && err == nil)
	case failed && s.d1StreakDue(ctx, run):
		_, _, readable := s.d1FolderOwed(ctx, run)
		return readable
	}
	return false
}

// containPreCut is the stop path's work in the call phase, before the cut. For a Kubernetes server-mode run
// with an owed copy back it stops the agent on every visit, with no record of having done so, so that the
// pass has a stopped agent to copy from; it answers that stop's error. A visit finds the run's lock held
// while its pass runs, and then leaves the agent alone and answers errPassInFlight; the lock is held across
// the stop, so no pass starts between the check and the kill. Every other run has no work here.
func (s *Server) containPreCut(ctx context.Context, run types.AgentRun) error {
	if !s.d1ServerMode(run) {
		return nil
	}
	if _, owed, ok := s.d1FolderOwed(ctx, run); !ok || !owed {
		return nil
	}
	lctx, unlock, locked, _ := s.tryLockRunOpHeld(ctx, run.ID)
	if !locked {
		return errPassInFlight
	}
	defer unlock()
	if !s.agentStoppable(lctx, run) {
		slog.WarnContext(ctx, "wardynd: an ended run's agent cannot be stopped from the server; its sandbox is stopped without waiting",
			slog.String("run_id", run.ID.String()), slog.Any("reason", errAgentNotStoppable))
		return errAgentNotStoppable
	}
	err := s.stopAgentForPass(lctx, run.SandboxRef)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: stopping an ended run's agent for its copy back failed; its sandbox is stopped without waiting",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
	return err
}

// runStopItem is the stop pool's item for one run. A run with a contain call is cut once more first. Each
// phase runs on a context of its own, so a phase that overruns takes nothing from the next: the item ends
// within containD1 (containD1Unsupported with no last cut). Nothing is given up or stopped while a pass
// back holds the run's lock: the lock is taken after the last cut and held across the give-up and the stop,
// and a lock that is held ends the item with nothing written, to be tried again on the next visit.
func (s *Server) runStopItem(ctx context.Context, run types.AgentRun) {
	base := context.WithoutCancel(ctx)
	run, ok := s.d1Reread(base, run)
	if !ok || (!s.d1NoCall(run) && !s.d1LastCut(base, run)) {
		return
	}
	_, unlock, locked, _ := s.tryLockRunOpHeld(base, run.ID)
	if !locked {
		slog.DebugContext(base, "wardynd: an ended run's lock is held; its stop waits for the next visit", slog.String("run_id", run.ID.String()))
		return
	}
	defer unlock()
	if !s.d1GiveUp(base, run) {
		return
	}
	s.d1Stop(base, run)
}
