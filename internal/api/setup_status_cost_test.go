// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

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
	calls := 0
	realDetect := hostProxyDetect
	hostProxyDetect = func() setup.HostProxyDetection {
		calls++
		return setup.HostProxyDetection{}
	}
	t.Cleanup(func() {
		hostProxyDetect = realDetect
		hostProxyCacheReset()
	})
	hostProxyCacheReset()

	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))
	for i := 0; i < 2; i++ {
		if w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""); w.Code != http.StatusOK {
			t.Fatalf("poll %d: code = %d; body=%s", i, w.Code, w.Body.String())
		}
	}

	if calls != 1 {
		t.Errorf("host-proxy sweep ran %d times across 2 polls, want 1 (memoized for %s)", calls, hostProxyTTL)
	}
}
