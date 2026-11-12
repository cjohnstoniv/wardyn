// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// d1Streak is one ContainmentStreakDue call, with the stamp as the store held it then.
type d1Streak struct {
	id    uuid.UUID
	tau   time.Duration
	stamp *time.Time
	at    time.Time
	got   bool
}

// d1Store is containStore plus what the stop path reads: the run row, the failure streak (compared on the
// store's own clock, as the SQL does) and the folder record of the runs a test makes owe a copy back.
type d1Store struct {
	*containStore
	getLag time.Duration      // each GetRun takes this long on the clock
	onGet  func(id uuid.UUID) // runs in each GetRun after its lag, before the read

	fmu          sync.Mutex
	folderRunner uuid.UUID
	foldered     map[uuid.UUID]bool
	status       map[uuid.UUID]*types.RunLocalFolderStatus
	statusErr    error
	seedErr      error
	dueErr       error // the due read fails with it
	giveUps      []uuid.UUID
	notApplied   bool // the give-up statement applies nothing
	otherWriter  bool // the statement applies nothing, and the record is final from another writer
	streaks      []d1Streak
	cleared      []string      // the refs a confirmed stop cleared a failure for
	clearCalls   atomic.Int32  // ClearRunContainmentErrorForRef calls, whether or not they cleared
	giveUpLag    time.Duration // the give-up statement takes this long, ignoring its ctx
	postLag      atomic.Int64  // a failure write takes this many nanoseconds more, ignoring its ctx
	afterSeed    func()        // runs after each seed read that answered, as the run lock is lost in a test
	afterGiveUp  func()        // runs after each give-up statement has returned
}

func newD1Store() *d1Store {
	return &d1Store{containStore: newContainStore(), folderRunner: uuid.New(),
		foldered: map[uuid.UUID]bool{}, status: map[uuid.UUID]*types.RunLocalFolderStatus{}}
}

// owe gives run id a seeded both-ways folder and a record that holds a Resume.
func (f *d1Store) owe(id uuid.UUID) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	st := owedStatus()
	f.status[id], f.foldered[id] = &st, true
}

func (f *d1Store) giveUpCount() int {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	return len(f.giveUps)
}

func (f *d1Store) streakLog() []d1Streak {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	return append([]d1Streak(nil), f.streaks...)
}

// dropRef is a confirmed stop: the ref is gone from the row, as the tail owner leaves it.
func (f *d1Store) dropRef(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.SandboxRef == ref {
			r.SandboxRef = ""
		}
	}
}

func (f *d1Store) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	time.Sleep(f.getLag)
	if err := ctx.Err(); err != nil {
		return types.AgentRun{}, err
	}
	if f.onGet != nil {
		f.onGet(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[id]
	if r == nil {
		return types.AgentRun{}, errors.New("no such run")
	}
	return containRunOf(r.TerminalSandboxRow), nil
}

// SetRunContainmentErrorSince is containStore's, after postLag when a test set one.
func (f *d1Store) SetRunContainmentErrorSince(ctx context.Context, id uuid.UUID, msg string, startedAgo time.Duration, unobservedAgo *time.Duration) (bool, error) {
	time.Sleep(time.Duration(f.postLag.Load()))
	return f.containStore.SetRunContainmentErrorSince(ctx, id, msg, startedAgo, unobservedAgo)
}

// ClearRunContainmentErrorForRef is the store's statement over the rows in memory: the failure is cleared
// only while the row holds ref.
func (f *d1Store) ClearRunContainmentErrorForRef(_ context.Context, id uuid.UUID, ref string) (bool, error) {
	f.clearCalls.Add(1)
	f.mu.Lock()
	r := f.rows[id]
	ok := r != nil && ref != "" && r.SandboxRef == ref && r.msg != ""
	if ok {
		r.msg, r.ContainmentErrorAt = "", nil
	}
	f.mu.Unlock()
	if ok {
		f.fmu.Lock()
		f.cleared = append(f.cleared, ref)
		f.fmu.Unlock()
	}
	return ok, nil
}

// setRef gives run id the sandbox ref ref, as a newer sandbox of the run would.
func (f *d1Store) setRef(id uuid.UUID, ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.rows[id]; r != nil {
		r.SandboxRef = ref
	}
}

// SetSandboxRef is the store write the terminal effects fall back to for a store that is not PG.
func (f *d1Store) SetSandboxRef(_ context.Context, id uuid.UUID, ref string) error {
	f.setRef(id, ref)
	return nil
}

// failure is run id's recorded containment failure message.
func (f *d1Store) failure(id uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.rows[id]; r != nil {
		return r.msg
	}
	return ""
}

// arrive is a pass back that completed: the record holds its Back with nothing left to run.
func (f *d1Store) arrive(id uuid.UUID) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	if st := f.status[id]; st != nil {
		st.Resume = nil
		st.Back = &types.FolderPassRecord{At: time.Now().UTC(), Files: 3, Refused: []types.FolderRefusal{}}
	}
}

// idOfRef is the run that holds ref.
func (f *d1Store) idOfRef(ref string) (uuid.UUID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, r := range f.rows {
		if r.SandboxRef == ref {
			return id, true
		}
	}
	return uuid.Nil, false
}

// ContainmentPastDue is the store's due test over the rows in memory, on the store's own clock, as the SQL
// is on the database's: the server's clock never reaches it.
func (f *d1Store) ContainmentPastDue(ctx context.Context, id uuid.UUID, wait time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.fmu.Lock()
	err := f.dueErr
	f.fmu.Unlock()
	if err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[id]
	if r == nil || !r.State.IsTerminal() {
		return false, nil
	}
	return !time.Now().Before(containRowKey(r.TerminalSandboxRow).Add(containmentLag + wait)), nil
}

// ContainmentStreakDue is the store's statement over the rows in memory, on the store's own clock: the
// streak start is the latest of the row's due, now minus startedAgo and now minus unobservedAgo.
func (f *d1Store) ContainmentStreakDue(ctx context.Context, id uuid.UUID, tau, startedAgo time.Duration, unobservedAgo *time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	r := f.rows[id]
	var stamp *time.Time
	got := false
	now := time.Now()
	if r != nil && r.ContainmentErrorAt != nil {
		at := *r.ContainmentErrorAt
		stamp = &at
		since := containRowKey(r.TerminalSandboxRow).Add(containmentLag)
		if t := now.Add(-startedAgo); t.After(since) {
			since = t
		}
		if unobservedAgo != nil {
			if t := now.Add(-*unobservedAgo); t.After(since) {
				since = t
			}
		}
		got = !at.Before(since) && !at.After(now.Add(-tau))
	}
	f.mu.Unlock()
	f.fmu.Lock()
	f.streaks = append(f.streaks, d1Streak{id: id, tau: tau, stamp: stamp, at: now, got: got})
	f.fmu.Unlock()
	return got, nil
}

func (f *d1Store) RunLocalFolderSeed(ctx context.Context, id uuid.UUID) (*types.RunLocalFolder, store.LocalFolderSeed, error) {
	if err := ctx.Err(); err != nil {
		return nil, store.LocalFolderSeed{}, err
	}
	if f.afterSeed != nil {
		defer f.afterSeed()
	}
	f.fmu.Lock()
	defer f.fmu.Unlock()
	if f.seedErr != nil {
		return nil, store.LocalFolderSeed{}, f.seedErr
	}
	if !f.foldered[id] {
		return nil, store.LocalFolderSeed{}, nil
	}
	seeded := time.Now()
	runnerID := f.folderRunner
	return &types.RunLocalFolder{Name: "work", RemoteDir: "/work", RunnerID: &runnerID, Direction: types.ComputerFolderBoth},
		store.LocalFolderSeed{SeededAt: &seeded}, nil
}

func (f *d1Store) SetRunLocalFolderSeed(context.Context, uuid.UUID, string) (bool, error) {
	return false, nil
}

func (f *d1Store) QueryAuditEvents(context.Context, uuid.UUID, int) ([]types.AuditEvent, error) {
	return nil, nil
}

func (f *d1Store) RunLocalFolderStatus(ctx context.Context, id uuid.UUID) (types.RunLocalFolderStatus, error) {
	if err := ctx.Err(); err != nil {
		return types.RunLocalFolderStatus{}, err
	}
	f.fmu.Lock()
	defer f.fmu.Unlock()
	if f.statusErr != nil {
		return types.RunLocalFolderStatus{}, f.statusErr
	}
	if st := f.status[id]; st != nil {
		return *st, nil
	}
	return types.RunLocalFolderStatus{}, nil
}

func (f *d1Store) SetRunLocalFolderStatusIf(context.Context, uuid.UUID, store.FolderStatusGuard, types.RunLocalFolderStatus) (bool, error) {
	return false, nil
}

func (f *d1Store) BumpRunLocalFolderGen(context.Context, uuid.UUID, store.FolderStatusGuard, time.Time) (types.RunLocalFolderStatus, bool, error) {
	return types.RunLocalFolderStatus{}, false, nil
}

func (f *d1Store) GiveUpRunLocalFolder(_ context.Context, id uuid.UUID, p store.GiveUpParams) (store.GiveUpOutcome, error) {
	if f.afterGiveUp != nil {
		defer f.afterGiveUp()
	}
	time.Sleep(f.giveUpLag)
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.giveUps = append(f.giveUps, id)
	st := f.status[id]
	if st == nil || f.notApplied {
		return store.GiveUpOutcome{}, nil
	}
	back := p.Back
	st.Resume, st.Back = nil, &back
	st.Gen++
	if f.otherWriter {
		return store.GiveUpOutcome{}, nil
	}
	return store.GiveUpOutcome{Applied: true, Gen: st.Gen}, nil
}

func (f *d1Store) MarkGivenUpAudited(context.Context, uuid.UUID, int) (bool, error) { return true, nil }

func (f *d1Store) ListGivenUpUnaudited(context.Context, *uuid.UUID, int) ([]uuid.UUID, *uuid.UUID, error) {
	return nil, nil, nil
}

// d1Runner is containRunner plus a StopSandbox and an ExecStream the test steers. stopFn answers the nth
// StopSandbox of a ref; execFn runs before the "cat" (agentStoppable) and "script" (stopAgentForPass) execs
// of a ref and fails or delays them; onCall sees every cut and stop call as it begins and returns what to
// run as it ends. afterStop runs after a StopSandbox that answered nil.
type d1Runner struct {
	*containRunner
	stopFn    func(ctx context.Context, ref string, n int) error
	execFn    func(ctx context.Context, what, ref string) error
	onCall    func(ref, what string) func()
	afterStop func(ref string)
	stopAt    map[string][]time.Time
	stopEnd   map[string][]time.Time // returns that answered nil
	execN     map[string]int
	claimed   func(ref, what string) // checks the run's claim as a cut or stop call begins
}

func newD1Runner(rn *containRunner) *d1Runner {
	return &d1Runner{containRunner: rn, stopAt: map[string][]time.Time{}, stopEnd: map[string][]time.Time{},
		execN: map[string]int{}}
}

func (r *d1Runner) enter(ref, what string) func() {
	r.mu.Lock()
	f, check := r.onCall, r.claimed
	r.mu.Unlock()
	if check != nil {
		check(ref, what)
	}
	if f == nil {
		return func() {}
	}
	return f(ref, what)
}

func (r *d1Runner) StopSandbox(ctx context.Context, ref string) error {
	defer r.enter(ref, "stop")()
	r.mu.Lock()
	r.stopAt[ref] = append(r.stopAt[ref], time.Now())
	n, fn, after := len(r.stopAt[ref]), r.stopFn, r.afterStop
	r.mu.Unlock()
	var err error
	if fn != nil {
		err = fn(ctx, ref, n)
	}
	if err == nil {
		r.mu.Lock()
		r.stopEnd[ref] = append(r.stopEnd[ref], time.Now())
		r.mu.Unlock()
		if after != nil {
			after(ref)
		}
	}
	return err
}

