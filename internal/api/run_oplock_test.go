// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sweepReadKey marks a context whose GetRun the delayedReadStore holds back.
type sweepReadKey struct{}

// delayedReadStore hands a marked context's GetRun result back only once
// resume is closed: a lease pass that has read the row and not yet acted on it.
type delayedReadStore struct {
	*reviveStore
	read   chan struct{}
	resume chan struct{}
}

func (s *delayedReadStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := s.reviveStore.GetRun(ctx, id)
	if ctx.Value(sweepReadKey{}) != nil {
		close(s.read)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return types.AgentRun{}, ctx.Err()
		}
	}
	return run, err
}

// orderedProxyRunner records the order of ReplaceProxy and StopProxy calls.
// onReplace runs right after a replace lands.
type orderedProxyRunner struct {
	*reviveRunner
	evMu      sync.Mutex
	events    []string
	onReplace func()
}

func (r *orderedProxyRunner) note(ev string) {
	r.evMu.Lock()
	r.events = append(r.events, ev)
	r.evMu.Unlock()
}

func (r *orderedProxyRunner) ReplaceProxy(ctx context.Context, ref string, cfg []byte) error {
	if err := r.reviveRunner.ReplaceProxy(ctx, ref, cfg); err != nil {
		return err
	}
	r.note("replace")
	if r.onReplace != nil {
		r.onReplace()
	}
	return nil
}

func (r *orderedProxyRunner) StopProxy(ctx context.Context, ref string) error {
	r.note("stop")
	return r.reviveRunner.StopProxy(ctx, ref)
}

// stopsAfterReplace counts StopProxy calls that came after the last replace.
func (r *orderedProxyRunner) stopsAfterReplace() int {
	r.evMu.Lock()
	defer r.evMu.Unlock()
	n := 0
	for _, ev := range r.events {
		switch ev {
		case "replace":
			n = 0
		case "stop":
			n++
		}
	}
	return n
}

