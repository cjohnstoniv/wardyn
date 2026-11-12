// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// containStore is the containment loop's store over rows in memory, with the same keyset order and the
// same stamp rule as the Postgres statements. Any other store call panics on the nil embedded Store.
type containStore struct {
	store.Store
	mu      sync.Mutex
	rows    map[uuid.UUID]*containRow
	lag     time.Duration // each write takes this long on the clock
	stamps  []time.Time   // when each stamp write ran
	fenceAt map[uuid.UUID]time.Time
}

type containRow struct {
	store.TerminalSandboxRow
	msg string
}

func newContainStore() *containStore {
	return &containStore{rows: map[uuid.UUID]*containRow{}, fenceAt: map[uuid.UUID]time.Time{}}
}

func (f *containStore) add(row store.TerminalSandboxRow) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[row.ID] = &containRow{TerminalSandboxRow: row}
	return row.ID
}

func containRowKey(r store.TerminalSandboxRow) time.Time {
	if r.EndedAt != nil {
		return *r.EndedAt
	}
	return r.UpdatedAt
}

func (f *containStore) ListTerminalSandboxPage(_ context.Context, after *store.TerminalSandboxCursor, limit int, unfencedOnly bool) ([]store.TerminalSandboxRow, *store.TerminalSandboxCursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []store.TerminalSandboxRow
	for _, r := range f.rows {
		if r.SandboxRef == "" || !r.State.IsTerminal() || (unfencedOnly && r.FencedAt != nil) {
			continue
		}
		if after != nil {
			if k := containRowKey(r.TerminalSandboxRow); k.After(after.At) || (k.Equal(after.At) && bytes.Compare(r.ID[:], after.ID[:]) >= 0) {
				continue
			}
		}
		all = append(all, r.TerminalSandboxRow)
	}
	slices.SortFunc(all, func(a, b store.TerminalSandboxRow) int {
		if ka, kb := containRowKey(a), containRowKey(b); !ka.Equal(kb) {
			if ka.After(kb) {
				return -1
			}
			return 1
		}
		return bytes.Compare(b.ID[:], a.ID[:])
	})
	if len(all) < limit {
		return all, nil, nil
	}
	all = all[:limit]
	last := all[len(all)-1]
	return all, &store.TerminalSandboxCursor{At: containRowKey(last), ID: last.ID}, nil
}

func (f *containStore) MarkRunFenced(_ context.Context, id uuid.UUID) error {
	time.Sleep(f.lag)
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.rows[id]; r != nil && r.FencedAt == nil {
		now := time.Now()
		r.FencedAt = &now
		f.fenceAt[id] = now
	}
	return nil
}

func (f *containStore) SetRunContainmentErrorSince(_ context.Context, id uuid.UUID, msg string, now, since time.Time) (bool, error) {
	time.Sleep(f.lag)
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[id]
	if r == nil || !r.State.IsTerminal() {
		return false, nil
	}
	r.msg = msg
	if r.ContainmentErrorAt == nil || r.ContainmentErrorAt.Before(since) {
		r.ContainmentErrorAt = &now
	}
	f.stamps = append(f.stamps, time.Now())
	return r.ContainmentErrorAt.Equal(now), nil
}

func (f *containStore) ClearRunContainmentError(_ context.Context, id uuid.UUID) (bool, error) {
	time.Sleep(f.lag)
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[id]
	if r == nil || r.msg == "" {
		return false, nil
	}
	r.msg, r.ContainmentErrorAt = "", nil
	return true, nil
}

func (f *containStore) row(id uuid.UUID) store.TerminalSandboxRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id].TerminalSandboxRow
}

func (f *containStore) stamp(id uuid.UUID) *time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id].ContainmentErrorAt
}

func (f *containStore) stampWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stamps)
}

// containRunner is a hosted runner that stops a proxy and records every call; containFenceRunner also
// fences. Any other Runner call panics on the nil embedded Runner, which is how a probe would show.
type containRunner struct {
	runner.Runner
	name string

	mu           sync.Mutex
	hook         func(ctx context.Context, ref string) error // the behaviour of a fence or a proxy stop
	fences       int
	stops        int
	sandboxStops int
	first        map[string]time.Time // ref -> the first call's time
	calls        map[string]int
	active, peak int
	perRef       map[string]int
	twoAtOnce    bool
}

func (r *containRunner) Name() string {
	if r.name != "" {
		return r.name
	}
	return "docker"
}

func (r *containRunner) setHook(f func(ctx context.Context, ref string) error) {
	r.mu.Lock()
	r.hook = f
	r.mu.Unlock()
}

