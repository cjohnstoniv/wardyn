// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
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

	t.Run("concurrent first-touch does not skip the hold", func(t *testing.T) {
		// W20-hold-fsm-4: concurrent first-touch connections to the SAME new
		// host. Only ONE goroutine's Resolve wins the raise race and blocks
		// on the (slow, widened-window) raise() network call; every OTHER
		// goroutine must observe apPending with the RAISER'S host claimed but
		// no id recorded yet — and must retry rather than bail out as if the
		// raise had already failed. Before the fix, those siblings returned
		// near-instantly with ApprovalID==Nil, skipping the hold entirely
		// (wait_for_review silently degraded to deny_with_review for them).
		// This subtest needs its own (larger) poll interval: the retry budget
		// is concurrentRaiseRetries * holdPollInterval, and it must clear the
		// raise's simulated network delay below. Save/restore around the
		// outer test's already-shrunk value.
		savedPoll := holdPollInterval
		holdPollInterval = 15 * time.Millisecond // budget = 5*15ms = 75ms > the 30ms raise delay
		defer func() { holdPollInterval = savedPoll }()

		var raises atomic.Int32
		cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				raises.Add(1)
				time.Sleep(30 * time.Millisecond) // widen the race window
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
				return
			}
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
		}))
		defer cp.Close()

		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
		const n = 8
		ap.configureHold(types.FirstUseWaitForReview, 400*time.Millisecond, n)

		start := make(chan struct{})
		var wg sync.WaitGroup
		results := make([]resolveResult, n)
		elapsed := make([]time.Duration, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				t0 := time.Now()
				results[i] = ap.ResolveWait(context.Background(), "egress.test")
				elapsed[i] = time.Since(t0)
			}(i)
		}
		close(start)
		wg.Wait()

		if got := raises.Load(); got != 1 {
			t.Fatalf("concurrent first-touch raised %d approvals, want exactly 1", got)
		}
		for i := range results {
			if results[i].ApprovalID == uuid.Nil {
				t.Errorf("goroutine %d: ApprovalID = Nil, want the raised approval's real id "+
					"(it must retry, not treat a sibling's in-flight raise as a failure)", i)
			}
			// Every goroutine must actually HOLD roughly the full timeout
			// (state stays Pending throughout) — not return near-instantly.
			if elapsed[i] < 200*time.Millisecond {
				t.Errorf("goroutine %d returned in %v, want it to hold near the 400ms timeout "+
					"(a near-instant return means it skipped the hold)", i, elapsed[i])
			}
			if results[i].State != apPending {
				t.Errorf("goroutine %d: state = %v, want apPending (never decided)", i, results[i].State)
			}
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

// F070: the hold deadline must bound the WHOLE of ResolveWait, not just the
// parked wait at the end of it.
//
// The 30s timer was armed only after the concurrent-raise retry loop, and
// nothing else bounded those calls: the request ctx from handleConnect carries
// no deadline, so the retry loop spent concurrentRaiseRetries+1 full
// control-plane client timeouts before the timer existed — 6x130s = 785s on the
// shipped proxy client, against a first_use_hold_seconds docs/POLICIES.md sells
// as 30s. This pins the operator's budget, not the timer's placement: a black-
// holed control plane must cost one hold, and must still fail CLOSED.
func TestResolveWaitBoundsTheWholeHoldAgainstAHungControlPlane(t *testing.T) {
	saved := holdPollInterval
	holdPollInterval = 20 * time.Millisecond
	defer func() { holdPollInterval = saved }()

	// A control plane that accepts the raise and never answers (partitioned or
	// black-holed), which is the shape that exposes the unbounded round trips.
	done := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(done); cp.Close() })

	const hold = 2 * time.Second
	// A client Timeout stands in for the proxy's own 130s ceiling: the only bound
	// these calls used to have, and one the retry loop multiplies.
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(),
		&http.Client{Transport: cp.Client().Transport, Timeout: time.Second})
	ap.configureHold(types.FirstUseWaitForReview, hold, 4)

	start := time.Now()
	r := ap.ResolveWait(context.Background(), "egress.test")
	elapsed := time.Since(start)

	if r.State != apPending {
		t.Fatalf("state = %v, want apPending — a hung control plane must fail closed", r.State)
	}
	if elapsed > hold+1500*time.Millisecond {
		t.Fatalf("ResolveWait held %s against a hung control plane; the operator's hold budget is %s. "+
			"The raise and the concurrent-raise retries run BEFORE the hold timer is armed, so nothing bounds them.",
			elapsed.Round(time.Millisecond), hold)
	}
}

// F071: one run must not be able to grow the approval cache — and the approvals
// table behind it — without bound from inside the sandbox.
//
// Nothing capped either side: a.hosts had no cap and its entries are never
// removed, and the control plane has no per-run cap, so a loop over generated
// hostnames produced one cache entry and one PENDING row per name (measured:
// 20,000 of each in 3.9s). Past the cap a NEW host must resolve to pending —
// fail closed, no raise — while hosts already in the cache keep their state, so
// a decision a human already made is never dropped and re-raised.
func TestResolveCapsTheHostCacheAndTheRaises(t *testing.T) {
	var raises atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/internal/approvals") {
			raises.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
			return
		}
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
	}))
	defer cp.Close()

	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	// Seed one host so we can prove an EXISTING entry still resolves past the cap.
	seeded := uuid.New()
	ap.mu.Lock()
	ap.hosts["kept.test"] = &hostApproval{state: apApproved, approvalID: seeded, lastPoll: time.Now()}
	ap.mu.Unlock()

	// The pin is BOUNDEDNESS, not the cap's value: flood distinctly more hosts
	// than any cap this file would sanely choose and require the two counts to
	// stop tracking the flood 1:1. Stated this way it compiles against the
	// pre-cap tree too, so the red is an assertion, not a build error.
	const flood = 8192
	for i := range flood {
		ap.Resolve(context.Background(), fmt.Sprintf("h%06d.flood.test", i))
	}

	ap.mu.Lock()
	entries := len(ap.hosts)
	ap.mu.Unlock()
	if entries >= flood {
		t.Errorf("%d distinct hosts produced %d cache entries — the cache grows 1:1 with names the sandbox invents", flood, entries)
	}
	if got := int(raises.Load()); got >= flood {
		t.Errorf("%d distinct hosts raised %d approvals — one run can insert unbounded PENDING rows", flood, got)
	}
	// The cap must not evict, or a human's decision would be silently re-asked.
	if r := ap.Resolve(context.Background(), "kept.test"); r.State != apApproved || r.ApprovalID != seeded {
		t.Errorf("a host decided BEFORE the cap now resolves %+v, want the seeded approval still granted", r)
	}
}
