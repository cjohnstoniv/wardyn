// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestApprovalStaleApprovedRevalidatesFailClosed locks down FIX E1: an approved
// cache entry older than approvalTTL must be re-polled, and a now-Expired (or
// Denied) result must flip it to denied so egress stops (fail closed) instead of
// flowing forever on a stale grant.
func TestApprovalStaleApprovedRevalidatesFailClosed(t *testing.T) {
	apID := uuid.New()
	var gets atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/internal/approvals/") {
			gets.Add(1)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: apID, State: types.ApprovalExpired})
			return
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	defer cp.Close()

	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	// Seed a granted entry whose observation is older than the TTL (the grant
	// has since expired/been revoked upstream).
	ap.mu.Lock()
	ap.hosts["egress.test"] = &hostApproval{
		state:      apApproved,
		approvalID: apID,
		lastPoll:   time.Now().Add(-2 * approvalTTL),
	}
	ap.mu.Unlock()

	res := ap.Resolve(context.Background(), "egress.test")
	if res.State != apDenied {
		t.Fatalf("stale approved -> state = %v, want apDenied (fail closed on expiry)", res.State)
	}
	if gets.Load() != 1 {
		t.Fatalf("re-poll count = %d, want 1 (TTL must trigger a re-poll)", gets.Load())
	}
}

// TestApprovalConcurrentFirstUseRaisesOnce locks down ITEM 30: concurrent first
// requests to an unknown host must raise EXACTLY ONE approval. Before the fix
// both goroutines snapshot apNone under the lock, release it, and both call
// raise() -> two duplicate approvals. The slow raise handler widens the window
// so the pre-fix double-raise is reliably observed.
func TestApprovalConcurrentFirstUseRaisesOnce(t *testing.T) {
	var raises atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/internal/approvals") {
			raises.Add(1)
			time.Sleep(25 * time.Millisecond) // widen the race window
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
			return
		}
		// Poll of the pending approval: still pending.
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
	}))
	defer cp.Close()

	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	states := make([]approvalState, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together
			states[i] = ap.Resolve(context.Background(), "egress.test").State
		}(i)
	}
	close(start)
	wg.Wait()

	if got := raises.Load(); got != 1 {
		t.Fatalf("concurrent first-use raised %d approvals, want exactly 1", got)
	}
	for i, s := range states {
		if s != apPending {
			t.Fatalf("goroutine %d observed state %v, want apPending", i, s)
		}
	}
}

// TestApprovalFreshApprovedServedFromCache asserts the fast path: an approval
// observed within approvalTTL is served from cache with no control-plane call.
func TestApprovalFreshApprovedServedFromCache(t *testing.T) {
	var gets atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		http.Error(w, "must not poll a fresh approval", http.StatusTeapot)
	}))
	defer cp.Close()

	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	apID := uuid.New()
	ap.mu.Lock()
	ap.hosts["egress.test"] = &hostApproval{
		state:      apApproved,
		approvalID: apID,
		lastPoll:   time.Now(),
	}
	ap.mu.Unlock()

	res := ap.Resolve(context.Background(), "egress.test")
	if res.State != apApproved {
		t.Fatalf("fresh approved -> state = %v, want apApproved", res.State)
	}
	if res.ApprovalID != apID {
		t.Fatalf("approval id = %v, want %v", res.ApprovalID, apID)
	}
	if gets.Load() != 0 {
		t.Fatalf("fresh approval made %d network calls, want 0 (served from cache)", gets.Load())
	}
}

// TestResolveWaitHold covers wait_for_review: the proxy HOLDS the connection
// until the approval is decided or the hold deadline passes, failing CLOSED on
// timeout/saturation (never to allow) and leaving the approval PENDING for a
// later retry.
func TestResolveWaitHold(t *testing.T) {
	saved := holdPollInterval
	holdPollInterval = 5 * time.Millisecond
	defer func() { holdPollInterval = saved }()

	t.Run("approved -> holds then allows", func(t *testing.T) {
		cp := approvalCPStub(apState(types.ApprovalApproved), nil, nil)
		defer cp.Close()
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
		ap.configureHold(types.FirstUseWaitForReview, 2*time.Second, 4)
		if res := ap.ResolveWait(context.Background(), "egress.test"); res.State != apApproved {
			t.Fatalf("wait_for_review approved -> %v, want apApproved", res.State)
		}
	})

	t.Run("denied -> holds then denies", func(t *testing.T) {
		cp := approvalCPStub(apState(types.ApprovalDenied), nil, nil)
		defer cp.Close()
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
		ap.configureHold(types.FirstUseWaitForReview, 2*time.Second, 4)
		if res := ap.ResolveWait(context.Background(), "egress.test"); res.State != apDenied {
			t.Fatalf("wait_for_review denied -> %v, want apDenied", res.State)
		}
	})

	t.Run("timeout -> fails closed pending, approval left raised", func(t *testing.T) {
		var raises atomic.Int32
		cp := approvalCPStub(apState(types.ApprovalPending), &raises, nil)
		defer cp.Close()
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
		ap.configureHold(types.FirstUseWaitForReview, 40*time.Millisecond, 4)
		start := time.Now()
		res := ap.ResolveWait(context.Background(), "egress.test")
		if res.State != apPending {
			t.Fatalf("wait_for_review timeout -> %v, want apPending (fail closed)", res.State)
		}
		if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
			t.Fatalf("returned before hold deadline (%v)", elapsed)
		}
		if raises.Load() != 1 {
			t.Fatalf("approval raised %d times, want 1 (left pending for retry)", raises.Load())
		}
	})

	t.Run("hold cap saturated -> fails fast pending", func(t *testing.T) {
		cp := approvalCPStub(apState(types.ApprovalPending), nil, nil)
		defer cp.Close()
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
		ap.configureHold(types.FirstUseWaitForReview, 5*time.Second, 1)
		ap.holdSem <- struct{}{} // saturate the single hold slot
		start := time.Now()
		res := ap.ResolveWait(context.Background(), "egress.test")
		if res.State != apPending {
			t.Fatalf("saturated cap -> %v, want apPending", res.State)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("saturated cap should fail fast, took %v", elapsed)
		}
	})
}