func (r *d1Runner) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	what, out := "cat", "sh\x00-c\x00"+runner.AgentIdleScript
	if spec.Argv[0] == "sh" {
		what, out = "script", agentStoppedMark+"\n"
	}
	r.mu.Lock()
	r.execN[what+" "+ref]++
	fn := r.execFn
	r.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, what, ref); err != nil {
			return nil, err
		}
	}
	return &runner.ExecSession{Stdout: strings.NewReader(out), Wait: func() (int, error) { return 0, nil }}, nil
}

func (r *d1Runner) execCount(what, ref string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.execN[what+" "+ref]
}

func (r *d1Runner) stopCount(ref string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stopAt[ref])
}

func (r *d1Runner) totalStops() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, at := range r.stopAt {
		n += len(at)
	}
	return n
}

// firstStop is when ref's first StopSandbox began and, when it answered nil, returned.
func (r *d1Runner) firstStop(ref string) (began, ended time.Time, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stopAt[ref]) == 0 {
		return began, ended, false
	}
	began = r.stopAt[ref][0]
	if len(r.stopEnd[ref]) > 0 {
		ended = r.stopEnd[ref][0]
	}
	return began, ended, true
}

// d1Fencer is d1Runner that also fences.
type d1Fencer struct{ *d1Runner }

func (r d1Fencer) FenceProxy(ctx context.Context, ref string) error {
	defer r.enter(ref, "cut")()
	r.mu.Lock()
	r.fences++
	r.mu.Unlock()
	return r.call(ctx, ref)
}

// d1Lab is a containLab whose server runs the real stop path over a d1Store and a d1Runner.
type d1Lab struct {
	*containLab
	ds   *d1Store
	dr   *d1Runner
	srvs *d1Servers
}

// d1Servers are the servers that share one runner, for the check that every cut and stop call is made under
// the run's claim.
type d1Servers struct {
	mu   sync.Mutex
	list []*Server
}

// claim reports whether any server holds id, and whether one holds it for the stop pool.
func (d *d1Servers) claim(id uuid.UUID) (busy, stopPool bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, srv := range d.list {
		c := &srv.contain
		c.mu.Lock()
		e := c.inFlight[id]
		c.mu.Unlock()
		if e != nil {
			busy = true
			stopPool = stopPool || e.pool == containPoolStop
		}
	}
	return busy, stopPool
}

func newD1Lab(t *testing.T, fencer bool) *d1Lab {
	t.Helper()
	ds := newD1Store()
	l := newContainLabOn(t, fencer, ds.containStore, &seamRecorder{})
	dr := newD1Runner(l.rn)
	dr.afterStop = ds.dropRef
	var rn runner.Runner = dr
	if fencer {
		rn = d1Fencer{dr}
	}
	l.srv.cfg.Store, l.srv.cfg.Runner = ds, rn
	l.srv.contain.d1DueFn = nil
	srvs := &d1Servers{list: []*Server{l.srv}}
	// The stop item re-reads its run under the stop pool's claim.
	ds.onGet = func(id uuid.UUID) {
		if busy, stopPool := srvs.claim(id); !busy || !stopPool {
			t.Errorf("the stop item re-read run %s while the stop pool did not hold it", id)
		}
	}
	// Every cut and stop call is made while its run is claimed, and a StopSandbox only by the stop pool.
	dr.claimed = func(ref, what string) {
		id, ok := ds.idOfRef(ref)
		if !ok {
			return
		}
		busy, stopPool := srvs.claim(id)
		if !busy {
			t.Errorf("a %s call for run %s was made with no claim on it", what, id)
		}
		if what == "stop" && !stopPool {
			t.Errorf("a stop call for run %s was made while the stop pool did not hold it", id)
		}
	}
	return &d1Lab{containLab: l, ds: ds, dr: dr, srvs: srvs}
}

// sharing is a second server over the same store and runner, as another replica.
func (l *d1Lab) sharing(t *testing.T) *d1Lab {
	t.Helper()
	other := newContainLabOn(t, true, l.ds.containStore, &seamRecorder{})
	other.srv.cfg.Store, other.srv.cfg.Runner = l.srv.cfg.Store, l.srv.cfg.Runner
	other.srv.contain.d1DueFn = nil
	l.srvs.mu.Lock()
	l.srvs.list = append(l.srvs.list, other.srv)
	l.srvs.mu.Unlock()
	return &d1Lab{containLab: other, ds: l.ds, dr: l.dr, srvs: l.srvs}
}

// row adds an ended STOPPED run, due a minute ago, of the machine kind on runnerID.
func (l *d1Lab) row(runnerID *uuid.UUID, kind types.MachineKind) store.TerminalSandboxRow {
	r := dueRow(runnerID, time.Minute)
	r.State, r.MachineKind = types.RunStopped, kind
	l.st.add(r)
	return r
}

// item runs one cut item for the run, as the containment loop would, in the caller.
func (l *d1Lab) item(row store.TerminalSandboxRow) bool {
	gen, ok := l.srv.containClaim(row.ID, containPoolCut)
	if !ok {
		return false
	}
	l.srv.containCutItem(context.Background(), containRunOf(row), gen)
	return true
}

// startStops starts the stop workers alone.
func (l *d1Lab) startStops() {
	s := l.srv
	ctx := s.watcherBaseCtx()
	s.contain.init()
	for range containStopWorkers {
		s.goBackground(func() { s.containStopWorker(ctx) })
	}
}

// d1Sleep waits for d, or for ctx to end.
func d1Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// d1CutHangs is a cut that takes its whole call phase and then fails.
func d1CutHangs(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// d1StopTakes is a StopSandbox that answers nil after d.
func d1StopTakes(d time.Duration) func(context.Context, string, int) error {
	return func(ctx context.Context, _ string, _ int) error { return d1Sleep(ctx, d) }
}

// d1Logs collects what slog writes while a test runs.
type d1Logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *d1Logs) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *d1Logs) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func d1CaptureLogs(t *testing.T) *d1Logs {
	t.Helper()
	logs := &d1Logs{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return logs
}

// A failure at t, then one at t + tau, is a streak: one give-up and one stop. One at t + tau/2 is not.
func TestContain_D1FallbackStopsAfterTwoFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, "")
		l.ds.owe(row.ID)
		l.rn.setHook(func(context.Context, string) error { return errContainBoom })
		l.startStops()
		l.item(row)
		time.Sleep(containTick / 2)
		l.item(row)
		if got := l.stopQueue(); got != 0 || l.busy(row.ID) {
			t.Fatalf("after a second failure tau/2 on: stop queue %d, busy %v; want nothing handed off", got, l.busy(row.ID))
		}
		time.Sleep(containTick / 2)
		l.item(row)
		time.Sleep(containD1)
		l.stop()
		if n := l.ds.giveUpCount(); n != 1 {
			t.Errorf("give-ups = %d, want 1", n)
		}
		if n := l.dr.stopCount(row.SandboxRef); n != 1 {
			t.Errorf("StopSandbox calls = %d, want 1", n)
		}
	})
}

// A run with no contain call is stopped at the first tick past its due, and not before.
func TestContain_UnsupportedStopsOnFirstTickPastDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, types.MachineLinuxVM)
		end := time.Now().Add(-containmentLag + 25*time.Second)
		row.EndedAt, row.UpdatedAt = &end, end
		l.st.add(row)
		due := end.Add(containmentLag)
		run := containRunOf(row)
		l.start()
		time.Sleep(time.Until(due) - time.Second)
		if l.srv.d1Due(context.Background(), run, cutUnsupported, nil) || l.dr.totalStops() != 0 {
			t.Errorf("due - 1s: d1Due or a stop came early (stops %d)", l.dr.totalStops())
		}
		time.Sleep(time.Second)
		if !l.srv.d1Due(context.Background(), run, cutUnsupported, nil) {
			t.Error("at the due: d1Due = false, want true")
		}
		time.Sleep(containTick)
		l.stop()
		began, _, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || began.Before(due) || began.Sub(due) > containTick {
			t.Errorf("StopSandbox began at due + %v (called %v), want within [0, %v]", began.Sub(due), ok, containTick)
		}
	})
}

// A give-up that applied nothing and a record that is not done is not final: the sandbox is not stopped.
func TestContain_GiveUpNotAppliedAndNotDoneDoesNotStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, types.MachineLinuxVM)
		l.ds.owe(row.ID)
		l.ds.notApplied = true
		l.startStops()
		l.item(row)
		time.Sleep(containD1Unsupported)
		l.stop()
		if n := l.ds.giveUpCount(); n != 1 {
			t.Errorf("give-up statements = %d, want 1", n)
		}
		if n := l.dr.totalStops(); n != 0 {
			t.Errorf("StopSandbox calls = %d, want 0: zero rows is not done", n)
		}
	})
}

// A give-up that applied nothing, with the record final from another writer, is final: the sandbox is stopped.
func TestContain_GiveUpNotAppliedButDoneStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, types.MachineLinuxVM)
		l.ds.owe(row.ID)
		l.ds.otherWriter = true
		l.startStops()
		l.item(row)
		time.Sleep(containD1Unsupported)
		l.stop()
		if n := l.dr.stopCount(row.SandboxRef); n != 1 {
			t.Errorf("StopSandbox calls = %d, want 1", n)
		}
	})
}

// A last cut that holds ends the item: no give-up, no stop, the failure cleared and the run fenced.
func TestContain_LastFenceSucceedsNoGiveUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, "")
		l.ds.owe(row.ID)
		var calls atomic.Int32
		l.rn.setHook(func(context.Context, string) error {
			if calls.Add(1) > 2 {
				return nil
			}
			return errContainBoom
		})
		l.startStops()
		l.item(row)
		time.Sleep(containTick)
		l.item(row)
		time.Sleep(containD1)
		l.stop()
		if l.ds.giveUpCount() != 0 || l.dr.totalStops() != 0 {
			t.Errorf("give-ups %d, stops %d, want none after a last cut that held", l.ds.giveUpCount(), l.dr.totalStops())
		}
		if l.st.row(row.ID).FencedAt == nil || l.st.stamp(row.ID) != nil {
			t.Errorf("fenced %v, stamp %v; want fenced with the failure cleared", l.st.row(row.ID).FencedAt, l.st.stamp(row.ID))
		}
	})
}

// A fence that takes its whole deadline still has its failure recorded by the post phase, so the loop's
// second round counts and the run is stopped.
func TestContain_DeadlineConsumingFenceStillRecords(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, "")
		l.rn.setHook(d1CutHangs)
		l.start()
		time.Sleep(6 * containItem)
		l.stop()
		if l.st.stampWrites() < 2 || l.st.stamp(row.ID) == nil {
			t.Errorf("stamp writes %d, stamp %v; want each deadline-consuming cut recorded", l.st.stampWrites(), l.st.stamp(row.ID))
		}
		if n := l.dr.stopCount(row.SandboxRef); n != 1 {
			t.Errorf("StopSandbox calls = %d, want 1", n)
		}
	})
}

// A StopSandbox that takes 59 s answers within its phase: observed, not a failure.
func TestContain_SlowStopSandboxIsObserved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, types.MachineLinuxVM)
		l.dr.stopFn = d1StopTakes(59 * time.Second)
		l.startStops()
		l.item(row)
		time.Sleep(containD1Unsupported)
		l.stop()
		if _, ended, ok := l.dr.firstStop(row.SandboxRef); !ok || ended.IsZero() || l.st.stampWrites() != 0 {
			t.Errorf("stop called %v, answered at %v, failure writes %d; want an observed stop and no write", ok, ended, l.st.stampWrites())
		}
	})
}

// A macOS VM's StopSandbox has 90 s: 89 s is observed, where a Linux VM's phase would give it up at 60 s.
func TestContain_MacStopUsesItsBudget(t *testing.T) {
	for kind, wantWrites := range map[types.MachineKind]int{types.MachineMacOSVM: 0, types.MachineLinuxVM: 1} {
		synctest.Test(t, func(t *testing.T) {
			l := newD1Lab(t, true)
			row := l.row(nil, kind)
			l.dr.stopFn = func(_ context.Context, _ string, _ int) error {
				time.Sleep(89 * time.Second)
				return nil
			}
			l.startStops()
			l.item(row)
			time.Sleep(containD1Unsupported + 40*time.Second)
			l.stop()
			if got := l.st.stampWrites(); got != wantWrites {
				t.Errorf("%s: failure writes = %d after an 89 s stop, want %d", kind, got, wantWrites)
			}
		})
	}
}

