// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// readbackFolderStore is readbackStore with a computer folder's pass record, read as rec or failing with err.
type readbackFolderStore struct {
	*readbackStore
	rec types.RunLocalFolderStatus
	err error
}

func (s *readbackFolderStore) RunLocalFolderStatus(context.Context, uuid.UUID) (types.RunLocalFolderStatus, error) {
	return s.rec, s.err
}

func (s *readbackFolderStore) SetRunLocalFolderStatusIf(context.Context, uuid.UUID, store.FolderStatusGuard, types.RunLocalFolderStatus) (bool, error) {
	return false, errors.New("this store records no pass")
}

func (s *readbackFolderStore) BumpRunLocalFolderGen(context.Context, uuid.UUID, store.FolderStatusGuard, time.Time) (types.RunLocalFolderStatus, bool, error) {
	return types.RunLocalFolderStatus{}, false, errors.New("this store records no pass")
}

// endedWithFailedStart is a run whose revived proxy failed its readback with that proxy's stop
// unresolved, then ended with its sandbox ref kept, its computer folder's record read as rec or
// failing with err. It answers the failed start's generation and the run's ref.
func endedWithFailedStart(t *testing.T, rec types.RunLocalFolderStatus, err error) (*readbackFixture, int, string) {
	t.Helper()
	f := newReadbackFixture(t)
	gen := f.failWithUnstoppableProxy(t)
	f.st.mu.Lock()
	f.st.state = types.RunFailed
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	ref := f.st.run.SandboxRef
	f.st.mu.Unlock()
	if ref == "" {
		t.Fatal("the run has no sandbox ref")
	}
	f.srv.cfg.Store = &readbackFolderStore{readbackStore: f.rb, rec: rec, err: err}
	return f, gen, ref
}

// assertSandboxKept checks containment stopped the proxy stops more times and never the sandbox,
// and left the run's ref.
func assertSandboxKept(t *testing.T, f *readbackFixture, ref string, proxyStops, sandboxStops, more int) {
	t.Helper()
	if n := f.lr.proxyStopCount(); n != proxyStops+more {
		t.Errorf("StopProxy = %d, want %d", n, proxyStops+more)
	}
	if n := f.lr.stopCount(); n != sandboxStops {
		t.Errorf("StopSandbox = %d, want %d: the sandbox is kept for its computer folder's copy back", n, sandboxStops)
	}
	if run, _ := f.srv.cfg.Store.GetRun(context.Background(), f.run.ID); run.SandboxRef != ref {
		t.Errorf("ref = %q, want %q kept", run.SandboxRef, ref)
	}
}

var readbackKeptResume = types.RunLocalFolderStatus{Resume: &types.FolderResume{Unsynced: 2}}

// An ended run whose sandbox is kept for its computer folder's copy back has only its failed
// start's proxy stopped; the sandbox and its ref stay, and the start is contained once that stop
// is confirmed.
func TestReadbackContainment_AKeptFolderSandboxHasOnlyItsProxyStopped(t *testing.T) {
	f, gen, ref := endedWithFailedStart(t, readbackKeptResume, nil)
	f.setProxyStopErr(nil)
	proxyStops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("containment = %+v, want contained", c)
	}
	assertSandboxKept(t, f, ref, proxyStops, sandboxStops, 1)
	if p := f.active(t); p.ContainedAt == nil || p.ContainmentError != "" {
		t.Errorf("start contained %v, error %q; want contained", p.ContainedAt, p.ContainmentError)
	}
}

// A folder record that cannot be read counts as kept: the sandbox is not stopped on a guess.
func TestReadbackContainment_AnUnreadableFolderRecordKeepsTheSandbox(t *testing.T) {
	f, gen, ref := endedWithFailedStart(t, types.RunLocalFolderStatus{}, errors.New("the database is unreachable"))
	f.setProxyStopErr(nil)
	proxyStops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("containment = %+v, want contained", c)
	}
	assertSandboxKept(t, f, ref, proxyStops, sandboxStops, 1)
}

