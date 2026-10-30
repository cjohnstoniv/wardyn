// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A control plane that accepts the connection and never answers delays a request's raise or re-check
// by approvalCallTimeout, not by the client's 130 s forward ceiling.
func TestApprovalResolveBoundsAStalledControlPlane(t *testing.T) {
	old := approvalCallTimeout
	approvalCallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { approvalCallTimeout = old })

	release := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(cp.Close)
	t.Cleanup(func() { close(release) })

	t.Run("raise", func(t *testing.T) {
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), &http.Client{Timeout: 5 * time.Second})
		start := time.Now()
		res := ap.Resolve(context.Background(), "new.example.com")
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("raise held the request for %v, want about %v", d, approvalCallTimeout)
		}
		if res.State != apPending {
			t.Fatalf("state = %v, want apPending (fail closed on a raise that did not answer)", res.State)
		}
	})
	t.Run("re-check keeps the cached grant", func(t *testing.T) {
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), &http.Client{Timeout: 5 * time.Second})
		id := uuid.New()
		ap.mu.Lock()
		ap.hosts["egress.test"] = &hostApproval{state: apApproved, approvalID: id, lastPoll: time.Now().Add(-2 * approvalTTL)}
		ap.mu.Unlock()
		start := time.Now()
		res := ap.Resolve(context.Background(), "egress.test")
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("re-check held the request for %v, want about %v", d, approvalCallTimeout)
		}
		if res.State != apApproved {
			t.Fatalf("state = %v, want apApproved (a transient poll error leaves the state unchanged)", res.State)
		}
	})
}

// A timed-out re-check serves the cached grant but leaves lastPoll alone, so each next request asks
// again, and the first answer, a revoke, takes effect at once.
func TestApprovalRecheckTimeoutsDoNotExtendTheTTL(t *testing.T) {
	old := approvalCallTimeout
	approvalCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { approvalCallTimeout = old })

	id := uuid.New()
	var calls atomic.Int32
	release := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 3 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: id, State: types.ApprovalDenied})
	}))
	t.Cleanup(cp.Close)
	t.Cleanup(func() { close(release) })
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), &http.Client{Timeout: 5 * time.Second})
	ap.mu.Lock()
	ap.hosts["egress.test"] = &hostApproval{state: apApproved, approvalID: id, lastPoll: time.Now().Add(-2 * approvalTTL), verifiedAt: time.Now().Add(-approvalTTL)}
	ap.mu.Unlock()

	for i := range 3 {
		if res := ap.Resolve(context.Background(), "egress.test"); res.State != apApproved {
			t.Fatalf("timed-out re-check %d = %v, want the cached apApproved", i+1, res.State)
		}
	}
	if res := ap.Resolve(context.Background(), "egress.test"); res.State != apDenied {
		t.Fatalf("first re-check that answered = %v, want apDenied", res.State)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("control plane saw %d re-checks, want 4 (one per request, none skipped by a bumped lastPoll)", got)
	}
}

// An approved host with no successful re-check for maxApprovalStaleness is dropped and refused as
// approval:stale; the next request raises a fresh approval instead of being waved through.
func TestApprovalRecheckStalenessFailsClosed(t *testing.T) {
	old := approvalCallTimeout
	approvalCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { approvalCallTimeout = old })

	release := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(cp.Close)
	t.Cleanup(func() { close(release) })
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), &http.Client{Timeout: 5 * time.Second})
	ap.mu.Lock()
	ap.hosts["egress.test"] = &hostApproval{state: apApproved, approvalID: uuid.New(),
		lastPoll: time.Now().Add(-2 * approvalTTL), verifiedAt: time.Now().Add(-maxApprovalStaleness - time.Second)}
	ap.mu.Unlock()

	res := ap.Resolve(context.Background(), "egress.test")
	if res.State != apStale {
		t.Fatalf("re-check with no answer past the bound = %v, want apStale", res.State)
	}
	if got := approvalPendingReason(res.State); got != ruleSourceApprovalStale {
		t.Fatalf("decision reason = %q, want %q", got, ruleSourceApprovalStale)
	}
	if res := ap.Resolve(context.Background(), "egress.test"); res.State != apPending || res.ApprovalID == uuid.Nil {
		t.Fatalf("next request = %+v, want a fresh pending approval raised", res)
	}
}