// d1Rows adds n ended runs, due a minute ago and one millisecond apart so no two share an end time, and the
// order the loop lists them in is fixed. kindOf says each one's machine kind. The kinds are returned by ref.
func d1Rows(l *d1Lab, n int, runnerID *uuid.UUID, kindOf func(i int) types.MachineKind) (rows []store.TerminalSandboxRow, kinds map[string]types.MachineKind) {
	kinds = map[string]types.MachineKind{}
	for i := range n {
		kind := types.MachineKind("")
		if kindOf != nil {
			kind = kindOf(i)
		}
		r := l.rowDue(-time.Minute-time.Duration(i)*time.Millisecond, runnerID, kind)
		rows, kinds[r.SandboxRef] = append(rows, r), kind
	}
	return rows, kinds
}

// d1Bound runs a lab over rows whose cuts take the whole call phase less a millisecond per row and fail,
// and whose stops take a phase less a millisecond per row, so no two calls end at the same instant, and
// answers how long after the first listing each ref's stop returned.
func d1Bound(t *testing.T, l *d1Lab, rows []store.TerminalSandboxRow, kinds map[string]types.MachineKind, horizon time.Duration) (last time.Duration) {
	t.Helper()
	l.st.lag = 4 * time.Second
	l.ds.getLag = 4 * time.Second
	index := map[string]int{}
	for i, r := range rows {
		index[r.SandboxRef] = i
	}
	l.rn.setHook(func(ctx context.Context, ref string) error {
		if err := d1Sleep(ctx, containCall-time.Duration(index[ref]+1)*time.Millisecond); err != nil {
			return err
		}
		return errContainBoom
	})
	l.dr.stopFn = func(ctx context.Context, ref string, _ int) error {
		d := d1StopPhase
		if kinds[ref].IsMacOSVM() {
			d = d1StopPhaseMac
		}
		return d1Sleep(ctx, d-time.Duration(index[ref]+1)*time.Millisecond)
	}
	start := time.Now()
	l.start()
	time.Sleep(horizon)
	l.stop()
	for _, r := range rows {
		_, ended, ok := l.dr.firstStop(r.SandboxRef)
		if !ok || ended.IsZero() {
			t.Fatalf("run %s was never stopped within %v", r.ID, horizon)
		}
		last = max(last, ended.Sub(start))
	}
	return last
}

// d1BoundTwice runs one bound scenario in two separate fake-clock bubbles, and both runs must end inside
// [lo, hi]. The two need not end at the same instant: cut items can end at one fake instant (rows due a
// whole ms apart, VM cut items on the tick), and the stop FIFO then takes them in goroutine order, which
// moves the last stop by a few ms. That order is unspecified by design, so only the bound is pinned.
func d1BoundTwice(t *testing.T, lo, hi time.Duration, scenario func(t *testing.T) time.Duration) {
	t.Helper()
	var got [2]time.Duration
	for i := range got {
		synctest.Test(t, func(t *testing.T) { got[i] = scenario(t) })
	}
	for i, g := range got {
		if g < lo || g > hi {
			t.Errorf("run %d: the last stop returned at +%v, want in [%v, %v]", i+1, g, lo, hi)
		}
	}
	t.Logf("last stop at +%v and +%v", got[0], got[1])
}

// 100 containers whose cuts fail at full length in two rounds: the last stop lands within 3895 s of the
// first listing, because stops run on their own workers.
func TestContain_HundredFailedFencesBound(t *testing.T) {
	d1BoundTwice(t, 2000*time.Second, 3895*time.Second, func(t *testing.T) time.Duration {
		l := newD1Lab(t, true)
		rows, kinds := d1Rows(l, 100, nil, nil)
		return d1Bound(t, l, rows, kinds, 4000*time.Second)
	})
}

// 3 macOS VMs (90 s stops) among 97 containers: the VMs queue behind no container.
func TestContain_MixedStopKindsBound(t *testing.T) {
	d1BoundTwice(t, 2000*time.Second, 3895*time.Second, func(t *testing.T) time.Duration {
		l := newD1Lab(t, true)
		rows, kinds := d1Rows(l, 100, nil, func(i int) types.MachineKind {
			if i < 3 {
				return types.MachineMacOSVM
			}
			return ""
		})
		return d1Bound(t, l, rows, kinds, 4000*time.Second)
	})
}

// 50 macOS VMs and 50 containers: every VM takes its cut round first, and no stop worker idles while a
// container waits.
func TestContain_HalfVMHalfContainersBound(t *testing.T) {
	d1BoundTwice(t, 2000*time.Second, 3000*time.Second, func(t *testing.T) time.Duration {
		l := newD1Lab(t, true)
		rows, kinds := d1Rows(l, 100, nil, func(i int) types.MachineKind {
			if i%2 == 0 {
				return types.MachineMacOSVM
			}
			return ""
		})
		return d1Bound(t, l, rows, kinds, 3200*time.Second)
	})
}

// 10 runner container runs whose cuts fail at full length: each is stopped within 485 s of the first
// listing; with 5 the bound is 390 s, and no second failure under a minute after the first counts.
func TestContain_TenRunnerContainersBound(t *testing.T) {
	for n, bound := range map[int]time.Duration{10: 485 * time.Second, 5: 390 * time.Second} {
		synctest.Test(t, func(t *testing.T) {
			l := newD1Lab(t, true)
			runnerID := l.connect(true)
			rows, kinds := d1Rows(l, n, &runnerID, nil)
			last := d1Bound(t, l, rows, kinds, 600*time.Second)
			if last > bound {
				t.Errorf("%d runner runs: the last stop returned at +%v, want within %v", n, last, bound)
			}
			t.Logf("%d runner runs: last stop at +%v", n, last)
			early := false
			for _, c := range l.ds.streakLog() {
				if c.tau != containStreakRunner {
					t.Errorf("%d runner runs: streak spacing %v, want %v", n, c.tau, containStreakRunner)
				}
				if c.got && c.at.Sub(*c.stamp) < containStreakRunner {
					t.Errorf("%d runner runs: a streak counted %v after the first failure", n, c.at.Sub(*c.stamp))
				}
				if !c.got && c.stamp != nil && c.at.Sub(*c.stamp) < containStreakRunner {
					early = true
				}
			}
			if n == 5 && !early {
				t.Error("5 runner runs: no second failure came under a minute after the first, so the spacing was not exercised")
			}
		})
	}
}

// 100 runs claimed by the stop pool do not slow 100 new contain-only runs: each is cut within 760 s.
func TestContain_ContainOnlyNotBehindStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		l.st.lag = 4 * time.Second
		for range 100 {
			r := l.row(nil, "")
			if _, ok := l.srv.containClaim(r.ID, containPoolStop); !ok {
				t.Fatal("the stop pool could not claim a run")
			}
		}
		var fresh []store.TerminalSandboxRow
		for range 100 {
			fresh = append(fresh, l.row(nil, ""))
		}
		l.rn.setHook(func(ctx context.Context, _ string) error { return d1Sleep(ctx, containCall-time.Millisecond) })
		start := time.Now()
		l.start()
		time.Sleep(containTick + 25*containItem + 10*time.Second)
		l.stop()
		var last time.Duration
		for _, r := range fresh {
			at, ok := l.st.fenceAt[r.ID]
			if !ok {
				t.Fatalf("run %s was never cut", r.ID)
			}
			last = max(last, at.Sub(start))
		}
		if bound := containTick + 25*containItem; last > bound || last < 15*containItem {
			t.Errorf("the last run was cut at +%v, want in [%v, %v]", last, 15*containItem, bound)
		}
		if fences, _, _ := l.rn.counts(); fences != 100 {
			t.Errorf("fences = %d, want 100: a run held by the stop pool was cut", fences)
		}
	})
}

// The continuation's last cut answers not observed: no give-up, no stop, no write, and the streak starts again.
func TestContain_LastCutNotObservedEndsTheItem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		runnerID := l.connect(true)
		row := l.row(&runnerID, "")
		l.ds.owe(row.ID)
		var calls atomic.Int32
		l.rn.setHook(func(context.Context, string) error {
			if calls.Add(1) == 3 {
				return runner.ErrRunnerOffline
			}
			return errContainBoom
		})
		l.startStops()
		l.item(row)
		time.Sleep(containStreakRunner + time.Second)
		l.item(row)
		writes := l.st.stampWrites()
		time.Sleep(containD1)
		if l.ds.giveUpCount() != 0 || l.dr.totalStops() != 0 || l.st.stampWrites() != writes {
			t.Errorf("give-ups %d, stops %d, writes %d -> %d; want none after a last cut that was not observed",
				l.ds.giveUpCount(), l.dr.totalStops(), writes, l.st.stampWrites())
		}
		l.srv.contain.mu.Lock()
		marked := !l.srv.contain.unobserved[row.ID].IsZero()
		l.srv.contain.mu.Unlock()
		if !marked {
			t.Error("the run's unobserved time was not set")
		}
		l.item(row)
		time.Sleep(containD1)
		l.stop()
		if l.stopQueue() != 0 || l.dr.totalStops() != 0 || l.ds.giveUpCount() != 0 {
			t.Error("the first failure after the gap was counted with the one before it")
		}
	})
}

// A runner run's second failure counts only a full missed-pong window after the first; a hosted run's, a tick.
func TestContain_RunnerStreakWaitsOutTheMissedPongWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		runnerID := l.connect(true)
		l.rn.setHook(func(context.Context, string) error { return errContainBoom })
		slow := l.row(&runnerID, "")
		l.item(slow)
		time.Sleep(15 * time.Second)
		l.item(slow)
		time.Sleep(15 * time.Second)
		l.item(slow)
		if got := l.stopQueue(); got != 0 {
			t.Errorf("runner run failing at 0, 15 s and 30 s: stop queue %d, want 0", got)
		}
		late := l.row(&runnerID, "")
		l.item(late)
		time.Sleep(61 * time.Second)
		l.item(late)
		if got := l.stopQueue(); got != 1 {
			t.Errorf("runner run failing at 0 and 61 s: stop queue %d, want 1", got)
		}
		hosted := l.row(nil, "")
		l.item(hosted)
		time.Sleep(containTick)
		l.item(hosted)
		if got := l.stopQueue(); got != 2 {
			t.Errorf("hosted run failing at 0 and %v: stop queue %d, want 2", containTick, got)
		}
		l.stop()
	})
}

// d1ReadFails are the two ways reading a run's folder record can fail: the status read, and the seed read
// that deciding whether a copy back is owed makes. Each sets the failure to err, or clears it with nil.
var d1ReadFails = map[string]func(f *d1Store, err error){
	"status": func(f *d1Store, err error) {
		f.fmu.Lock()
		defer f.fmu.Unlock()
		f.statusErr = err
	},
	"seed": func(f *d1Store, err error) {
		f.fmu.Lock()
		defer f.fmu.Unlock()
		f.seedErr = err
	},
}

// A folder record that cannot be read stops nothing and gives nothing up, for a streaked run and for a
// run with no contain call; the next tick with a good read stops them.
func TestContain_StatusReadErrorSkipsD1(t *testing.T) {
	logs := d1CaptureLogs(t)
	for name, fail := range d1ReadFails {
		synctest.Test(t, func(t *testing.T) {
			l := newD1Lab(t, true)
			l.rn.setHook(func(context.Context, string) error { return errContainBoom })
			streaked := l.row(nil, "")
			vm := l.row(nil, types.MachineLinuxVM)
			fail(l.ds, errors.New("database down"))
			l.item(streaked)
			time.Sleep(containTick)
			l.item(streaked)
			l.item(vm)
			if got := l.stopQueue(); got != 0 || l.ds.giveUpCount() != 0 {
				t.Errorf("%s read failing: stop queue %d, give-ups %d; want none", name, got, l.ds.giveUpCount())
			}
			if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, streaked.ID.String()) || !strings.Contains(out, vm.ID.String()) {
				t.Errorf("%s read failing: the skipped rows were not logged at warn with their run ids:\n%s", name, out)
			}
			fail(l.ds, nil)
			time.Sleep(containTick)
			l.item(streaked)
			l.item(vm)
			if got := l.stopQueue(); got != 2 {
				t.Errorf("%s read good again: stop queue %d, want 2", name, got)
			}
			l.stop()
		})
	}
}