// An ended run with no folder record, or one with no resume, has its sandbox stopped as before.
func TestReadbackContainment_AnEndedRunNotKeptForItsFolderHasItsSandboxStopped(t *testing.T) {
	for name, tc := range map[string]struct {
		rec types.RunLocalFolderStatus
		err error
	}{
		"no record": {err: store.ErrNotFound},
		"no resume": {rec: types.RunLocalFolderStatus{Back: &types.FolderPassRecord{Error: "the pass failed"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f, gen, _ := endedWithFailedStart(t, tc.rec, tc.err)
			f.setProxyStopErr(nil)
			proxyStops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
			if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
				t.Fatalf("containment = %+v, want contained", c)
			}
			if n := f.lr.stopCount(); n != sandboxStops+1 {
				t.Errorf("StopSandbox = %d, want %d", n, sandboxStops+1)
			}
			if n := f.lr.proxyStopCount(); n != proxyStops {
				t.Errorf("StopProxy = %d, want %d: the sandbox stop takes its proxy", n, proxyStops)
			}
		})
	}
}

// A kept sandbox's proxy stop that fails leaves the containment unresolved, recorded on the start
// and audited, with nothing claimed stopped and the sandbox not stopped in its place; the retry
// that stops it resolves it.
func TestReadbackContainment_AKeptFolderSandboxsFailedProxyStopStaysUnresolved(t *testing.T) {
	f, gen, ref := endedWithFailedStart(t, readbackKeptResume, nil)
	proxyStops, sandboxStops := f.lr.proxyStopCount(), f.lr.stopCount()
	rows := len(f.audit.eventsFor(f.run.ID, "run.reconcile"))
	c := f.srv.containProxyStart(context.Background(), f.run.ID, gen)
	if c.Contained || !errors.Is(c.Err, errProxyStopUnconfirmed) {
		t.Fatalf("containment = %+v, want unresolved with errProxyStopUnconfirmed", c)
	}
	assertSandboxKept(t, f, ref, proxyStops, sandboxStops, 1)
	if p := f.active(t); p.ContainedAt != nil || !strings.Contains(p.ContainmentError, "proxy stop failed") {
		t.Errorf("start contained %v, error %q; want unresolved, naming the proxy stop", p.ContainedAt, p.ContainmentError)
	}
	evs := f.audit.eventsFor(f.run.ID, "run.reconcile")
	if len(evs) != rows+1 || evs[len(evs)-1].Outcome != "failure" || !strings.Contains(string(evs[len(evs)-1].Data), "daemon down") ||
		!strings.Contains(string(evs[len(evs)-1].Data), `"teardown_error"`) || strings.Contains(string(evs[len(evs)-1].Data), "proxy_stop_error") {
		t.Errorf("run.reconcile rows = %v, want one failure naming the stop's error as teardown_error", evs[rows:])
	}
	f.setProxyStopErr(nil)
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("the retry's containment = %+v, want contained", c)
	}
	assertSandboxKept(t, f, ref, proxyStops, sandboxStops, 2)
}

// A running run that cannot be kept has its proxy stopped first and is then ended: a cut copy
// back of its computer folder keeps the sandbox, which is never stopped before that decision.
func TestReadbackContainment_ARunningRunsEndDecidesItsSandboxAfterTheProxyStop(t *testing.T) {
	s, cs, _, run, sb := hangingFolderBack(t)
	stoppedBefore := false
	sb.stopped = func() { stoppedBefore = stoppedBefore || cs.folderStatus().Back == nil }
	run.State, run.SandboxRef = types.RunRunning, "sbx"
	ctx, cancel := context.WithTimeout(context.Background(), runTeardownBudget+computerFolderAccountTime+300*time.Millisecond)
	defer cancel()
	if err := s.stopUnconfirmedProxy(ctx, run); err != nil {
		t.Fatalf("stopUnconfirmedProxy = %v, want the proxy stop confirmed", err)
	}
	if stoppedBefore {
		t.Error("the sandbox was stopped before the end's computer folder decision")
	}
	if !sb.proxyStopped.Load() {
		t.Error("the run's proxy was not stopped")
	}
	if n := sb.stopCount(); n != 0 {
		t.Errorf("StopSandbox succeeded %d times, want the sandbox kept for its copy back", n)
	}
	if st := cs.folderStatus(); st.Resume == nil || *st.Resume != (types.FolderResume{Stalled: 1}) {
		t.Errorf("resume = %+v, want the cut pass's", st.Resume)
	}
	if got := cs.State(); got != types.RunFailed {
		t.Errorf("state = %s, want FAILED", got)
	}
}