func (r *containRunner) call(ctx context.Context, ref string) error {
	r.mu.Lock()
	if r.first == nil {
		r.first, r.calls, r.perRef = map[string]time.Time{}, map[string]int{}, map[string]int{}
	}
	if _, seen := r.first[ref]; !seen {
		r.first[ref] = time.Now()
	}
	r.calls[ref]++
	r.active++
	r.peak = max(r.peak, r.active)
	if r.perRef[ref]++; r.perRef[ref] > 1 {
		r.twoAtOnce = true
	}
	hook := r.hook
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active--
		r.perRef[ref]--
		r.mu.Unlock()
	}()
	if hook == nil {
		return nil
	}
	return hook(ctx, ref)
}

func (r *containRunner) StopProxy(ctx context.Context, ref string) error {
	r.mu.Lock()
	r.stops++
	r.mu.Unlock()
	return r.call(ctx, ref)
}

func (r *containRunner) StopSandbox(context.Context, string) error {
	r.mu.Lock()
	r.sandboxStops++
	r.mu.Unlock()
	return nil
}

func (r *containRunner) callsTo(ref string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[ref]
}

func (r *containRunner) firstCall(ref string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.first[ref]
	return at, ok
}

func (r *containRunner) counts() (fences, stops, sandboxStops int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fences, r.stops, r.sandboxStops
}

// containBare stops no proxy and fences nothing.
type containBare struct{ runner.Runner }

func (containBare) Name() string { return "docker" }

// containRemoteRunner sends a proxy stop through a real remote substrate.
type containRemoteRunner struct {
	runner.Runner
	sub *remote.Substrate
}

func (containRemoteRunner) Name() string { return "docker" }

func (r containRemoteRunner) StopProxy(ctx context.Context, ref string) error {
	return r.sub.StopProxy(ctx, ref)
}

type containFenceRunner struct{ *containRunner }

func (r containFenceRunner) FenceProxy(ctx context.Context, ref string) error {
	r.mu.Lock()
	r.fences++
	r.mu.Unlock()
	return r.call(ctx, ref)
}

// containD1Call is one call of the stand-in for d1Due, with the stamp as the store held it then.
type containD1Call struct {
	run    types.AgentRun
	kind   cutKind
	err    error
	preErr error
	at     time.Time
	stamp  *time.Time
}

// containLab is a Server running the containment loop over a fake store and a fake runner.
type containLab struct {
	t      *testing.T
	srv    *Server
	st     *containStore
	rn     *containRunner
	rec    *seamRecorder
	online atomic.Bool
	cancel context.CancelFunc

	mu    sync.Mutex
	d1    []containD1Call
	d1Ask func(n int) bool // the stand-in's answer to its nth call; nil answers false
}

func newContainLab(t *testing.T, fencer bool) *containLab {
	t.Helper()
	return newContainLabOn(t, fencer, newContainStore(), &seamRecorder{})
}

func newContainLabOn(t *testing.T, fencer bool, st *containStore, rec *seamRecorder) *containLab {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &containLab{t: t, st: st, rn: &containRunner{}, rec: rec, cancel: cancel}
	var rn runner.Runner = l.rn
	if fencer {
		rn = containFenceRunner{l.rn}
	}
	l.srv = New(Config{Runner: rn, Store: st, Audit: rec, BaseCtx: ctx, RunnerOnline: func(uuid.UUID) bool { return l.online.Load() }})
	l.srv.contain.d1DueFn = l.d1Due
	return l
}

func (l *containLab) d1Due(_ context.Context, run types.AgentRun, kind cutKind, err, preErr error) bool {
	call := containD1Call{run: run, kind: kind, err: err, preErr: preErr, at: time.Now(), stamp: l.st.stamp(run.ID)}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.d1 = append(l.d1, call)
	return l.d1Ask != nil && l.d1Ask(len(l.d1))
}

func (l *containLab) d1Calls() []containD1Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.d1)
}

func (l *containLab) start()                 { l.srv.startContainLoop() }
func (l *containLab) stop()                  { l.cancel(); l.srv.bg.Wait() }
func (l *containLab) busy(id uuid.UUID) bool { return l.srv.containBusy(id) }

// startCutOnly starts the loop and the cut workers without the stop workers, so a run handed to the stop
// pool stays in its FIFO where a test can see it.
func (l *containLab) startCutOnly() {
	s := l.srv
	ctx := s.watcherBaseCtx()
	c := &s.contain
	c.init()
	c.mu.Lock()
	c.started = s.cfg.Now()
	c.mu.Unlock()
	s.goBackground(func() { s.containLoop(ctx, containTick) })
	for range containWorkers {
		s.goBackground(func() { s.containCutWorker(ctx) })
	}
}

func (l *containLab) stopQueue() int {
	c := &l.srv.contain
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.stopFIFO)
}