// A folder record that cannot be read in the stop item's give-up phase, after the hand-off, gives nothing up
// and stops nothing: a read that fails is not a record that is final. The run is left claimed by no one.
func TestContain_GiveUpPhaseReadErrorStopsNothing(t *testing.T) {
	logs := d1CaptureLogs(t)
	for name, fail := range d1ReadFails {
		synctest.Test(t, func(t *testing.T) {
			l := newD1Lab(t, true)
			l.rn.setHook(func(context.Context, string) error { return errContainBoom })
			row := l.row(nil, "")
			l.ds.owe(row.ID)
			// The reads fail from the stop item's own re-read on: the hand-off was decided with good reads.
			held := l.ds.onGet
			l.ds.onGet = func(id uuid.UUID) {
				held(id)
				fail(l.ds, errors.New("database down"))
			}
			l.startStops()
			l.item(row)
			time.Sleep(containTick)
			l.item(row)
			time.Sleep(containD1)
			l.stop()
			if n, g := l.dr.totalStops(), l.ds.giveUpCount(); n != 0 || g != 0 {
				t.Errorf("%s read failing in the give-up phase: stops %d, give-ups %d; want none", name, n, g)
			}
			if !strings.Contains(logs.String(), row.ID.String()) {
				t.Errorf("%s: the failed read was not logged with the run id", name)
			}
			if l.busy(row.ID) {
				t.Errorf("%s: the run is still claimed after the item ended", name)
			}
		})
	}
}

// A run handed to the stop pool is never claimed for a cut meanwhile: it is cut three times in all.
func TestContain_HandOffLeavesNoGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, "")
		l.rn.setHook(func(context.Context, string) error { return errContainBoom })
		l.dr.stopFn = d1StopTakes(50 * time.Second)
		l.start()
		time.Sleep(3 * containD1)
		l.stop()
		if n := l.rn.callsTo(row.SandboxRef); n != 3 {
			t.Errorf("cuts = %d, want 3: two failures and the last cut", n)
		}
		if l.rn.twoAtOnce {
			t.Error("a run had two cuts out at once")
		}
		if n := l.dr.stopCount(row.SandboxRef); n != 1 {
			t.Errorf("StopSandbox calls = %d, want 1", n)
		}
	})
}

// A failure stamped before the run's due is a kept-phase failure: the first failure past due starts the streak.
func TestContain_D1IgnoresAStampOlderThanDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		r := dueRow(nil, time.Minute)
		old := containRowKey(r).Add(-time.Minute)
		r.ContainmentErrorAt = &old
		l.st.add(r)
		l.rn.setHook(func(context.Context, string) error { return errContainBoom })
		l.item(r)
		time.Sleep(time.Second)
		l.item(r)
		if got := l.stopQueue(); got != 0 {
			t.Errorf("stop queue = %d, want 0: the old stamp was counted", got)
		}
		if st := l.st.stamp(r.ID); st == nil || st.Before(containRowKey(r).Add(containmentLag)) {
			t.Errorf("stamp = %v, want one past the due", st)
		}
		l.stop()
	})
}

// A runner that is not connected is never cut, counted or stopped, however long past due.
func TestContain_OfflineRunnerNeverReachesD1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		runnerID := l.connect(true)
		l.online.Store(false)
		r := dueRow(&runnerID, time.Hour)
		r.State = types.RunStopped
		l.st.add(r)
		l.ds.owe(r.ID)
		l.start()
		time.Sleep(10 * containTick)
		l.stop()
		if fences, _, _ := l.rn.counts(); fences != 0 || l.dr.totalStops() != 0 || l.ds.giveUpCount() != 0 {
			t.Errorf("fences %d, stops %d, give-ups %d; want none for a runner that is not here", fences, l.dr.totalStops(), l.ds.giveUpCount())
		}
	})
}

// Two replicas: A's failure and B's, a tick later, make one streak and one stop.
func TestContain_StreakTwoReplicas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newD1Lab(t, true)
		b := a.sharing(t)
		row := a.row(nil, "")
		a.rn.setHook(func(context.Context, string) error { return errContainBoom })
		b.startStops()
		a.item(row)
		time.Sleep(containTick)
		b.item(row)
		time.Sleep(containD1)
		a.stop()
		b.stop()
		if a.stopQueue() != 0 {
			t.Error("replica A queued a stop")
		}
		if n := a.dr.stopCount(row.SandboxRef); n != 1 {
			t.Errorf("StopSandbox calls = %d, want 1", n)
		}
	})
}

// A StopSandbox that ignores its ctx is a failed stop at its phase's end and frees the stop worker, but the
// run stays claimed until the call returns. So does a last cut that ignores its ctx, and the item still stops.
func TestContain_StopPastItsPhaseFreesTheStopWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, types.MachineLinuxVM)
		l.dr.stopFn = func(context.Context, string, int) error {
			time.Sleep(120 * time.Second)
			return nil
		}
		l.startStops()
		l.item(row)
		began := time.Now()
		time.Sleep(100 * time.Second)
		l.st.mu.Lock()
		stamps := append([]time.Time(nil), l.st.stamps...)
		l.st.mu.Unlock()
		if len(stamps) != 1 || stamps[0].Sub(began) > containD1 {
			t.Errorf("failed-stop writes at %v after the item began, want one within %v", stamps, containD1)
		}
		if !l.busy(row.ID) {
			t.Error("the run was released while its StopSandbox was still out")
		}
		time.Sleep(25 * time.Second)
		if l.busy(row.ID) {
			t.Error("the run is still claimed after its StopSandbox returned")
		}
		l.stop()
	})
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, "")
		l.ds.owe(row.ID)
		var calls atomic.Int32
		l.rn.setHook(func(context.Context, string) error {
			if calls.Add(1) == 3 {
				time.Sleep(120 * time.Second)
			}
			return errContainBoom
		})
		l.startStops()
		l.item(row)
		time.Sleep(containTick)
		l.item(row)
		time.Sleep(containD1)
		if l.ds.giveUpCount() != 1 || l.dr.totalStops() != 1 {
			t.Errorf("give-ups %d, stops %d after an abandoned last cut, want 1 each", l.ds.giveUpCount(), l.dr.totalStops())
		}
		if !l.busy(row.ID) {
			t.Error("the run was released while its last cut was still out")
		}
		time.Sleep(30 * time.Second)
		if l.busy(row.ID) {
			t.Error("the run is still claimed after its last cut returned")
		}
		l.stop()
	})
}

// A call that stalls for a minute past its phase: the stop pool never has more than five calls out for each
// stop worker, both pools together never more than 40, and no run more than two, and two only inside its
// stop item.
func TestContain_HungStopCallsInFlightBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		runnerID := l.connect(true)
		ids := map[string]uuid.UUID{}
		for range 60 {
			r := l.row(&runnerID, "")
			ids[r.SandboxRef] = r.ID
		}
		for i := range 40 {
			kind := types.MachineLinuxVM
			if i%2 == 0 {
				kind = types.MachineMacOSVM
			}
			r := l.row(nil, kind)
			ids[r.SandboxRef] = r.ID
		}
		hang := func(ctx context.Context) error {
			<-ctx.Done()
			time.Sleep(60 * time.Second)
			return errContainBoom
		}
		l.rn.setHook(func(ctx context.Context, _ string) error { return hang(ctx) })
		l.dr.stopFn = func(ctx context.Context, _ string, _ int) error { return hang(ctx) }
		var mu sync.Mutex
		var all, inStop, peakAll, peakStop, peakRun int
		perRun := map[string]int{}
		outside := false
		l.dr.onCall = func(ref, _ string) func() {
			c := &l.srv.contain
			c.mu.Lock()
			stop := c.inFlight[ids[ref]] != nil && c.inFlight[ids[ref]].pool == containPoolStop
			c.mu.Unlock()
			mu.Lock()
			defer mu.Unlock()
			all++
			perRun[ref]++
			if stop {
				inStop++
			}
			peakAll, peakStop, peakRun = max(peakAll, all), max(peakStop, inStop), max(peakRun, perRun[ref])
			if perRun[ref] > 1 && !stop {
				outside = true
			}
			return func() {
				mu.Lock()
				defer mu.Unlock()
				all--
				perRun[ref]--
				if stop {
					inStop--
				}
			}
		}
		l.start()
		time.Sleep(300 * containTick)
		l.stop()
		mu.Lock()
		defer mu.Unlock()
		if bound := containStopWorkers * 5; peakStop > bound || peakStop <= containStopWorkers {
			t.Errorf("stop-pool calls in flight peaked at %d, want in (%d, %d]", peakStop, containStopWorkers, bound)
		}
		if bound := (containWorkers + containStopWorkers) * 5; peakAll > bound {
			t.Errorf("calls in flight peaked at %d, want at most %d", peakAll, bound)
		}
		if peakRun > 2 || outside {
			t.Errorf("one run had %d calls out (two outside its stop item: %v), want at most 2, inside the stop item", peakRun, outside)
		}
	})
}

// d1LateLab is one connected runner container run whose every fence answers late by lateAfter with
// late(n) for the nth call, over a store whose writes take lag. The first listing is the lab's start.
func d1LateLab(t *testing.T, lateAfter time.Duration, lag time.Duration, late func(n int) error) (*d1Lab, store.TerminalSandboxRow, *atomic.Int32) {
	t.Helper()
	l := newD1Lab(t, true)
	runnerID := l.connect(true)
	row := l.row(&runnerID, "")
	l.st.lag = lag
	var calls atomic.Int32
	l.rn.setHook(func(ctx context.Context, _ string) error {
		n := int(calls.Add(1))
		<-ctx.Done()
		time.Sleep(lateAfter)
		return late(n)
	})
	l.dr.stopFn = d1StopTakes(10 * time.Second)
	return l, row, &calls
}