// failEndOnceStore fails the next write ending the run, as a database blip would.
type failEndOnceStore struct {
	*computerFolderStore
	fail atomic.Bool
}

func (s *failEndOnceStore) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if to.IsTerminal() && s.fail.CompareAndSwap(true, false) {
		return false, errors.New("injected end-write failure")
	}
	return s.computerFolderStore.UpdateRunStateIf(ctx, id, from, to)
}

// A running run's end that could not be written leaves the containment unresolved, the run live and
// its sandbox standing; the retry ends it and makes the computer folder decision.
func TestReadbackContainment_AnUnwrittenEndStaysUnresolvedUntilARetryEndsTheRun(t *testing.T) {
	s, cs, _, run, sb := hangingFolderBack(t)
	fs := &failEndOnceStore{computerFolderStore: cs}
	fs.fail.Store(true)
	s.cfg.Store = fs
	run.State, run.SandboxRef = types.RunRunning, "sbx"
	if err := s.stopUnconfirmedProxy(context.Background(), run); !errors.Is(err, errRunEndUnwritten) {
		t.Fatalf("stopUnconfirmedProxy = %v, want errRunEndUnwritten", err)
	}
	if got, n := cs.State(), sb.stopCount(); got != types.RunRunning || n != 0 {
		t.Fatalf("state %s, StopSandbox %d; want the run still RUNNING with its sandbox standing", got, n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runTeardownBudget+computerFolderAccountTime+300*time.Millisecond)
	defer cancel()
	if err := s.stopUnconfirmedProxy(ctx, run); err != nil {
		t.Fatalf("the retry = %v, want the end recorded", err)
	}
	if got := cs.State(); got != types.RunFailed {
		t.Errorf("state = %s, want FAILED", got)
	}
	if n := sb.stopCount(); n != 0 {
		t.Errorf("StopSandbox succeeded %d times, want the sandbox kept for its copy back", n)
	}
	if st := cs.folderStatus(); st.Resume == nil {
		t.Errorf("resume = %+v, want the cut pass's", st.Resume)
	}
}

// A substrate that cannot stop a proxy alone keeps no sandbox for a copy back, so an ended run's
// failed start has its sandbox stopped even when its folder record reads as kept.
func TestReadbackContainment_ASubstrateThatCannotStopAProxyAloneStopsTheSandbox(t *testing.T) {
	f, gen, _ := endedWithFailedStart(t, types.RunLocalFolderStatus{}, errors.New("the database is unreachable"))
	f.setProxyStopErr(runner.ErrEndUnsupported)
	sandboxStops := f.lr.stopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("containment = %+v, want contained", c)
	}
	if n := f.lr.stopCount(); n != sandboxStops+1 {
		t.Errorf("StopSandbox = %d, want %d", n, sandboxStops+1)
	}
}

// folderStatusStore answers the run's computer folder record: "kept" (a pass back keeps its sandbox for a
// resume), "unreadable", or none; sets counts record writes.
type folderStatusStore struct {
	*failMarkStore
	mode string
	sets atomic.Int32
}

func (s *folderStatusStore) RunLocalFolderStatus(context.Context, uuid.UUID) (types.RunLocalFolderStatus, error) {
	switch s.mode {
	case "kept":
		return types.RunLocalFolderStatus{Resume: &types.FolderResume{Stalled: 1, Unsynced: 3}}, nil
	case "unreadable":
		return types.RunLocalFolderStatus{}, errors.New("injected folder record read failure")
	}
	return types.RunLocalFolderStatus{}, nil
}

