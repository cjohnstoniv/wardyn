// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A retry sent right after a person decides a first-use approval is answered by a fresh status check, not by
// the cached pending state. These tests hold the floor, the single-flight check and the budget.

const retryHost = "egress.test"

// retryCP is a fake control plane whose answer for the approval it hands out is set by the test. A GET samples
// the state on arrival and, when gate is set, parks until released before answering with that SAMPLE, which is
// how a test pins "the check saw pending, then the decision landed".
type retryCP struct {
	srv *httptest.Server
	id  uuid.UUID // the approval a raise hands out; a GET of any other id is pending
	// freshRaise makes a raise hand out a NEW approval each time, as the real control plane does once the old
	// row is decided; the default hands out id again, which a test that re-raises must not rely on.
	freshRaise atomic.Bool

	mu       sync.Mutex
	state    types.ApprovalState
	scope    types.ApprovalScope
	expires  *time.Time
	gate     chan struct{}
	getTimes []time.Time

	gets    atomic.Int32
	raises  atomic.Int32
	entered chan struct{} // one send per GET that parked on gate
}

func newRetryCP(t *testing.T) *retryCP {
	t.Helper()
	cp := &retryCP{id: uuid.New(), state: types.ApprovalPending, entered: make(chan struct{}, 64)}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			cp.raises.Add(1)
			w.WriteHeader(http.StatusCreated)
			raised := cp.id
			if cp.freshRaise.Load() {
				raised = uuid.New()
			}
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: raised, State: types.ApprovalPending})
			return
		}
		cp.gets.Add(1)
		cp.mu.Lock()
		cp.getTimes = append(cp.getTimes, time.Now())
		state, scope, exp, gate := cp.state, cp.scope, cp.expires, cp.gate
		cp.mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/"+cp.id.String()) {
			state, scope, exp = types.ApprovalPending, "", nil
		}
		if gate != nil {
			cp.entered <- struct{}{}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: cp.id, State: state, DecisionScope: scope, DecisionExpiresAt: exp})
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func (cp *retryCP) decide(state types.ApprovalState, scope types.ApprovalScope, exp *time.Time) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.state, cp.scope, cp.expires = state, scope, exp
}

func (cp *retryCP) hold() chan struct{} {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.gate = make(chan struct{})
	return cp.gate
}

func (cp *retryCP) client() *approvalClient {
	return newApprovalClient(cp.srv.URL, newTokenSource("tok"), uuid.New(), cp.srv.Client())
}

// retryTimings shortens the floor and the budget for one test and restores them after.
func retryTimings(t *testing.T, floor, budget time.Duration) {
	t.Helper()
	oldFloor, oldBudget := pendingPollFloor, approvalCallTimeout
	pendingPollFloor, approvalCallTimeout = floor, budget
	t.Cleanup(func() { pendingPollFloor, approvalCallTimeout = oldFloor, oldBudget })
}

// seedPending installs a pending entry for cp's approval whose last check was ago ago.
func seedPending(ap *approvalClient, id uuid.UUID, ago time.Duration) {
	seed(ap, retryHost, hostApproval{state: apPending, approvalID: id, lastPoll: time.Now().Add(-ago)})
}

func entryOf(ap *approvalClient) hostApproval {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return *ap.hosts[retryHost]
}

// stillWaiting fails the test if the request behind ch has already answered: a joiner of a check in flight
// must wait for it, so an answer before the check is released is the cached pending state in disguise.
func stillWaiting(t *testing.T, name string, ch <-chan resolveResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("request %s answered %+v before the check it joined finished", name, r)
	default:
	}
}

func awaitEntered(t *testing.T, cp *retryCP, n int) {
	t.Helper()
	for range n {
		select {
		case <-cp.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the status check never reached the control plane")
		}
	}
}

func resolveAsync(ap *approvalClient, ctx context.Context) <-chan resolveResult {
	ch := make(chan resolveResult, 1)
	go func() { ch <- ap.Resolve(ctx, retryHost) }()
	return ch
}

