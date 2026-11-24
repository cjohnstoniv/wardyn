// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
)

// fakeStamp is a lifecycle.Config.StampIdle: it records each call's runner (nil kept as nil), returns the
// next queued error, and, when given the store, sets UpdatedAt to at on every row it covers.
type fakeStamp struct {
	mu    sync.Mutex
	calls []*uuid.UUID
	errs  []error
	store *fakeStore
	at    time.Time
}

func (f *fakeStamp) stamp(_ context.Context, runner *uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if runner != nil {
		id := *runner
		runner = &id
	}
	f.calls = append(f.calls, runner)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return 0, err
		}
	}
	var n int64
	if f.store != nil {
		f.store.mu.Lock()
		for i := range f.store.rows {
			if runner == nil || (f.store.rows[i].RunnerID != nil && *f.store.rows[i].RunnerID == *runner) {
				f.store.rows[i].UpdatedAt = f.at
				n++
			}
		}
		f.store.mu.Unlock()
	}
	return n, nil
}

// runners is the recorded calls as runner ids, uuid.Nil standing for a nil runner.
func (f *fakeStamp) runners() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uuid.UUID, len(f.calls))
	for i, c := range f.calls {
		if c != nil {
			out[i] = *c
		}
	}
	return out
}

func countAction(rec *fakeRecorder, action string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, ev := range rec.events {
		if ev.Action == action {
			n++
		}
	}
	return n
}

// outageReaper builds a reaper over the fakes with the given outage hooks and a fixed clock at base.
func outageReaper(store *fakeStore, stopper *fakeStopper, rec *fakeRecorder, base time.Time, cfg lifecycle.Config) *lifecycle.Reaper {
	cfg.Interval = time.Hour
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return base }
	}
	return lifecycle.New(store, stopper, rec, cfg)
}

func idleRun(base time.Time, runner *uuid.UUID) lifecycle.RunSummary {
	return lifecycle.RunSummary{
		ID:                     uuid.New(),
		CreatedAt:              base.Add(-36 * time.Minute),
		UpdatedAt:              base.Add(-35 * time.Minute),
		PolicyAutoStopAfterSec: int((30 * time.Minute).Seconds()),
		RunnerID:               runner,
	}
}

func TestReaperHoldsIdleStopsWhileTheStoreRecovers(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	a := idleRun(base, nil)
	b := lifecycle.RunSummary{ID: uuid.New(), CreatedAt: base.Add(-2 * time.Hour), UpdatedAt: base}
	store := &fakeStore{rows: []lifecycle.RunSummary{a, b}}
	stopper, rec := newFakeStopper(), &fakeRecorder{}
	recovering := true
	r := outageReaper(store, stopper, rec, base, lifecycle.Config{
		MaxAge:          time.Hour,
		StoreRecovering: func() bool { return recovering },
	})

	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if stopper.wasStopped(a.ID) || countAction(rec, "run.autostop") != 0 {
		t.Error("an idle run was stopped while the store was recovering")
	}
	if !stopper.wasStopped(b.ID) || countAction(rec, "run.max_age.expire") != 1 {
		t.Error("the max-age stop must not be held while the store recovers")
	}

	recovering = false
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !stopper.wasStopped(a.ID) || countAction(rec, "run.autostop") != 1 {
		t.Error("the idle run was not stopped once the store stopped recovering")
	}
}