func (s *folderStatusStore) SetRunLocalFolderStatusIf(context.Context, uuid.UUID, store.FolderStatusGuard, types.RunLocalFolderStatus) (bool, error) {
	s.sets.Add(1)
	return true, nil
}

func (s *folderStatusStore) BumpRunLocalFolderGen(ctx context.Context, id uuid.UUID, _ store.FolderStatusGuard, at time.Time) (types.RunLocalFolderStatus, bool, error) {
	st, err := s.RunLocalFolderStatus(ctx, id)
	st.Gen, st.GenAt = st.Gen+1, at
	return st, err == nil, err
}

// keptFolderFailedStart revives the fixture's run and has its new proxy report other bytes while another operation
// holds the run's lock, so the start fails uncontained. The run then stands as a finalize that kept its sandbox for a
// computer folder's pass back leaves it: terminal, its sandbox ref still recorded, its folder record as mode says.
// It answers the store and the start's generation.
func (f *readbackFixture) keptFolderFailedStart(t *testing.T, mode string) (*folderStatusStore, int) {
	t.Helper()
	fs := &folderStatusStore{failMarkStore: &failMarkStore{readbackStore: f.rb}, mode: mode}
	f.srv.cfg.Store = fs
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, _, jti := f.lastHandover(t)
	_, unlock, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("take the run's lock")
	}
	code, state := postLoaded(t, f.srv, f.run.ID, jti, hashHex([]byte("other bytes")))
	unlock()
	if code != http.StatusOK || state != string(types.ProxyStartFailed) {
		t.Fatalf("mismatched report: %d %q, want 200 failed", code, state)
	}
	f.st.mu.Lock()
	f.st.state = types.RunFailed
	f.st.mu.Unlock()
	return fs, f.active(t).Generation
}

// A failed start of a run whose sandbox is kept for its computer folder's pass back has only its proxy stopped:
// the sandbox and the folder's record are left as they are, and the start is contained once that stop is confirmed.
func TestReadback_KeptFolderSandboxHasOnlyItsProxyStopped(t *testing.T) {
	f := newReadbackFixture(t)
	fs, gen := f.keptFolderFailedStart(t, "kept")
	sandboxStops, proxyStops := f.lr.stopCount(), f.lr.proxyStopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("containment = %+v, want contained once the proxy stop is confirmed", c)
	}
	if n := f.lr.stopCount() - sandboxStops; n != 0 {
		t.Errorf("sandbox stops +%d, want none: the sandbox is kept for its folder's pass back", n)
	}
	if n := f.lr.proxyStopCount() - proxyStops; n != 1 {
		t.Errorf("proxy stops +%d, want 1", n)
	}
	if fs.sets.Load() != 0 {
		t.Error("the folder's record must be left as it is")
	}
}

// The overdue sweep contains the same start the same way: proxy only, and never a later sandbox stop.
func TestReadback_SweepKeepsAKeptFolderSandbox(t *testing.T) {
	f := newReadbackFixture(t)
	f.keptFolderFailedStart(t, "kept")
	sandboxStops := f.lr.stopCount()
	for range 2 {
		if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.lr.stopCount() - sandboxStops; n != 0 {
		t.Errorf("sandbox stops +%d, want none: the sandbox is kept for its folder's pass back", n)
	}
	if p := f.active(t); p.ContainedAt == nil || p.ContainmentError != "" {
		t.Errorf("start contained %v, error %q; want contained by its proxy stop", p.ContainedAt, p.ContainmentError)
	}
	if overdue, err := f.rb.OverdueProxyStarts(context.Background()); err != nil || len(overdue) != 0 {
		t.Errorf("overdue starts = %+v (%v), want none", overdue, err)
	}
}

// A terminal run with no folder pass to resume still has its sandbox stopped: containment stays fail-closed.
func TestReadback_TerminalSandboxWithNoFolderResumeIsStopped(t *testing.T) {
	f := newReadbackFixture(t)
	f.keptFolderFailedStart(t, "")
	sandboxStops := f.lr.stopCount()
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.lr.stopCount() - sandboxStops; n != 1 {
		t.Errorf("sandbox stops +%d, want 1", n)
	}
	if p := f.active(t); p.ContainedAt == nil {
		t.Error("the start must be contained once its sandbox stop lands")
	}
}

// A folder record that cannot be read counts as kept, as every sweep reads it: the failed start's proxy alone is
// stopped, the sandbox stays, and the start is contained, with one run.reconcile failure row naming the unread record.
func TestReadback_UnreadableFolderRecordKeepsTheSandboxContainedAndAudited(t *testing.T) {
	f := newReadbackFixture(t)
	_, gen := f.keptFolderFailedStart(t, "unreadable")
	sandboxStops, proxyStops := f.lr.stopCount(), f.lr.proxyStopCount()
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
		t.Fatalf("containment = %+v, want contained once the proxy stop is confirmed", c)
	}
	if n := f.lr.stopCount() - sandboxStops; n != 0 {
		t.Errorf("sandbox stops +%d, want none while the folder record cannot be read", n)
	}
	if n := f.lr.proxyStopCount() - proxyStops; n != 1 {
		t.Errorf("proxy stops +%d, want 1", n)
	}
	if p := f.active(t); p.ContainedAt == nil || p.ContainmentError != "" {
		t.Errorf("start contained %v, error %q; want contained", p.ContainedAt, p.ContainmentError)
	}
	if n := folderRecordRows(f); n != 1 {
		t.Errorf("%d run.reconcile failure rows with folder_record_error, want 1", n)
	}
}

