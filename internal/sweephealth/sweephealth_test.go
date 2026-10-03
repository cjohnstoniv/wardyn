// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sweephealth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock shared by replicas in one test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func tickOf(t *testing.T, st Store, sweep string) Tick {
	t.Helper()
	m, err := st.Ticks(context.Background())
	if err != nil {
		t.Fatalf("Ticks: %v", err)
	}
	return m[sweep]
}

// An erroring tick moves the attempt and never the success, for every sweep.
func TestTick_ErrorMovesAttemptNotSuccess(t *testing.T) {
	for _, name := range Names {
		t.Run(name, func(t *testing.T) {
			clk, st := newClock(), NewMemStore()
			tr := New(st, "r1", clk.now)
			tr.Register(Sweep{Name: name, Interval: time.Minute})

			if err := tr.Tick(context.Background(), name, func(context.Context) error { return nil }); err != nil {
				t.Fatalf("clean tick: %v", err)
			}
			first := clk.now()
			clk.advance(time.Minute)
			boom := errors.New("boom")
			if err := tr.Tick(context.Background(), name, func(context.Context) error { return boom }); !errors.Is(err, boom) {
				t.Fatalf("Tick returned %v, want the body's error", err)
			}
			got := tickOf(t, st, name)
			if !got.SucceededAt.Equal(first) {
				t.Errorf("success moved to %s after an erroring tick, want it kept at %s", got.SucceededAt, first)
			}
			if !got.AttemptedAt.Equal(clk.now()) {
				t.Errorf("attempt = %s, want %s", got.AttemptedAt, clk.now())
			}
		})
	}
}

// A body that panics leaves the success where it was: the success write follows
// the call, it is not a deferred write that ignores how the body ended.
func TestTick_PanicLeavesSuccessUnchanged(t *testing.T) {
	clk, st := newClock(), NewMemStore()
	tr := New(st, "r1", clk.now)
	_ = tr.Tick(context.Background(), CredentialExpiry, func(context.Context) error { return nil })
	first := clk.now()
	clk.advance(time.Hour)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not propagate to the caller's recover")
			}
		}()
		_ = tr.Tick(context.Background(), CredentialExpiry, func(context.Context) error { panic("stalled") })
	}()
	got := tickOf(t, st, CredentialExpiry)
	if !got.SucceededAt.Equal(first) {
		t.Fatalf("success = %s after a panicking tick, want it unchanged at %s", got.SucceededAt, first)
	}
	if !got.AttemptedAt.Equal(clk.now()) {
		t.Fatalf("attempt = %s, want the panicking tick's %s", got.AttemptedAt, clk.now())
	}
}

// A tick write that fails never stops the sweep: the body still runs, once.
func TestTick_FailedWriteStillRunsBody(t *testing.T) {
	st := NewMemStore()
	st.SetErr(errors.New("store down"))
	tr := New(st, "r1", nil)
	runs := 0
	if err := tr.Tick(context.Background(), RunSecret, func(context.Context) error { runs++; return nil }); err != nil {
		t.Fatalf("Tick returned %v: a failed write must not become the sweep's error", err)
	}
	if runs != 1 {
		t.Fatalf("body ran %d times with the store down, want 1", runs)
	}
}

// A nil Tracker runs the body and records nothing, so a caller with no health
// wiring needs no branch.
func TestTick_NilTrackerRunsBody(t *testing.T) {
	var tr *Tracker
	ran := false
	if err := tr.Tick(context.Background(), IdleReaper, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("nil tracker: ran=%v err=%v, want the body run and no error", ran, err)
	}
	tr.Register(Sweep{Name: IdleReaper, Interval: time.Minute})
	if got, err := tr.Status(context.Background()); got != nil || err != nil {
		t.Fatalf("nil tracker Status = %v, %v, want nothing", got, err)
	}
}

// Every replica reads the shared record: a follower that never ticks reports
// the leader's ticks, not its own empty ones.
func TestStatus_FollowerReportsTheLeadersTicks(t *testing.T) {
	clk, st := newClock(), NewMemStore()
	leader, follower := New(st, "leader", clk.now), New(st, "follower", clk.now)
	sweeps := []Sweep{{Name: ApprovalExpiry, Interval: 10 * time.Minute}, {Name: RunSecret, Interval: 15 * time.Minute}}
	leader.Register(sweeps...)
	follower.Register(sweeps...)

	clk.advance(10 * time.Minute)
	_ = leader.Tick(context.Background(), ApprovalExpiry, func(context.Context) error { return nil })

	got, err := follower.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(got) != 2 || got[0].Name != ApprovalExpiry {
		t.Fatalf("follower status = %+v, want both registered sweeps in order", got)
	}
	if !got[0].SucceededAt.Equal(clk.now()) || got[0].Stale {
		t.Errorf("follower reads approval_expiry as %+v, want the leader's tick at %s and not stale", got[0], clk.now())
	}
	if !got[1].SucceededAt.IsZero() {
		t.Errorf("run_secret has no tick yet, follower reads success %s", got[1].SucceededAt)
	}
}

