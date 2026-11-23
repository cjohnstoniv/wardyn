// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// fakeTerminalSandboxPager simulates a store.RunsKeysetPager-backed
// SweepTerminalSandboxesAfter over a table of runs, newest first, paged the
// same way real Postgres would: a page starts after the cursor's run, and a
// page crossing the end is short. swept is always 0 — the production reality
// (an abandoned kill tail or a leaked sandbox is rare) — so a test asserting
// on cursors cannot pass by accident if the paging arithmetic wraps or
// advances on swept instead of on pageLen. Every call is recorded so a test
// can assert on the exact sequence of pages a run of ticks issued.
type fakeTerminalSandboxPager struct {
	mu     sync.Mutex
	runs   []store.RunCursor // newest first
	calls  []store.RunCursor // the cursor each call was made after
	limits []int
	firsts []store.RunCursor // the first run each call returned
}

// newRunCursors is n runs, newest first, each a minute older than the last.
func newRunCursors(n int) []store.RunCursor {
	now := time.Now().UTC().Truncate(time.Second)
	out := make([]store.RunCursor, n)
	for i := range out {
		out[i] = store.RunCursor{CreatedAt: now.Add(-time.Duration(i) * time.Minute), ID: uuid.New()}
	}
	return out
}

// prepend adds n runs newer than every run in the table.
func (f *fakeTerminalSandboxPager) prepend(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	newer := newRunCursors(n)
	for i := range newer {
		newer[i].CreatedAt = newer[i].CreatedAt.Add(24 * time.Hour)
	}
	f.runs = append(newer, f.runs...)
}

func (f *fakeTerminalSandboxPager) SweepTerminalSandboxesAfter(_ context.Context, after store.RunCursor, limit int) (int, int, store.RunCursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, after)
	f.limits = append(f.limits, limit)
	start := 0
	if after.ID != uuid.Nil {
		start = len(f.runs)
		for i, r := range f.runs {
			if r.ID == after.ID {
				start = i + 1
				break
			}
		}
	}
	page := f.runs[start:min(start+limit, len(f.runs))]
	var first, next store.RunCursor
	if len(page) > 0 {
		first, next = page[0], page[len(page)-1]
	}
	f.firsts = append(f.firsts, first)
	return 0, len(page), next, nil // swept=0: nothing in the fake table is ever an orphan
}

func (f *fakeTerminalSandboxPager) snapshot() []store.RunCursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.RunCursor(nil), f.calls...)
}

