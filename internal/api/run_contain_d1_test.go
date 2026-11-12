// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

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
	getLag time.Duration // each GetRun takes this long on the clock

	fmu          sync.Mutex
	folderRunner uuid.UUID
	foldered     map[uuid.UUID]bool
	status       map[uuid.UUID]*types.RunLocalFolderStatus
	statusErr    error
	seedErr      error
	giveUps      []uuid.UUID
	notApplied   bool // the give-up statement applies nothing
	otherWriter  bool // the statement applies nothing, and the record is final from another writer
	streaks      []d1Streak
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
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rows[id]
	if r == nil {
		return types.AgentRun{}, errors.New("no such run")
	}
	return containRunOf(r.TerminalSandboxRow), nil
}

// ContainmentStreakDue is the store's statement over the rows in memory.
func (f *d1Store) ContainmentStreakDue(ctx context.Context, id uuid.UUID, tau time.Duration, since time.Time) (bool, error) {
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
		got = !at.Before(containRowKey(r.TerminalSandboxRow).Add(containmentLag)) && !at.Before(since) && !at.After(now.Add(-tau))
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
}

func newD1Runner(rn *containRunner) *d1Runner {
	return &d1Runner{containRunner: rn, stopAt: map[string][]time.Time{}, stopEnd: map[string][]time.Time{},
		execN: map[string]int{}}
}

func (r *d1Runner) enter(ref, what string) func() {
	r.mu.Lock()
	f := r.onCall
	r.mu.Unlock()
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
	ds *d1Store
	dr *d1Runner
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
	return &d1Lab{containLab: l, ds: ds, dr: dr}
}

// sharing is a second server over the same store and runner, as another replica.
func (l *d1Lab) sharing(t *testing.T) *d1Lab {
	t.Helper()
	other := newContainLabOn(t, true, l.ds.containStore, &seamRecorder{})
	other.srv.cfg.Store, other.srv.cfg.Runner = l.srv.cfg.Store, l.srv.cfg.Runner
	other.srv.contain.d1DueFn = nil
	return &d1Lab{containLab: other, ds: l.ds, dr: l.dr}
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
	if !l.srv.containClaim(row.ID, containPoolCut) {
		return false
	}
	l.srv.containCutItem(context.Background(), containRunOf(row))
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
		if l.srv.d1Due(context.Background(), run, cutUnsupported, nil, nil) || l.dr.totalStops() != 0 {
			t.Errorf("due - 1s: d1Due or a stop came early (stops %d)", l.dr.totalStops())
		}
		time.Sleep(time.Second)
		if !l.srv.d1Due(context.Background(), run, cutUnsupported, nil, nil) {
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

// d1Bound runs a lab over rows whose cuts take the whole call phase and fail, and whose stops take a phase
// less a millisecond, and answers how long after the first listing each ref's stop returned.
func d1Bound(t *testing.T, l *d1Lab, rows []store.TerminalSandboxRow, kinds map[string]types.MachineKind, horizon time.Duration) (last time.Duration) {
	t.Helper()
	l.st.lag = 4 * time.Second
	l.ds.getLag = 4 * time.Second
	l.rn.setHook(d1CutHangs)
	l.dr.stopFn = func(ctx context.Context, ref string, _ int) error {
		d := d1StopPhase - time.Millisecond
		if kinds[ref].IsMacOSVM() {
			d = d1StopPhaseMac - time.Millisecond
		}
		return d1Sleep(ctx, d)
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

// 100 containers whose cuts fail at full length in two rounds: the last stop lands within 3895 s of the
// first listing, because stops run on their own workers.
func TestContain_HundredFailedFencesBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		var rows []store.TerminalSandboxRow
		for range 100 {
			rows = append(rows, l.row(nil, ""))
		}
		last := d1Bound(t, l, rows, nil, 4000*time.Second)
		if bound := 3895 * time.Second; last > bound || last < 2000*time.Second {
			t.Errorf("the last stop returned at +%v, want in [2000s, %v]", last, bound)
		}
		t.Logf("last stop at +%v", last)
	})
}

// 3 macOS VMs (90 s stops) among 97 containers: the VMs queue behind no container.
func TestContain_MixedStopKindsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		var rows []store.TerminalSandboxRow
		kinds := map[string]types.MachineKind{}
		for i := range 100 {
			kind := types.MachineKind("")
			if i < 3 {
				kind = types.MachineMacOSVM
			}
			r := l.row(nil, kind)
			rows, kinds[r.SandboxRef] = append(rows, r), kind
		}
		last := d1Bound(t, l, rows, kinds, 4000*time.Second)
		if bound := 3895 * time.Second; last > bound || last < 2000*time.Second {
			t.Errorf("the last stop returned at +%v, want in [2000s, %v]", last, bound)
		}
		t.Logf("last stop at +%v", last)
	})
}