// Raise, a person decides, a retry 100 ms later: the request waits out the rest of the floor, checks, and is
// allowed with the releasing approval's id.
func TestApprovalRetryAfterDecisionWaitsOutFloorAndIsAllowed(t *testing.T) {
	retryTimings(t, 300*time.Millisecond, 5*time.Second)
	cp := newRetryCP(t)
	ap := cp.client()

	first := ap.Resolve(context.Background(), retryHost)
	if first.State != apPending || first.ApprovalID != cp.id {
		t.Fatalf("raise -> %+v, want pending on %s", first, cp.id)
	}
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	res := ap.Resolve(context.Background(), retryHost)
	elapsed := time.Since(start)
	if res.State != apApproved || res.ApprovalID != cp.id {
		t.Fatalf("retry -> %+v, want apApproved naming %s", res, cp.id)
	}
	if elapsed < 100*time.Millisecond || elapsed > pendingPollFloor {
		t.Fatalf("retry took %v, want the rest of the %v floor (about 200ms)", elapsed, pendingPollFloor)
	}
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("control plane saw %d checks, want 1", got)
	}
	// The grant is now cached: the next request makes no call.
	if res := ap.Resolve(context.Background(), retryHost); res.State != apApproved || cp.gets.Load() != 1 {
		t.Fatalf("follow-up -> %+v after %d checks, want a cached allow", res, cp.gets.Load())
	}
}

// The same retry through the proxy: allowed, and the decision names the approval.
func TestApprovalRetryAfterDecisionThroughProxy(t *testing.T) {
	retryTimings(t, 300*time.Millisecond, 5*time.Second)
	cp := newRetryCP(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached")
	}))
	defer upstream.Close()
	p, buf := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains:   []string{"known.test"},
		FirstUseApproval: types.FirstUseDenyWithReview,
	}, upstreamAddr(upstream), cp.client(), nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://retry-host.test/"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("first request status = %d, want 403", rec.Code)
	}
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	time.Sleep(100 * time.Millisecond)

	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://retry-host.test/again"))
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if d := lastDecision(t, buf); d.Decision != egress.Allow || d.RuleSource != "approval:"+cp.id.String() {
		t.Fatalf("retry decision = %q from %q, want allow from approval:%s", d.Decision, d.RuleSource, cp.id)
	}
}

// A retry that arrives while a check is in flight joins it and makes no second call.
func TestApprovalRetryJoinsTheCheckInFlight(t *testing.T) {
	retryTimings(t, 100*time.Millisecond, 5*time.Second)
	cp := newRetryCP(t)
	ap := cp.client()
	seedPending(ap, cp.id, time.Second)
	release := cp.hold()

	a := resolveAsync(ap, context.Background())
	awaitEntered(t, cp, 1)
	b := resolveAsync(ap, context.Background())
	c := resolveAsync(ap, context.Background())
	time.Sleep(50 * time.Millisecond) // b and c reach the join well inside this, and neither waits out the floor
	stillWaiting(t, "a", a)
	stillWaiting(t, "b", b)
	stillWaiting(t, "c", c)
	close(release)

	for name, ch := range map[string]<-chan resolveResult{"a": a, "b": b, "c": c} {
		if res := <-ch; res.State != apPending || res.ApprovalID != cp.id {
			t.Errorf("request %s -> %+v, want pending on %s", name, res, cp.id)
		}
	}
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("control plane saw %d checks, want 1 (joiners make no call)", got)
	}
	if e := entryOf(ap); e.pending != nil || e.state != apPending {
		t.Fatalf("entry after the check = state %v, pending check %v; want pending with none in flight", e.state, e.pending)
	}
}