// waitForCalls polls until the fake has recorded at least n calls, or fails
// the test after a generous deadline.
func waitForCalls(t *testing.T, f *fakeTerminalSandboxPager, n int) []store.RunCursor {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls := f.snapshot(); len(calls) >= n {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("sweeper issued %d calls in time, want at least %d", len(f.snapshot()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRunTerminalSandboxSweeper_SweepsEveryRunAcrossTicksOnePageAtATime is
// #710's paging pin: a table with more runs than one page is swept fully
// across several ticks, each of which reads at most one page. The cursor
// advances to the page's last run (not by swept, which the fake holds at 0
// throughout — the production reality) and wraps back to the newest run only
// once a page comes back short (the end of the table), never on every tick.
func TestRunTerminalSandboxSweeper_SweepsEveryRunAcrossTicksOnePageAtATime(t *testing.T) {
	fake := &fakeTerminalSandboxPager{runs: newRunCursors(2*terminalSandboxSweepPageSize + 50)} // two full pages + a short one
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTerminalSandboxSweeper(ctx, fake, nil, time.Millisecond, nil)
		close(done)
	}()

	// 4 calls: full, full, short (wraps the cursor to the newest run), full
	// again — enough to prove both the paging and the wrap.
	calls := waitForCalls(t, fake, 4)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runTerminalSandboxSweeper did not exit after ctx was cancelled")
	}

	fake.mu.Lock()
	limits := append([]int(nil), fake.limits...)
	fake.mu.Unlock()
	for i, l := range limits[:4] {
		if l != terminalSandboxSweepPageSize {
			t.Errorf("call %d: limit = %d, want %d — each tick must read at most one page", i, l, terminalSandboxSweepPageSize)
		}
	}
	wantCursors := []store.RunCursor{{}, fake.runs[terminalSandboxSweepPageSize-1], fake.runs[2*terminalSandboxSweepPageSize-1], {}}
	for i, want := range wantCursors {
		if calls[i] != want {
			t.Errorf("call %d cursor = %+v, want %+v — the cursor must advance to the page's last run and "+
				"wrap only on a short page, never on swept (which is 0 here, like production almost always is)",
				i, calls[i], want)
		}
	}
}

// TestTerminalSandboxSweepTick_RunsCreatedBetweenTicksDoNotHoldItBack pins the reason for a cursor: runs created
// between two ticks do not move the next page, which an offset would have shifted by one row per new run.
func TestTerminalSandboxSweepTick_RunsCreatedBetweenTicksDoNotHoldItBack(t *testing.T) {
	fake := &fakeTerminalSandboxPager{runs: newRunCursors(2*terminalSandboxSweepPageSize + 50)}
	original := append([]store.RunCursor(nil), fake.runs...)
	cursor, err := terminalSandboxSweepTick(context.Background(), fake, store.RunCursor{})
	if err != nil {
		t.Fatalf("first tick: %v", err)
	}
	fake.prepend(250)
	if _, err := terminalSandboxSweepTick(context.Background(), fake, cursor); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got, want := fake.firsts[1], original[terminalSandboxSweepPageSize]; got != want {
		t.Errorf("the second tick began at %+v, want the run after the first page, %+v", got, want)
	}
}

// alwaysDeniedLock never grants the tick lock — the "another control plane
// holds it" case.
func alwaysDeniedLock(context.Context) (func(), bool) { return nil, false }

// TestRunTerminalSandboxSweeper_SkipsTheTickWhenTheLockIsHeldElsewhere pins
// that a tick which cannot take the per-control-plane lock does no work at
// all — it neither sweeps nor advances the cursor — rather than racing
// whoever holds it.
func TestRunTerminalSandboxSweeper_SkipsTheTickWhenTheLockIsHeldElsewhere(t *testing.T) {
	fake := &fakeTerminalSandboxPager{runs: newRunCursors(10)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTerminalSandboxSweeper(ctx, fake, alwaysDeniedLock, time.Millisecond, nil)
		close(done)
	}()

	// Several tick intervals' worth of real time, so a sweep that ignored the
	// lock would have shown up by now.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runTerminalSandboxSweeper did not exit after ctx was cancelled")
	}

	if calls := fake.snapshot(); len(calls) != 0 {
		t.Errorf("SweepTerminalSandboxesAfter was called %d times while the lock was held elsewhere, want 0: %+v", len(calls), calls)
	}
}

// fakeBackgrounder pairs fakeTerminalSandboxPager with a GoBackground that
// tracks its own WaitGroup, so a test can prove startTerminalSandboxSweeper's
// registration without a real *api.Server.
type fakeBackgrounder struct {
	fakeTerminalSandboxPager
	wg sync.WaitGroup
}

func (f *fakeBackgrounder) GoBackground(fn func()) {
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		fn()
	}()
}

// TestStartTerminalSandboxSweeper_ShutdownWaitsForTheTickerToExit is #710's
// shutdown pin: the ticker is registered through GoBackground (not a bare
// `go`), so a wait tracking that registration — the same shape WaitBackground
// applies to a real *api.Server — must not return before the ticker's own
// ctx-cancelled exit, and must not hang forever after it.
func TestStartTerminalSandboxSweeper_ShutdownWaitsForTheTickerToExit(t *testing.T) {
	fake := &fakeBackgrounder{}
	ctx, cancel := context.WithCancel(context.Background())
	startTerminalSandboxSweeper(ctx, fake, nil, time.Millisecond, nil)

	waited := make(chan struct{})
	go func() {
		fake.wg.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("the tracked wait returned before shutdown even began — the ticker was never registered through GoBackground")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("the tracked wait never returned after ctx was cancelled — the ticker did not exit, or was not registered through GoBackground")
	}
}

// TestStartBackgroundWorkers_WiresTerminalSandboxSweeper pins the wiring:
// startTerminalSandboxSweeper needs a real *pgxpool.Pool and *api.Server to
// exercise for real, so nothing else in this package's test suite calls it —
// removing its call from startBackgroundWorkers would otherwise pass the
// whole package silently. Walks the AST rather than grepping source text, so
// a reformat cannot defeat it (the same shape the deleted TestServeShutdownOrder
// used).
func TestStartBackgroundWorkers_WiresTerminalSandboxSweeper(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "boot_serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "startBackgroundWorkers" {
			fn = f
		}
	}
	if fn == nil {
		t.Fatal("boot_serve.go no longer defines startBackgroundWorkers")
	}
	var reconcilePos, sweeperPos token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "startTerminalSandboxSweeper" && sweeperPos == token.NoPos {
				sweeperPos = call.Pos()
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "ReconcileOnBoot" && reconcilePos == token.NoPos {
				reconcilePos = call.Pos()
			}
		}
		return true
	})
	if sweeperPos == token.NoPos {
		t.Fatal("startBackgroundWorkers no longer calls startTerminalSandboxSweeper — the ticker would never run")
	}
	if reconcilePos == token.NoPos {
		t.Fatal("startBackgroundWorkers no longer calls ReconcileOnBoot — the guard below has nothing to order against")
	}
	if sweeperPos < reconcilePos {
		t.Error("startBackgroundWorkers calls startTerminalSandboxSweeper before ReconcileOnBoot — the ticker must start " +
			"AFTER boot reconciliation returns, so an overlapping tick cannot double-tear an orphan " +
			"TestReconcileOnBoot_SweepsOrphanedTerminalSandbox requires be torn down exactly once")
	}
}