// The unread record's row is written once, by the containment that keeps the sandbox: a second containment of the
// same start and the overdue sweep write no other, and stop the proxy no more.
func TestReadback_UnreadableFolderRecordRowIsWrittenOnce(t *testing.T) {
	f := newReadbackFixture(t)
	_, gen := f.keptFolderFailedStart(t, "unreadable")
	proxyStops := f.lr.proxyStopCount()
	for range 2 {
		if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained || c.Err != nil {
			t.Fatalf("containment = %+v, want contained", c)
		}
		if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := folderRecordRows(f); n != 1 {
		t.Errorf("%d run.reconcile failure rows with folder_record_error, want 1", n)
	}
	if n := f.lr.proxyStopCount() - proxyStops; n != 1 {
		t.Errorf("proxy stops +%d, want 1", n)
	}
}

// folderRecordRows counts the run's run.reconcile failure rows naming an unread computer folder record.
func folderRecordRows(f *readbackFixture) int {
	n := 0
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.reconcile") {
		if ev.Outcome == "failure" && strings.Contains(string(ev.Data), `"folder_record_error"`) {
			n++
		}
	}
	return n
}

// orderRunner records the order of proxy and sandbox stops, each sandbox stop with the run's state as it was.
type orderRunner struct {
	*hashedReviveRunner
	mu    sync.Mutex
	calls []string
	state func() types.RunState
}

func (r *orderRunner) StopProxy(ctx context.Context, ref string) error {
	r.mu.Lock()
	r.calls = append(r.calls, "proxy")
	r.mu.Unlock()
	return r.hashedReviveRunner.StopProxy(ctx, ref)
}

func (r *orderRunner) StopSandbox(ctx context.Context, ref string) error {
	r.mu.Lock()
	r.calls = append(r.calls, "sandbox:"+string(r.state()))
	r.mu.Unlock()
	return r.hashedReviveRunner.StopSandbox(ctx, ref)
}