func TestReaperRestartsIdleClocksOncePerRecovery(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	stamp := &fakeStamp{}
	recovering := true
	r := outageReaper(&fakeStore{}, newFakeStopper(), &fakeRecorder{}, base, lifecycle.Config{
		StoreRecovering: func() bool { return recovering },
		StampIdle:       stamp.stamp,
	})
	tick := func() {
		t.Helper()
		if err := r.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	tick()
	tick()
	if got := stamp.runners(); !slices.Equal(got, []uuid.UUID{uuid.Nil}) {
		t.Fatalf("two passes inside one recovery made stamp calls %v, want exactly one for every run", got)
	}
	recovering = false
	tick()
	if got := len(stamp.runners()); got != 1 {
		t.Fatalf("a pass outside a recovery stamped: %d calls", got)
	}
	recovering = true
	tick()
	if got := len(stamp.runners()); got != 2 {
		t.Fatalf("the next recovery made %d stamp calls in all, want 2", got)
	}
}

func TestReaperStampsAgainWhenTheStoreGoesAwayInsideTheGrace(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	healthyFor := 10 * time.Second
	stamp := &fakeStamp{}
	r := outageReaper(&fakeStore{}, newFakeStopper(), &fakeRecorder{}, base, lifecycle.Config{
		Now:             func() time.Time { return now },
		StoreRecovering: func() bool { return true },
		StoreHealthyFor: func() time.Duration { return healthyFor },
		StampIdle:       stamp.stamp,
	})
	steps := []struct {
		at         time.Duration
		healthyFor time.Duration
		calls      int
	}{
		{0, 10 * time.Second, 1},
		{time.Minute, 70 * time.Second, 1},
		{10 * time.Minute, 20 * time.Second, 2},
	}
	for _, s := range steps {
		now, healthyFor = base.Add(s.at), s.healthyFor
		if err := r.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if got := len(stamp.runners()); got != s.calls {
			t.Fatalf("at +%s with the store healthy for %s: %d stamp calls, want %d", s.at, s.healthyFor, got, s.calls)
		}
	}
}

func TestReaperRetriesAFailedRecoveryStamp(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	boom := errors.New("boom")
	stamp := &fakeStamp{errs: []error{boom}}
	store := &fakeStore{rows: []lifecycle.RunSummary{idleRun(base, nil)}}
	stopper, rec := newFakeStopper(), &fakeRecorder{}
	r := outageReaper(store, stopper, rec, base, lifecycle.Config{
		StoreRecovering: func() bool { return true },
		StampIdle:       stamp.stamp,
	})
	if err := r.Tick(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("first Tick = %v, want an error wrapping the stamp's", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("third Tick: %v", err)
	}
	if got := len(stamp.runners()); got != 2 {
		t.Fatalf("%d stamp calls, want 2 (the failure, then the retry that landed)", got)
	}
	if countAction(rec, "run.autostop") != 0 {
		t.Error("a run was idle-stopped while the store recovered")
	}
}

func TestReaperSkipsIdleStopsOnAnOfflineRunner(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	rOff, rOn := uuid.New(), uuid.New()
	a, b, c := idleRun(base, &rOff), idleRun(base, nil), idleRun(base, &rOn)
	online := map[uuid.UUID]bool{rOff: false, rOn: true}
	hooks := func(recovering *bool) lifecycle.Config {
		return lifecycle.Config{
			RunnerOnline:    func(id uuid.UUID) bool { return online[id] },
			StoreRecovering: func() bool { return *recovering },
		}
	}
	t.Run("not recovering", func(t *testing.T) {
		store := &fakeStore{rows: []lifecycle.RunSummary{a, b, c}}
		stopper, rec, stamp := newFakeStopper(), &fakeRecorder{}, &fakeStamp{}
		off := false
		cfg := hooks(&off)
		cfg.StampIdle = stamp.stamp
		r := outageReaper(store, stopper, rec, base, cfg)
		if err := r.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		if stopper.wasStopped(a.ID) || !stopper.wasStopped(b.ID) || !stopper.wasStopped(c.ID) {
			t.Errorf("stopped a=%v b=%v c=%v, want only a (offline runner) kept", stopper.wasStopped(a.ID), stopper.wasStopped(b.ID), stopper.wasStopped(c.ID))
		}
		if len(stamp.runners()) != 0 {
			t.Errorf("stamp calls %v, want none", stamp.runners())
		}
	})
	t.Run("recovering first", func(t *testing.T) {
		store := &fakeStore{rows: []lifecycle.RunSummary{a}}
		stopper, rec, stamp := newFakeStopper(), &fakeRecorder{}, &fakeStamp{}
		recovering := true
		cfg := hooks(&recovering)
		cfg.StampIdle = stamp.stamp
		r := outageReaper(store, stopper, rec, base, cfg)
		for i := 0; i < 2; i++ {
			if err := r.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			recovering = false
		}
		if stopper.wasStopped(a.ID) {
			t.Error("a run on an offline runner was stopped")
		}
		if got := stamp.runners(); !slices.Equal(got, []uuid.UUID{uuid.Nil}) {
			t.Errorf("stamp calls %v, want only the recovery's", got)
		}
	})
}

func TestReaperRestartsAReturningRunnersIdleClocks(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	runner := uuid.New()
	for _, withRecovery := range []bool{false, true} {
		a := idleRun(base, &runner)
		store := &fakeStore{rows: []lifecycle.RunSummary{a}}
		stopper, rec := newFakeStopper(), &fakeRecorder{}
		stamp := &fakeStamp{store: store, at: base}
		online, recovering := false, withRecovery
		r := outageReaper(store, stopper, rec, base, lifecycle.Config{
			RunnerOnline:    func(uuid.UUID) bool { return online },
			StoreRecovering: func() bool { return recovering },
			StampIdle:       stamp.stamp,
		})
		tick := func() {
			t.Helper()
			if err := r.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
		}
		tick()
		if stopper.wasStopped(a.ID) {
			t.Fatalf("recovery=%v: run on an offline runner stopped", withRecovery)
		}
		recovering = false
		stamp.mu.Lock()
		before := len(stamp.calls)
		stamp.mu.Unlock()
		online = true
		tick()
		after := stamp.runners()[before:]
		if !slices.Equal(after, []uuid.UUID{runner}) {
			t.Fatalf("recovery=%v: stamp calls on return %v, want one for the runner", withRecovery, after)
		}
		if stopper.wasStopped(a.ID) {
			t.Fatalf("recovery=%v: the returning runner's run was stopped on the pass its clock restarted", withRecovery)
		}
		tick()
		if got := len(stamp.runners()); got != before+1 {
			t.Fatalf("recovery=%v: %d stamp calls, want no new one on the third pass", withRecovery, got)
		}
		if stopper.wasStopped(a.ID) {
			t.Fatalf("recovery=%v: the run was stopped after its clock restarted", withRecovery)
		}
	}
}

func TestReaperRetriesAFailedRunnerStamp(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	runner := uuid.New()
	a := idleRun(base, &runner)
	store := &fakeStore{rows: []lifecycle.RunSummary{a}}
	stopper, rec := newFakeStopper(), &fakeRecorder{}
	boom := errors.New("boom")
	stamp := &fakeStamp{store: store, at: base}
	online := false
	r := outageReaper(store, stopper, rec, base, lifecycle.Config{
		RunnerOnline: func(uuid.UUID) bool { return online },
		StampIdle:    stamp.stamp,
	})
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	online = true
	stamp.errs = []error{boom}
	if err := r.Tick(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Tick 2 = %v, want an error wrapping the stamp's", err)
	}
	if stopper.wasStopped(a.ID) {
		t.Fatal("run stopped on the pass whose runner stamp failed")
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick 3: %v", err)
	}
	if got := stamp.runners(); !slices.Equal(got, []uuid.UUID{runner, runner}) {
		t.Fatalf("stamp calls %v, want the failure and one retry for the runner", got)
	}
	if stopper.wasStopped(a.ID) {
		t.Fatal("run stopped on the pass that retried the stamp")
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick 4: %v", err)
	}
	if got := len(stamp.runners()); got != 2 {
		t.Fatalf("%d stamp calls after Tick 4, want 2", got)
	}
}

func TestReaperWithoutOutageHooksStopsARunnerRunAsToday(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	runner := uuid.New()
	a := idleRun(base, &runner)
	store := &fakeStore{rows: []lifecycle.RunSummary{a}}
	stopper, rec := newFakeStopper(), &fakeRecorder{}
	r := outageReaper(store, stopper, rec, base, lifecycle.Config{})
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !stopper.wasStopped(a.ID) || countAction(rec, "run.autostop") != 1 {
		t.Error("with no outage hooks an idle run on a runner must be stopped as before")
	}
}

func TestReaperStopsAnOfflineRunnersRunAtItsMaxAge(t *testing.T) {
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	runner := uuid.New()
	a := idleRun(base, &runner)
	a.CreatedAt = base.Add(-2 * time.Hour)
	store := &fakeStore{rows: []lifecycle.RunSummary{a}}
	stopper, rec := newFakeStopper(), &fakeRecorder{}
	r := outageReaper(store, stopper, rec, base, lifecycle.Config{
		MaxAge:       time.Hour,
		RunnerOnline: func(uuid.UUID) bool { return false },
	})
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !slices.Contains(stopper.maxAgeStopped, a.ID) || countAction(rec, "run.max_age.expire") != 1 {
		t.Error("a run on an offline runner must still be stopped at its max age")
	}
	if countAction(rec, "run.autostop") != 0 {
		t.Error("an idle stop was written for a run on an offline runner")
	}
}