// connect gives the server a runner session whose caps say whether it fences; the runner reads as online.
func (l *containLab) connect(fence bool) uuid.UUID {
	id := uuid.New()
	data, err := json.Marshal(runnerwire.Caps{Version: "test", ProxyFence: fence})
	if err != nil {
		l.t.Fatal(err)
	}
	sub := remote.New(id.String(), &capsTransport{online: true}, remote.Options{})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	l.srv.runnerStreams.subsMu.Lock()
	if l.srv.runnerStreams.subs == nil {
		l.srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	l.srv.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
	l.srv.runnerStreams.subsMu.Unlock()
	l.online.Store(true)
	return id
}

// dueRow is an ended run whose containment is due now: it ended past the lag. Its sandbox ref is its id.
func dueRow(runnerID *uuid.UUID, past time.Duration) store.TerminalSandboxRow {
	end := time.Now().Add(-containmentLag - past)
	id := uuid.New()
	return store.TerminalSandboxRow{ID: id, State: types.RunKilled, SandboxRef: "sbx-" + id.String(), RunnerID: runnerID, EndedAt: &end, UpdatedAt: end}
}

func failuresAndSuccesses(rec *seamRecorder) (failures, successes int) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ev := range rec.rows {
		if ev.Action != "run.containment.reassert" {
			continue
		}
		if ev.Outcome == "failure" {
			failures++
		} else {
			successes++
		}
	}
	return failures, successes
}

var errContainBoom = errors.New("boom")

// 10 ticks of a proxy-stop-only substrate: calls every round, never a stop of the sandbox, never a state write
// (the fake store has none and would panic).
func TestContain_QuiescableContainOnlyNeverStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, false)
		for range 3 {
			l.st.add(dueRow(nil, time.Minute))
		}
		l.start()
		time.Sleep(10*containTick + time.Second)
		l.stop()
		if _, stops, sandbox := l.rn.counts(); stops < 30 || sandbox != 0 {
			t.Errorf("proxy stops = %d, sandbox stops = %d; want a stop of the proxy every round for 3 runs and no sandbox stop", stops, sandbox)
		}
	})
}

func TestContain_TransientFailureThenSuccessDoesNotStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		id := l.st.add(dueRow(nil, time.Minute))
		var n atomic.Int32
		l.rn.setHook(func(context.Context, string) error {
			if n.Add(1) == 1 {
				return errContainBoom
			}
			return nil
		})
		l.start()
		time.Sleep(3 * containTick)
		l.stop()
		if failures, successes := failuresAndSuccesses(l.rec); failures != 1 || successes != 1 {
			t.Errorf("failure rows = %d, resolution rows = %d; want 1 and 1", failures, successes)
		}
		if l.st.row(id).FencedAt == nil || l.st.stamp(id) != nil {
			t.Error("the run was not fenced and cleared after the success")
		}
		if _, _, sandbox := l.rn.counts(); sandbox != 0 {
			t.Errorf("sandbox stops = %d, want 0", sandbox)
		}
	})
}

// Two replicas, one shared store: the failure that starts the streak is audited once.
func TestContain_TwoReplicasOneFailureRow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, rec := newContainStore(), &seamRecorder{}
		a, b := newContainLabOn(t, true, st, rec), newContainLabOn(t, true, st, rec)
		st.add(dueRow(nil, time.Minute))
		// Both are up before either fails; B fails a second after A.
		a.rn.setHook(func(context.Context, string) error { time.Sleep(time.Second); return errContainBoom })
		b.rn.setHook(func(context.Context, string) error { time.Sleep(2 * time.Second); return errContainBoom })
		a.start()
		b.start()
		time.Sleep(3 * containTick)
		a.stop()
		b.stop()
		if failures, _ := failuresAndSuccesses(rec); failures != 1 {
			t.Errorf("failure rows = %d, want 1", failures)
		}
	})
}

// A stamp left by the kept phase, from before the end, is not the start of an ended run's streak.
func TestContain_StaleKeptPhaseStampRestartsAtDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		row := dueRow(nil, time.Minute)
		old := time.Now().Add(-2 * time.Hour)
		row.ContainmentErrorAt = &old
		id := l.st.add(row)
		l.st.rows[id].msg = "left from the kept phase"
		l.rn.setHook(func(context.Context, string) error { return errContainBoom })
		l.start()
		time.Sleep(time.Second)
		l.stop()
		calls := l.d1Calls()
		if len(calls) != 1 {
			t.Fatalf("d1Due calls = %d, want 1", len(calls))
		}
		if st := calls[0].stamp; st == nil || !st.Equal(calls[0].at) {
			t.Errorf("stamp seen by d1Due = %v, want now (%v): the old stamp is no streak", st, calls[0].at)
		}
	})
}

func TestContain_FencedRunLeavesTheListing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		id := l.st.add(dueRow(nil, time.Minute))
		l.start()
		time.Sleep(5 * containTick)
		l.stop()
		if fences, _, _ := l.rn.counts(); fences != 1 {
			t.Errorf("fences = %d over 5 ticks, want 1: a fenced run is listed again", fences)
		}
		if l.st.row(id).FencedAt == nil {
			t.Error("fenced_at not recorded after a fence")
		}
	})
}

func TestContain_ProxyStoppedIsRecutNextRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, false)
		id := l.st.add(dueRow(nil, time.Minute))
		l.start()
		time.Sleep(3*containTick + time.Second)
		l.stop()
		if _, stops, _ := l.rn.counts(); stops != 4 {
			t.Errorf("proxy stops = %d over 4 ticks, want 4", stops)
		}
		if l.st.row(id).FencedAt != nil || l.st.stamp(id) != nil {
			t.Error("a stop of the proxy alone was recorded")
		}
	})
}

// An agent that exited but is still present is cut: no probe stands in for the fence. The runner has no
// Status, so a probe would panic.
func TestContain_ExitedDockerAgentIsStillCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		row := dueRow(nil, time.Minute)
		row.State = types.RunCompleted
		id := l.st.add(row)
		l.start()
		time.Sleep(time.Second)
		l.stop()
		if fences, _, _ := l.rn.counts(); fences != 1 || l.st.row(id).FencedAt == nil {
			t.Errorf("fences = %d, fenced = %v; want one fence recorded", fences, l.st.row(id).FencedAt != nil)
		}
	})
}

// A Kubernetes runner's proxy stop is never recorded as contained, and server-mode Kubernetes, which
// answers unsupported, writes nothing.
func TestContain_KubernetesRunIsNeverRecorded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		runnerID := l.connect(false)
		id := l.st.add(dueRow(&runnerID, time.Minute))
		l.start()
		time.Sleep(2*containTick + time.Second)
		l.stop()
		if fences, stops, _ := l.rn.counts(); fences != 0 || stops != 3 {
			t.Errorf("fences = %d, stops = %d; want a proxy stop every round and no fence", fences, stops)
		}
		if l.st.row(id).FencedAt != nil {
			t.Error("a proxy stop on a runner without the fence was recorded as fenced")
		}
	})
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, false)
		l.rn.name = "k8s"
		l.rn.setHook(func(context.Context, string) error { return runner.ErrEndUnsupported })
		id := l.st.add(dueRow(nil, time.Minute))
		l.start()
		time.Sleep(2 * containTick)
		l.stop()
		if l.st.row(id).FencedAt != nil || l.st.stampWrites() != 0 {
			t.Error("server-mode Kubernetes wrote a record")
		}
	})
}

// A run claimed by the stop pool is skipped by the cut pool, and cut once it is released.
func TestContain_StopPoolRunIsNotDispatched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		row := dueRow(nil, time.Minute)
		l.st.add(row)
		if !l.srv.containClaim(row.ID, containPoolStop) {
			t.Fatal("the stop pool could not claim the run")
		}
		l.start()
		time.Sleep(3 * containTick)
		if fences, _, _ := l.rn.counts(); fences != 0 {
			t.Errorf("fences = %d while the stop pool held the run, want 0", fences)
		}
		l.srv.containRelease(row.ID)
		time.Sleep(containTick)
		l.stop()
		if fences, _, _ := l.rn.counts(); fences != 1 {
			t.Errorf("fences = %d after the release, want 1", fences)
		}
	})
}

// One substrate hangs to the call deadline; the other three runs are cut at once, not after it.
func TestContain_HungCallCostsOneWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		hung := dueRow(nil, 10*time.Minute)
		l.st.add(hung)
		var others []store.TerminalSandboxRow
		for i := range 3 {
			r := dueRow(nil, time.Duration(i)*time.Minute)
			others = append(others, r)
			l.st.add(r)
		}
		l.rn.setHook(func(ctx context.Context, ref string) error {
			if ref == hung.SandboxRef {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		})
		start := time.Now()
		l.start()
		time.Sleep(containItem)
		l.stop()
		for _, r := range others {
			if at, ok := l.st.fenceAt[r.ID]; !ok || at.Sub(start) > containItem {
				t.Errorf("run %s fenced at +%v (recorded %v), want within %v", r.ID, at.Sub(start), ok, containItem)
			}
		}
	})
}

// 100 runs whose every cut takes its whole call phase and whose stamp takes the post phase: the last of
// them is reached within tau + ceil(N/K)*T of the listing.
func TestContain_ContainOnlyHundredBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		l.st.lag = containPost
		var rows []store.TerminalSandboxRow
		for range 100 {
			r := dueRow(nil, time.Minute)
			rows = append(rows, r)
			l.st.add(r)
		}
		l.rn.setHook(func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		})
		start := time.Now()
		l.start()
		time.Sleep(containTick + 25*containItem + 5*time.Second)
		l.stop()
		var last time.Duration
		for _, r := range rows {
			at, ok := l.rn.firstCall(r.SandboxRef)
			if !ok {
				t.Fatalf("run %s was never cut", r.ID)
			}
			last = max(last, at.Sub(start))
		}
		if bound := containTick + 25*containItem; last > bound || last < 15*containItem {
			t.Errorf("the last run was cut at +%v, want in [%v, %v]: K = %d workers", last, 15*containItem, bound, containWorkers)
		}
	})
}