// A fence whose deadline error comes 15 s after its phase. The run is on a runner, so a second failure
// counts only a minute (containStreakRunner) after the first, and the call phase is 20 s. The cut item's pre
// phase reads nothing here and takes no time, so the times are:
//   - round one starts at 0, its call is abandoned at 20 s and the first failure is stamped then; the call
//     answers at 35 s and the run is released;
//   - the next visit is at 40 s: round two is abandoned at 60 s, only 40 s after the first stamp, so
//     d1Due is false; its call answers at 75 s;
//   - round three starts at 80 s and is abandoned at 100 s, 80 s after the stamp, so d1Due is true; its call
//     answers at 115 s and that answer is the hand-off;
//   - the stop item cuts once more from 115 s (abandoned at 135 s), has nothing to give up, and calls
//     StopSandbox at 135 s, which takes 10 s and returns at 145 s.
//
// So d1Due is asked 3 times, there are 4 fence calls in all, and never more than two calls are out for the run.
func TestContain_LateDeadlineErrorsStillReachTheStop(t *testing.T) {
	deadline := func(int) error { return context.DeadlineExceeded }
	synctest.Test(t, func(t *testing.T) {
		l, row, calls := d1LateLab(t, 15*time.Second, 0, deadline)
		var asked atomic.Int32
		l.srv.contain.d1DueFn = func(ctx context.Context, run types.AgentRun, kind cutKind, err error) bool {
			asked.Add(1)
			return l.srv.d1Due(ctx, run, kind, err)
		}
		var mu sync.Mutex
		peak, out := 0, 0
		l.dr.onCall = func(string, string) func() {
			mu.Lock()
			defer mu.Unlock()
			out++
			peak = max(peak, out)
			return func() {
				mu.Lock()
				defer mu.Unlock()
				out--
			}
		}
		start := time.Now()
		l.start()
		time.Sleep(300 * time.Second)
		l.stop()
		began, ended, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || ended.IsZero() {
			t.Fatal("StopSandbox never returned")
		}
		if got := began.Sub(start); got != 135*time.Second {
			t.Errorf("StopSandbox began at +%v, want +135s", got)
		}
		if got := ended.Sub(start); got != 145*time.Second {
			t.Errorf("StopSandbox returned at +%v, want +145s", got)
		}
		if asked.Load() != 3 || calls.Load() != 4 {
			t.Errorf("d1Due asked %d times over %d fences, want 3 times over 4 fences", asked.Load(), calls.Load())
		}
		mu.Lock()
		defer mu.Unlock()
		if peak > 2 {
			t.Errorf("calls in flight peaked at %d for one run, want at most 2", peak)
		}
	})
	// The same run with nothing owed and a StopSandbox that hangs to the end of its 60 s phase. The chain is
	// the same up to the stop's start at 135 s; the stop is abandoned at 195 s, and that failure is stamped at
	// once because nothing is owed (no give-up phase) and the write is quick.
	synctest.Test(t, func(t *testing.T) {
		l, row, _ := d1LateLab(t, 15*time.Second, 0, deadline)
		l.dr.stopFn = func(ctx context.Context, _ string, _ int) error {
			<-ctx.Done()
			return ctx.Err()
		}
		start := time.Now()
		l.start()
		time.Sleep(197 * time.Second) // after the abandoned stop's stamp at 195 s, before the 200 s tick that cuts again (off the tick instants)
		l.stop()
		began, _, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || began.Sub(start) != 135*time.Second {
			t.Errorf("nothing owed: StopSandbox called %v at +%v, want +135s", ok, began.Sub(start))
		}
		l.st.mu.Lock()
		stamps := slices.Clone(l.st.stamps)
		l.st.mu.Unlock()
		if n := len(stamps); n != 4 || stamps[3].Sub(start) != 195*time.Second {
			t.Errorf("nothing owed: failure stamps %v, want three cut failures and the abandoned stop's at +195s", stamps)
		}
	})
	// The same again with a copy back owed, so the item runs every phase to its end: the hand-off at 115 s,
	// the last cut abandoned at 135 s, the give-up statement taking its whole 5 s (to 140 s), the stop hanging
	// to the end of its 60 s phase (200 s), and the write of its failure taking its whole 5 s: 205 s. This is
	// the one pin that times the give-up phase.
	synctest.Test(t, func(t *testing.T) {
		l, row, _ := d1LateLab(t, 15*time.Second, 0, deadline)
		l.ds.owe(row.ID)
		l.ds.giveUpLag = d1GiveUpPhase
		l.dr.stopFn = func(ctx context.Context, _ string, _ int) error {
			l.ds.postLag.Store(int64(containPost))
			<-ctx.Done()
			return ctx.Err()
		}
		start := time.Now()
		l.start()
		time.Sleep(207 * time.Second) // after the stop's failure written at 205 s, before the 210 s tick that cuts again (off the tick instants)
		l.stop()
		began, _, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || began.Sub(start) != 140*time.Second {
			t.Errorf("owed: StopSandbox called %v at +%v, want +140s, after the 5 s give-up", ok, began.Sub(start))
		}
		if n := l.ds.giveUpCount(); n != 1 {
			t.Errorf("owed: give-ups = %d, want 1", n)
		}
		l.st.mu.Lock()
		stamps := slices.Clone(l.st.stamps)
		l.st.mu.Unlock()
		if n := len(stamps); n != 4 || stamps[3].Sub(start) != 205*time.Second {
			t.Errorf("owed: failure stamps %v, want three cut failures and the stop's, written at +205s", stamps)
		}
	})
	// The late answer of the round whose d1Due was true (the third) is not observed: nothing is handed off
	// at 115 s, and no stamp before it counts. The streak starts again, and the next round past a minute is
	// the sixth, so the hand-off comes with its answer at 235 s and the stop is called at 255 s.
	synctest.Test(t, func(t *testing.T) {
		l, row, calls := d1LateLab(t, 15*time.Second, 0, func(n int) error {
			if n == 3 {
				return runner.ErrRunnerOffline
			}
			return context.DeadlineExceeded
		})
		start := time.Now()
		l.start()
		time.Sleep(400 * time.Second)
		l.stop()
		began, _, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || began.Sub(start) < 235*time.Second || calls.Load() < 7 {
			t.Errorf("stop called %v at +%v after %d fences, want it no earlier than +235s (the sixth round's answer) after 7", ok, began.Sub(start), calls.Load())
		}
	})
	// Every late answer is a cut that held: no stop.
	synctest.Test(t, func(t *testing.T) {
		l, _, _ := d1LateLab(t, 15*time.Second, 0, func(int) error { return nil })
		l.start()
		time.Sleep(400 * time.Second)
		l.stop()
		if n := l.dr.totalStops(); n != 0 {
			t.Errorf("StopSandbox calls = %d, want 0: every late answer was a cut that held", n)
		}
	})
	// A late answer that comes during post, before d1Due answers: exactly one hand-off.
	synctest.Test(t, func(t *testing.T) {
		l, _, _ := d1LateLab(t, 2*time.Second, 4*time.Second, deadline)
		l.start()
		time.Sleep(300 * time.Second)
		l.stop()
		if n := l.dr.totalStops(); n != 1 {
			t.Errorf("StopSandbox calls = %d, want exactly one hand-off", n)
		}
	})
}

// rowDue is row with its containment due dueIn from now (negative: already due).
func (l *d1Lab) rowDue(dueIn time.Duration, runnerID *uuid.UUID, kind types.MachineKind) store.TerminalSandboxRow {
	r := l.row(runnerID, kind)
	end := time.Now().Add(dueIn - containmentLag)
	r.EndedAt, r.UpdatedAt = &end, end
	l.st.add(r)
	return r
}

// d1ServerLab is a lab whose server runs Kubernetes in server mode: a cut is unsupported, and the agent
// checks and stops each take the given time. The script times are recorded in scripts.
func d1ServerLab(t *testing.T, cat, script time.Duration) (l *d1Lab, scripts *[]time.Time) {
	t.Helper()
	l = newD1Lab(t, false)
	l.rn.name = "k8s"
	l.rn.setHook(func(context.Context, string) error { return runner.ErrEndUnsupported })
	var mu sync.Mutex
	scripts = &[]time.Time{}
	l.dr.execFn = func(ctx context.Context, what, _ string) error {
		if what == "cat" {
			return d1Sleep(ctx, cat)
		}
		mu.Lock()
		*scripts = append(*scripts, time.Now())
		mu.Unlock()
		return d1Sleep(ctx, script)
	}
	return l, scripts
}

// A server-mode run whose record holds a Resume has its agent stopped on every visit, and is given up and
// stopped at its due plus the server-mode wait: not before. At the visit that began a second before it, the
// clock is read when d1Due is asked, so that visit hands it off; StopSandbox returns within 770 s of the run's end. A stop of
// the agent that fails, or an agent that cannot be stopped, makes it due on that visit.
func TestContain_HeldKubernetesServerRunStoppedAfterItsHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, scripts := d1ServerLab(t, 3*time.Second, 5*time.Second)
		l.dr.stopFn = d1StopTakes(d1StopPhase - time.Millisecond)
		t0 := time.Now()
		row := l.rowDue(11*time.Second, nil, "")
		l.ds.owe(row.ID)
		keepEnd := row.EndedAt.Add(containmentLag + containServerKeepWait)
		l.start()
		time.Sleep(450 * time.Second)
		l.stop()
		began, ended, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || ended.IsZero() {
			t.Fatal("the run was never stopped")
		}
		if began.Before(keepEnd) {
			t.Errorf("stopped %v before its due plus the server-mode wait", keepEnd.Sub(began))
		}
		if late := began.Sub(t0.Add(310 * time.Second)); late > 10*time.Second {
			t.Errorf("the stop began %v after the visit that started a second before its due plus the server-mode wait, want within 10s", late)
		}
		if got := ended.Sub(*row.EndedAt); got > 770*time.Second {
			t.Errorf("StopSandbox returned %v after the run's end, want within 770s", got)
		} else {
			t.Logf("StopSandbox returned %v after the run's end", got)
		}
		if n, cats := len(*scripts), l.dr.execCount("cat", row.SandboxRef); n < 25 || n != cats {
			t.Errorf("agent stops = %d, agent checks = %d; want one of each on every visit", n, cats)
		}
		if n := l.ds.giveUpCount(); n != 1 {
			t.Errorf("give-ups = %d, want 1", n)
		}
	})
	for _, failing := range []string{"cat", "script"} {
		synctest.Test(t, func(t *testing.T) {
			l, _ := d1ServerLab(t, time.Second, time.Second)
			l.dr.stopFn = d1StopTakes(time.Second)
			inner := l.dr.execFn
			l.dr.execFn = func(ctx context.Context, what, ref string) error {
				if what == failing {
					return errContainBoom
				}
				return inner(ctx, what, ref)
			}
			t0 := time.Now()
			row := l.rowDue(-time.Minute, nil, "")
			l.ds.owe(row.ID)
			l.start()
			time.Sleep(containItem)
			l.stop()
			if began, _, ok := l.dr.firstStop(row.SandboxRef); !ok || began.Sub(t0) > containItem {
				t.Errorf("failing %s at due + 1 min: stop called %v at +%v, want on the first visit", failing, ok, began.Sub(t0))
			}
		})
	}
}

// Ten server-mode runs, owed and with a stoppable agent: the agent is stopped on every visit, with no
// cache, and each run is stopped no earlier than its due plus the server-mode wait and within 685 s of the
// first listing.
func TestContain_ServerModeRunsStoppedWithinTheirBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, _ := d1ServerLab(t, time.Second, time.Second)
		l.dr.stopFn = d1StopTakes(d1StopPhase - time.Millisecond)
		var rows []store.TerminalSandboxRow
		for range 10 {
			r := l.rowDue(-time.Minute, nil, "")
			l.ds.owe(r.ID)
			rows = append(rows, r)
		}
		start := time.Now()
		l.start()
		time.Sleep(700 * time.Second)
		l.stop()
		for _, r := range rows {
			began, ended, ok := l.dr.firstStop(r.SandboxRef)
			if !ok || ended.IsZero() {
				t.Fatalf("run %s was never stopped", r.ID)
			}
			if keepEnd := r.EndedAt.Add(containmentLag + containServerKeepWait); began.Before(keepEnd) {
				t.Errorf("run %s stopped %v before its due plus the server-mode wait", r.ID, keepEnd.Sub(began))
			}
			if got := ended.Sub(start); got > 685*time.Second {
				t.Errorf("run %s stopped at +%v, want within 685s", r.ID, got)
			}
			if n, cats := l.dr.execCount("script", r.SandboxRef), l.dr.execCount("cat", r.SandboxRef); n < 20 || n != cats {
				t.Errorf("run %s: agent stops %d, agent checks %d; want one of each on every visit, not a cached stop", r.ID, n, cats)
			}
		}
		if n := l.ds.giveUpCount(); n != len(rows) {
			t.Errorf("give-ups = %d, want %d", n, len(rows))
		}
	})
}

