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

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// fakeTerminalSandboxPager simulates a store.Pager-backed
// SweepTerminalSandboxesPage over a fixed-size run table, offset-paged the
// same way real Postgres would: a page starting past the end is empty, and a
// page crossing the end is short. swept is always 0 — the production reality
// (an abandoned kill tail or a leaked sandbox is rare) — so a test asserting
// on offsets cannot pass by accident if the paging arithmetic wraps or
// advances on swept instead of on pageLen. Every call is recorded so a test
// can assert on the exact sequence of pages a run of ticks issued.
type fakeTerminalSandboxPager struct {
	mu        sync.Mutex
	totalRuns int
	calls     []store.Page
}

func (f *fakeTerminalSandboxPager) SweepTerminalSandboxesPage(_ context.Context, page store.Page) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, page)
	remaining := f.totalRuns - page.Offset
	if remaining < 0 {
		remaining = 0
	}
	n := remaining
	if n > page.Limit {
		n = page.Limit
	}
	return 0, n, nil // swept=0: nothing in the fake table is ever an orphan
}

func (f *fakeTerminalSandboxPager) snapshot() []store.Page {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Page(nil), f.calls...)
}

// waitForCalls polls until the fake has recorded at least n calls, or fails
// the test after a generous deadline.
func waitForCalls(t *testing.T, f *fakeTerminalSandboxPager, n int) []store.Page {
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
// across several ticks, each of which reads at most one page. The offset
// advances by pageLen (not by swept, which the fake holds at 0 throughout —
// the production reality) and wraps back to 0 only once a page comes back
// short (the end of the table), never on every tick.
func TestRunTerminalSandboxSweeper_SweepsEveryRunAcrossTicksOnePageAtATime(t *testing.T) {
	fake := &fakeTerminalSandboxPager{totalRuns: 2*terminalSandboxSweepPageSize + 50} // two full pages + a short one
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTerminalSandboxSweeper(ctx, fake, nil, time.Millisecond)
		close(done)
	}()

	// 4 calls: full, full, short (wraps offset to 0), full again — enough to
	// prove both the paging and the wrap.
	calls := waitForCalls(t, fake, 4)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runTerminalSandboxSweeper did not exit after ctx was cancelled")
	}

	for i, c := range calls {
		if c.Limit != terminalSandboxSweepPageSize {
			t.Errorf("call %d: Limit = %d, want %d — each tick must read at most one page", i, c.Limit, terminalSandboxSweepPageSize)
		}
	}
	wantOffsets := []int{0, terminalSandboxSweepPageSize, 2 * terminalSandboxSweepPageSize, 0}
	for i, want := range wantOffsets {
		if calls[i].Offset != want {
			t.Errorf("call %d offset = %d, want %d (offsets so far: %+v) — the offset must advance by pageLen and "+
				"wrap only on a short page, never on swept (which is 0 here, like production almost always is)",
				i, calls[i].Offset, want, calls)
		}
	}
}

// alwaysDeniedLock never grants the tick lock — the "another control plane
// holds it" case.
func alwaysDeniedLock(context.Context) (func(), bool) { return nil, false }

// TestRunTerminalSandboxSweeper_SkipsTheTickWhenTheLockIsHeldElsewhere pins
// that a tick which cannot take the per-control-plane lock does no work at
// all — it neither sweeps nor advances the offset — rather than racing
// whoever holds it.
func TestRunTerminalSandboxSweeper_SkipsTheTickWhenTheLockIsHeldElsewhere(t *testing.T) {
	fake := &fakeTerminalSandboxPager{totalRuns: 10}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTerminalSandboxSweeper(ctx, fake, alwaysDeniedLock, time.Millisecond)
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
		t.Errorf("SweepTerminalSandboxesPage was called %d times while the lock was held elsewhere, want 0: %+v", len(calls), calls)
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
	startTerminalSandboxSweeper(ctx, fake, nil, time.Millisecond)

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
// R2-1's control case: a tick that completes normally takes the lock once and
// releases it exactly once.
func TestTerminalSandboxSweepLockedTick_ReleasesExactlyOnceOnANormalTick(t *testing.T) {
	lock := &countingLock{}
	fake := &fakeTerminalSandboxPager{totalRuns: 5}

	terminalSandboxSweepLockedTick(context.Background(), fake, lock.lock, 0)

	taken, released := lock.counts()
	if taken != 1 || released != 1 {
		t.Errorf("taken=%d released=%d, want 1 and 1", taken, released)
	}
}

// panicPager panics on every call, simulating a bug surfacing inside the
// sweep itself (Runner.Status, stopSandboxOrAudit, revokeRunCascade) rather
// than a lock or deadline failure.
type panicPager struct{}

func (panicPager) SweepTerminalSandboxesPage(context.Context, store.Page) (int, int, error) {
	panic("boom")
}

// TestTerminalSandboxSweepLockedTick_ReleasesEvenWhenThePagerPanics is R2-1's
// pin: a panic unwinding out of the sweep must still release the lock,
// because release is deferred immediately after the lock is taken, not
// called inline after the sweep returns — the shape production's outer
// goSafe (main.go) also recovers from, reproduced here with a plain recover
// so the test does not depend on that helper.
func TestTerminalSandboxSweepLockedTick_ReleasesEvenWhenThePagerPanics(t *testing.T) {
	lock := &countingLock{}

	func() {
		defer func() { _ = recover() }()
		terminalSandboxSweepLockedTick(context.Background(), panicPager{}, lock.lock, 0)
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

func (slowPager) SweepTerminalSandboxesPage(ctx context.Context, _ store.Page) (int, int, error) {
	<-ctx.Done()
	return 0, 0, ctx.Err()
}

// TestTerminalSandboxSweepLockedTick_ReturnsAndReleasesWhenTheDeadlineIsExceeded
// is R2-2's pin: a tick whose sweep call wedges past
// terminalSandboxSweepTickTimeout still returns (bounded by that timeout, not
// by the wedged call) and still releases the lock.
func TestTerminalSandboxSweepLockedTick_ReturnsAndReleasesWhenTheDeadlineIsExceeded(t *testing.T) {
	prev := terminalSandboxSweepTickTimeout
	terminalSandboxSweepTickTimeout = 20 * time.Millisecond
	t.Cleanup(func() { terminalSandboxSweepTickTimeout = prev })

	lock := &countingLock{}
	done := make(chan struct{})
	go func() {
		terminalSandboxSweepLockedTick(context.Background(), slowPager{}, lock.lock, 0)
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