// A runner run whose runner is not connected here is skipped: no call, and the round is marked unobserved.
func TestW2_RunnerNotConnectedSkippedNotCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		runnerID := l.connect(true)
		l.online.Store(false) // caps are still held, the session is not
		row := dueRow(&runnerID, time.Minute)
		l.st.add(row)
		l.start()
		time.Sleep(2*containTick + time.Second)
		if fences, stops, _ := l.rn.counts(); fences+stops != 0 || l.st.stampWrites() != 0 {
			t.Errorf("calls = %d, stamps = %d while the runner was away; want none", fences+stops, l.st.stampWrites())
		}
		if got := l.srv.streakSince(containRunOf(row)); time.Since(got) > containTick {
			t.Errorf("streakSince = %v, want a round within the last tick", got)
		}
		l.online.Store(true)
		time.Sleep(containTick)
		l.stop()
		if fences, _, _ := l.rn.counts(); fences != 1 {
			t.Errorf("fences = %d once the runner is back, want 1", fences)
		}
	})
}

// 1200 listed runs: the first page and a rolling page that wraps reach every one within 3 ticks.
func TestW2_PagesWrap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, false)
		var rows []store.TerminalSandboxRow
		for i := range 1200 {
			r := dueRow(nil, time.Minute+time.Duration(i)*time.Millisecond)
			rows = append(rows, r)
			l.st.add(r)
		}
		l.start()
		time.Sleep(2*containTick + time.Second)
		l.stop()
		for _, r := range rows {
			if l.rn.callsTo(r.SandboxRef) == 0 {
				t.Fatalf("run %s (end %v) was not visited in 3 ticks", r.ID, *r.EndedAt)
			}
		}
	})
}

func TestW2_NotYetDueIsNotCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		row := dueRow(nil, -3*time.Second) // due in 3 s
		l.st.add(row)
		l.start()
		time.Sleep(time.Second)
		if fences, _, _ := l.rn.counts(); fences != 0 {
			t.Errorf("fences = %d before the run was due, want 0", fences)
		}
		time.Sleep(containTick)
		l.stop()
		if fences, _, _ := l.rn.counts(); fences != 1 {
			t.Errorf("fences = %d once due, want 1", fences)
		}
	})
}

// containDispatch says a call is made exactly when cutAgent makes one.
func TestDispatch_AgreesWithCutAgent(t *testing.T) {
	for name, tc := range map[string]struct {
		rn       func(*containRunner) runner.Runner
		k8s      bool
		onRunner bool
		fenceCap bool
		vm       bool
		nref     bool
		wantCall bool
	}{
		"hosted fencer":             {rn: func(r *containRunner) runner.Runner { return containFenceRunner{r} }, wantCall: true},
		"hosted proxy stop only":    {rn: func(r *containRunner) runner.Runner { return r }, wantCall: true},
		"hosted Kubernetes server":  {rn: func(r *containRunner) runner.Runner { return r }, k8s: true, wantCall: false},
		"hosted nothing":            {rn: func(*containRunner) runner.Runner { return containBare{} }, wantCall: false},
		"runner with the fence":     {rn: func(r *containRunner) runner.Runner { return containFenceRunner{r} }, onRunner: true, fenceCap: true, wantCall: true},
		"runner without the fence":  {rn: func(r *containRunner) runner.Runner { return containFenceRunner{r} }, onRunner: true, wantCall: true},
		"runner, stop only":         {rn: func(r *containRunner) runner.Runner { return r }, onRunner: true, wantCall: true},
		"virtual machine":           {rn: func(r *containRunner) runner.Runner { return containFenceRunner{r} }, vm: true, wantCall: false},
		"virtual machine on runner": {rn: func(r *containRunner) runner.Runner { return containFenceRunner{r} }, vm: true, onRunner: true, fenceCap: true, wantCall: false},
		"no sandbox":                {rn: func(r *containRunner) runner.Runner { return containFenceRunner{r} }, nref: true, wantCall: false},
	} {
		t.Run(name, func(t *testing.T) {
			base := &containRunner{}
			if tc.k8s {
				base.name = "k8s"
				base.setHook(func(context.Context, string) error { return runner.ErrEndUnsupported })
			}
			l := newContainLabOn(t, false, newContainStore(), &seamRecorder{})
			l.srv.cfg.Runner = tc.rn(base)
			run := containRunOf(dueRow(nil, time.Minute))
			var caps runnerwire.Caps
			if tc.onRunner {
				id := l.connect(tc.fenceCap)
				run.RunnerID = &id
				caps = runnerwire.Caps{ProxyFence: tc.fenceCap}
			}
			if tc.vm {
				run.MachineKind = types.MachineLinuxVM
			}
			if tc.nref {
				run.SandboxRef = ""
			}
			gotCall, reason := l.srv.containDispatch(run, caps)
			kind, err := l.srv.cutAgent(context.Background(), run)
			cut := !(kind == cutUnsupported && err == nil)
			if gotCall != tc.wantCall || gotCall != cut {
				t.Errorf("containDispatch = %v (%s), cutAgent = %v, %v (a call: %v); want %v", gotCall, reason, kind, err, cut, tc.wantCall)
			}
		})
	}
}

