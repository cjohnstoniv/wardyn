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

// errContainSkip is what the stop path's call-phase work (containCallPre) wraps to say it skipped: another
// holder has the run's agent, say. A skip is not a failure: no stamp and no failure row, no mark that the
// round was unobserved. The visit still asks d1Due with it, and releases the claim as usual. The one
// exception is a skip that arrives after its phase ended: the item's answer is then the phase's abandonment,
// which is stamped and audited as a failure, and the late rule drops the stop the skip would have owed.
var errContainSkip = errors.New("the stop path skipped this visit")

// errContainCallPanicked is a cut call that panicked. It is not an observation of the sandbox.
var errContainCallPanicked = errors.New("the cut panicked")

// containAnswer is what the call phase produced: cutAgent's answer.
type containAnswer struct {
	kind cutKind
	err  error
}

// containEntry is one run in the in-flight set: the pool that has it, whether an item holds it, and how
// many calls are still out for it.
type containEntry struct {
	gen   uint64 // the claim's generation: every claim of a run, even one right after another, has its own
	pool  containPool
	held  bool
	calls int
}

// containJob is a cut item's work: the run and the claim it holds.
type containJob struct {
	run types.AgentRun
	gen uint64
}

// containFlight is one call run through containCallWithin. Its fields are guarded by containState.mu.
type containFlight struct {
	run       types.AgentRun
	gen       uint64 // the generation of the claim the call was made under
	counted   bool   // the call is on its run's entry
	abandoned bool   // the worker moved on without the answer
	done      bool   // the call has returned
	ans       containAnswer
	dueKnown  bool // post has made d1Due's answer
	due       bool
}

// containState is the containment loop's process state. The zero value is ready to use.
type containState struct {
	once sync.Once
	mu   sync.Mutex

	inFlight   map[uuid.UUID]*containEntry // claimed runs, shared by the cut and stop pools
	lastGen    uint64                      // the generation of the latest claim
	unobserved map[uuid.UUID]time.Time     // run id -> when its sandbox was last not observed (time.Now, monotonic)
	seen       map[uuid.UUID]struct{}      // ids listed since the cursor last wrapped
	started    time.Time                   // when this process started the loop (time.Now, monotonic)
	cursor     *store.TerminalSandboxCursor

	cutQ       chan containJob
	stopFIFO   []containJob // each run with the generation of the claim it was handed off under
	stopQueued map[uuid.UUID]bool
	stopWake   chan struct{}

	// Tests only: zero means the constants; d1DueFn stands in for d1Due.
	prePhase, callPhase, postPhase time.Duration
	preCutFn                       func(ctx context.Context, run types.AgentRun) error // stands in for containPreCut
	containCallFn                  func(ctx context.Context, run types.AgentRun) error // stands in for containCallPre
	d1DueFn                        func(ctx context.Context, run types.AgentRun, kind cutKind, err error) bool
	callStartFn                    func() // runs on a call's goroutine just before its work; nil in production
}

