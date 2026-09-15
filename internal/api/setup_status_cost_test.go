// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /api/v1/setup/status is POLLED (every 5s by the console's Getting-started
// funnel), which makes its per-request cost a standing load, not a one-off. Two
// costs were unbounded in exactly the way a poll cannot afford; these are the
// pins that keep them bounded.

// setupStatusCostStore answers the store surface /setup/status touches and
// records HOW it was asked for runs: the unbounded ListRuns, or the bounded
// Pager page (and with what limit).
type setupStatusCostStore struct {
	fakeSiteConfigStore
	// Embedded nil Pager: only ListRunsPage below is overridden, so any OTHER
	// paged read this handler might grow panics loudly instead of silently
	// answering nil (store.Pager's own doc comment on why it is not part of
	// store.Store).
	store.Pager
	listRunsCalls int
	pages         []store.Page
}

func (s *setupStatusCostStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	s.listRunsCalls++
	return nil, nil
}

func (s *setupStatusCostStore) ListRunsPage(_ context.Context, p store.Page) ([]types.AgentRun, error) {
	s.pages = append(s.pages, p)
	return nil, nil
}

// The permissions-posture row checks err AFTER the call, so an unimplemented
// embedded method would panic before the guard could skip it (same reason
// fakeOnboardingStatusStore carries this).
func (s *setupStatusCostStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return nil, context.Canceled
}

// TestSetupStatus_HasRunsReadsOneRow pins that the has_runs existence check is
// bounded. It used to call ListRuns, whose SQL is `SELECT <every column> FROM
// agent_runs ORDER BY created_at DESC` with no LIMIT — every run the install
// ever launched, fully decoded, on every poll, to compute len(runs) > 0.
func TestSetupStatus_HasRunsReadsOneRow(t *testing.T) {
	st := &setupStatusCostStore{}
	if _, ok := store.Store(st).(store.Pager); !ok {
		t.Fatal("test double must implement store.Pager — the bounded path is the one under test")
	}
	srv := New(baseTestConfig(newHarness(t), st))

	if w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("setup/status: code = %d; body=%s", w.Code, w.Body.String())
	}

	if st.listRunsCalls != 0 {
		t.Errorf("has_runs used the UNBOUNDED ListRuns %d time(s); a Pager store must be asked for one page", st.listRunsCalls)
	}
	if len(st.pages) != 1 {
		t.Fatalf("want exactly one bounded runs page per poll, got %d: %+v", len(st.pages), st.pages)
	}
	if st.pages[0].Limit != 1 {
		t.Errorf("has_runs asked for Page{Limit: %d}; an existence check reads ONE row", st.pages[0].Limit)
	}
}

// TestSetupStatus_HostSweepIsMemoized pins that the host-proxy sweep is not
// re-run per poll. setup.DetectHostProxy's OS tier measured ~450ms/call on a
// WSL host (registry/scutil/gsettings shell-outs) — 90%+ of this handler's
// cost, repeated every 5s for a host setting that changes about never.
func TestSetupStatus_HostSweepIsMemoized(t *testing.T) {
	var calls atomic.Int64
	swept := make(chan struct{}, 4)
	realDetect := hostProxyDetect
	hostProxyDetect = func() setup.HostProxyDetection {
		calls.Add(1)
		swept <- struct{}{}
		return setup.HostProxyDetection{}
	}
	t.Cleanup(func() {
		hostProxyDetect = realDetect
		hostProxyCacheReset()
	})
	hostProxyCacheReset()

	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))
	// The sweep is now taken OFF the request goroutine (see the non-blocking
	// pin below), so the first poll only STARTS it. Wait for it to land before
	// polling again, or the second poll would find the memo still empty and
	// legitimately start a second one — an async memo is still a memo.
	if w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("poll 0: code = %d; body=%s", w.Code, w.Body.String())
	}
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("the first poll never started a host-proxy sweep")
	}
	waitHostProxyMemo(t)

	if w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("poll 1: code = %d; body=%s", w.Code, w.Body.String())
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("host-proxy sweep ran %d times across 2 polls, want 1 (memoized for %s)", n, hostProxyTTL)
	}
}

// waitHostProxyMemo blocks until a started sweep has STORED its answer — the
// moment the memo turns fresh. Reading hostProxyAt under its own mutex is the
// honest wait; sleeping a guessed interval is how this test would flake.
func waitHostProxyMemo(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hostProxyMu.Lock()
		fresh := !hostProxyAt.IsZero()
		hostProxyMu.Unlock()
		if fresh {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("host-proxy sweep never stored its answer")
}

// TestSetupStatus_HostSweepNeverBlocksThePoll is the THIRD cost pin, and the
// one the other two implied: the sweep must not merely be rare, it must never
// run on the request goroutine at all.
//
// It cost the 0.7.3 e2e suite 17 spec files. setup.DetectHostProxy's OS tier
// shells out to WSL interop (powershell.exe, then netsh.exe), each bounded by
// its own 3s probeTimeout — so a host whose interop is wedged turns the FIRST
// GET /setup/status after every daemon boot into a 6-second call, the console's
// first paint waits on it, and Playwright's 5s expect times out before any
// heading renders. The memo above only ever helped the SECOND caller.
//
// The bound is generous on purpose (1s against a 2s sweep): this pins "the
// handler does not wait for the sweep", not a latency budget.
func TestSetupStatus_HostSweepNeverBlocksThePoll(t *testing.T) {
	const sweep = 2 * time.Second
	realDetect := hostProxyDetect
	hostProxyDetect = func() setup.HostProxyDetection {
		time.Sleep(sweep)
		return setup.HostProxyDetection{}
	}
	t.Cleanup(func() {
		hostProxyDetect = realDetect
		hostProxyCacheReset()
	})
	hostProxyCacheReset()

	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))
	start := time.Now()
	if w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("setup/status: code = %d; body=%s", w.Code, w.Body.String())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("GET /setup/status took %s with a %s host sweep in flight — the poll is waiting on a subprocess", elapsed, sweep)
	}
}