// The agent is stopped on each visit before its due plus the server-mode wait, and the sandbox only after the last of those stops.
func TestContain_ServerModeAgentStoppedBeforeTheSandbox(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, scripts := d1ServerLab(t, time.Second, time.Second)
		row := l.rowDue(-(containServerKeepWait - 30*time.Second), nil, "")
		l.ds.owe(row.ID)
		keepEnd := row.EndedAt.Add(containmentLag + containServerKeepWait)
		l.start()
		time.Sleep(containD1)
		l.stop()
		began, _, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || began.Before(keepEnd) {
			t.Fatalf("stop called %v at %v, want at or after its due plus the server-mode wait (%v)", ok, began, keepEnd)
		}
		early := 0
		for _, at := range *scripts {
			if at.After(began) {
				t.Errorf("an agent stop ran at %v, after the sandbox's stop began at %v", at, began)
			}
			if at.Before(keepEnd) {
				early++
			}
		}
		if early < 3 {
			t.Errorf("agent stops before its due plus the server-mode wait = %d, want one on each of at least 3 visits", early)
		}
	})
}

// A VM is never held for a pass: no agent stop, and it is given up and stopped at the first tick past its due.
func TestContain_VMStoppedAtItsDueWithoutAnAgentStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, scripts := d1ServerLab(t, time.Second, time.Second)
		l.dr.stopFn = d1StopTakes(time.Second)
		t0 := time.Now()
		row := l.rowDue(-(containServerKeepWait - 30*time.Second), nil, types.MachineLinuxVM)
		l.ds.owe(row.ID)
		l.start()
		time.Sleep(containD1)
		l.stop()
		began, _, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || began.Sub(t0) > containTick {
			t.Errorf("stop called %v at +%v, want at the first tick", ok, began.Sub(t0))
		}
		if n, cats := len(*scripts), l.dr.execCount("cat", row.SandboxRef); n != 0 || cats != 0 {
			t.Errorf("agent stops %d, agent checks %d for a VM, want none", n, cats)
		}
		if n := l.ds.giveUpCount(); n != 1 {
			t.Errorf("give-ups = %d, want 1", n)
		}
	})
}

// The stop item's re-read of its run is a phase of its own, 5 s long: one that overruns it ends the item with
// no stop, no failure written and no give-up, and releases the claim, so the run is cut and stopped again
// on a later visit.
func TestContain_SlowPreReadStopsNothingAndIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, types.MachineLinuxVM)
		l.ds.getLag = containPre + time.Second
		l.startStops()
		l.item(row)
		time.Sleep(containD1Unsupported + containPre)
		if n, w, g := l.dr.totalStops(), l.st.stampWrites(), l.ds.giveUpCount(); n != 0 || w != 0 || g != 0 {
			t.Errorf("a slow re-read: stops %d, failure writes %d, give-ups %d; want none", n, w, g)
		}
		if l.busy(row.ID) {
			t.Error("the run is still claimed after the item whose re-read overran ended")
		}
		l.ds.getLag = 0
		l.item(row)
		time.Sleep(containD1Unsupported)
		l.stop()
		if n := l.dr.stopCount(row.SandboxRef); n != 1 {
			t.Errorf("StopSandbox calls after a visit with a good re-read = %d, want 1", n)
		}
	})
}

// A confirmed StopSandbox clears the failure the run recorded, when the run still holds the sandbox that was
// stopped; a stop of a sandbox the run no longer holds clears nothing.
func TestContain_ConfirmedStopClearsTheFailureOfItsOwnRef(t *testing.T) {
	for name, tc := range map[string]struct {
		newerRef  bool
		wantClear bool
	}{"the same ref": {false, true}, "a different ref": {true, false}} {
		synctest.Test(t, func(t *testing.T) {
			l := newD1Lab(t, true)
			row := l.row(nil, types.MachineLinuxVM)
			l.dr.afterStop = func(string) {
				if tc.newerRef {
					l.ds.setRef(row.ID, "sbx-newer")
				}
			}
			l.dr.stopFn = func(_ context.Context, _ string, n int) error {
				if n == 1 {
					return errContainBoom
				}
				return nil
			}
			l.startStops()
			l.item(row)
			time.Sleep(containD1Unsupported)
			if l.ds.failure(row.ID) == "" {
				t.Fatalf("%s: the first stop's failure was not recorded", name)
			}
			l.item(row)
			time.Sleep(containD1Unsupported)
			l.stop()
			if n := l.dr.stopCount(row.SandboxRef); n != 2 {
				t.Fatalf("%s: StopSandbox calls = %d, want 2", name, n)
			}
			if cleared := l.ds.failure(row.ID) == ""; cleared != tc.wantClear {
				t.Errorf("%s: failure cleared = %v after the confirmed stop, want %v", name, cleared, tc.wantClear)
			}
		})
	}
}

// A copy back that spans several visits is not killed by the hold's agent stop: while the run's lock is held
// the visits leave the agent alone, write no failure, and do not stop the sandbox. After the pass the changes
// are where it left them, and the sandbox is stopped; a pass that ended with changes still owed gets its
// agent stopped on the next visit.
func TestContain_RunningPassIsNotKilledByTheHold(t *testing.T) {
	for name, arrives := range map[string]bool{"the changes arrive": true, "the changes are still owed": false} {
		synctest.Test(t, func(t *testing.T) {
			l, scripts := d1ServerLab(t, time.Second, time.Second)
			l.dr.stopFn = d1StopTakes(time.Second)
			row := l.rowDue(-time.Minute, nil, "")
			l.ds.owe(row.ID)
			keepEnd := row.EndedAt.Add(containmentLag + containServerKeepWait)
			_, unlock, err := l.srv.lockRunOp(context.Background(), row.ID)
			if err != nil {
				t.Fatalf("%s: take the run's lock: %v", name, err)
			}
			start := time.Now()
			l.start()
			time.Sleep(45 * time.Second)
			if n, cats := len(*scripts), l.dr.execCount("cat", row.SandboxRef); n != 0 || cats != 0 {
				t.Errorf("%s: the agent was checked %d and stopped %d times while the pass held the lock, want none", name, cats, n)
			}
			if n, w := l.dr.totalStops(), l.st.stampWrites(); n != 0 || w != 0 {
				t.Errorf("%s: stops %d, failure writes %d during the pass, want none: a skipped visit is not a failed cut", name, n, w)
			}
			passEnds := time.Now()
			if arrives {
				l.ds.arrive(row.ID)
			}
			unlock()
			time.Sleep(300 * time.Second)
			l.stop()
			if g := l.ds.giveUpCount(); arrives && g != 0 {
				t.Errorf("%s: %d give-ups, want none: the changes had arrived", name, g)
			}
			l.ds.fmu.Lock()
			st := l.ds.status[row.ID]
			arrived := st.Back != nil && st.Back.GivenUpAt == nil && st.Back.Files == 3
			l.ds.fmu.Unlock()
			if arrives && !arrived {
				t.Errorf("%s: the arrived changes' record was overwritten", name)
			}
			began, _, ok := l.dr.firstStop(row.SandboxRef)
			switch {
			case !ok:
				t.Errorf("%s: the sandbox was never stopped", name)
			case arrives && (began.Before(passEnds) || began.Sub(passEnds) > 2*containTick):
				t.Errorf("%s: StopSandbox began at +%v, want within a visit or two of the pass ending at +%v", name, began.Sub(start), passEnds.Sub(start))
			case !arrives && began.Before(keepEnd):
				t.Errorf("%s: StopSandbox began at +%v, before its due plus the server-mode wait", name, began.Sub(start))
			}
			if n := len(*scripts); !arrives && n == 0 {
				t.Errorf("%s: the agent was not stopped on the visits after the pass", name)
			}
		})
	}
}

// A late answer that lands between d1Due's answer being recorded and the cut item's release leaves one stop
// item: the release does not end the stop pool's claim, no cut claims the run meanwhile, and no more than two
// calls are out for it.
func TestContain_LateAnswerBetweenDueKnownAndReleaseKeepsOneStopItem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		row := l.row(nil, "")
		run := containRunOf(row)
		l.rn.setHook(func(ctx context.Context, _ string) error {
			if err := d1Sleep(ctx, 5*time.Second); err != nil {
				return err
			}
			return errContainBoom
		})
		var mu sync.Mutex
		peak, out := 0, 0
		l.dr.onCall = func(string, string) func() {
			mu.Lock()
			defer mu.Unlock()
			out++
			peak = max(peak, out)
			return func() {
				mu.Lock()
				defer mu.Unlock()
				out--
			}
		}
		l.startStops()
		gen, ok := l.srv.containClaim(row.ID, containPoolCut)
		if !ok {
			t.Fatal("the cut pool could not claim the run")
		}
		answer := make(chan struct{})
		callCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, flight := l.srv.containCallWithin(callCtx, run, gen, func() containAnswer {
			<-answer
			return containAnswer{err: errContainBoom}
		})
		if flight == nil {
			t.Fatal("the call was not abandoned at the end of its phase")
		}
		if l.srv.containDueKnown(flight, true) {
			t.Fatal("handed off before the call answered")
		}
		// The late answer lands now, after d1Due's answer was recorded and before the item releases.
		close(answer)
		synctest.Wait()
		if !l.busy(row.ID) || l.stopQueue() != 0 {
			t.Fatalf("after the late answer the run is busy = %v with %d queued; want the stop item holding it", l.busy(row.ID), l.stopQueue())
		}
		l.srv.containRelease(row.ID, containPoolCut, gen)
		if !l.busy(row.ID) {
			t.Error("the cut item's release ended the stop pool's claim")
		}
		if _, got := l.srv.containClaim(row.ID, containPoolCut); got {
			t.Error("a cut claimed the run while the stop item held it")
		}
		time.Sleep(containD1)
		l.stop()
		if n := l.dr.stopCount(row.SandboxRef); n != 1 {
			t.Errorf("StopSandbox calls = %d, want exactly one stop item", n)
		}
		mu.Lock()
		defer mu.Unlock()
		if peak > 2 {
			t.Errorf("calls in flight peaked at %d for one run, want at most 2", peak)
		}
	})
}

// A pass back that holds the run's lock across the end of the server-mode wait (its due plus five minutes)
// stops nothing: the visits after the wait hand the run to the stop item, which finds the lock held and ends
// with no give-up, no stop and no failure write, every visit until the pass is done. The first visit after
// it stops the sandbox; the changes that arrived are untouched, and changes still owed are given up first.
func TestContain_StopWaitsForAPassHoldingTheLockPastTheWait(t *testing.T) {
	for name, arrives := range map[string]bool{"the changes arrive": true, "the changes are still owed": false} {
		synctest.Test(t, func(t *testing.T) {
			l, _ := d1ServerLab(t, time.Second, time.Second)
			l.dr.stopFn = d1StopTakes(time.Second)
			row := l.rowDue(-time.Minute, nil, "")
			l.ds.owe(row.ID)
			keepEnd := row.EndedAt.Add(containmentLag + containServerKeepWait)
			_, unlock, err := l.srv.lockRunOp(context.Background(), row.ID)
			if err != nil {
				t.Fatalf("%s: take the run's lock: %v", name, err)
			}
			start := time.Now()
			l.start()
			// The pass holds the lock from the start to a minute past the end of the wait.
			time.Sleep(keepEnd.Sub(start) + time.Minute)
			if n, w, g := l.dr.totalStops(), l.st.stampWrites(), l.ds.giveUpCount(); n != 0 || w != 0 || g != 0 {
				t.Errorf("%s: stops %d, failure writes %d, give-ups %d while the pass held the lock past the wait; want none", name, n, w, g)
			}
			passEnds := time.Now()
			if arrives {
				l.ds.arrive(row.ID)
			}
			unlock()
			time.Sleep(containServerKeepWait)
			l.stop()
			began, _, ok := l.dr.firstStop(row.SandboxRef)
			if !ok || began.Before(passEnds) || began.Sub(passEnds) > 2*containTick {
				t.Errorf("%s: StopSandbox called %v at +%v, want within a visit or two of the pass ending at +%v", name, ok, began.Sub(start), passEnds.Sub(start))
			}
			wantGiveUps := 1
			if arrives {
				wantGiveUps = 0
			}
			if g := l.ds.giveUpCount(); g != wantGiveUps {
				t.Errorf("%s: give-ups = %d, want %d", name, g, wantGiveUps)
			}
			l.ds.fmu.Lock()
			st := l.ds.status[row.ID]
			arrived := st.Back != nil && st.Back.GivenUpAt == nil && st.Back.Files == 3
			l.ds.fmu.Unlock()
			if arrives && !arrived {
				t.Errorf("%s: the arrived changes' record was overwritten", name)
			}
			if w := l.st.stampWrites(); w != 0 {
				t.Errorf("%s: %d failure writes, want none: a held lock is not a failure", name, w)
			}
		})
	}
}