// Two replicas, the leader stops ticking: the follower, which registered the
// sweep without ever running it, reports it stale after three intervals.
func TestStatus_FollowerSeesAStoppedLeaderAfterThreeIntervals(t *testing.T) {
	clk, st := newClock(), NewMemStore()
	iv := 10 * time.Minute
	leader, follower := New(st, "leader", clk.now), New(st, "follower", clk.now)
	for _, tr := range []*Tracker{leader, follower} {
		tr.Register(Sweep{Name: ApprovalExpiry, Interval: iv})
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ { // the leader ticks on schedule, then stops
		clk.advance(iv)
		_ = leader.Tick(ctx, ApprovalExpiry, func(context.Context) error { return nil })
	}
	stale := func() bool {
		s, err := follower.Status(ctx)
		if err != nil || len(s) != 1 {
			t.Fatalf("Status = %+v, %v", s, err)
		}
		return s[0].Stale
	}
	if stale() {
		t.Fatal("stale while the leader is ticking")
	}
	clk.advance(3*iv - time.Second)
	if stale() {
		t.Fatal("stale before three intervals without a success")
	}
	clk.advance(time.Second)
	if !stale() {
		t.Fatal("not stale after three intervals without a success")
	}
}

// A sweep with no success yet warns only once three intervals have passed since
// this process started, however old the record's other rows are.
func TestStatus_GraceRunsFromProcessStart(t *testing.T) {
	clk, st := newClock(), NewMemStore()
	// An old success from a previous process: far older than three intervals.
	_ = st.RecordTick(context.Background(), CredentialExpiry, "old", true, clk.now().Add(-30*24*time.Hour))
	tr := New(st, "r1", clk.now)
	tr.Register(Sweep{Name: CredentialExpiry, Interval: 24 * time.Hour})
	ctx := context.Background()

	s, _ := tr.Status(ctx)
	if s[0].Stale {
		t.Fatal("stale at process start on the strength of a previous process's old success")
	}
	clk.advance(72*time.Hour - time.Second)
	if s, _ = tr.Status(ctx); s[0].Stale {
		t.Fatal("stale before three intervals since process start")
	}
	clk.advance(time.Second)
	if s, _ = tr.Status(ctx); !s[0].Stale {
		t.Fatal("not stale three intervals after process start with no success")
	}
}

// A sweep nobody registered is never reported, so one whose start condition
// does not hold can never be stale.
func TestStatus_UnregisteredSweepIsNeverReported(t *testing.T) {
	clk, st := newClock(), NewMemStore()
	tr := New(st, "r1", clk.now)
	tr.Register(Sweep{Name: RunSecret, Interval: time.Minute})
	clk.advance(1000 * time.Hour)
	s, err := tr.Status(context.Background())
	if err != nil || len(s) != 1 || s[0].Name != RunSecret {
		t.Fatalf("Status = %+v, %v, want only the registered run_secret", s, err)
	}
}

// A record that cannot be read is "cannot tell", never stale.
func TestStatus_UnreadableRecordIsAnErrorNotStale(t *testing.T) {
	clk, st := newClock(), NewMemStore()
	tr := New(st, "r1", clk.now)
	tr.Register(Sweep{Name: RunSecret, Interval: time.Minute})
	clk.advance(1000 * time.Hour)
	st.SetErr(errors.New("store down"))
	if s, err := tr.Status(context.Background()); err == nil || s != nil {
		t.Fatalf("Status = %+v, %v, want an error and no sweeps", s, err)
	}
}

// An older write does not move a timestamp backwards.
func TestMemStore_OlderWriteDoesNotMoveTimeBackwards(t *testing.T) {
	st := NewMemStore()
	ctx := context.Background()
	newer := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	_ = st.RecordTick(ctx, RunWatcher, "new", true, newer)
	_ = st.RecordTick(ctx, RunWatcher, "old", false, newer.Add(-time.Hour))
	_ = st.RecordTick(ctx, RunWatcher, "old", true, newer.Add(-time.Hour))
	got := tickOf(t, st, RunWatcher)
	if !got.AttemptedAt.Equal(newer) || !got.SucceededAt.Equal(newer) || got.Replica != "new" {
		t.Fatalf("an older write moved the record: %+v", got)
	}
}