// A failure, a round that did not see the runner, and a failure after it: the second failure starts a streak.
func TestContain_UnobservedRoundRestartsTheStreak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		runnerID := l.connect(true)
		id := l.st.add(dueRow(&runnerID, time.Minute))
		l.rn.setHook(func(context.Context, string) error { return errContainBoom })
		l.start() // t0: a failure
		time.Sleep(5 * time.Second)
		l.online.Store(false)
		time.Sleep(containTick) // t1 = tau: the runner is not here
		l.online.Store(true)
		time.Sleep(containTick) // t2 = 2 tau: a failure
		l.stop()
		calls := l.d1Calls()
		if len(calls) != 2 {
			t.Fatalf("d1Due calls = %d, want 2", len(calls))
		}
		if st := calls[1].stamp; st == nil || !st.Equal(calls[1].at) {
			t.Errorf("stamp at the second failure = %v, want now (%v): a failure before the gap does not count", st, calls[1].at)
		}
		if st := l.st.stamp(id); st == nil || !st.Equal(calls[1].at) {
			t.Errorf("stored stamp = %v, want the second failure's time %v", st, calls[1].at)
		}
	})
}

// A stamp from the old process does not outlive a restart: the new process's first failure replaces it.
func TestContain_RestartRestartsTheStreak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, rec := newContainStore(), &seamRecorder{}
		old := newContainLabOn(t, true, st, rec)
		id := st.add(dueRow(nil, time.Minute))
		old.rn.setHook(func(context.Context, string) error { return errContainBoom })
		old.start()
		time.Sleep(time.Second)
		old.stop()
		before := st.stamp(id)
		if before == nil {
			t.Fatal("the old process left no stamp")
		}
		time.Sleep(time.Minute)
		fresh := newContainLabOn(t, true, st, rec)
		fresh.rn.setHook(func(context.Context, string) error { return errContainBoom })
		if got := st.stamp(id); got == nil || !got.Equal(*before) {
			t.Fatalf("stamp = %v before the failure, want it unchanged at %v: a restart clears nothing", got, *before)
		}
		fresh.start()
		time.Sleep(time.Second)
		fresh.stop()
		if got := st.stamp(id); got == nil || !got.After(*before) {
			t.Errorf("stamp = %v after the new process's failure, want after %v", got, *before)
		}
	})
}

// A cut that panics ends the item, not the process: nothing is stamped, d1Due is not asked, the claim is
// released, and the next tick cuts the run again.
func TestContain_PanickingItemReleasesItsClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		row := dueRow(nil, time.Minute)
		l.st.add(row)
		l.rn.setHook(func(context.Context, string) error { panic("fence exploded") })
		l.start()
		time.Sleep(time.Second)
		if l.busy(row.ID) || l.st.stampWrites() != 0 || len(l.d1Calls()) != 0 {
			t.Errorf("busy = %v, stamps = %d, d1Due calls = %d after a panic; want none of them", l.busy(row.ID), l.st.stampWrites(), len(l.d1Calls()))
		}
		time.Sleep(containTick)
		l.stop()
		if got := l.rn.callsTo(row.SandboxRef); got != 2 {
			t.Errorf("cut calls = %d over two ticks, want 2: the run is dispatched again", got)
		}
	})
}

// A cut that ignores its ctx and panics after the item moved on frees the claim at the panic, and hands
// nothing to the stop pool even with d1Due true.
func TestContain_PanicAfterAbandonReleasesItsClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		l.d1Ask = func(n int) bool { return n == 1 }
		row := dueRow(nil, time.Minute)
		l.st.add(row)
		var n atomic.Int32
		l.rn.setHook(func(context.Context, string) error {
			if n.Add(1) > 1 {
				return nil
			}
			time.Sleep(containCall + 15*time.Second)
			panic("late fence exploded")
		})
		l.startCutOnly()
		time.Sleep(containCall + containPost) // the item has ended
		if !l.busy(row.ID) || l.st.stampWrites() != 1 || len(l.d1Calls()) != 1 {
			t.Errorf("busy = %v, stamps = %d, d1Due calls = %d at the end of the item; want true, 1, 1", l.busy(row.ID), l.st.stampWrites(), len(l.d1Calls()))
		}
		time.Sleep(15 * time.Second)
		if l.busy(row.ID) || l.stopQueue() != 0 {
			t.Errorf("busy = %v, stop queue = %d after the late panic; want false, 0", l.busy(row.ID), l.stopQueue())
		}
		time.Sleep(containTick)
		l.stop()
		if got := l.rn.callsTo(row.SandboxRef); got < 2 {
			t.Errorf("cut calls = %d, want the run dispatched again", got)
		}
	})
}