// A running hosted run that cannot be kept, whose start failed, has its proxy stopped before its end runs, so the
// end's computer folder decision (and its pass back) comes before any sandbox stop: every sandbox stop finds the
// run already ended, never one stopped right after its proxy.
func TestReadback_UnkeepableRunningRunStopsItsProxyBeforeItsEnd(t *testing.T) {
	f := newReadbackFixture(t)
	_, gen := f.keptFolderFailedStart(t, "")
	f.st.mu.Lock()
	f.st.state, f.st.run.Interactive, f.st.run.Placement = types.RunRunning, false, types.PlacementRemote
	f.st.mu.Unlock()
	ordered := &orderRunner{hashedReviveRunner: f.hr, state: f.st.State}
	f.srv.cfg.Runner = ordered
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); !c.Contained {
		t.Fatalf("containment = %+v, want contained once the end landed", c)
	}
	ordered.mu.Lock()
	calls := append([]string(nil), ordered.calls...)
	ordered.mu.Unlock()
	if len(calls) == 0 || calls[0] != "proxy" {
		t.Errorf("stops = %v, want the proxy stopped first", calls)
	}
	for _, c := range calls[1:] {
		if state, ok := strings.CutPrefix(c, "sandbox:"); ok && !isTerminalRunState(types.RunState(state)) {
			t.Errorf("stops = %v: the sandbox was stopped with the run %s, before its end", calls, state)
		}
	}
	if !isTerminalRunState(f.st.State()) {
		t.Errorf("state %s, want the run ended", f.st.State())
	}
}

// sweepProxyRunner is sweepAliveRunner that can also stop a proxy alone, failing with proxyErr.
type sweepProxyRunner struct {
	*sweepAliveRunner
	proxyErr error
	proxied  []string
}

func (r *sweepProxyRunner) StopProxy(_ context.Context, ref string) error {
	r.proxied = append(r.proxied, ref)
	return r.proxyErr
}

// keptSweepStore is sweepStore whose run kept has a computer folder's pass back that keeps its sandbox, and whose
// run unreadable has a folder record that cannot be read.
type keptSweepStore struct {
	*sweepStore
	kept, unreadable uuid.UUID
}

func (s *keptSweepStore) RunLocalFolderStatus(_ context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	switch id {
	case s.kept:
		return types.RunLocalFolderStatus{Resume: &types.FolderResume{Stalled: 1}}, nil
	case s.unreadable:
		return types.RunLocalFolderStatus{}, errors.New("injected folder record read failure")
	}
	return types.RunLocalFolderStatus{}, nil
}

func (s *keptSweepStore) SetRunLocalFolderStatusIf(context.Context, uuid.UUID, store.FolderStatusGuard, types.RunLocalFolderStatus) (bool, error) {
	return true, nil
}

func (s *keptSweepStore) BumpRunLocalFolderGen(ctx context.Context, id uuid.UUID, _ store.FolderStatusGuard, at time.Time) (types.RunLocalFolderStatus, bool, error) {
	st, err := s.RunLocalFolderStatus(ctx, id)
	st.Gen, st.GenAt = st.Gen+1, at
	return st, err == nil, err
}

// newKeptSweep is a terminal-sandbox sweep over runs, every sandbox probed live, with the folder records fake answers.
func newKeptSweep(t *testing.T, fake *keptSweepStore) (*Server, *sweepProxyRunner, *harness) {
	t.Helper()
	h := newHarness(t)
	fake.written = h.audit
	rr := &sweepProxyRunner{sweepAliveRunner: &sweepAliveRunner{fakeRunner: &fakeRunner{}}}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = rr
	cfg.Broker = h.broker
	return New(cfg), rr, h
}

// runRows is the audit rows for runID.
func runRows(events []types.AuditEvent, runID uuid.UUID) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range events {
		if ev.RunID != nil && *ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out
}

// keptSweepRows is runRows less the identity provider's identity.revoke rows: sweepTerminalSandboxRun runs the revoke
// cascade on every pass, a kept sandbox's too, and that row is the revoke's (Provider.RevokeRun), not the sweep's.
func keptSweepRows(events []types.AuditEvent, runID uuid.UUID) []types.AuditEvent {
	return slices.DeleteFunc(runRows(events, runID), func(ev types.AuditEvent) bool { return ev.Action == "identity.revoke" })
}

// The terminal-sandbox sweep leaves a sandbox kept for its computer folder's pass back with only its proxy stopped,
// and still stops one that is not kept.
func TestSweepTerminalSandboxes_LeavesAKeptFolderSandbox(t *testing.T) {
	kept, orphan := sweepRun(types.RunFailed, "sbx-kept"), sweepRun(types.RunFailed, "sbx-orphan")
	srv, rr, _ := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{kept, orphan}}, kept: kept.ID})
	swept, err := srv.SweepTerminalSandboxes(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(rr.stopped) != 1 || rr.stopped[0] != "sbx-orphan" || len(rr.proxied) != 1 || rr.proxied[0] != "sbx-kept" || swept != 1 {
		t.Errorf("StopSandbox %v, StopProxy %v, swept %d; want [sbx-orphan], [sbx-kept], 1", rr.stopped, rr.proxied, swept)
	}
}

