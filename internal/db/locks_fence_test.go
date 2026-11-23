// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// The fence path with no database: the in-process gate, its refcount, the backoff and the mutex scope, over a fake
// connection whose statements are fenceStmts.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFence answers every try true unless busy names the key (a key another session holds), each after delay, and
// every check of the session's keys with verifyErr after verifyDelay.
type fakeFence struct {
	busy        func(LockKey) bool
	delay       time.Duration
	tries       atomic.Int64
	verifyDelay time.Duration
	verifyErr   error
}

func (f *fakeFence) try(_ context.Context, k LockKey) (bool, error) {
	f.tries.Add(1)
	time.Sleep(f.delay)
	return f.busy == nil || !f.busy(k), nil
}

func (f *fakeFence) unlock(context.Context, LockKey) (bool, error) { return true, nil }

func (f *fakeFence) verify(context.Context, []LockKey) error {
	time.Sleep(f.verifyDelay)
	return f.verifyErr
}

// fakeLocker is a PGLocker whose fence connections are one fake connection over stmts, so every take shares it.
func fakeLocker(stmts fenceStmts) *PGLocker {
	l := &PGLocker{sem: make(chan struct{}, 1)}
	c := newFenceConn(nil, stmts)
	l.fence.dial = func(context.Context) (*lockConn, error) { return c, nil }
	return l
}

func startKey(obj int32) LockKey { return LockKey{Class: RunStartLockClass, Obj: obj} }

func gateSize(l *PGLocker) int {
	l.fence.gateMu.Lock()
	defer l.fence.gateMu.Unlock()
	return len(l.fence.gate)
}

// TestFenceInProcessGateExcludesSameKey: the fake session answers every try true, as a re-entrant advisory lock
// does on one connection, yet a second holder in this process waits for the first and is refused at its wait.
func TestFenceInProcessGateExcludesSameKey(t *testing.T) {
	l := fakeLocker(&fakeFence{})
	ctx := context.Background()
	k := startKey(1)
	_, release, err := l.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	if _, _, err := l.Lock(ctx, k, 100*time.Millisecond); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("second Lock of a held key = %v; want ErrLockBusy", err)
	}
	if _, _, ok, err := l.TryLock(ctx, k); ok || err != nil {
		t.Fatalf("TryLock of a held key = %v, %v; want false, nil", ok, err)
	}
	release()
	_, again, err := l.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	again()
	if n := gateSize(l); n != 0 {
		t.Fatalf("gate holds %d keys after every hold was released; want 0", n)
	}
}

// TestFenceNestOtherClassIsOrderError: on a start lock's fence hold only a further start lock may be taken; any
// other class, through Lock, TryLock or LockQueued, is out of order.
func TestFenceNestOtherClassIsOrderError(t *testing.T) {
	l := fakeLocker(&fakeFence{})
	hctx, release, err := l.Lock(context.Background(), startKey(1), time.Second)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer release()
	other := NewLockKey(RunOpLockClass, "run")
	if _, _, err := l.Lock(hctx, other, time.Second); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("Lock of another class on a fence hold = %v; want ErrLockOrder", err)
	}
	if _, _, _, err := l.TryLock(hctx, other); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("TryLock of another class on a fence hold = %v; want ErrLockOrder", err)
	}
	if _, _, err := LockQueued(hctx, l, other, time.Second); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("LockQueued of another class on a fence hold = %v; want ErrLockOrder", err)
	}
	nctx, nrelease, err := l.Lock(hctx, startKey(2), time.Second)
	if err != nil {
		t.Fatalf("a further start lock on the fence hold: %v", err)
	}
	if got := HeldLocks(nctx); len(got) != 2 || heldFrom(nctx).conn != heldFrom(hctx).conn {
		t.Fatalf("nested hold carries %v on its own connection; want both keys on the fence hold's", got)
	}
	nrelease()
}

// TestFenceBackoffSchedule: the wait between tries starts at 25 ms and doubles up to 200 ms.
func TestFenceBackoffSchedule(t *testing.T) {
	want := []time.Duration{25, 50, 100, 200, 200, 200}
	d := fenceBackoffFirst
	for i, w := range want {
		if d != w*time.Millisecond {
			t.Fatalf("wait %d = %s; want %s", i, d, w*time.Millisecond)
		}
		d = nextFenceBackoff(d)
	}
}