// TestReviveRun_StaleLeaseSnapshotCannotStopTheRevivedProxy (#1480): a lease
// pass that has read a run's lost mark and is about to stop its proxy races a
// revive of the same run. Whichever way they order, the revive's new proxy
// must never be stopped by the lease pass: the pass holds the run's lock
// through its read and its stop, so the revive's claim waits for it.
func TestReviveRun_StaleLeaseSnapshotCannotStopTheRevivedProxy(t *testing.T) {
	f := newReviveFixture(t)
	st := &delayedReadStore{reviveStore: f.rs, read: make(chan struct{}), resume: make(chan struct{})}
	rn := &orderedProxyRunner{reviveRunner: f.rr}
	f.srv.cfg.Store, f.srv.cfg.Runner = st, rn
	var revokesAtReplace int
	rn.onReplace = func() { revokesAtReplace = f.brk.count(f.run.ID) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		f.srv.leaseRun(context.WithValue(ctx, sweepReadKey{}, true), st, f.run)
	}()
	select {
	case <-st.read:
	case <-ctx.Done():
		t.Fatal("the lease pass never reached its read of the run")
	}

	reviveDone := make(chan *reviveError, 1)
	go func() {
		_, err := f.srv.reviveRunProxy(ctx, f.run, types.ActorHuman, "owner", true)
		reviveDone <- err
	}()
	// Unfixed, the revive finishes while the lease pass sits on its snapshot.
	// Fixed, it waits on the run's lock, so the window just lapses.
	var rerr *reviveError
	revived := false
	select {
	case rerr = <-reviveDone:
		revived = true
	case <-time.After(300 * time.Millisecond):
	}
	close(st.resume)
	select {
	case <-leaseDone:
	case <-ctx.Done():
		t.Fatal("the lease pass did not finish")
	}
	if !revived {
		rerr = <-reviveDone
	}
	if rerr != nil {
		t.Fatalf("revive: %+v", rerr)
	}

	cur, err := st.GetRun(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := rn.stopsAfterReplace(); n != 0 {
		t.Errorf("StopProxy ran %d time(s) after the revive replaced the proxy; want 0 (events %v)", n, rn.events)
	}
	if cur.State != types.RunRunning || cur.LostAt != nil {
		t.Errorf("state %s, lost mark %v; want RUNNING with no lost mark", cur.State, cur.LostAt)
	}
	if got := f.brk.count(f.run.ID); got != revokesAtReplace {
		t.Errorf("broker revoked %d time(s) after the revive replaced the proxy; want 0", got-revokesAtReplace)
	}
	if ce := f.rs.containmentError(); ce != "" {
		t.Errorf("containment_error %q after the revive; want none", ce)
	}
}

// TestRunOp_AHeldRunDoesNotDelayAnotherOrTheTokenSweep: the lock is per run,
// and the sweep never waits on it. A lease pass that finds its run's lock held
// skips it, leaving the run alone until next pass, and a different run's lock
// is free.
func TestRunOp_AHeldRunDoesNotDelayAnotherOrTheTokenSweep(t *testing.T) {
	f := newReviveFixture(t)
	other := uuid.New()
	_, unlock, err := f.srv.lockRunOp(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stops, revokes := f.lr.proxyStopCount(), f.brk.count(f.run.ID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.leaseRun(context.Background(), f.rs, f.run)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a lease pass blocked on a held run lock; the sweep must skip it")
	}
	if f.lr.proxyStopCount() != stops || f.brk.count(f.run.ID) != revokes {
		t.Error("a lease pass acted on a run whose lock was held")
	}
	if _, release, ok := f.srv.tryLockRunOp(context.Background(), other); !ok {
		t.Error("a held lock on one run blocked another run")
	} else {
		release()
	}
	if _, _, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID); ok {
		t.Error("tryLockRunOp took a lock that was held")
	}
	// The lapsed-token sweep never takes the lock.
	g := newLostFixture(t)
	g.ls.lapsed = true
	_, hold, err := g.srv.lockRunOp(context.Background(), g.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	g.sweepTokens(t)
	hold()
	if lostAt, _ := g.st.lost(); lostAt == nil {
		t.Error("the lapsed-token sweep waited on, or was blocked by, a run lock")
	}

	unlock()
	f.srv.leaseRun(context.Background(), f.rs, f.run)
	if f.lr.proxyStopCount() == stops {
		t.Error("a kept run with no revive in progress was not stopped on the next pass")
	}
}

// TestRunOp_AnExpiredKeptRunIsTornDownWhileItsReviveIsInFlight: a member who
// keeps reviving must not dodge expiry. The teardown sits outside the lock's
// skip path.
func TestRunOp_AnExpiredKeptRunIsTornDownWhileItsReviveIsInFlight(t *testing.T) {
	f := newLostFixture(t)
	end := f.now.Add(time.Hour)
	f.st.run.EndsAt = &end
	f.ls.lapsed = true
	f.sweepTokens(t)
	f.now = end.Add(8 * 24 * time.Hour)
	run, _ := f.st.GetRun(context.Background(), f.run.ID)
	f.srv.reviving.Store(run.ID, struct{}{})
	_, unlock, err := f.srv.lockRunOp(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	f.srv.leaseRun(context.Background(), f.ls, run)
	if f.st.State() != types.RunStopped {
		t.Fatalf("state %s; want the expired kept run STOPPED even with a revive in flight", f.st.State())
	}
}

// TestRunOp_ReviveFailuresReleaseTheLock: a revive that fails its replace or
// its agent start has settled by the time it returns, and its lock is free.
func TestRunOp_ReviveFailuresReleaseTheLock(t *testing.T) {
	t.Run("replace fails", func(t *testing.T) {
		f := newReviveFixture(t)
		f.rr.replaceErr = runner.ErrProxyReplaceFailed
		if code := f.revive(t); code != http.StatusBadGateway {
			t.Fatalf("revive: code %d, want 502", code)
		}
		_, release, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
		if !ok {
			t.Fatal("the revive left the run's lock held")
		}
		release()
	})
	t.Run("agent start fails", func(t *testing.T) {
		f, sr := newRebootFixture(t)
		sr.startErr = context.DeadlineExceeded
		if code := f.revive(t); code != http.StatusBadGateway {
			t.Fatalf("revive: code %d, want 502", code)
		}
		_, release, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
		if !ok {
			t.Fatal("the revive left the run's lock held")
		}
		release()
	})
}

// hangingEndRunner's first EndSandbox hangs until its context ends, as a
// wedged daemon's container stop would.
type hangingEndRunner struct {
	*startingRunner
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *hangingEndRunner) EndSandbox(ctx context.Context, ref string) error {
	hang := false
	r.once.Do(func() { hang = true })
	if !hang {
		return r.startingRunner.EndSandbox(ctx, ref)
	}
	close(r.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}

// TestKeepRebootedRun_AWedgedStopReleasesTheLockWithinItsBound: the watcher's
// keep takes the run's lock and calls the daemon. A stop that never answers
// must not hold the lock for ever, or every later revive of the run waits on it.
// The bound is the one the lapsed-token sweep puts on its own work.
func TestKeepRebootedRun_AWedgedStopReleasesTheLockWithinItsBound(t *testing.T) {
	old := keepRebootedRunTimeout
	keepRebootedRunTimeout = 100 * time.Millisecond
	t.Cleanup(func() { keepRebootedRunTimeout = old })

	f := newReviveFixture(t)
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	live := f.st.run
	hr := &hangingEndRunner{startingRunner: &startingRunner{reviveRunner: f.rr}, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(hr.release) })
	f.srv.cfg.Runner = hr
	code := 137

	kept := make(chan bool, 1)
	go func() { kept <- f.srv.keepRebootedRun(context.Background(), live, runner.Status{ExitCode: &code}) }()
	select {
	case <-hr.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the keep never reached the stop")
	}
	lost, err := f.st.GetRun(context.Background(), f.run.ID)
	if err != nil || lost.LostReason != types.LostReboot {
		t.Fatalf("the run is not lost to a reboot yet: %v %q", err, lost.LostReason)
	}

	revived := make(chan *reviveError, 1)
	go func() {
		_, rerr := f.srv.reviveRunProxy(context.Background(), lost, types.ActorHuman, "owner", true)
		revived <- rerr
	}()
	select {
	case <-kept:
	case <-time.After(5 * time.Second):
		t.Fatal("a wedged stop held keepRebootedRun, and the run's lock, past its bound")
	}
	select {
	case rerr := <-revived:
		if rerr != nil {
			t.Fatalf("the revive that waited on the lock: %+v", rerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revive waiting on the run's lock never proceeded")
	}
}