// containStopOwed over its inputs. A Kubernetes server-mode run with an owed copy back is not owed its stop
// inside the hold (its due plus the server-mode wait) and is once the hold has expired; one with nothing
// owed is owed at its due. A run that is not in server mode follows d1Due: no contain call, past due: owed;
// a failure a streak old: owed; a first failure: not. A read that fails answers owed, the due read included.
// A run not yet due is never owed.
func TestContain_StopOwedValueList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, _ := d1ServerLab(t, time.Second, time.Second)
		ctx := context.Background()
		owedRow := func(dueIn time.Duration) types.AgentRun {
			r := l.rowDue(dueIn, nil, "")
			l.ds.owe(r.ID)
			return containRunOf(r)
		}
		for name, tc := range map[string]struct {
			run  types.AgentRun
			want bool
		}{
			"server mode, the hold expired":        {owedRow(-(containServerKeepWait + time.Second)), true},
			"server mode, the hold ends right now": {owedRow(-containServerKeepWait), true},
			"server mode, inside the hold":         {owedRow(-(containServerKeepWait - time.Second)), false},
			"server mode, not yet due":             {owedRow(time.Minute), false},
			"server mode, nothing owed":            {containRunOf(l.rowDue(-time.Second, nil, "")), true},
			"a virtual machine, past due":          {containRunOf(l.rowDue(-time.Second, nil, types.MachineLinuxVM)), true},
		} {
			if got := l.srv.containStopOwed(ctx, tc.run); got != tc.want {
				t.Errorf("%s: containStopOwed = %v, want %v", name, got, tc.want)
			}
		}
		for readName, fail := range d1ReadFails {
			inside := owedRow(-(containServerKeepWait - time.Second))
			fail(l.ds, errors.New("database down"))
			if !l.srv.containStopOwed(ctx, inside) {
				t.Errorf("%s read failing inside the hold: containStopOwed = false, want true: a read that fails answers owed", readName)
			}
			fail(l.ds, nil)
		}
		notYet := owedRow(time.Minute)
		l.ds.fmu.Lock()
		l.ds.dueErr = errors.New("database down")
		l.ds.fmu.Unlock()
		if !l.srv.containStopOwed(ctx, notYet) {
			t.Error("the due read failing: containStopOwed = false, want true: a read that fails answers owed")
		}
		l.ds.fmu.Lock()
		l.ds.dueErr = nil
		l.ds.fmu.Unlock()
	})
	// A hosted container: owed only once a failure a streak old is recorded.
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		l.rn.setHook(func(context.Context, string) error { return errContainBoom })
		row := l.row(nil, "")
		run := containRunOf(row)
		ctx := context.Background()
		if l.srv.containStopOwed(ctx, run) {
			t.Error("a container with no failure recorded: containStopOwed = true, want false")
		}
		l.item(row)
		if l.srv.containStopOwed(ctx, run) {
			t.Error("a container with a first failure: containStopOwed = true, want false")
		}
		time.Sleep(containTick)
		if !l.srv.containStopOwed(ctx, run) {
			t.Error("a container whose failure is a streak old: containStopOwed = false, want true")
		}
		l.ds.fmu.Lock()
		l.ds.statusErr = errors.New("database down")
		l.ds.fmu.Unlock()
		if !l.srv.containStopOwed(ctx, run) {
			t.Error("a container whose folder record cannot be read: containStopOwed = false, want true")
		}
	})
}

// A run whose stop is owed gets no pass from the resume sweep, on either of two servers that share the
// store and whose sweeps both fire in the window: nothing is stopped, nothing revoked by a tail, no agent
// touched. A run inside its hold is resumed as before (its tail stops the sandbox when the pass keeps
// nothing), which shows the sweep does reach a run when its stop is not owed.
func TestContain_ResumeSweepLeavesARunWhoseStopIsOwed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, scripts := d1ServerLab(t, time.Second, time.Second)
		b := a.sharing(t)
		a.dr.claimed = nil // the sweep's tail stops sandboxes outside the stop pool
		a.ds.onGet = nil   // the sweep's tail re-reads the run outside the stop pool
		owed := a.rowDue(-(containServerKeepWait + time.Second), nil, "")
		a.ds.owe(owed.ID)
		var wg sync.WaitGroup
		for _, l := range []*d1Lab{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.srv.resumeComputerFolderBack(context.Background(), owed.ID)
			}()
		}
		wg.Wait()
		if n := a.dr.stopCount(owed.SandboxRef); n != 0 || len(*scripts) != 0 || a.dr.execCount("cat", owed.SandboxRef) != 0 {
			t.Errorf("a run whose stop is owed: stops %d, agent stops %d, agent checks %d on the two servers; want none",
				n, len(*scripts), a.dr.execCount("cat", owed.SandboxRef))
		}
		inside := a.rowDue(-(containServerKeepWait - time.Minute), nil, "")
		a.ds.owe(inside.ID)
		a.srv.resumeComputerFolderBack(context.Background(), inside.ID)
		if n := a.dr.stopCount(inside.SandboxRef); n == 0 {
			t.Error("a run inside its hold: the sweep did not reach its tail, so the owed case proves nothing")
		}
	})
}

// d1FailExec makes the server lab's agent check ("cat") or agent stop ("script") fail with err.
func d1FailExec(l *d1Lab, failing string, err error) {
	inner := l.dr.execFn
	l.dr.execFn = func(ctx context.Context, what, ref string) error {
		if what == failing {
			return err
		}
		return inner(ctx, what, ref)
	}
}

// A server-mode run inside its hold whose agent check or agent stop fails: the real d1Due takes that failure
// on the visit and the run is queued for the stop pool at once, with no cut made and no second failure
// waited for. Caught by: a failed agent stop read as a skip or an unobserved round, d1Due waiting out the
// hold after a failed agent stop, or the pin run against a stand-in instead of the real d1Due.
func TestContain_AgentStopFailureQueuesTheStopAtOnce(t *testing.T) {
	for _, failing := range []string{"cat", "script"} {
		synctest.Test(t, func(t *testing.T) {
			l, _ := d1ServerLab(t, time.Second, time.Second)
			d1FailExec(l, failing, errContainBoom)
			row := l.rowDue(-time.Minute, nil, "")
			l.ds.owe(row.ID)
			if l.srv.contain.d1DueFn != nil || l.srv.contain.containCallFn != nil {
				t.Fatal("a stand-in is set: this pin needs the real d1Due and the real hook")
			}
			l.item(row)
			if got := l.stopQueue(); got != 1 {
				t.Errorf("failing %s inside the hold: stop FIFO = %d, want the run queued on that visit", failing, got)
			}
			if !l.busy(row.ID) {
				t.Errorf("failing %s: the run was released instead of handed to the stop pool", failing)
			}
			if n := l.rn.callsTo(row.SandboxRef); n != 0 {
				t.Errorf("failing %s: cut calls = %d, want none after a failed agent stop", failing, n)
			}
			if l.st.stamp(row.ID) == nil {
				t.Errorf("failing %s: no failure stamp", failing)
			}
			l.stop()
		})
	}
}

// A server-mode run inside its hold whose call phase answers that it did not finish (errContainCallAbandoned)
// or ran out (context.DeadlineExceeded, as the "call phase ended before the cut" answer wraps it) is queued
// for the stop pool on that visit, like a failed agent stop. The call phase's answer is set through the
// hook's seam; d1Due is the real one. Caught by: d1Due dropping either sentinel from its stalled case, which
// leaves the run to wait for a second failure.
func TestContain_ServerModeCallPhaseRunningOutQueuesTheStopAtOnce(t *testing.T) {
	answers := map[string]error{
		"abandoned": fmt.Errorf("%w: still running", errContainCallAbandoned),
		"deadline":  fmt.Errorf("the call phase ended before the cut: %w", context.DeadlineExceeded),
	}
	for name, answer := range answers {
		synctest.Test(t, func(t *testing.T) {
			l, _ := d1ServerLab(t, time.Second, time.Second)
			l.srv.contain.containCallFn = func(context.Context, types.AgentRun) error { return answer }
			row := l.rowDue(-time.Minute, nil, "")
			l.ds.owe(row.ID)
			if l.srv.contain.d1DueFn != nil {
				t.Fatal("a stand-in for d1Due is set: this pin needs the real d1Due")
			}
			l.item(row)
			if got := l.stopQueue(); got != 1 {
				t.Errorf("%s: stop FIFO = %d, want the run queued on that visit", name, got)
			}
			if !l.busy(row.ID) {
				t.Errorf("%s: the run was released instead of handed to the stop pool", name)
			}
			if n := l.rn.callsTo(row.SandboxRef); n != 0 {
				t.Errorf("%s: cut calls = %d, want none", name, n)
			}
			if l.st.stamp(row.ID) == nil {
				t.Errorf("%s: no failure stamp", name)
			}
			l.stop()
		})
	}
}

// A server-mode run's failed agent check or agent stop is a failure of the visit, never a runner that is
// away, even when the error under it names one: errors.Is is false for all three runner sentinels, and the
// visit stamps the failure with one failure row and leaves the round marked observed. Caught by: wrapping
// the exec's own error, which reads as an unobserved round (no stamp, no row, and no stop queued).
func TestContain_ServerModeAgentStopFailureIsAFailureNotUnobserved(t *testing.T) {
	away := []error{errRunnerNotHere, runner.ErrRunnerOffline, runner.ErrPendingOnRunner}
	for _, failing := range []string{"cat", "script"} {
		for _, cause := range away {
			synctest.Test(t, func(t *testing.T) {
				l, _ := d1ServerLab(t, time.Second, time.Second)
				d1FailExec(l, failing, fmt.Errorf("exec: %w", cause))
				row := l.rowDue(-time.Minute, nil, "")
				l.ds.owe(row.ID)
				err := l.srv.containCallPre(context.Background(), containRunOf(row))
				for _, sentinel := range away {
					if errors.Is(err, sentinel) {
						t.Errorf("failing %s with %v: the hook's error wraps %v; server mode has no runner", failing, cause, sentinel)
					}
				}
				if err == nil || notObserved(err) || errors.Is(err, errContainSkip) || !errors.Is(err, errAgentStopFailed) {
					t.Errorf("failing %s with %v: the hook answered %v, want a plain failure wrapping errAgentStopFailed", failing, cause, err)
				}
				l.item(row)
				if l.st.stamp(row.ID) == nil {
					t.Errorf("failing %s with %v: no failure stamp", failing, cause)
				}
				if failures, _ := failuresAndSuccesses(l.rec); failures != 1 {
					t.Errorf("failing %s with %v: failure rows = %d, want 1", failing, cause, failures)
				}
				if _, ago := l.srv.streakAges(row.ID); ago != nil {
					t.Errorf("failing %s with %v: the round was marked unobserved", failing, cause)
				}
				l.stop()
			})
		}
	}
}