func (c *containState) init() {
	c.once.Do(func() {
		c.inFlight = map[uuid.UUID]*containEntry{}
		c.unobserved = map[uuid.UUID]time.Time{}
		c.seen = map[uuid.UUID]struct{}{}
		c.stopQueued = map[uuid.UUID]bool{}
		c.cutQ = make(chan containJob, containQueueCap)
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
	if _, ok := s.cfg.Store.(store.RunContainer); !ok {
		slog.Error("wardynd: the containment loop is not running: the store cannot list ended runs")
		return
	}
	c := &s.contain
	c.init()
	c.mu.Lock()
	c.started = time.Now()
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
		if !row.Due { // the database decided it: the server's clock never gates a visit
			continue
		}
		gen, ok := s.containClaim(row.ID, containPoolCut)
		if !ok {
			continue
		}
		select {
		case c.cutQ <- containJob{run: run, gen: gen}:
		default:
			s.containRelease(row.ID, containPoolCut, gen)
		}
	}
}

// containRunOf is the run the listing row stands for: what cutAgent reads, with the listing's Due column deciding whether it is visited.
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

// containClaim claims id for pool and answers the claim's generation, which its holder releases with;
// false when the run is already claimed.
func (s *Server) containClaim(id uuid.UUID, pool containPool) (uint64, bool) {
	c := &s.contain
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, busy := c.inFlight[id]; busy {
		return 0, false
	}
	c.lastGen++
	c.inFlight[id] = &containEntry{gen: c.lastGen, pool: pool, held: true}
	return c.lastGen, true
}

// containGenOf is the generation of the claim now on id, 0 when there is none.
func (s *Server) containGenOf(id uuid.UUID) uint64 {
	c := &s.contain
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.inFlight[id]; e != nil {
		return e.gen
	}
	return 0
}

// containRelease ends a hold on id. It acts only on the claim of generation gen, and only while pool still
// owns it: a cut item whose run was handed to the stop pool meanwhile (by the answer of its abandoned call)
// must not drop the stop pool's claim, and one whose claim has since ended must not drop a newer claim of the
// same run. The entry goes once no item holds it and no call is out. A release that does not act is logged
// with both generations.
func (s *Server) containRelease(id uuid.UUID, pool containPool, gen uint64) {
	c := &s.contain
	c.init()
	c.mu.Lock()
	e := c.inFlight[id]
	if e == nil || e.gen != gen || e.pool != pool {
		var claimGen uint64
		if e != nil {
			claimGen = e.gen
		}
		poolMatches := e != nil && e.pool == pool
		movedToStop := e != nil && e.gen == gen && e.pool == containPoolStop
		c.mu.Unlock()
		// The same claim, now held by the stop pool, is the designed outcome of a hand-off, not a fault.
		// A missing claim or another generation is a release that lost its claim.
		level := slog.LevelWarn
		if movedToStop {
			level = slog.LevelDebug
		}
		slog.Log(context.Background(), level, "wardynd: a containment release did not act: its claim is not the run's now",
			slog.String("run_id", id.String()), slog.Uint64("release_gen", gen), slog.Uint64("claim_gen", claimGen), slog.Bool("pool_matches", poolMatches))
		return
	}
	e.held = false
	if e.calls == 0 {
		delete(c.inFlight, id)
	}
	c.mu.Unlock()
}

// containHandOffLocked moves a cut run to the stop pool with no moment unclaimed: a run held by a cut
// item, or kept for that item's unanswered call, becomes the stop item's. False when it is neither.
func (c *containState) containHandOffLocked(id uuid.UUID, gen uint64) bool {
	e := c.inFlight[id]
	if e == nil || e.gen != gen || e.pool != containPoolCut || (!e.held && e.calls == 0) {
		return false
	}
	e.pool, e.held = containPoolStop, true
	return true
}

// containHandOff is the hand-off from the cut pool to the stop pool; true when this call made it, in
// which case the caller no longer holds the claim and must not release it.
func (s *Server) containHandOff(id uuid.UUID, gen uint64) bool {
	c := &s.contain
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.containHandOffLocked(id, gen)
}

// enqueueStop hands the run to the stop pool and adds it to the stop FIFO, once. A cut item calls it while
// it still holds its claim, and releases only when it answered false.
func (s *Server) enqueueStop(run types.AgentRun, gen uint64) bool {
	if !s.containHandOff(run.ID, gen) {
		return false
	}
	s.pushStop(run, gen)
	return true
}

// pushStop adds run to the stop FIFO unless it is already in it, and wakes a stop worker. gen is the
// generation of the claim the stop pool now holds for run; the stop item releases that claim and no other.
func (s *Server) pushStop(run types.AgentRun, gen uint64) {
	c := &s.contain
	c.init()
	c.mu.Lock()
	if c.stopQueued[run.ID] {
		c.mu.Unlock()
		return
	}
	c.stopQueued[run.ID] = true
	c.stopFIFO = append(c.stopFIFO, containJob{run: run, gen: gen})
	c.mu.Unlock()
	select {
	case c.stopWake <- struct{}{}:
	default:
	}
}

// popStop takes the oldest run in the stop FIFO, with the generation of its claim.
func (s *Server) popStop() (containJob, bool) {
	c := &s.contain
	c.mu.Lock()
	if len(c.stopFIFO) == 0 {
		c.mu.Unlock()
		return containJob{}, false
	}
	job := c.stopFIFO[0]
	c.stopFIFO = c.stopFIFO[1:]
	delete(c.stopQueued, job.run.ID)
	more := len(c.stopFIFO) > 0
	c.mu.Unlock()
	if more {
		select {
		case c.stopWake <- struct{}{}:
		default:
		}
	}
	return job, true
}

// containStopWorker takes the stop FIFO in order and runs the stop item for each run.
func (s *Server) containStopWorker(ctx context.Context) {
	c := &s.contain
	for ctx.Err() == nil {
		job, ok := s.popStop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-c.stopWake:
			}
			continue
		}
		s.containStopItem(ctx, job.run, job.gen)
	}
}