// TestFenceGateDropsIdleKeys: after 1000 distinct keys are taken and released, and 100 takes have timed out at the
// gate, the gate holds no key.
func TestFenceGateDropsIdleKeys(t *testing.T) {
	l := fakeLocker(&fakeFence{})
	ctx := context.Background()
	for i := range 1000 {
		_, release, err := l.Lock(ctx, startKey(int32(i)), time.Second)
		if err != nil {
			t.Fatalf("Lock %d: %v", i, err)
		}
		release()
	}
	_, release, err := l.Lock(ctx, startKey(-1), time.Second)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := l.Lock(ctx, startKey(-1), time.Millisecond); !errors.Is(err, ErrLockBusy) {
				t.Errorf("Lock of a held key = %v; want ErrLockBusy", err)
			}
		}()
	}
	wg.Wait()
	release()
	if n := gateSize(l); n != 0 {
		t.Fatalf("gate holds %d keys; want 0", n)
	}
}

// TestFenceBackoffReleasesConnMutex: a take of a key held elsewhere polls for its whole wait, yet 20 takes of other
// keys on the same connection finish within a second together, because the connection's mutex is held for one
// statement. Were it held across the poll's sleeps (up to fenceBackoffMax each), they would take about twice that.
func TestFenceBackoffReleasesConnMutex(t *testing.T) {
	contended := startKey(1)
	stmts := &fakeFence{busy: func(k LockKey) bool { return k == contended }, delay: time.Millisecond}
	l := fakeLocker(stmts)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _, err := l.Lock(ctx, contended, 2*time.Second)
		done <- err
	}()
	waitFor(t, "the contended take to poll", time.Second, func() bool { return stmts.tries.Load() >= 2 })
	start := time.Now()
	for i := range 20 {
		_, release, err := l.Lock(ctx, startKey(int32(100+i)), time.Second)
		if err != nil {
			t.Fatalf("Lock %d: %v", i, err)
		}
		release()
	}
	if d := time.Since(start); d > 5*fenceBackoffMax {
		t.Fatalf("20 takes took %s beside a polling take; want under %s", d, 5*fenceBackoffMax)
	}
	if err := <-done; !errors.Is(err, ErrLockBusy) {
		t.Fatalf("the contended take = %v; want ErrLockBusy at its wait", err)
	}
}

// TestFenceTakeBoundedWhileDialling: with a dial that hangs, a take with a 200 ms wait returns within it, and so does
// a later take on the same slot, which waits for that one dial rather than starting another.
func TestFenceTakeBoundedWhileDialling(t *testing.T) {
	l := &PGLocker{sem: make(chan struct{}, 1)}
	var dials atomic.Int32
	unblock := make(chan struct{})
	defer close(unblock)
	l.fence.dial = func(context.Context) (*lockConn, error) {
		dials.Add(1)
		<-unblock
		return nil, errors.New("the database is unreachable")
	}
	const wait = 200 * time.Millisecond
	for i := range LockFenceConns + 1 {
		start := time.Now()
		if _, _, err := l.Lock(context.Background(), startKey(int32(i)), wait); !errors.Is(err, ErrLockUnavailable) {
			t.Fatalf("take %d while the dial hangs = %v; want ErrLockUnavailable", i, err)
		}
		if d := time.Since(start); d > wait+300*time.Millisecond {
			t.Fatalf("take %d with a %s wait returned after %s while the dial hung", i, wait, d)
		}
	}
	if n := dials.Load(); n != LockFenceConns {
		t.Fatalf("%d dials for %d slots; want one per slot", n, LockFenceConns)
	}
}

// TestFenceLockHeldWaitsOutABusyConnection: 50 holds on one fence connection ask LockHeld at once, each check taking
// 80 ms, so the last waits about 4 s for the connection; every one answers held, none lost. A check whose query
// fails then ends every hold on the connection with ErrLockLost.
func TestFenceLockHeldWaitsOutABusyConnection(t *testing.T) {
	stmts := &fakeFence{verifyDelay: 80 * time.Millisecond}
	l := fakeLocker(stmts)
	ctx := context.Background()
	holds := make([]context.Context, 50)
	for i := range holds {
		hctx, release, err := l.Lock(ctx, startKey(int32(i)), time.Second)
		if err != nil {
			t.Fatalf("Lock %d: %v", i, err)
		}
		defer release()
		holds[i] = hctx
	}
	var wg sync.WaitGroup
	for i, hctx := range holds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := LockHeld(hctx); err != nil {
				t.Errorf("LockHeld of hold %d on a busy fence connection = %v; want nil", i, err)
			}
		}()
	}
	wg.Wait()

	stmts.verifyErr = errors.New("the connection broke")
	if err := LockHeld(holds[0]); !errors.Is(err, ErrLockLost) {
		t.Fatalf("LockHeld whose query failed = %v; want ErrLockLost", err)
	}
	waitFor(t, "the other holds on the failed connection to end", time.Second, func() bool {
		return errors.Is(context.Cause(holds[1]), ErrLockLost)
	})
}
