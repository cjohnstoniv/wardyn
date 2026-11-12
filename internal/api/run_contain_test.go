// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	name   string
	status types.RunState

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
	statusReads  int
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

// Status reads as r.status, the sandbox as the kept-agent kill's check finds it.
func (r *containRunner) Status(context.Context, string) (runner.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusReads++
	return runner.Status{State: r.status}, nil
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

// An agent that exited but is still present is cut: no probe stands in for the fence. The runner counts
// its Status reads, and there are none.
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
		if l.rn.statusReads != 0 {
			t.Errorf("status reads = %d, want none: no probe stands in for the fence", l.rn.statusReads)
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
		gen, ok := l.srv.containClaim(row.ID, containPoolStop)
		if !ok {
			t.Fatal("the stop pool could not claim the run")
		}
		l.start()
		time.Sleep(3 * containTick)
		if fences, _, _ := l.rn.counts(); fences != 0 {
			t.Errorf("fences = %d while the stop pool held the run, want 0", fences)
		}
		l.srv.containRelease(row.ID, containPoolStop, gen)
		time.Sleep(containTick)
		l.stop()
		if fences, _, _ := l.rn.counts(); fences != 1 {
			t.Errorf("fences = %d after the release, want 1", fences)
		}
	})
}

// One substrate hangs to the call deadline; the other three runs are cut at once, not after it. The hung run
// is the newest, so it is dispatched first: with one worker the others would wait out its call phase.
func TestContain_HungCallCostsOneWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newContainLab(t, true)
		hung := dueRow(nil, time.Minute)
		l.st.add(hung)
		var others []store.TerminalSandboxRow
		for i := range 3 {
			r := dueRow(nil, 10*time.Minute+time.Duration(i)*time.Minute)
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
			at, ok := l.st.fenceAt[r.ID]
			if !ok || at.Sub(start) >= containCall {
				t.Errorf("run %s fenced at +%v (recorded %v), want before the hung call's phase (%v) ends", r.ID, at.Sub(start), ok, containCall)
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

// A cut that ignores its ctx and panics after the item moved on frees the claim at the panic, hands
// nothing to the stop pool even with d1Due true, and logs an error naming the run.
func TestContain_PanicAfterAbandonReleasesItsClaim(t *testing.T) {
	logs := captureSlog(t)
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
		var named bool
		for _, line := range strings.Split(logs.String(), "\n") {
			named = named || (strings.Contains(line, "level=ERROR") && strings.Contains(line, "a containment cut panicked") && strings.Contains(line, "run_id="+row.ID.String()))
		}
		if !named {
			t.Errorf("log = %q, want an error line for the panic naming run %s", logs.String(), row.ID)
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
// end the test can delay, and waits for its capabilities. The closer ends the link and the served runner.
func containAttach(t *testing.T, srv *Server, id uuid.UUID, sub *containHeldSub) (*runnerwire.LoopConn, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ep := srv.runnerEndpoint(id)
	org, run := runnerwire.Loopback()
	peer := ep.link.Attach(ctx, org)
	served := runnertest.Serve(ctx, run, id.String(), sub)
	closer := func() { cancel(); peer.Close(); served.Peer.Close() }
	if err := served.PublishCaps(ctx, placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192}, nil, "test"); err != nil {
		closer()
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ok := ep.sub.Caps(); ok {
			return org, closer
		}
		if time.Now().After(deadline) {
			closer()
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
			synctest.Test(t, func(t *testing.T) {
				l := newContainLab(t, false)
				l.srv.contain.callPhase, l.srv.contain.postPhase = callLen, postLen
				id := uuid.New()
				sub := &containHeldSub{Fake: runnertest.NewFake(), arrived: make(chan struct{}, 1), release: make(chan struct{})}
				org, closeAttach := containAttach(t, l.srv, id, sub)
				defer closeAttach()
				defer close(sub.release)
				defer l.stop()
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
			})
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

// quiesceRun is an ended (or, with state, a live) run for the quiesce pins.
func quiesceRun(runnerID *uuid.UUID, state types.RunState, execID string) types.AgentRun {
	run := containRunOf(dueRow(runnerID, time.Minute))
	run.State, run.AgentExecID = state, execID
	return run
}

func quiesceCalls(l *containLab) (fences, stops int) {
	fences, stops, _ = l.rn.counts()
	return fences, stops
}

func wantNotQuiescable(t *testing.T, err error, text string) {
	t.Helper()
	var cannot notQuiescable
	if !errors.As(err, &cannot) || !strings.Contains(string(cannot), text) {
		t.Fatalf("quiesce = %v, want notQuiescable containing %q", err, text)
	}
}

func TestQuiesce_VMOnAProxyFenceRunnerKeepsTheUnquiescedFirstPass(t *testing.T) {
	l := newContainLab(t, true)
	id := l.connect(true)
	run := quiesceRun(&id, types.RunKilled, runner.MainProcessExecID)
	run.MachineKind = types.MachineLinuxVM
	wantNotQuiescable(t, l.srv.quiesceKeptSandbox(context.Background(), run, run.SandboxRef), "virtual machine")
	if fences, stops := quiesceCalls(l); fences+stops != 0 {
		t.Errorf("calls = %d fences, %d stops; want none for a VM", fences, stops)
	}
}

func TestQuiesce_RunnerNotConnectedKeepsRC19Answer(t *testing.T) {
	l := newContainLab(t, true)
	id := l.connect(true)
	l.online.Store(false)
	run := quiesceRun(&id, types.RunKilled, runner.MainProcessExecID)
	wantNotQuiescable(t, l.srv.quiesceKeptSandbox(context.Background(), run, run.SandboxRef), "a sandbox on a runner is not kept")
	if fences, stops := quiesceCalls(l); fences+stops != 0 {
		t.Errorf("calls = %d fences, %d stops; want none to an absent runner", fences, stops)
	}
}

func TestQuiesce_RunnerFenceErrorIsNotQuiescable(t *testing.T) {
	l := newContainLab(t, true)
	id := l.connect(true)
	l.rn.setHook(func(context.Context, string) error { return errContainBoom })
	run := quiesceRun(&id, types.RunKilled, runner.MainProcessExecID)
	err := l.srv.quiesceKeptSandbox(context.Background(), run, run.SandboxRef)
	wantNotQuiescable(t, err, "a sandbox on a runner is not kept")
	if errors.Is(err, errContainBoom) {
		t.Errorf("quiesce = %v: a fence error must not reach the caller as a plain error", err)
	}
}

// A fenced runner run reaches the kill tail: a sandbox already stopped is passed through as such.
func TestQuiesce_RunnerWithProxyFenceIsQuiesced(t *testing.T) {
	l := newContainLab(t, true)
	id := l.connect(true)
	l.rn.status = types.RunStopped
	run := quiesceRun(&id, types.RunKilled, runner.MainProcessExecID)
	if err := l.srv.quiesceKeptSandbox(context.Background(), run, run.SandboxRef); err != errSandboxStopped {
		t.Errorf("quiesce = %v, want errSandboxStopped from the kill tail", err)
	}
	if fences, stops := quiesceCalls(l); fences != 1 || stops != 0 {
		t.Errorf("calls = %d fences, %d stops; want one fence", fences, stops)
	}
}

// A run still RUNNING reaches quiesce when a kept lease ends and its first pass back runs (the run is
// read before that end writes its state), not at a person's bring-back, which moves the run to STOPPED
// first. Such a run is quiesced as before the cut existed: a runner's is not quiescable, a hosted one has
// its proxy stopped directly, and cutAgent, which refuses a run that is not over, is never asked.
func TestQuiesce_RunningRunAtAKeptLeaseEndKeepsTheLiveQuiesce(t *testing.T) {
	l := newContainLab(t, true)
	id := l.connect(true)
	live := quiesceRun(&id, types.RunRunning, "")
	wantNotQuiescable(t, l.srv.quiesceKeptSandbox(context.Background(), live, live.SandboxRef), "a sandbox on a runner is not kept")
	if fences, stops := quiesceCalls(l); fences+stops != 0 {
		t.Errorf("a live runner run made %d fences, %d stops; want none", fences, stops)
	}
	hosted := quiesceRun(nil, types.RunRunning, "")
	err := l.srv.quiesceKeptSandbox(context.Background(), hosted, hosted.SandboxRef)
	var cannot notQuiescable
	if err == nil || errors.As(err, &cannot) {
		t.Errorf("a live hosted run answered %v, want the kill tail's plain error", err)
	}
	if fences, stops := quiesceCalls(l); fences != 0 || stops != 1 {
		t.Errorf("a live hosted run made %d fences, %d stops; want the proxy stopped once and no cut", fences, stops)
	}
}

func TestQuiesce_TerminalRunTakesTheCut(t *testing.T) {
	l := newContainLab(t, true)
	hosted := quiesceRun(nil, types.RunKilled, "")
	if err := l.srv.quiesceKeptSandbox(context.Background(), hosted, hosted.SandboxRef); err == nil {
		t.Error("a hosted terminal run with no known agent process was quiesced")
	}
	if fences, stops := quiesceCalls(l); fences != 1 || stops != 0 {
		t.Errorf("a terminal hosted run made %d fences, %d stops; want the cut (one fence) and no direct proxy stop", fences, stops)
	}
	id := l.connect(true)
	l.rn.status = types.RunStopped
	onRunner := quiesceRun(&id, types.RunKilled, runner.MainProcessExecID)
	if err := l.srv.quiesceKeptSandbox(context.Background(), onRunner, onRunner.SandboxRef); err != errSandboxStopped {
		t.Errorf("a terminal fence runner run answered %v, want errSandboxStopped", err)
	}
	if fences, _ := quiesceCalls(l); fences != 2 {
		t.Errorf("fences = %d after the runner run, want 2", fences)
	}
}

func TestQuiesce_RunnerKillTailErrorIsNotQuiescable(t *testing.T) {
	l := newContainLab(t, true)
	id := l.connect(true)
	onRunner := quiesceRun(&id, types.RunKilled, "")
	wantNotQuiescable(t, l.srv.quiesceKeptSandbox(context.Background(), onRunner, onRunner.SandboxRef), "its agent's process is not known")
	l.rn.status = types.RunStopped
	stopped := quiesceRun(&id, types.RunKilled, runner.MainProcessExecID)
	if err := l.srv.quiesceKeptSandbox(context.Background(), stopped, stopped.SandboxRef); err != errSandboxStopped {
		t.Errorf("a stopped sandbox answered %v, want errSandboxStopped unwrapped", err)
	}
	hosted := quiesceRun(nil, types.RunKilled, "")
	err := l.srv.quiesceKeptSandbox(context.Background(), hosted, hosted.SandboxRef)
	var cannot notQuiescable
	if err == nil || errors.As(err, &cannot) {
		t.Errorf("a hosted run's kill error = %v, want it plain", err)
	}
}

// containClass is one class of answer an abandoned cut call can return. owed says whether a stop that
// d1Due called for is handed to the stop pool after it: not when the cut held, was not observed, or panicked.
type containClass struct {
	name   string
	ans    containAnswer
	panics bool
	owed   bool
}

var containClasses = []containClass{
	{"ordinary late error", containAnswer{err: errContainBoom}, false, true},
	{"cut held", containAnswer{kind: cutFenced}, false, false},
	{"unobserved", containAnswer{err: runner.ErrRunnerOffline}, false, false},
	{"panic", containAnswer{}, true, false},
	{"unsupported with no error", containAnswer{kind: cutUnsupported}, false, true},
}

// containModel is what one run's claim must look like after each step of an order, worked out from the
// steps alone. The steps: C, the abandoned call returns; D, post's d1Due answer is known; R, the cut item's
// own release, which the item runs unless D handed the run on (or, for an item that ended before post, with
// no D); P, a stop worker takes the run from the stop FIFO; S, the stop item's release; N, a new tick claims
// the run.
type containModel struct {
	handoff                              bool // a stop is owed once both C and D have happened
	panicItem                            bool // the item ends by R and never reaches D
	cDone, dDone, dHanded, rDone, popped bool
	sDone, nTry                          bool
	newHolds                             bool
}

func (m containModel) pushed() bool { return m.handoff && m.cDone && m.dDone }

func (m containModel) cutHolds() bool  { return !m.rDone && !m.pushed() }
func (m containModel) stopHolds() bool { return m.pushed() && !m.sDone }
func (m containModel) callOut() bool   { return !m.cDone }
func (m containModel) held() bool      { return m.cutHolds() || m.stopHolds() || m.newHolds }

func (m containModel) enabled() []string {
	var out []string
	if !m.cDone {
		out = append(out, "C")
	}
	if !m.panicItem && !m.dDone {
		out = append(out, "D")
	}
	if (m.panicItem || m.dDone && !m.dHanded) && !m.rDone {
		out = append(out, "R")
	}
	if m.pushed() && !m.popped {
		out = append(out, "P")
	}
	if m.popped && !m.sDone {
		out = append(out, "S")
	}
	if !m.nTry {
		out = append(out, "N")
	}
	return out
}

func (m *containModel) apply(ev string) {
	switch ev {
	case "C":
		m.cDone = true
	case "D":
		m.dHanded = m.handoff && m.cDone
		m.dDone = true
	case "R":
		m.rDone = true
	case "P":
		m.popped = true
	case "S":
		m.sDone = true
	case "N":
		m.newHolds = !m.held() && !m.callOut()
		m.nTry = true
	}
}

// containOrders lists every order of the steps that can happen: a step is taken only when it is enabled,
// and an order ends when only N is left (N is tried at every point, and once).
func containOrders(m containModel) [][]string {
	var out [][]string
	var walk func(m containModel, path []string)
	walk = func(m containModel, path []string) {
		en := m.enabled()
		if !slices.ContainsFunc(en, func(e string) bool { return e != "N" }) {
			out = append(out, slices.Clone(path))
			if !slices.Contains(en, "N") {
				return
			}
		}
		for _, e := range en {
			m2 := m
			m2.apply(e)
			walk(m2, append(slices.Clone(path), e))
		}
	}
	walk(m, nil)
	return out
}

// containDrive is one run's real claim state being driven through an order of steps.
type containDrive struct {
	t       *testing.T
	srv     *Server
	id      uuid.UUID
	f       *containFlight
	gate    chan struct{}
	due     bool
	path    []string
	cutGen  uint64
	stopGen uint64
	newGen  uint64
	existed bool
	pops    int
}

func (d *containDrive) fail(inv int, what string, step int) {
	d.t.Helper()
	d.t.Fatalf("invariant %d broken after %s: %s", inv, strings.Join(d.path[:step+1], " "), what)
}

// snapshot is the run's entry now, and whether there is one.
func (d *containDrive) snapshot() (containEntry, bool) {
	c := &d.srv.contain
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.inFlight[d.id]; e != nil {
		return *e, true
	}
	return containEntry{}, false
}

func (d *containDrive) queued() int {
	c := &d.srv.contain
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.stopFIFO {
		if r.ID == d.id {
			n++
		}
	}
	return n
}

// step calls the real function for ev. prev and m are the model before and after the step.
func (d *containDrive) step(ev string, i int, prev, m containModel) {
	srv := d.srv
	switch ev {
	case "C":
		close(d.gate)
		srv.bg.Wait()
	case "D":
		if handed := srv.containDueKnown(d.f, d.due); handed != m.dHanded {
			d.fail(4, fmt.Sprintf("post's answer handed the run on = %v, want %v", handed, m.dHanded), i)
		}
	case "R":
		srv.containRelease(d.id, containPoolCut, d.cutGen)
	case "P":
		got, ok := srv.popStop()
		if !ok || got.ID != d.id {
			d.fail(4, "the stop pool had nothing to take for the run", i)
		}
		d.stopGen = srv.containGenOf(d.id)
		d.pops++
	case "S":
		srv.containRelease(d.id, containPoolStop, d.stopGen)
	case "N":
		gen, got := srv.containClaim(d.id, containPoolCut)
		d.newGen = gen
		switch {
		case got && prev.held():
			d.fail(1, "a new claim succeeded while the run was held: two holders", i)
		case got && prev.callOut():
			d.fail(2, "a new claim succeeded while a call was out: unclaimed", i)
		case !got && m.newHolds:
			d.fail(2, "the run stayed claimed with nothing holding it and no call out", i)
		}
	}
}

// check asserts the four invariants against the real state after step i.
func (d *containDrive) check(i int, m containModel) {
	snap, exists := d.snapshot()
	// 1: the entry names the single holder.
	switch {
	case m.newHolds:
		if !exists {
			d.fail(3, "the new claim's entry was deleted while held, by an earlier holder's release", i)
		}
		if snap.gen != d.newGen || snap.pool != containPoolCut || !snap.held {
			d.fail(1, "the entry is not the new claim, held", i)
		}
	case m.stopHolds() && !(exists && snap.pool == containPoolStop && snap.held):
		d.fail(1, "the stop pool's hold is not on the entry", i)
	case m.cutHolds() && !(exists && snap.pool == containPoolCut && snap.held):
		d.fail(1, "the cut item's hold is not on the entry", i)
	}
	// 2: never unclaimed while a call is out or a stop is owed.
	if (m.callOut() || m.stopHolds()) && !exists {
		d.fail(2, "unclaimed while a call is out or a stop is owed", i)
	}
	if !m.newHolds && m.callOut() && exists && snap.calls != 1 {
		d.fail(2, "the call is not on the entry", i)
	}
	// 3: the entry goes only when unheld and no call is out.
	if d.existed && !exists && (m.held() || m.callOut()) {
		d.fail(3, "the entry was deleted while held or a call was out", i)
	}
	d.existed = exists
	// 4: a stop owed by a true d1Due is handed off exactly once, and only dropped for held, unobserved or panic.
	if got, want := d.queued()+d.pops, boolInt(m.pushed()); got != want {
		d.fail(4, fmt.Sprintf("stop hand-offs = %d, want %d", got, want), i)
	}
}

// containReplay drives the real functions through one order, by hand: the call is gated and its phase
// ctx already ended, so it is abandoned at once and returns exactly when the order says.
func containReplay(t *testing.T, cls containClass, due bool, m containModel, path []string) {
	t.Helper()
	l := newContainLab(t, true)
	defer l.cancel()
	srv := l.srv
	id := uuid.New()
	run := types.AgentRun{ID: id, State: types.RunKilled, SandboxRef: "sbx-" + id.String()}
	cutGen, claimed := srv.containClaim(id, containPoolCut)
	if !claimed {
		t.Fatal("the first claim failed")
	}
	gate := make(chan struct{})
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	_, f := srv.containCallWithin(ended, run, cutGen, func() containAnswer {
		<-gate
		if cls.panics {
			panic("late cut exploded")
		}
		return cls.ans
	})
	if f == nil {
		t.Fatal("the gated call was not abandoned")
	}
	d := &containDrive{t: t, srv: srv, id: id, f: f, gate: gate, due: due, path: path, cutGen: cutGen, existed: true}
	for i, ev := range path {
		prev := m
		m.apply(ev)
		d.step(ev, i, prev, m)
		d.check(i, m)
	}
	if _, exists := d.snapshot(); exists && !m.newHolds {
		t.Fatalf("after %s the run is still claimed with nothing holding it: a leak", strings.Join(path, " "))
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Every order, for one run, of the cut item's release, the abandoned call's return in each answer class,
// post's d1Due answer (true and false), the stop hand-off and the stop item's release, and a new tick's
// claim. No clocks or goroutine races: the steps are called in the chosen order.
func TestContain_InFlightClaimInterleavings(t *testing.T) {
	total := 0
	for _, cls := range containClasses {
		for _, sc := range []struct {
			name      string
			due       bool
			panicItem bool
		}{{"d1Due true", true, false}, {"d1Due false", false, false}, {"item ends before post", false, true}} {
			m := containModel{handoff: cls.owed && sc.due && !sc.panicItem, panicItem: sc.panicItem}
			orders := containOrders(m)
			total += len(orders)
			t.Run(cls.name+"/"+sc.name, func(t *testing.T) {
				for _, path := range orders {
					containReplay(t, cls, sc.due, m, path)
				}
			})
		}
	}
	t.Logf("%d orders", total)
}

// containHandedOffRun is a run whose cut item holds it, with an abandoned call out and post's answer (a
// true d1Due) already known to the flight, then the call's late error: the run is the stop pool's. It
// answers the claim the cut item took.
func containHandedOffRun(t *testing.T) (*Server, uuid.UUID, uint64) {
	t.Helper()
	l := newContainLab(t, true)
	t.Cleanup(l.cancel)
	srv := l.srv
	id := uuid.New()
	run := types.AgentRun{ID: id, State: types.RunKilled, SandboxRef: "sbx-" + id.String()}
	cutGen, claimed := srv.containClaim(id, containPoolCut)
	if !claimed {
		t.Fatal("the first claim failed")
	}
	gate := make(chan struct{})
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	_, f := srv.containCallWithin(ended, run, cutGen, func() containAnswer {
		<-gate
		return containAnswer{err: errContainBoom}
	})
	if f == nil {
		t.Fatal("the gated call was not abandoned")
	}
	srv.contain.mu.Lock()
	f.dueKnown, f.due = true, true
	srv.contain.mu.Unlock()
	close(gate)
	srv.bg.Wait()
	if got := l.stopQueue(); got != 1 {
		t.Fatalf("stop queue = %d after the late error, want 1", got)
	}
	return srv, id, cutGen
}

// The cut item's release, arriving after the late error handed the run to the stop pool, leaves the stop
// pool's claim. Caught by: an unconditional release.
func TestContain_CutReleaseAfterHandOffKeepsTheStopClaim(t *testing.T) {
	srv, id, cutGen := containHandedOffRun(t)
	srv.containRelease(id, containPoolCut, cutGen)
	c := &srv.contain
	c.mu.Lock()
	e := c.inFlight[id]
	var got containEntry
	if e != nil {
		got = *e
	}
	queued := len(c.stopFIFO)
	c.mu.Unlock()
	if e == nil || got.pool != containPoolStop || !got.held {
		t.Errorf("entry after the cut item's release = %+v (present %v), want the stop pool's, held", got, e != nil)
	}
	if queued != 1 {
		t.Errorf("stop FIFO = %d, want the run still queued once", queued)
	}
}

// With the stop pool holding the run, a new cut tick cannot claim it, even after the cut item's release;
// once the stop item releases, it can. Caught by: an unconditional release.
func TestContain_NewCutClaimWaitsForTheStopItem(t *testing.T) {
	srv, id, cutGen := containHandedOffRun(t)
	srv.containRelease(id, containPoolCut, cutGen)
	if _, got := srv.containClaim(id, containPoolCut); got {
		t.Fatal("a new cut claim succeeded while the stop pool held the run: two holders")
	}
	if got, ok := srv.popStop(); !ok || got.ID != id {
		t.Fatalf("popStop = %v, %v; want the run", got.ID, ok)
	}
	stopGen := srv.containGenOf(id)
	if _, got := srv.containClaim(id, containPoolCut); got {
		t.Fatal("a new cut claim succeeded while the stop item was still running")
	}
	srv.containRelease(id, containPoolStop, stopGen)
	if _, got := srv.containClaim(id, containPoolCut); !got {
		t.Error("the run stayed claimed after the stop item's release")
	}
}

// The stop item finishes and removes the entry, a new tick claims the run, and only then does the old cut
// item's deferred release run: the new claim survives it. Caught by: a release that checks the pool but not
// the generation of the claim it was taken against.
func TestContain_OldCutReleaseDoesNotEndANewClaim(t *testing.T) {
	srv, id, oldGen := containHandedOffRun(t)
	if _, ok := srv.popStop(); !ok {
		t.Fatal("nothing in the stop FIFO")
	}
	srv.containRelease(id, containPoolStop, srv.containGenOf(id))
	if srv.containBusy(id) {
		t.Fatal("the run stayed claimed after the stop item's release")
	}
	newGen, got := srv.containClaim(id, containPoolCut)
	if !got {
		t.Fatal("a new cut claim failed on a free run")
	}
	if newGen == oldGen {
		t.Fatalf("the new claim has the old generation %d", oldGen)
	}
	srv.containRelease(id, containPoolCut, oldGen)
	if now := srv.containGenOf(id); now != newGen {
		t.Errorf("claim generation after the old cut item's release = %d, want the new claim's %d", now, newGen)
	}
	c := &srv.contain
	c.mu.Lock()
	held := c.inFlight[id] != nil && c.inFlight[id].held && c.inFlight[id].pool == containPoolCut
	c.mu.Unlock()
	if !held {
		t.Error("the new claim is no longer held by the cut pool")
	}
}

// fencingAgentSandbox is the bring-back fixture's sandbox on a runner that fences a proxy: the fence also
// takes the sandbox's egress away, as a stopped proxy does.
type fencingAgentSandbox struct {
	*agentSandbox
	fences atomic.Int32
}

func (r *fencingAgentSandbox) FenceProxy(context.Context, string) error {
	r.fences.Add(1)
	r.proxyStopped.Store(true)
	return nil
}

// A person's bring-back moves the run to STOPPED before the pass, so quiesce sees an ended run: on a runner
// that advertises a proxy fence it is cut off every network (one fence), its agent is killed and seen
// ended, and the pass is sent with no separate stop of the agent. Caught by: the ended-run arm of quiesce
// answering notQuiescable for a runner (the pass would then follow the separate stop), or never cutting.
func TestBringComputerFolderBack_AFencingRunnerRunIsCutAndItsPassSent(t *testing.T) {
	s, _, _, run, sb, app := bringBackFixture(t)
	id := *run.RunnerID
	caps, ok := s.runnerLastCaps(id)
	if !ok {
		t.Fatal("the fixture's runner has no caps")
	}
	caps.ProxyFence = true
	data, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	s.runnerStreams.subsMu.Lock()
	ep := s.runnerStreams.subs[id]
	s.runnerStreams.subsMu.Unlock()
	ep.sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	fr := &fencingAgentSandbox{agentSandbox: sb}
	s.cfg.Runner = fr
	if res := bringBack(t, s, bringBackOwnerSession(t), run); res.code != http.StatusOK {
		t.Fatalf("bring-back = %d %s", res.code, res.body)
	}
	sb.mu.Lock()
	calls, stops := slices.Clone(sb.calls), sb.stops
	sb.mu.Unlock()
	if fr.fences.Load() != 1 || !slices.Equal(calls, []string{"quiesce", "verify", "pass"}) || stops != 0 {
		t.Errorf("fences = %d, sandbox calls = %v, %d separate agent stops; want 1, [quiesce verify pass], 0", fr.fences.Load(), calls, stops)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "README.md")); string(b) != "edited by the agent" {
		t.Errorf("README.md = %q: the changes made before the request did not come back", b)
	}
}