// Two passes over a kept sandbox stop its proxy once each and leave the sandbox: a stop that lands writes no row of
// the sweep's, and the sandbox is never counted as swept, on either pass.
func TestSweepTerminalSandboxes_KeptFolderSandboxIsNotReapedEachPass(t *testing.T) {
	kept := sweepRun(types.RunFailed, "sbx-kept")
	srv, rr, h := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{kept}}, kept: kept.ID})
	for pass := 1; pass <= 2; pass++ {
		swept, err := srv.SweepTerminalSandboxes(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if swept != 0 || len(rr.proxied) != pass || len(rr.stopped) != 0 {
			t.Errorf("pass %d: swept %d, StopProxy %v, StopSandbox %v; want 0, %d proxy stops, no sandbox stop",
				pass, swept, rr.proxied, rr.stopped, pass)
		}
		if rows := keptSweepRows(h.audit.snapshot(), kept.ID); len(rows) != 0 {
			t.Errorf("pass %d: rows %+v, want none for a proxy stop that landed", pass, rows)
		}
	}
}

// A folder record the sweep cannot read keeps the sandbox too, with its proxy stopped, and is not counted as swept.
func TestSweepTerminalSandboxes_UnreadableFolderRecordKeepsTheSandbox(t *testing.T) {
	run := sweepRun(types.RunFailed, "sbx-unread")
	srv, rr, _ := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{run}}, unreadable: run.ID})
	swept, err := srv.SweepTerminalSandboxes(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 || len(rr.proxied) != 1 || len(rr.stopped) != 0 {
		t.Errorf("swept %d, StopProxy %v, StopSandbox %v; want 0, one proxy stop, no sandbox stop", swept, rr.proxied, rr.stopped)
	}
}

// A kept sandbox whose proxy stop fails is the sweep's sandbox.sweep failure row with teardown_error, never silent.
func TestSweepTerminalSandboxes_KeptFolderProxyStopFailureIsAudited(t *testing.T) {
	kept := sweepRun(types.RunFailed, "sbx-kept")
	srv, rr, h := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{kept}}, kept: kept.ID})
	rr.proxyErr = errors.New("the daemon did not answer the proxy stop")
	if _, err := srv.SweepTerminalSandboxes(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rows := keptSweepRows(h.audit.snapshot(), kept.ID)
	if len(rows) != 1 || rows[0].Action != "sandbox.sweep" || rows[0].Outcome != "failure" ||
		!strings.Contains(string(rows[0].Data), `"teardown_error"`) || strings.Contains(string(rows[0].Data), "proxy_stop_error") || len(rr.stopped) != 0 {
		t.Errorf("rows %+v, StopSandbox %v; want one sandbox.sweep failure with teardown_error alone and no sandbox stop", rows, rr.stopped)
	}
}

// A kept sandbox whose proxy stop fails stays unresolved, as one run.reconcile failure row naming that stop as
// teardown_error (the key stopProxyOrAudit writes); the sandbox, and the files it is kept for, are not stopped.
func TestReadback_KeptFolderProxyStopFailureIsAuditedAndUnresolved(t *testing.T) {
	f := newReadbackFixture(t)
	_, gen := f.keptFolderFailedStart(t, "kept")
	sandboxStops := f.lr.stopCount()
	f.setProxyStopErr(errors.New("daemon down"))
	if c := f.srv.containProxyStart(context.Background(), f.run.ID, gen); c.Contained || c.Err == nil {
		t.Fatalf("containment = %+v, want unresolved while the proxy stop fails", c)
	}
	if n := f.lr.stopCount() - sandboxStops; n != 0 {
		t.Errorf("sandbox stops +%d, want none: the sandbox is kept for its folder's pass back", n)
	}
	var rows int
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.reconcile") {
		if ev.Outcome == "failure" && strings.Contains(string(ev.Data), "daemon down") {
			if !strings.Contains(string(ev.Data), `"teardown_error"`) || strings.Contains(string(ev.Data), "proxy_stop_error") {
				t.Errorf("row %s, want the stop's error as teardown_error alone", ev.Data)
			}
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("%d run.reconcile failure rows naming the stop, want 1", rows)
	}
}