// countingLock grants the tick lock every time it is called and counts how
// many times it was taken vs. released, so a test can prove release always
// runs — even when the tick panics or overruns its deadline — without a real
// Postgres connection.
type countingLock struct {
	mu       sync.Mutex
	taken    int
	released int
}

func (l *countingLock) lock(context.Context) (func(), bool) {
	l.mu.Lock()
	l.taken++
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		l.released++
		l.mu.Unlock()
	}, true
}

func (l *countingLock) counts() (taken, released int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.taken, l.released
}

// TestTerminalSandboxSweepLockedTick_ReleasesExactlyOnceOnANormalTick is
// the control case for the release pins: a tick that completes normally takes the lock once and
// releases it exactly once.
func TestTerminalSandboxSweepLockedTick_ReleasesExactlyOnceOnANormalTick(t *testing.T) {
	lock := &countingLock{}
	fake := &fakeTerminalSandboxPager{runs: newRunCursors(5)}

	terminalSandboxSweepLockedTick(context.Background(), fake, lock.lock, store.RunCursor{}, nil)

	taken, released := lock.counts()
	if taken != 1 || released != 1 {
		t.Errorf("taken=%d released=%d, want 1 and 1", taken, released)
	}
}

// panicPager panics on every call, simulating a bug surfacing inside the
// sweep itself (Runner.Status, stopSandboxOrAudit, revokeRunCascade) rather
// than a lock or deadline failure.
type panicPager struct{}

func (panicPager) SweepTerminalSandboxesAfter(context.Context, store.RunCursor, int) (int, int, store.RunCursor, error) {
	panic("boom")
}

// TestTerminalSandboxSweepLockedTick_ReleasesEvenWhenThePagerPanics pins that
// a panic unwinding out of the sweep must still release the lock,
// because release is deferred immediately after the lock is taken, not
// called inline after the sweep returns — the shape production's outer
// goSafe (main.go) also recovers from, reproduced here with a plain recover
// so the test does not depend on that helper.
func TestTerminalSandboxSweepLockedTick_ReleasesEvenWhenThePagerPanics(t *testing.T) {
	lock := &countingLock{}

	func() {
		defer func() { _ = recover() }()
		terminalSandboxSweepLockedTick(context.Background(), panicPager{}, lock.lock, store.RunCursor{}, nil)
	}()

	taken, released := lock.counts()
	if taken != 1 || released != 1 {
		t.Errorf("taken=%d released=%d, want 1 and 1 — a panic inside the tick must not leak the lock", taken, released)
	}
}

// slowPager blocks until ctx is done, then returns — simulating a wedged
// Runner.Status/stopSandboxOrAudit/revokeRunCascade call that only the tick's
// own deadline, never the call itself, can bound.
type slowPager struct{}

func (slowPager) SweepTerminalSandboxesAfter(ctx context.Context, _ store.RunCursor, _ int) (int, int, store.RunCursor, error) {
	<-ctx.Done()
	return 0, 0, store.RunCursor{}, ctx.Err()
}

// TestTerminalSandboxSweepLockedTick_ReturnsAndReleasesWhenTheDeadlineIsExceeded
// pins that a tick whose sweep call wedges past
// terminalSandboxSweepTickTimeout still returns (bounded by that timeout, not
// by the wedged call) and still releases the lock.
func TestTerminalSandboxSweepLockedTick_ReturnsAndReleasesWhenTheDeadlineIsExceeded(t *testing.T) {
	prev := terminalSandboxSweepTickTimeout
	terminalSandboxSweepTickTimeout = 20 * time.Millisecond
	t.Cleanup(func() { terminalSandboxSweepTickTimeout = prev })

	lock := &countingLock{}
	done := make(chan struct{})
	go func() {
		terminalSandboxSweepLockedTick(context.Background(), slowPager{}, lock.lock, store.RunCursor{}, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminalSandboxSweepLockedTick did not return after its deadline elapsed — a wedged sweep call pinned it")
	}

	taken, released := lock.counts()
	if taken != 1 || released != 1 {
		t.Errorf("taken=%d released=%d, want 1 and 1 — a tick that exceeds its deadline must still release the lock", taken, released)
	}
}