// t0: a check starts and samples pending. t1: a person decides. t2: a request joins the t0 check and is
// answered pending. The next retry starts a fresh check and is allowed.
func TestApprovalJoinerOfAPendingSampleIsRefusedThenNextRetryIsAllowed(t *testing.T) {
	retryTimings(t, 100*time.Millisecond, 5*time.Second)
	cp := newRetryCP(t)
	ap := cp.client()
	seedPending(ap, cp.id, time.Second)
	release := cp.hold()

	t0 := resolveAsync(ap, context.Background())
	awaitEntered(t, cp, 1)                                 // the check has sampled pending
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil) // t1
	t2 := resolveAsync(ap, context.Background())
	time.Sleep(50 * time.Millisecond)
	stillWaiting(t, "t0", t0)
	stillWaiting(t, "t2", t2)
	close(release)

	for name, ch := range map[string]<-chan resolveResult{"t0": t0, "t2": t2} {
		if res := <-ch; res.State != apPending {
			t.Fatalf("request %s -> %+v, want pending (the check sampled pending)", name, res)
		}
	}
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("control plane saw %d checks before the retry, want 1", got)
	}

	start := time.Now()
	res := ap.Resolve(context.Background(), retryHost) // the next retry
	if res.State != apApproved || res.ApprovalID != cp.id {
		t.Fatalf("next retry -> %+v, want apApproved naming %s", res, cp.id)
	}
	if d := time.Since(start); d > pendingPollFloor+time.Second {
		t.Fatalf("next retry waited %v, want at most the %v floor", d, pendingPollFloor)
	}
	if got := cp.gets.Load(); got != 2 {
		t.Fatalf("control plane saw %d checks, want 2 (a fresh check for the retry)", got)
	}
}

// A joiner of a check that sampled APPROVED is answered approved, by the check's answer and not a second call.
func TestApprovalJoinerOfAnApprovedSampleIsAllowed(t *testing.T) {
	retryTimings(t, 100*time.Millisecond, 5*time.Second)
	cp := newRetryCP(t)
	ap := cp.client()
	seedPending(ap, cp.id, time.Second)
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	release := cp.hold()

	a := resolveAsync(ap, context.Background())
	awaitEntered(t, cp, 1)
	b := resolveAsync(ap, context.Background())
	c := resolveAsync(ap, context.Background())
	time.Sleep(50 * time.Millisecond)
	stillWaiting(t, "a", a)
	stillWaiting(t, "b", b)
	stillWaiting(t, "c", c)
	close(release)

	for name, ch := range map[string]<-chan resolveResult{"a": a, "b": b, "c": c} {
		if res := <-ch; res.State != apApproved || res.ApprovalID != cp.id {
			t.Errorf("request %s -> %+v, want apApproved naming %s", name, res, cp.id)
		}
	}
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("control plane saw %d checks, want 1", got)
	}
}

// The request that started a check is cancelled while it runs: the check goes on, and a request that joined it
// still gets its answer.
func TestApprovalStarterCancelDoesNotPoisonAJoiner(t *testing.T) {
	retryTimings(t, 100*time.Millisecond, 5*time.Second)
	cp := newRetryCP(t)
	ap := cp.client()
	seedPending(ap, cp.id, time.Second)
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	release := cp.hold()

	ctx, cancel := context.WithCancel(context.Background())
	starter := resolveAsync(ap, ctx)
	awaitEntered(t, cp, 1)
	joiner := resolveAsync(ap, context.Background())
	time.Sleep(50 * time.Millisecond)
	cancel()
	if res := <-starter; res.State != apPending {
		t.Fatalf("starter -> %+v, want pending (it was cancelled)", res)
	}
	stillWaiting(t, "joiner", joiner)
	close(release)
	if res := <-joiner; res.State != apApproved || res.ApprovalID != cp.id {
		t.Fatalf("joiner -> %+v, want apApproved: the starter's cancel must not end the check", res)
	}
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("control plane saw %d checks, want 1", got)
	}
}