// 50 macOS VMs and 50 containers: every VM takes its cut round first, and no stop worker idles while a
// container waits.
func TestContain_HalfVMHalfContainersBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		var rows []store.TerminalSandboxRow
		kinds := map[string]types.MachineKind{}
		for i := range 100 {
			kind := types.MachineKind("")
			if i%2 == 0 {
				kind = types.MachineMacOSVM
			}
			r := l.row(nil, kind)
			rows, kinds[r.SandboxRef] = append(rows, r), kind
		}
		last := d1Bound(t, l, rows, kinds, 3200*time.Second)
		if bound := 3000 * time.Second; last > bound || last < 2000*time.Second {
			t.Errorf("the last stop returned at +%v, want in [2000s, %v]", last, bound)
		}
		t.Logf("last stop at +%v", last)
	})
}

// 10 runner container runs whose cuts fail at full length: each is stopped within 485 s of the first
// listing; with 5 the bound is 390 s, and no second failure under a minute after the first counts.
func TestContain_TenRunnerContainersBound(t *testing.T) {
	for n, bound := range map[int]time.Duration{10: 485 * time.Second, 5: 390 * time.Second} {
		synctest.Test(t, func(t *testing.T) {
			l := newD1Lab(t, true)
			runnerID := l.connect(true)
			var rows []store.TerminalSandboxRow
			for range n {
				rows = append(rows, l.row(&runnerID, ""))
			}
			last := d1Bound(t, l, rows, nil, 600*time.Second)
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
			if !l.srv.containClaim(r.ID, containPoolStop) {
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

// A folder record that cannot be read stops nothing and gives nothing up, for a streaked run and for a
// run with no contain call; the next tick with a good read stops them.
func TestContain_StatusReadErrorSkipsD1(t *testing.T) {
	logs := d1CaptureLogs(t)
	synctest.Test(t, func(t *testing.T) {
		l := newD1Lab(t, true)
		l.rn.setHook(func(context.Context, string) error { return errContainBoom })
		streaked := l.row(nil, "")
		vm := l.row(nil, types.MachineLinuxVM)
		l.ds.fmu.Lock()
		l.ds.statusErr = errors.New("database down")
		l.ds.fmu.Unlock()
		l.item(streaked)
		time.Sleep(containTick)
		l.item(streaked)
		l.item(vm)
		if got := l.stopQueue(); got != 0 || l.ds.giveUpCount() != 0 {
			t.Errorf("status read failing: stop queue %d, give-ups %d; want none", got, l.ds.giveUpCount())
		}
		if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, streaked.ID.String()) || !strings.Contains(out, vm.ID.String()) {
			t.Errorf("the skipped rows were not logged at warn with their run ids:\n%s", out)
		}
		l.ds.fmu.Lock()
		l.ds.statusErr = nil
		l.ds.fmu.Unlock()
		time.Sleep(containTick)
		l.item(streaked)
		l.item(vm)
		if got := l.stopQueue(); got != 2 {
			t.Errorf("status read good again: stop queue %d, want 2", got)
		}
		l.stop()
	})
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

// A call that stalls for a minute past its phase: the stop pool never has more than K_s * 5 calls out, both
// pools together never more than 40, and no run more than two, and two only inside its stop item.
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

// A fence whose deadline error comes 15 s after its phase: d1Due runs at every abandonment, the stop is
// handed off only when a call answers, and StopSandbox returns within 225 + 2L = 255 s of the first
// listing, with never more than two calls out for the run.
func TestContain_LateDeadlineErrorsStillReachTheStop(t *testing.T) {
	deadline := func(int) error { return context.DeadlineExceeded }
	synctest.Test(t, func(t *testing.T) {
		l, row, calls := d1LateLab(t, 15*time.Second, 0, deadline)
		var asked atomic.Int32
		l.srv.contain.d1DueFn = func(ctx context.Context, run types.AgentRun, kind cutKind, err, preErr error) bool {
			asked.Add(1)
			return l.srv.d1Due(ctx, run, kind, err, preErr)
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
		if got := ended.Sub(start); got > 255*time.Second {
			t.Errorf("StopSandbox returned at +%v, want within 255s (225 + 2L, L = 15s)", got)
		} else {
			t.Logf("StopSandbox returned at +%v", got)
		}
		if int(asked.Load()) < 3 || int(calls.Load()) < 4 {
			t.Errorf("d1Due asked %d times over %d fences, want it asked at each of three abandoned cuts and a last cut after", asked.Load(), calls.Load())
		}
		if began.Sub(start) < 100*time.Second {
			t.Errorf("the stop began at +%v: handed off before the third cut answered", began.Sub(start))
		}
		mu.Lock()
		defer mu.Unlock()
		if peak > 2 {
			t.Errorf("calls in flight peaked at %d for one run, want at most 2", peak)
		}
	})
	// The second late answer is not observed: that round hands nothing off, and no stamp before it counts.
	synctest.Test(t, func(t *testing.T) {
		l, row, calls := d1LateLab(t, 15*time.Second, 0, func(n int) error {
			if n == 2 {
				return runner.ErrRunnerOffline
			}
			return context.DeadlineExceeded
		})
		l.start()
		time.Sleep(400 * time.Second)
		l.stop()
		_, _, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || calls.Load() < 6 {
			t.Errorf("stop called %v after %d fences, want a stop only after the streak restarted (6 fences)", ok, calls.Load())
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
// stopped at its due + P_C: not before; at the visit that began a second before it, the clock is read when
// d1Due is asked, so that visit hands it off; StopSandbox returns within 770 s of the run's end. A stop of
// the agent that fails, or an agent that cannot be stopped, makes it due on that visit.
func TestContain_HeldKubernetesServerRunStoppedByPC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, scripts := d1ServerLab(t, 3*time.Second, 5*time.Second)
		l.dr.stopFn = d1StopTakes(d1StopPhase - time.Millisecond)
		t0 := time.Now()
		row := l.rowDue(11*time.Second, nil, "")
		l.ds.owe(row.ID)
		duePC := row.EndedAt.Add(containmentLag + containOptionC)
		l.start()
		time.Sleep(450 * time.Second)
		l.stop()
		began, ended, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || ended.IsZero() {
			t.Fatal("the run was never stopped")
		}
		if began.Before(duePC) {
			t.Errorf("stopped %v before due + P_C", duePC.Sub(began))
		}
		if late := began.Sub(t0.Add(310 * time.Second)); late > 10*time.Second {
			t.Errorf("the stop began %v after the visit that started a second before due + P_C, want within 10s", late)
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
// cache, and each run is stopped no earlier than its due + P_C and within 685 s of the first listing.
func TestContain_OptionCStopsWithinItsBound(t *testing.T) {
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
			if duePC := r.EndedAt.Add(containmentLag + containOptionC); began.Before(duePC) {
				t.Errorf("run %s stopped %v before due + P_C", r.ID, duePC.Sub(began))
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

// The agent is stopped on each visit before due + P_C, and the sandbox only after the last of those stops.
func TestOptionC_AgentStoppedFirstThenStopAtPC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, scripts := d1ServerLab(t, time.Second, time.Second)
		row := l.rowDue(-(containOptionC - 30*time.Second), nil, "")
		l.ds.owe(row.ID)
		duePC := row.EndedAt.Add(containmentLag + containOptionC)
		l.start()
		time.Sleep(containD1)
		l.stop()
		began, _, ok := l.dr.firstStop(row.SandboxRef)
		if !ok || began.Before(duePC) {
			t.Fatalf("stop called %v at %v, want at or after due + P_C (%v)", ok, began, duePC)
		}
		early := 0
		for _, at := range *scripts {
			if at.After(began) {
				t.Errorf("an agent stop ran at %v, after the sandbox's stop began at %v", at, began)
			}
			if at.Before(duePC) {
				early++
			}
		}
		if early < 3 {
			t.Errorf("agent stops before due + P_C = %d, want one on each of at least 3 visits", early)
		}
	})
}

// A VM is never held for a pass: no agent stop, and it is given up and stopped at the first tick past its due.
func TestOptionC_VMGoesToRuleA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, scripts := d1ServerLab(t, time.Second, time.Second)
		l.dr.stopFn = d1StopTakes(time.Second)
		t0 := time.Now()
		row := l.rowDue(-(containOptionC - 30*time.Second), nil, types.MachineLinuxVM)
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