// A terminal sandbox whose folder record cannot be read keeps its egress cut and its revoke on every substrate:
// it is kept only once its proxy is stopped alone, as a folder_record_error row; it is stopped where it cannot be
// kept (a virtual machine), where its proxy cannot be stopped alone, and where that stop failed. A run on a runner
// that is not connected here is left in place, with nothing sent, until its runner is.
func TestSweepTerminalSandboxes_UnreadableFolderRecordFailsClosed(t *testing.T) {
	runnerID := uuid.New()
	for _, tc := range []struct {
		name     string
		noProxy  bool
		proxyErr error
		edit     func(*types.AgentRun)
		connect  bool // the run's runner is connected here, without the fence
		left     bool
		row      string
		proxied  int
	}{
		{name: "proxy stopped", left: true, row: "folder_record_error", proxied: 1},
		{name: "proxy stop failed", proxyErr: errors.New("the daemon did not answer the proxy stop"), row: `"teardown_error"`, proxied: 1},
		{name: "proxy stop unsupported", proxyErr: runner.ErrEndUnsupported, proxied: 1},
		{name: "no proxy stop", noProxy: true},
		{name: "on a runner whose cut failed", proxyErr: errors.New("the runner did not answer the proxy stop"), edit: func(r *types.AgentRun) { r.RunnerID = &runnerID }, connect: true, row: `"teardown_error"`, proxied: 1},
		{name: "on a runner that is not connected", edit: func(r *types.AgentRun) { r.RunnerID = &runnerID }, left: true},
		{name: "virtual machine", edit: func(r *types.AgentRun) { r.MachineKind = types.MachineLinuxVM }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := sweepRun(types.RunFailed, "sbx-unread")
			if tc.edit != nil {
				tc.edit(&run)
			}
			srv, rr, h := newKeptSweep(t, &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{run}}, unreadable: run.ID})
			rr.proxyErr = tc.proxyErr
			if tc.connect {
				srv.cfg.RunnerOnline = func(uuid.UUID) bool { return true }
				seamConnect(t, srv, runnerID, runnerwire.Caps{})
			}
			if tc.noProxy {
				srv.cfg.Runner = rr.sweepAliveRunner
			}
			swept, err := srv.SweepTerminalSandboxes(context.Background())
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			wantSwept, wantStopped := 1, []string{"sbx-unread"}
			if tc.left {
				wantSwept, wantStopped = 0, nil
			}
			if swept != wantSwept || len(rr.proxied) != tc.proxied || !slices.Equal(rr.stopped, wantStopped) {
				t.Errorf("swept %d, StopProxy %v, StopSandbox %v; want %d, %d proxy stops, %v", swept, rr.proxied, rr.stopped, wantSwept, tc.proxied, wantStopped)
			}
			var failures []types.AuditEvent
			for _, ev := range runRows(h.audit.snapshot(), run.ID) {
				if ev.Outcome == "failure" {
					failures = append(failures, ev)
				}
				if strings.Contains(string(ev.Data), "proxy_stop_error") {
					t.Errorf("row %s names proxy_stop_error, want teardown_error alone", ev.Data)
				}
			}
			if tc.row == "" && len(failures) != 0 || tc.row != "" && (len(failures) != 1 || failures[0].Action != "sandbox.sweep" || !strings.Contains(string(failures[0].Data), tc.row)) {
				t.Errorf("failure rows %+v, want %q", failures, tc.row)
			}
			if !slices.Contains(h.broker.revoked, run.ID) {
				t.Errorf("the run's credentials were not revoked: %v", h.broker.revoked)
			}
		})
	}
}