// Cancelling the request, or running out of its one budget, during the floor wait or the check answers
// pending and changes nothing.
func TestApprovalRetryWaitEndsPendingWithNoStateChange(t *testing.T) {
	check := func(t *testing.T, ap *approvalClient, before hostApproval) {
		t.Helper()
		after := entryOf(ap)
		if after.state != before.state || after.approvalID != before.approvalID || !after.lastPoll.Equal(before.lastPoll) {
			t.Fatalf("entry changed: before %+v, after %+v", before, after)
		}
	}

	t.Run("cancelled during the floor wait", func(t *testing.T) {
		retryTimings(t, 5*time.Second, 10*time.Second)
		cp := newRetryCP(t)
		ap := cp.client()
		seedPending(ap, cp.id, 0)
		before := entryOf(ap)
		ctx, cancel := context.WithCancel(context.Background())
		ch := resolveAsync(ap, ctx)
		time.Sleep(30 * time.Millisecond)
		cancel()
		select {
		case res := <-ch:
			if res.State != apPending || res.ApprovalID != cp.id {
				t.Fatalf("cancelled -> %+v, want pending on %s", res, cp.id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a cancelled request kept waiting")
		}
		if cp.gets.Load() != 0 {
			t.Fatalf("control plane saw %d checks, want 0", cp.gets.Load())
		}
		check(t, ap, before)
	})
	t.Run("budget ends during the floor wait", func(t *testing.T) {
		retryTimings(t, 5*time.Second, 60*time.Millisecond)
		cp := newRetryCP(t)
		ap := cp.client()
		seedPending(ap, cp.id, 0)
		before := entryOf(ap)
		start := time.Now()
		res := ap.Resolve(context.Background(), retryHost)
		if res.State != apPending || res.ApprovalID != cp.id || time.Since(start) > 2*time.Second {
			t.Fatalf("budget end -> %+v after %v, want pending at about %v", res, time.Since(start), approvalCallTimeout)
		}
		if cp.gets.Load() != 0 {
			t.Fatalf("control plane saw %d checks, want 0", cp.gets.Load())
		}
		check(t, ap, before)
	})
	t.Run("budget ends during the check", func(t *testing.T) {
		// The check's own timeout (approvalCallTimeout, 10 s) is far longer than the request's budget (80 ms,
		// set by its context deadline), so only the request's budget can end the wait.
		retryTimings(t, 100*time.Millisecond, 10*time.Second)
		cp := newRetryCP(t)
		ap := cp.client()
		seedPending(ap, cp.id, time.Second)
		cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
		release := cp.hold()
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		start := time.Now()
		res := ap.Resolve(ctx, retryHost)
		if res.State != apPending || res.ApprovalID != cp.id || time.Since(start) > 2*time.Second {
			t.Fatalf("budget end -> %+v after %v, want pending at about 80ms", res, time.Since(start))
		}
		if e := entryOf(ap); e.state != apPending || e.approvalID != cp.id {
			t.Fatalf("entry after the budget ended = %+v, want the same pending approval", e)
		}
	})
	t.Run("cancelled during the check", func(t *testing.T) {
		retryTimings(t, 100*time.Millisecond, 10*time.Second)
		cp := newRetryCP(t)
		ap := cp.client()
		seedPending(ap, cp.id, time.Second)
		release := cp.hold()
		defer close(release)
		ctx, cancel := context.WithCancel(context.Background())
		ch := resolveAsync(ap, ctx)
		awaitEntered(t, cp, 1)
		cancel()
		select {
		case res := <-ch:
			if res.State != apPending || res.ApprovalID != cp.id {
				t.Fatalf("cancelled -> %+v, want pending on %s", res, cp.id)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a cancelled request kept waiting on the check")
		}
		if e := entryOf(ap); e.state != apPending || e.approvalID != cp.id {
			t.Fatalf("entry after the cancel = %+v, want the same pending approval", e)
		}
	})
}

// Each decided shape a pending host's check can find.
func TestApprovalRetryDecidedShapes(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	cases := []struct {
		name      string
		state     types.ApprovalState
		scope     types.ApprovalScope
		expires   *time.Time
		want      approvalState
		wantEntry approvalState // the entry after the request
		cached    bool          // a second request is answered with no further check
	}{
		{"denied", types.ApprovalDenied, types.ScopeRun, nil, apDenied, apDenied, true},
		{"expired", types.ApprovalExpired, "", nil, apExpired, apNone, false},
		{"revoked (cancelled)", types.ApprovalCancelled, "", nil, apExpired, apNone, false},
		{"approved until a time already past", types.ApprovalApproved, types.ScopeUntil, &past, apPending, apPending, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			retryTimings(t, 50*time.Millisecond, 5*time.Second)
			cp := newRetryCP(t)
			cp.freshRaise.Store(true)
			ap := cp.client()
			seedPending(ap, cp.id, time.Second)
			cp.decide(tc.state, tc.scope, tc.expires)

			res := ap.Resolve(context.Background(), retryHost)
			if res.State != tc.want {
				t.Fatalf("state = %v, want %v", res.State, tc.want)
			}
			if got := entryOf(ap).state; got != tc.wantEntry {
				t.Fatalf("entry state = %v, want %v", got, tc.wantEntry)
			}
			if tc.cached {
				gets := cp.gets.Load()
				if again := ap.Resolve(context.Background(), retryHost); again.State != tc.want || cp.gets.Load() != gets {
					t.Fatalf("second request -> %+v after %d more checks, want the cached %v", again, cp.gets.Load()-gets, tc.want)
				}
			}
		})
	}

	// A grant that was served and is re-validated after approvalTTL is revoked: the unchanged re-check path.
	t.Run("approved then revoked at re-validation", func(t *testing.T) {
		retryTimings(t, 50*time.Millisecond, 5*time.Second)
		cp := newRetryCP(t)
		ap := cp.client()
		seed(ap, retryHost, hostApproval{state: apApproved, approvalID: cp.id, lastPoll: time.Now().Add(-2 * approvalTTL), verifiedAt: time.Now()})
		cp.decide(types.ApprovalCancelled, "", nil)
		if res := ap.Resolve(context.Background(), retryHost); res.State != apExpired {
			t.Fatalf("revoked grant -> %+v, want apExpired", res)
		}
	})
}

// A check whose approval was replaced while it ran is dropped: it never writes onto the new approval.
func TestApprovalStaleCheckResultAfterReplacementIsDropped(t *testing.T) {
	retryTimings(t, 100*time.Millisecond, 5*time.Second)
	cp := newRetryCP(t)
	ap := cp.client()
	seedPending(ap, cp.id, time.Second)
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	release := cp.hold()

	ch := resolveAsync(ap, context.Background())
	awaitEntered(t, cp, 1)
	newID := uuid.New()
	seed(ap, retryHost, hostApproval{state: apPending, approvalID: newID, lastPoll: time.Now()})
	close(release)

	res := <-ch
	if res.State != apPending || res.ApprovalID != newID {
		t.Fatalf("stale result -> %+v, want pending naming the replacement %s", res, newID)
	}
	if e := entryOf(ap); e.state != apPending || e.approvalID != newID {
		t.Fatalf("entry = state %v id %v, want the replacement still pending (the stale approve must not land)", e.state, e.approvalID)
	}
}

// A once grant lets exactly one of N concurrent post-decision requests through; the rest see it spent and
// fall through to the re-raise, answered pending on a new approval.
func TestApprovalOnceGrantReleasesExactlyOneConcurrentRetry(t *testing.T) {
	retryTimings(t, 100*time.Millisecond, 5*time.Second)
	cp := newRetryCP(t)
	ap := cp.client()
	cp.freshRaise.Store(true)
	seedPending(ap, cp.id, time.Second)
	cp.decide(types.ApprovalApproved, types.ScopeOnce, nil)
	release := cp.hold()

	const n = 16
	results := make([]<-chan resolveResult, n)
	for i := range results {
		results[i] = resolveAsync(ap, context.Background())
	}
	awaitEntered(t, cp, 1)
	time.Sleep(50 * time.Millisecond)
	close(release)

	allowed := 0
	for i, ch := range results {
		switch res := <-ch; {
		case res.State == apApproved:
			allowed++
		case res.State != apPending:
			t.Errorf("request %d -> %+v, want the allow or pending", i, res)
		}
	}
	if allowed != 1 {
		t.Fatalf("%d of %d requests were let through, want exactly 1", allowed, n)
	}
	if e := entryOf(ap); e.state == apApproved {
		t.Fatalf("a once grant is still approved in the entry after it was spent")
	}
}

// N concurrent requests on a pending host cost at most one control-plane call per floor window.
func TestApprovalConcurrentPendingRetriesCallOncePerFloorWindow(t *testing.T) {
	retryTimings(t, 200*time.Millisecond, 5*time.Second)
	cp := newRetryCP(t)
	ap := cp.client()
	seedPending(ap, cp.id, time.Second)

	const n, rounds = 20, 2
	start := time.Now()
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				if res := ap.Resolve(context.Background(), retryHost); res.State != apPending {
					t.Errorf("state = %v, want pending", res.State)
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	cp.mu.Lock()
	times := append([]time.Time(nil), cp.getTimes...)
	cp.mu.Unlock()
	if len(times) == 0 {
		t.Fatal("no check reached the control plane")
	}
	if max := int(elapsed/pendingPollFloor) + 1; len(times) > max {
		t.Fatalf("%d checks in %v, want at most %d (one per %v window)", len(times), elapsed, max, pendingPollFloor)
	}
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < pendingPollFloor-50*time.Millisecond {
			t.Fatalf("checks %d and %d came %v apart, want at least the %v floor", i-1, i, gap, pendingPollFloor)
		}
	}
}

// A request that waited on a check, and then meets a spent once grant and re-raises, still ends within its one
// budget: the re-raise is bounded by what is left of it, not by a fresh approvalCallTimeout.
func TestApprovalRetryNeverTakesMoreThanOneBudget(t *testing.T) {
	retryTimings(t, 50*time.Millisecond, 400*time.Millisecond)
	id := uuid.New()
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done() // a stalled raise
			return
		}
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: id, State: types.ApprovalApproved, DecisionScope: types.ScopeOnce})
	}))
	t.Cleanup(srv.Close)
	ap := newApprovalClient(srv.URL, newTokenSource("tok"), uuid.New(), srv.Client())
	seedPending(ap, id, time.Second)

	start := time.Now()
	type out struct {
		res     resolveResult
		elapsed time.Duration
	}
	ch := make(chan out, 2)
	for range 2 {
		go func() {
			res := ap.Resolve(context.Background(), retryHost)
			ch <- out{res, time.Since(start)}
		}()
	}
	allowed := 0
	for range 2 {
		o := <-ch
		if o.res.State == apApproved {
			allowed++
		}
		if o.elapsed > approvalCallTimeout+150*time.Millisecond {
			t.Errorf("a request took %v, beyond its one %v budget", o.elapsed, approvalCallTimeout)
		}
	}
	if allowed != 1 {
		t.Fatalf("%d requests were let through, want exactly 1 (the once grant)", allowed)
	}
	if posts.Load() == 0 {
		t.Fatal("no request re-raised, so the bound on the re-raise was never exercised")
	}
}