// A visit that finds the run's lock held by its pass is a skip: the hook answers errPassInFlight, which wraps
// errContainSkip, and the visit writes no stamp and no audit row, marks nothing unobserved, queues nothing,
// leaves the agent alone and releases the claim. Caught by: a held lock read as a failed agent stop, which
// would stop the sandbox under a running pass.
func TestContain_PassInFlightSkipIsNotAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, scripts := d1ServerLab(t, time.Second, time.Second)
		row := l.rowDue(-time.Minute, nil, "")
		l.ds.owe(row.ID)
		_, unlock, err := l.srv.lockRunOp(context.Background(), row.ID)
		if err != nil {
			t.Fatalf("take the run's lock: %v", err)
		}
		if got := l.srv.containCallPre(context.Background(), containRunOf(row)); !errors.Is(got, errPassInFlight) || !errors.Is(got, errContainSkip) {
			t.Errorf("the hook answered %v, want errPassInFlight as a skip", got)
		}
		l.item(row)
		if n := l.st.stampWrites(); n != 0 {
			t.Errorf("stamp writes = %d, want none for a skip", n)
		}
		if failures, successes := failuresAndSuccesses(l.rec); failures+successes != 0 {
			t.Errorf("audit rows = %d failures, %d successes, want none", failures, successes)
		}
		if _, ago := l.srv.streakAges(row.ID); ago != nil {
			t.Error("a skip marked the round unobserved")
		}
		if got := l.stopQueue(); got != 0 || l.busy(row.ID) {
			t.Errorf("stop FIFO = %d, busy %v; want nothing queued and the claim released", got, l.busy(row.ID))
		}
		if n, cats := len(*scripts), l.dr.execCount("cat", row.SandboxRef); n != 0 || cats != 0 {
			t.Errorf("agent checks %d, agent stops %d while the pass held the lock, want none", cats, n)
		}
		unlock()
		l.stop()
	})
}

// The stop path's due tests are the database's, never the server's clock. With the server's clock two
// minutes ahead, a virtual machine a minute short of its due is not due, and a held server-mode run a minute
// inside its hold is not owed its stop; with it two minutes behind, the same runs a minute past are. Caught
// by: any due or hold judged on the server's clock.
func TestContain_StopPathReadsTheDatabasesClock(t *testing.T) {
	for name, tc := range map[string]struct {
		skew   time.Duration
		offset time.Duration // past the due (the VM) and past the hold (server mode); negative: short of it
		want   bool
	}{
		"clock ahead, a minute short": {2 * time.Minute, -time.Minute, false},
		"clock behind, a minute past": {-2 * time.Minute, time.Minute, true},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l, _ := d1ServerLab(t, time.Second, time.Second)
				l.srv.cfg.Now = func() time.Time { return time.Now().Add(tc.skew) }
				ctx := context.Background()
				vm := containRunOf(l.rowDue(-tc.offset, nil, types.MachineLinuxVM))
				if got := l.srv.d1Due(ctx, vm, cutUnsupported, nil); got != tc.want {
					t.Errorf("a virtual machine: d1Due = %v, want %v", got, tc.want)
				}
				held := l.rowDue(-(containServerKeepWait + tc.offset), nil, "")
				l.ds.owe(held.ID)
				run := containRunOf(held)
				if got := l.srv.containStopOwed(ctx, run); got != tc.want {
					t.Errorf("a held server-mode run: containStopOwed = %v, want %v", got, tc.want)
				}
				if got := l.srv.d1Due(ctx, run, cutUnsupported, nil); got != tc.want {
					t.Errorf("a held server-mode run: d1Due = %v, want %v", got, tc.want)
				}
			})
		})
	}
}

// A due read that fails answers not due: a database error never stops a sandbox.
func TestContain_D1DueReadFailureIsNotDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, _ := d1ServerLab(t, time.Second, time.Second)
		ctx := context.Background()
		vm := containRunOf(l.rowDue(-time.Minute, nil, types.MachineLinuxVM))
		held := l.rowDue(-(containServerKeepWait + time.Minute), nil, "")
		l.ds.owe(held.ID)
		run := containRunOf(held)
		if !l.srv.d1Due(ctx, vm, cutUnsupported, nil) || !l.srv.d1Due(ctx, run, cutUnsupported, nil) {
			t.Fatal("both rows must be due before the read fails")
		}
		l.ds.dueErr = errors.New("database down")
		if l.srv.d1Due(ctx, vm, cutUnsupported, nil) {
			t.Error("a virtual machine past its due: d1Due = true on a failed due read")
		}
		if l.srv.d1Due(ctx, run, cutUnsupported, nil) {
			t.Error("a held server-mode run past its wait: d1Due = true on a failed due read")
		}
	})
}

// d1LostLocker hands out run locks whose context a test can cancel, as the database's does when the lock's
// connection is lost, and counts the releases. The lock itself stays held until unlock, as another
// replica's view of a lost lock is not modelled: the test takes the lock again after the item to show it is
// free.
type d1LostLocker struct {
	db.Locker
	mu       sync.Mutex
	lose     context.CancelFunc
	onTake   func(lose context.CancelFunc) // runs as the lock is handed out
	kept     atomic.Bool                   // set: no lock is lost from here on
	unlocked int
}

func (l *d1LostLocker) TryLock(ctx context.Context, k db.LockKey) (context.Context, func(), bool, error) {
	lctx, unlock, ok, err := l.Locker.TryLock(ctx, k)
	if err != nil || !ok {
		return lctx, unlock, ok, err
	}
	lctx, cancel := context.WithCancel(lctx)
	l.mu.Lock()
	l.lose = cancel
	l.mu.Unlock()
	if l.onTake != nil && !l.kept.Load() {
		l.onTake(cancel)
	}
	return lctx, func() {
		l.mu.Lock()
		l.unlocked++
		l.mu.Unlock()
		cancel()
		unlock()
	}, true, nil
}

// loseNow cancels the lock context handed out last, if any.
func (l *d1LostLocker) loseNow() {
	l.mu.Lock()
	cancel := l.lose
	l.mu.Unlock()
	if cancel != nil && !l.kept.Load() {
		cancel()
	}
}

func (l *d1LostLocker) releases() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unlocked
}

// The stop item keeps the run lock's context: once the lock is lost, no give-up write and no StopSandbox
// begins, the claim accounting is left alone (no failure stamp), the item still releases its lock, and
// another holder can take the run's lock again. The lock is lost on acquisition, after the owed read and
// before the give-up statement, after the give-up statement and before the stop, or after the stop item has
// handed its call to the call's goroutine and is waiting for it, before the call runs: that stop is no attempt
// (no confirmation, no unobserved mark, no failure stamp or streak, d1Due's verdict unchanged). The run is
// then left owed, its claim released as the held-lock early return leaves it: a second visit on a fresh lock
// reaches StopSandbox once. A lock that is kept gives up and stops on the first visit. A kept lock whose stop
// call does not start within the stop phase is abandoned and stamped as before; once the item unlocks, the
// late call finds the lock gone and begins no stop.
func TestContain_LostRunLockBeginsNoGiveUpAndNoStop(t *testing.T) {
	cases := []struct {
		name         string
		lose         func(l *d1Lab, lk *d1LostLocker)
		firstGiveUps int
		lost         bool // the first visit stops nothing; a second visit on a kept lock stops once
		finalGiveUps int
		stamps       int // failure writes on the first visit
	}{
		{"lost on acquisition", func(_ *d1Lab, lk *d1LostLocker) { lk.onTake = func(lose context.CancelFunc) { lose() } }, 0, true, 1, 0},
		{"lost after the owed read, before the give-up", func(l *d1Lab, lk *d1LostLocker) { l.ds.afterSeed = lk.loseNow }, 0, true, 1, 0},
		{"lost after the give-up, before the stop", func(l *d1Lab, lk *d1LostLocker) { l.ds.afterGiveUp = lk.loseNow }, 1, true, 1, 0},
		// The loss lands once every other goroutine is parked, the stop item in its wait for the call's answer.
		// The work then waits until every other goroutine is parked again: the item has taken the abandoned
		// answer, withheld the stop and released its lock before the work runs.
		{"lost while the stop waits for its call to run", func(l *d1Lab, lk *d1LostLocker) {
			l.srv.contain.callStartFn = func() {
				if lk.kept.Load() {
					return
				}
				synctest.Wait()
				lk.loseNow()
				synctest.Wait()
				lk.mu.Lock()
				taken := lk.lose != nil
				lk.mu.Unlock()
				if n := lk.releases(); taken && n != 1 {
					l.t.Errorf("the item released its lock %d times before its abandoned call ran, want 1", n)
				}
			}
		}, 1, true, 1, 0},
		// The lock is kept, and the stop's call (the first call made under a lock) starts only after its phase.
		{"kept, the stop call starts after its phase", func(l *d1Lab, lk *d1LostLocker) {
			var slept atomic.Bool
			l.srv.contain.callStartFn = func() {
				lk.mu.Lock()
				taken := lk.lose != nil
				lk.mu.Unlock()
				if taken && slept.CompareAndSwap(false, true) {
					time.Sleep(d1StopPhase + time.Second)
				}
			}
		}, 1, true, 1, 1},
		{"kept", func(*d1Lab, *d1LostLocker) {}, 1, false, 1, 0},
	}
	for _, c := range cases {
		synctest.Test(t, func(t *testing.T) {
			l := newD1Lab(t, true)
			lk := &d1LostLocker{Locker: l.srv.locker()}
			l.srv.locks.override = lk
			row := l.row(nil, types.MachineLinuxVM)
			l.ds.owe(row.ID)
			c.lose(l, lk)
			ctx, run := context.Background(), containRunOf(row)
			dueBefore := [2]bool{l.srv.d1Due(ctx, run, cutUnsupported, nil), l.srv.d1Due(ctx, run, cutFenced, errContainBoom)}
			l.startStops()
			l.item(row)
			time.Sleep(containD1Unsupported)
			if g := l.ds.giveUpCount(); g != c.firstGiveUps {
				t.Errorf("%s: give-up statements = %d, want %d", c.name, g, c.firstGiveUps)
			}
			wantStops := 0
			if !c.lost {
				wantStops = 1
			}
			if n := l.dr.totalStops(); n != wantStops {
				t.Errorf("%s: StopSandbox calls = %d, want %d", c.name, n, wantStops)
			}
			if w := l.st.stampWrites(); w != c.stamps {
				t.Errorf("%s: %d failure writes, want %d: a lost lock is not a failure", c.name, w, c.stamps)
			}
			if n := l.ds.clearCalls.Load(); n != int32(wantStops) {
				t.Errorf("%s: %d confirmed-stop clears, want %d", c.name, n, wantStops)
			}
			l.srv.contain.mu.Lock()
			_, unobserved := l.srv.contain.unobserved[row.ID]
			l.srv.contain.mu.Unlock()
			if unobserved {
				t.Errorf("%s: the run was marked unobserved, want no mark", c.name)
			}
			if n := lk.releases(); n != 1 {
				t.Errorf("%s: the item released its lock %d times, want 1", c.name, n)
			}
			if l.busy(row.ID) {
				t.Errorf("%s: the run is still claimed after its item ended, want it released for the next visit", c.name)
			}
			// The probe goes to the wrapped locker, so its release is not counted as an item's.
			_, unlock, ok, err := lk.Locker.TryLock(context.Background(), db.NewLockKey(db.RunOpLockClass, row.ID.String()))
			if err != nil || !ok {
				t.Fatalf("%s: another holder cannot take the run's lock after the item: ok=%v err=%v", c.name, ok, err)
			}
			unlock()
			if c.lost {
				// The next visit holds a fresh lock that is not lost: the run is still owed its stop.
				lk.kept.Store(true)
				dueAfter := [2]bool{l.srv.d1Due(ctx, run, cutUnsupported, nil), l.srv.d1Due(ctx, run, cutFenced, errContainBoom)}
				if c.stamps == 0 && dueAfter != dueBefore {
					t.Errorf("%s: d1Due (no call, failed cut) = %v after the lost visit, want %v as before it", c.name, dueAfter, dueBefore)
				}
				if !l.item(row) {
					t.Fatalf("%s: the run could not be claimed for a second visit", c.name)
				}
				time.Sleep(containD1Unsupported)
				if n := l.dr.stopCount(row.SandboxRef); n != 1 {
					t.Errorf("%s: StopSandbox calls after the second visit = %d, want 1", c.name, n)
				}
				if g := l.ds.giveUpCount(); g != c.finalGiveUps {
					t.Errorf("%s: give-up statements after the second visit = %d, want %d", c.name, g, c.finalGiveUps)
				}
				if n := lk.releases(); n != 2 {
					t.Errorf("%s: the two visits released their locks %d times, want 2", c.name, n)
				}
			}
			l.stop()
		})
	}
}
