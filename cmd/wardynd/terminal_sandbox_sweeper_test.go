// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// fakeTerminalSandboxPager simulates a store.Pager-backed
// SweepTerminalSandboxesPage over a fixed-size run table, offset-paged the
// same way real Postgres would: a page starting past the end is empty, and a
// page crossing the end is short. Every call is recorded so a test can assert
// on the exact sequence of pages a run of ticks issued.
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
	return n, n, nil
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
// across several ticks, each of which reads at most one page, and the
// sweeper wraps back to offset 0 once a page comes back short (the end of the
// table) rather than paging forever.
func TestRunTerminalSandboxSweeper_SweepsEveryRunAcrossTicksOnePageAtATime(t *testing.T) {
	fake := &fakeTerminalSandboxPager{totalRuns: 2*terminalSandboxSweepPageSize + 50} // two full pages + a short one
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runTerminalSandboxSweeper(ctx, fake, time.Millisecond)
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
			t.Errorf("call %d offset = %d, want %d (offsets so far: %+v)", i, calls[i].Offset, want, calls)
		}
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
	startTerminalSandboxSweeper(ctx, fake, time.Millisecond)

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