// containStopItem runs one stop item. The claim is released when it ends, a panic included.
func (s *Server) containStopItem(ctx context.Context, run types.AgentRun, gen uint64) {
	defer s.containRelease(run.ID, containPoolStop, gen)
	defer containRecover(run.ID)
	s.runStopItem(ctx, run, gen)
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
		case job := <-q:
			s.containCutItem(ctx, job.run, job.gen)
		}
	}
}

// containPhases are the pre, call and post phase lengths: the constants, unless a test set them.
func (s *Server) containPhases() (pre, call, post time.Duration) {
	pre, call, post = containPre, containCall, containPost
	if s.contain.prePhase > 0 {
		pre = s.contain.prePhase
	}
	if s.contain.callPhase > 0 {
		call = s.contain.callPhase
	}
	if s.contain.postPhase > 0 {
		post = s.contain.postPhase
	}
	return pre, call, post
}

// containCutItem cuts one ended run's agent in three phases: the stop path's reads (pre), the call (the
// stop path's acts, then the cut) and the writes (post). The call and the writes never share a deadline: a hung cut must still stamp its
// failure, or the stop path never sees a streak. Each phase runs on its own ctx, made when the phase starts
// from the parent without its cancel, so a shutdown does not cut a phase short. A pre phase that fails or
// outlasts its budget did not try the cut, so it records no failure and asks nothing of the stop path: the
// claim is released and the next round takes the run again.
func (s *Server) containCutItem(ctx context.Context, run types.AgentRun, gen uint64) {
	handed := false
	defer func() {
		if !handed {
			s.containRelease(run.ID, containPoolCut, gen)
		}
	}()
	defer containRecover(run.ID)
	preLen, callLen, postLen := s.containPhases()
	base := context.WithoutCancel(ctx)
	preCtx, cancelPre := context.WithTimeout(base, preLen)
	readErr := s.containPreCut(preCtx, run)
	if readErr == nil {
		readErr = preCtx.Err()
	}
	cancelPre()
	if readErr != nil {
		slog.Warn("wardynd: the reads before cutting an ended run's agent did not finish; the next round tries again",
			slog.String("run_id", run.ID.String()), slog.Any("err", readErr))
		return
	}
	callCtx, cancelCall := context.WithTimeout(base, callLen)
	defer cancelCall()
	ans, flight := s.containCallWithin(callCtx, run, gen, func() containAnswer {
		if err := s.containCallPre(callCtx, run); err != nil {
			return containAnswer{err: err}
		}
		if err := callCtx.Err(); err != nil {
			return containAnswer{err: fmt.Errorf("the call phase ended before the cut: %w", err)}
		}
		kind, err := s.cutAgent(callCtx, run)
		return containAnswer{kind: kind, err: err}
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
		handed = s.enqueueStop(run, gen)
	}
}

// containD1Due asks d1Due, through the test stand-in when one is set.
func (s *Server) containD1Due(ctx context.Context, run types.AgentRun, ans containAnswer) bool {
	if f := s.contain.d1DueFn; f != nil {
		return f(ctx, run, ans.kind, ans.err)
	}
	return s.d1Due(ctx, run, ans.kind, ans.err)
}

// containCallWithin runs the call phase's work on its own goroutine and waits for its answer or for
// ctx's end, whichever is first. A runner call can outlive its ctx (it writes under the link's ctx), so
// the worker never waits past its phase: a call still running at the phase's end is abandoned, and the
// item's answer is errContainCallAbandoned. The run stays claimed until the call returns; its answer then
// goes through the late rule (containCallDone). The returned flight is non-nil only for an abandoned call.
// An abandoned call still counts: its stop waits for the answer, never the worker.
func (s *Server) containCallWithin(ctx context.Context, run types.AgentRun, gen uint64, work func() containAnswer) (containAnswer, *containFlight) {
	c := &s.contain
	c.init()
	f := &containFlight{run: run, gen: gen}
	c.mu.Lock()
	if e := c.inFlight[run.ID]; e != nil && e.gen == gen {
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
		if h := c.callStartFn; h != nil {
			h()
		}
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
		if notObserved(ans.err) {
			c.unobserved[f.run.ID] = time.Now()
		}
		push = c.containLateStopLocked(f)
	}
	if f.counted {
		if e := c.inFlight[f.run.ID]; e != nil && e.gen == f.gen {
			e.calls--
			if e.calls == 0 && !e.held {
				delete(c.inFlight, f.run.ID)
			}
		}
	}
	c.mu.Unlock()
	if push {
		s.pushStop(f.run, f.gen)
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
		s.pushStop(f.run, f.gen)
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
	err := f.ans.err
	switch {
	case errors.Is(err, errContainCallPanicked), errors.Is(err, errContainSkip), notObserved(err):
		return false
	case err == nil && (f.ans.kind == cutFenced || f.ans.kind == cutProxyStopped):
		return false
	}
	return c.containHandOffLocked(f.run.ID, f.gen)
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
	case errors.Is(ans.err, errContainSkip):
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
	startedAgo, unobservedAgo := s.streakAges(run.ID)
	started, err := st.SetRunContainmentErrorSince(ctx, run.ID, cause.Error(), startedAgo, unobservedAgo)
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
// the mark never counts toward a streak. A late answer that lands while a stamp is still being written
// leaves the stamp later than the mark, so that stamp can count: it is kept up to containPost (5 s) past
// the mark, an accepted bound. Nothing is written to the database: a replica that never sees
// the runner would otherwise reset the streak of the one that does.
func (s *Server) containNoteUnobserved(id uuid.UUID) {
	c := &s.contain
	c.init()
	c.mu.Lock()
	c.unobserved[id] = time.Now()
	c.mu.Unlock()
}

// streakAges is how long ago this process started the loop, and how long ago the run's last round that did
// not observe it, if any. They are lengths of time on the process's monotonic clock, never wall times: the
// store turns them into times on the database's clock, so a wrong server clock cannot move a streak.
func (s *Server) streakAges(id uuid.UUID) (startedAgo time.Duration, unobservedAgo *time.Duration) {
	c := &s.contain
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	startedAgo = time.Since(c.started)
	if t, ok := c.unobserved[id]; ok {
		ago := time.Since(t)
		unobservedAgo = &ago
	}
	return startedAgo, unobservedAgo
}

// d1Due reports whether the cut item should hand run to the stop pool, in its post phase. It is true only
// once the database's clock is past the run's containment due, and then in two cases. The run's substrate
// has no contain call (cutUnsupported with no error: a virtual machine, Kubernetes in server mode), or the
// cut failed and the database holds a failure at least a streak older than its now() that is not older than
// the run's due, its last unobserved round or this process's start. An answer that did not observe the
// sandbox is never a failure, and neither is a visit the stop path skipped. A Kubernetes server-mode run
// with an owed copy back, whose agent was stopped on this visit or whose pass was running, waits for
// containServerKeepWait past its due; a stop of its agent that failed (errAgentStopFailed), or a call phase
// that ran out, makes it due at once. A read that fails makes it false for the tick. Every time it compares
// is the database's, read when it is called, not when the item began.
func (s *Server) d1Due(ctx context.Context, run types.AgentRun, kind cutKind, err error) bool {
	if !s.d1PastDueRead(ctx, run, 0) {
		return false
	}
	serverMode := s.d1ServerMode(run)
	skipped := errors.Is(err, errContainSkip)
	failed := err != nil && !notObserved(err) && !skipped
	// A server-mode run has no cut to fail: a stop of its agent that failed, or a call phase that ran out,
	// fails closed. A visit skipped for a running pass is neither.
	stalled := serverMode && failed &&
		(errors.Is(err, errAgentStopFailed) || errors.Is(err, errContainCallAbandoned) || errors.Is(err, context.DeadlineExceeded))
	switch {
	case stalled:
		return s.d1UnsupportedDue(ctx, run, false)
	case serverMode && skipped, err == nil && kind == cutUnsupported:
		return s.d1UnsupportedDue(ctx, run, serverMode)
	case failed && s.d1StreakDue(ctx, run):
		_, _, readable := s.d1FolderOwed(ctx, run)
		return readable
	}
	return false
}

// containPreCut is the stop path's work in the pre phase, before the call. It is READS ONLY: no runner,
// agent or exec call, only lock and database reads. It must honour its ctx, because the pre phase runs on
// the cut worker itself and nothing abandons it: a read that outlives the pre budget holds the worker. An
// error or an expired budget is not a cut failure: no stamp, no d1Due, the claim is released and the next
// round tries again. Work that acts on the run belongs in containCallPre. It has none yet.
func (s *Server) containPreCut(ctx context.Context, run types.AgentRun) error {
	if f := s.contain.preCutFn; f != nil {
		return f(ctx, run)
	}
	return nil
}

// runStopItem is the stop pool's item for one run, under the claim of generation gen. A run with a contain
// call is cut once more first. Each phase runs on a context of its own, so a phase that overruns takes
// nothing from the next: the item ends within containD1 (containD1Unsupported with no last cut). Nothing is
// given up or stopped while a pass back holds the run's lock: the lock is taken after the last cut and held
// across the give-up and the stop, and a lock that is held ends the item with nothing written, to be tried
// again on the next visit. The item keeps the lock's own context: base has no deadline, so that context ends
// only when the lock is lost. A lost lock ends the item before any give-up or stop begins, since another
// holder may now own the pass. A stop already sent is not recalled; its outcome is accounted on base, under
// the claim generation it was made under.
func (s *Server) runStopItem(ctx context.Context, run types.AgentRun, gen uint64) {
	base := context.WithoutCancel(ctx)
	run, ok := s.d1Reread(base, run)
	if !ok || (!s.d1NoCall(run) && !s.d1LastCut(base, run, gen)) {
		return
	}
	lctx, unlock, locked, _ := s.tryLockRunOpHeld(base, run.ID)
	if !locked {
		slog.DebugContext(base, "wardynd: an ended run's lock is held; its stop waits for the next visit", slog.String("run_id", run.ID.String()))
		return
	}
	defer unlock()
	if !s.d1GiveUp(lctx, run) {
		return
	}
	s.d1Stop(base, lctx, run, gen)
}

// containCallPre is the stop path's work at the start of the call phase, before the cut: the acts that
// touch the run's agent. It runs inside containCallWithin on the call ctx and budget, so the worker never
// waits past the phase on it. An error wrapping errContainSkip is a skip, not a failure. Any other error is
// the call's outcome: it is stamped like a failed cut, reaches d1Due, and the cut is not made. The one
// exception is an error wrapping errRunnerNotHere, runner.ErrRunnerOffline or runner.ErrPendingOnRunner,
// which is an unobserved round, not a failure: a hook must wrap those only when the runner truly is away.
// The cut is made only while the call ctx is still live after the hook returns.
//
// For a Kubernetes server-mode run with an owed copy back it stops the agent on every visit, with no record
// of having done so, so that the pass has a stopped agent to copy from. A visit that cannot take the run's
// lock (its copy back is running) leaves the agent alone and answers errPassInFlight, a skip; the lock is
// held across the stop, so no pass starts between the check and the kill. An agent that cannot be stopped,
// or a stop that failed, answers an error wrapping errAgentStopFailed, a plain failure: the stop's own error
// is kept as text only, so it never reads as a runner that is away (server mode has no runner). Every other
// run has no work here.
func (s *Server) containCallPre(ctx context.Context, run types.AgentRun) error {
	if f := s.contain.containCallFn; f != nil {
		return f(ctx, run)
	}
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
			slog.String("run_id", run.ID.String()))
		return errAgentNotStoppable
	}
	if err := s.stopAgentForPass(lctx, run.SandboxRef); err != nil {
		slog.WarnContext(ctx, "wardynd: stopping an ended run's agent for its copy back failed; its sandbox is stopped without waiting",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return fmt.Errorf("%w: %v", errAgentStopFailed, err)
	}
	return nil
}