// A held (wait_for_review) request's own first resolve checks a pending host inline at most once per
// holdInlinePollInterval (2 s): ResolveWait's throttle, not the floor deny_with_review waits out.
func TestApprovalHoldInlineCheckKeepsItsTwoSecondThrottle(t *testing.T) {
	saved := holdPollInterval
	holdPollInterval = time.Hour // the hold's own ticker must not poll in this test
	t.Cleanup(func() { holdPollInterval = saved })

	t.Run("one second since the last check: not checked", func(t *testing.T) {
		cp := newRetryCP(t)
		ap := cp.client()
		ap.configureHold(types.FirstUseWaitForReview, 100*time.Millisecond, 2)
		seedPending(ap, cp.id, time.Second)
		cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
		if res := ap.ResolveWait(context.Background(), retryHost); res.State != apPending {
			t.Fatalf("held -> %+v, want pending: a check only 1 s after the last is inside the 2 s throttle", res)
		}
		if got := cp.gets.Load(); got != 0 {
			t.Fatalf("control plane saw %d checks, want 0", got)
		}
	})
	t.Run("three seconds since the last check: checked", func(t *testing.T) {
		cp := newRetryCP(t)
		ap := cp.client()
		ap.configureHold(types.FirstUseWaitForReview, 100*time.Millisecond, 2)
		seedPending(ap, cp.id, 3*time.Second)
		cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
		if res := ap.ResolveWait(context.Background(), retryHost); res.State != apApproved {
			t.Fatalf("held -> %+v, want apApproved from the inline check", res)
		}
		if got := cp.gets.Load(); got != 1 {
			t.Fatalf("control plane saw %d checks, want 1", got)
		}
	})
}