// The call phase takes its 20 s and the writes their own 5 s: the stamp is written even though the cut
// hung, the item ends within T, and a second such cut tau later sees a streak.
func TestContain_HungCutStillStampsItsError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		row := dueRow(nil, time.Minute)
		id := l.st.add(row)
		l.rn.setHook(func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		})
		start := time.Now()
		l.start()
		time.Sleep(containItem)
		if stamp := l.st.stamp(id); stamp == nil {
			t.Fatal("the hung cut left no stamp")
		} else if stamp.Sub(start) != containCall {
			t.Errorf("stamp written at +%v, want the end of the %v call phase", stamp.Sub(start), containCall)
		}
		if failures, _ := failuresAndSuccesses(l.rec); failures != 1 {
			t.Errorf("failure rows = %d, want 1", failures)
		}
		time.Sleep(containTick + containItem)
		l.stop()
		calls := l.d1Calls()
		if len(calls) < 2 {
			t.Fatalf("d1Due calls = %d, want 2", len(calls))
		}
		if first, second := calls[0], calls[1]; first.at.Sub(start) > containItem {
			t.Errorf("the first item reached d1Due at +%v, want within %v", first.at.Sub(start), containItem)
		} else if first.at.Sub(*first.stamp) >= containTick || second.at.Sub(*second.stamp) < containTick {
			t.Errorf("streak at the calls = %v then %v; want under tau, then at least tau", first.at.Sub(*first.stamp), second.at.Sub(*second.stamp))
		}
	})
}

// A cut that ignores its ctx and answers 35 s in frees the worker at 20 s; the run stays claimed until the
// answer; four such runs do not hold a fifth back.
func TestContain_CallPastItsPhaseFreesTheWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		var hung []store.TerminalSandboxRow
		for i := range 4 {
			r := dueRow(nil, time.Minute+time.Duration(i)*time.Second)
			hung = append(hung, r)
			l.st.add(r)
		}
		quick := dueRow(nil, 10*time.Minute) // an older end: listed after the four
		l.st.add(quick)
		l.rn.setHook(func(_ context.Context, ref string) error {
			if ref == quick.SandboxRef {
				return nil
			}
			time.Sleep(35 * time.Second)
			return nil
		})
		start := time.Now()
		l.start()
		time.Sleep(containItem)
		if at, ok := l.rn.firstCall(quick.SandboxRef); !ok || at.Sub(start) > containTick+2*containItem {
			t.Errorf("the fifth run was cut at +%v (cut: %v), want within %v", at.Sub(start), ok, containTick+2*containItem)
		}
		abandoned := 0
		for _, c := range l.d1Calls() {
			if errors.Is(c.err, errContainCallAbandoned) && c.kind == cutFenced {
				abandoned++
			}
		}
		if abandoned != 4 {
			t.Errorf("d1Due calls with the abandoned error = %d, want 4", abandoned)
		}
		if l.st.stampWrites() < 4 {
			t.Errorf("stamps = %d, want one failure stamp per hung run", l.st.stampWrites())
		}
		if !l.busy(hung[0].ID) {
			t.Error("a run was released while its call was still out")
		}
		time.Sleep(6 * time.Second)
		if l.busy(hung[0].ID) {
			t.Error("a run stayed claimed after its call returned")
		}
		l.stop()
	})
}

// A call that answers offline after its phase marks the round unobserved, so its stamp starts no streak.
func TestContain_LateOfflineAnswerMarksTheRoundUnobserved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		runnerID := l.connect(true)
		row := dueRow(&runnerID, time.Minute)
		l.st.add(row)
		l.rn.setHook(func(context.Context, string) error {
			time.Sleep(containCall + 15*time.Second)
			return runner.ErrRunnerOffline
		})
		l.startCutOnly()
		time.Sleep(containCall + 16*time.Second)
		answered := time.Now()
		if got := l.srv.streakSince(containRunOf(row)); answered.Sub(got) > 2*time.Second {
			t.Errorf("streakSince = %v, want the late answer's time %v", got, answered)
		}
		l.stop()
	})
}

// The stop owed for an abandoned call waits for the answer: a late error hands the run to the stop pool, a
// late cut that held does not.
func TestContain_AbandonedCallLateAnswerDecidesTheStop(t *testing.T) {
	for name, tc := range map[string]struct {
		late     error
		wantStop int
	}{"late error": {errContainBoom, 1}, "late cut held": {nil, 0}} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := newContainLab(t, true)
				l.d1Ask = func(n int) bool { return n == 1 }
				row := dueRow(nil, time.Minute)
				l.st.add(row)
				var n atomic.Int32
				l.rn.setHook(func(context.Context, string) error {
					if n.Add(1) > 1 {
						return nil
					}
					time.Sleep(containCall + 8*time.Second)
					return tc.late
				})
				l.startCutOnly()
				time.Sleep(containCall + containPost)
				if got := l.stopQueue(); got != 0 {
					t.Errorf("stop queue = %d while the call was out, want 0", got)
				}
				time.Sleep(10 * time.Second)
				if got := l.stopQueue(); got != tc.wantStop {
					t.Errorf("stop queue = %d after the answer, want %d", got, tc.wantStop)
				}
				if got := l.busy(row.ID); got != (tc.wantStop == 1) {
					t.Errorf("busy = %v after the answer, want %v: handed over with no moment unclaimed", got, tc.wantStop == 1)
				}
				l.stop()
			})
		})
	}
}

