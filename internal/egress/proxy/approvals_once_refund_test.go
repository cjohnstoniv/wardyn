// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// spentOnceClient is an approval client whose once grant id for egress.test a first request has spent.
func spentOnceClient(t *testing.T, id uuid.UUID) *approvalClient {
	t.Helper()
	cp, _ := decidedCP(t, id, types.ApprovalApproved, types.ScopeOnce, nil)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	seed(ap, "egress.test", hostApproval{state: apApproved, approvalID: id, scope: types.ScopeOnce, lastPoll: time.Now()})
	if r := ap.Resolve(context.Background(), "egress.test"); r.State != apApproved || r.ApprovalID != id {
		t.Fatalf("the first request = %+v, want the once grant %s", r, id)
	}
	return ap
}

// cachedEntry is a copy of host's cache entry, read under the lock.
func cachedEntry(ap *approvalClient, host string) hostApproval {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if st := ap.hosts[host]; st != nil {
		return *st
	}
	return hostApproval{}
}

// TestRefundOnce_GivesTheGrantToExactlyOneRacer: a once grant given back is spent again by exactly one of N racing
// requests, as a fresh one is (TestOnceServedToExactlyOneConcurrentResolver).
func TestRefundOnce_GivesTheGrantToExactlyOneRacer(t *testing.T) {
	id := uuid.New()
	ap := spentOnceClient(t, id)
	if !ap.refundOnce("egress.test", id) {
		t.Fatal("refundOnce of the grant just spent = false, want true")
	}
	const racers = 24
	var granted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if r := ap.Resolve(context.Background(), "egress.test"); r.State == apApproved && r.ApprovalID == id {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := granted.Load(); got != 1 {
		t.Fatalf("%d of %d racers got the grant given back, want exactly 1", got, racers)
	}
}

// TestRefundOnce_ARequestSinceKeepsItsOwnApproval: once a request for the host has raised a new approval, the spent
// grant is not given back: the new approval is the one in force.
func TestRefundOnce_ARequestSinceKeepsItsOwnApproval(t *testing.T) {
	id := uuid.New()
	ap := spentOnceClient(t, id)
	r := ap.Resolve(context.Background(), "egress.test")
	if r.State != apPending || r.ApprovalID == uuid.Nil || r.ApprovalID == id {
		t.Fatalf("the request after the spend = %+v, want a new approval raised", r)
	}
	if ap.refundOnce("egress.test", id) {
		t.Fatal("refundOnce over a newer approval = true, want false")
	}
	if st := cachedEntry(ap, "egress.test"); st.state != apPending || st.approvalID != r.ApprovalID {
		t.Fatalf("entry after the refused refund = %+v, want approval %s still pending", st, r.ApprovalID)
	}
}

// TestRefundOnce_OnlyOncePerApproval: an approval comes back once; after its second use it stays spent.
func TestRefundOnce_OnlyOncePerApproval(t *testing.T) {
	id := uuid.New()
	ap := spentOnceClient(t, id)
	if !ap.refundOnce("egress.test", id) {
		t.Fatal("first refundOnce = false, want true")
	}
	if r := ap.Resolve(context.Background(), "egress.test"); r.State != apApproved || r.ApprovalID != id {
		t.Fatalf("the retry = %+v, want the grant given back", r)
	}
	if ap.refundOnce("egress.test", id) {
		t.Fatal("second refundOnce of one approval = true, want false")
	}
}

// TestRefundOnce_ChangesNothingItWasNotGiven: no grant comes back for an id that was not the one spent, for no id,
// for a run grant still in force, for a spent deny, or for a host with no entry.
func TestRefundOnce_ChangesNothingItWasNotGiven(t *testing.T) {
	ctx := context.Background()
	t.Run("another id", func(t *testing.T) {
		ap := spentOnceClient(t, uuid.New())
		if ap.refundOnce("egress.test", uuid.New()) {
			t.Fatal("refundOnce of an id never spent = true")
		}
		if r := ap.Resolve(ctx, "egress.test"); r.State == apApproved {
			t.Fatalf("after a refused refund the next request = %+v, want no grant", r)
		}
	})
	t.Run("no id", func(t *testing.T) {
		ap := spentOnceClient(t, uuid.New())
		if ap.refundOnce("egress.test", uuid.Nil) {
			t.Fatal("refundOnce of uuid.Nil = true")
		}
	})
	t.Run("a run grant", func(t *testing.T) {
		id := uuid.New()
		cp, _ := decidedCP(t, id, types.ApprovalApproved, types.ScopeRun, nil)
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
		seed(ap, "egress.test", hostApproval{state: apApproved, approvalID: id, scope: types.ScopeRun, lastPoll: time.Now()})
		if r := ap.Resolve(ctx, "egress.test"); r.State != apApproved {
			t.Fatalf("run grant = %+v, want approved", r)
		}
		before := cachedEntry(ap, "egress.test")
		if ap.refundOnce("egress.test", id) {
			t.Fatal("refundOnce of a run grant = true")
		}
		if after := cachedEntry(ap, "egress.test"); after != before {
			t.Fatalf("entry changed from %+v to %+v", before, after)
		}
	})
	t.Run("a spent deny", func(t *testing.T) {
		id := uuid.New()
		cp, _ := decidedCP(t, id, types.ApprovalDenied, types.ScopeOnce, nil)
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
		seed(ap, "egress.test", hostApproval{state: apDenied, approvalID: id, scope: types.ScopeOnce, lastPoll: time.Now()})
		if r := ap.Resolve(ctx, "egress.test"); r.State != apDenied {
			t.Fatalf("once deny = %+v, want denied", r)
		}
		if ap.refundOnce("egress.test", id) {
			t.Fatal("refundOnce of a spent deny = true")
		}
	})
	t.Run("no entry", func(t *testing.T) {
		ap := spentOnceClient(t, uuid.New())
		if ap.refundOnce("other.test", uuid.New()) {
			t.Fatal("refundOnce for a host with no entry = true")
		}
	})
}

// TestRefundOnce_TheGrantLapses: a grant given back waits onceRefundTTL at most, past it nothing is served, and the
// not-spent note names the same time.
func TestRefundOnce_TheGrantLapses(t *testing.T) {
	id := uuid.New()
	ap := spentOnceClient(t, id)
	if !ap.refundOnce("egress.test", id) {
		t.Fatal("refundOnce = false, want true")
	}
	if left := time.Until(cachedEntry(ap, "egress.test").expiresAt); left <= 0 || left > onceRefundTTL {
		t.Fatalf("the grant given back lapses in %v, want within (0, %v]", left, onceRefundTTL)
	}
	if want := strconv.Itoa(int(onceRefundTTL/time.Second)) + " seconds"; !strings.Contains(onceRefundNote, want) {
		t.Fatalf("onceRefundNote = %q, want it to say %q", onceRefundNote, want)
	}
	ap.mu.Lock()
	ap.hosts["egress.test"].expiresAt = time.Now().Add(-time.Second)
	ap.mu.Unlock()
	if r := ap.Resolve(context.Background(), "egress.test"); r.State == apApproved {
		t.Fatalf("a grant past its time was served: %+v", r)
	}
}

// TestRefundOnce_AHeldSiblingTakesItOnce: under wait_for_review, requests held on one pending approval share its
// decision through the cache. A once decision goes to one of them. Given back while the others are still held, it
// goes to exactly one more, and the rest end pending. A second refund of the approval is refused.
func TestRefundOnce_AHeldSiblingTakesItOnce(t *testing.T) {
	saved := holdPollInterval
	holdPollInterval = 5 * time.Millisecond
	defer func() { holdPollInterval = saved }()

	id := uuid.New()
	decided := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	var waiting atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost { // a held request's rejoin
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
			return
		}
		select {
		case <-decided:
		default:
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: id, State: types.ApprovalPending})
			return
		}
		// The first poll after the decision is answered at once. Every later one waits for release, so those
		// requests are still held on the approval when the grant is given back.
		if !first.CompareAndSwap(false, true) {
			waiting.Add(1)
			<-release
		}
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: id, State: types.ApprovalApproved, DecisionScope: types.ScopeOnce})
	}))
	t.Cleanup(cp.Close)
	freeHolders := sync.OnceFunc(func() { close(release) })
	t.Cleanup(freeHolders) // runs before cp.Close, so no poll is left waiting
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	ap.configureHold(types.FirstUseWaitForReview, 10*time.Second, 8)
	seed(ap, "egress.test", hostApproval{state: apPending, approvalID: id, lastPoll: time.Now()})

	const holders = 8
	var granted, pending atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < holders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch r := ap.ResolveWait(context.Background(), "egress.test"); {
			case r.State == apApproved && r.ApprovalID == id:
				granted.Add(1)
			case r.State == apPending:
				pending.Add(1)
			}
		}()
	}
	waitFor(5*time.Second, func() bool { return len(ap.holdSem) == holders })
	if n := len(ap.holdSem); n != holders {
		t.Fatalf("%d of %d requests are held, want all of them", n, holders)
	}
	close(decided)
	waitFor(5*time.Second, func() bool { return granted.Load() == 1 && waiting.Load() == holders-1 })
	if g, w := granted.Load(), waiting.Load(); g != 1 || w != holders-1 {
		t.Fatalf("after the decision %d requests got the grant and %d polls wait, want 1 and %d", g, w, holders-1)
	}
	if !ap.refundOnce("egress.test", id) {
		t.Fatal("refundOnce of the grant a held request spent = false, want true")
	}
	freeHolders()
	wg.Wait()
	if g, p := granted.Load(), pending.Load(); g != 2 || p != holders-2 {
		t.Fatalf("%d requests got the grant and %d ended pending, want 2 and %d", g, p, holders-2)
	}
	if ap.refundOnce("egress.test", id) {
		t.Fatal("a second refundOnce of one approval = true, want false")
	}
}