// containAttach connects id's endpoint on srv to a served runner backed by sub over a loopback whose org
// end the test can delay, and waits for its capabilities.
func containAttach(t *testing.T, srv *Server, id uuid.UUID, sub *containHeldSub) *runnerwire.LoopConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ep := srv.runnerEndpoint(id)
	org, run := runnerwire.Loopback()
	peer := ep.link.Attach(ctx, org)
	served := runnertest.Serve(ctx, run, id.String(), sub)
	t.Cleanup(func() { peer.Close(); served.Peer.Close() })
	if err := served.PublishCaps(ctx, placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192}, nil, "test"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ok := ep.sub.Caps(); ok {
			return org
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner's caps never arrived")
		}
	}
}

// containHeldSub is a runner substrate whose proxy stop waits for release, and says when one arrived.
type containHeldSub struct {
	*runnertest.Fake
	arrived chan struct{}
	release chan struct{}
}

func (h *containHeldSub) StopProxy(ctx context.Context, ref string) error {
	select {
	case h.arrived <- struct{}{}:
	default:
	}
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	return h.Fake.StopProxy(ctx, ref)
}

// The runner call goes through the real remote substrate and its link. A frame delayed past the call phase
// stalls the CALL write (first case) or the RESET after the cancel (second), and the call outlives its ctx;
// the item still ends within its phases, and the run is released when the call returns.
func TestContain_StalledRunnerWriteFreesTheWorker(t *testing.T) {
	const callLen, postLen, delay = 150 * time.Millisecond, 150 * time.Millisecond, 500 * time.Millisecond
	for name, delayBeforeCall := range map[string]bool{"call write stalled": true, "reset write stalled": false} {
		t.Run(name, func(t *testing.T) {
			l := newContainLab(t, false)
			l.srv.contain.callPhase, l.srv.contain.postPhase = callLen, postLen
			id := uuid.New()
			sub := &containHeldSub{Fake: runnertest.NewFake(), arrived: make(chan struct{}, 1), release: make(chan struct{})}
			org := containAttach(t, l.srv, id, sub)
			defer close(sub.release)
			l.srv.cfg.Runner = containRemoteRunner{sub: l.srv.runnerEndpoint(id).sub}
			l.online.Store(true)
			row := dueRow(&id, time.Minute)
			row.SandboxRef = placement.RunnerRefPrefix(id.String()) + "sbx"
			rowID := l.st.add(row)
			if delayBeforeCall {
				org.SetDelay(delay)
			}
			start := time.Now()
			l.start()
			if !delayBeforeCall {
				<-sub.arrived
				org.SetDelay(delay)
			}
			ended := func() bool { return l.st.stamp(rowID) != nil }
			for deadline := time.Now().Add(5 * time.Second); !ended(); time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the item never stamped its failure")
				}
			}
			if took := time.Since(start); took > callLen+postLen+delay/2 {
				t.Errorf("the item ended after %v, want within its phases (%v) and well before the stalled write (%v)", took, callLen+postLen, delay)
			}
			if !l.busy(rowID) {
				t.Error("the run was released while its call was still out")
			}
			for deadline := time.Now().Add(10 * time.Second); l.busy(rowID); time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the run stayed claimed after the stalled write")
				}
			}
			if !l.srv.runnerEndpoint(id).link.Online() {
				t.Error("the link read offline: a late deadline error on a connected runner was expected")
			}
			l.stop()
		})
	}
}

// Every call ignores its ctx and answers offline E after its phase ended. Over 300 ticks and 200 runs the
// calls in flight stay at or under K x 5, no run has two out, and the peak is above K.
func TestContain_HungCallsInFlightBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		for range 200 {
			id := l.connect(true)
			l.st.add(dueRow(&id, time.Minute))
		}
		l.rn.setHook(func(ctx context.Context, _ string) error {
			<-ctx.Done()
			time.Sleep(60 * time.Second)
			return runner.ErrRunnerOffline
		})
		l.start()
		time.Sleep(300 * containTick)
		l.stop()
		l.rn.mu.Lock()
		defer l.rn.mu.Unlock()
		if bound := containWorkers * 5; l.rn.peak > bound || l.rn.peak <= containWorkers {
			t.Errorf("peak calls in flight = %d, want in (%d, %d]", l.rn.peak, containWorkers, bound)
		}
		if l.rn.twoAtOnce {
			t.Error("a run had two calls in flight")
		}
	})
}
