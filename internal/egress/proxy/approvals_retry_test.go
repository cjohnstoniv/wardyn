// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
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
// the cached pending state. These tests hold the floor, the single-flight check, the budget and the once grant.
//
// Time is a fakeClock the test advances itself, and every interleaving is pinned by a handshake: the gated
// fake control plane says when a check's GET has arrived, and the clock says which timers are armed. Nothing
// here sleeps or measures elapsed real time. The only real-time waits are the failure guards in await and get,
// which a passing test never reaches.

const retryHost = "egress.test"

// guard bounds how long a handshake waits before the test fails instead of hanging.
const guard = 10 * time.Second

// fakeClock is an approvalClock the test drives. A timer fires, in the order armed, when Advance reaches it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	seq     int
	timers  map[*fakeTimer]struct{}
	changed chan struct{} // one pending signal that a timer was armed or stopped
}

type fakeTimer struct {
	seq int
	at  time.Time
	d   time.Duration
	f   func()
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Now(), timers: map[*fakeTimer]struct{}{}, changed: make(chan struct{}, 1)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) signal() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	c.seq++
	t := &fakeTimer{seq: c.seq, at: c.now.Add(d), d: d, f: f}
	c.timers[t] = struct{}{}
	c.mu.Unlock()
	c.signal()
	return func() bool {
		c.mu.Lock()
		_, armed := c.timers[t]
		delete(c.timers, t)
		c.mu.Unlock()
		c.signal()
		return armed
	}
}

// Advance moves the clock forward by d, firing each timer it passes, earliest first, on this goroutine.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var next *fakeTimer
		for t := range c.timers {
			if !t.at.After(target) && (next == nil || t.at.Before(next.at) || (t.at.Equal(next.at) && t.seq < next.seq)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		delete(c.timers, next)
		if next.at.After(c.now) {
			c.now = next.at
		}
		c.mu.Unlock()
		next.f()
		c.mu.Lock()
	}
	c.now = target
	c.mu.Unlock()
	c.signal()
}

func (c *fakeClock) armed(d time.Duration) (n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for t := range c.timers {
		if d < 0 || t.d == d {
			n++
		}
	}
	return n
}

// await blocks until cond holds, waking on each timer change; it fails the test after guard.
func (c *fakeClock) await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(guard)
	for !cond() {
		select {
		case <-c.changed:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// awaitTimers waits until at least n timers of length d are armed: n requests parked on a wait of d.
func (c *fakeClock) awaitTimers(t *testing.T, d time.Duration, n int) {
	t.Helper()
	c.await(t, "timers of "+d.String(), func() bool { return c.armed(d) >= n })
}

// awaitArmed waits until at least n timers of any length are armed.
func (c *fakeClock) awaitArmed(t *testing.T, n int) {
	t.Helper()
	c.await(t, "armed timers", func() bool { return c.armed(-1) >= n })
}

// retryCP is a fake control plane whose answer for the approval it hands out is set by the test. A GET samples
// the state on arrival and, when gate is set, parks until released before answering with that SAMPLE, which is
// how a test pins "the check saw pending, then the decision landed".
type retryCP struct {
	srv *httptest.Server
	id  uuid.UUID // the approval a raise hands out; a GET of any other id is pending
	// freshRaise makes a raise hand out a NEW approval each time, as the real control plane does once the old
	// row is decided; the default hands out id again, which a test that re-raises must not rely on.
	freshRaise atomic.Bool
	// stallPost makes a raise park until the request's context ends.
	stallPost atomic.Bool

	mu      sync.Mutex
	state   types.ApprovalState
	scope   types.ApprovalScope
	expires *time.Time
	gate    chan struct{}

	gets    atomic.Int32
	raises  atomic.Int32
	entered chan struct{} // one send per GET that parked on gate
	stop    chan struct{} // closed at cleanup, so a stalled raise never outlives its test
}

func newRetryCP(t *testing.T) *retryCP {
	t.Helper()
	cp := &retryCP{id: uuid.New(), state: types.ApprovalPending, entered: make(chan struct{}, 64), stop: make(chan struct{})}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.Copy(io.Discard, r.Body)
			cp.raises.Add(1)
			if cp.stallPost.Load() {
				select {
				case <-r.Context().Done():
				case <-cp.stop:
				}
				return
			}
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
	t.Cleanup(func() { close(cp.stop) })
	return cp
}

func (cp *retryCP) decide(state types.ApprovalState, scope types.ApprovalScope, exp *time.Time) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.state, cp.scope, cp.expires = state, scope, exp
}

// hold makes every GET park until the returned release runs. It also runs at cleanup, so a test that failed
// early never leaves a GET parked and the control plane's Close waiting on it.
func (cp *retryCP) hold(t *testing.T) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	cp.mu.Lock()
	cp.gate = gate
	cp.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

// client is an approvals client on cp whose timing runs on the returned fake clock.
func (cp *retryCP) client() (*approvalClient, *fakeClock) {
	ap := newApprovalClient(cp.srv.URL, newTokenSource("tok"), uuid.New(), cp.srv.Client())
	fc := newFakeClock()
	ap.clock = fc
	return ap, fc
}

// setFloor changes the floor for one test and restores it after.
func setFloor(t *testing.T, floor time.Duration) {
	t.Helper()
	old := pendingPollFloor
	pendingPollFloor = floor
	t.Cleanup(func() { pendingPollFloor = old })
}

// seedPending installs a pending entry for cp's approval whose last check was ago ago on ap's clock.
func seedPending(ap *approvalClient, id uuid.UUID, ago time.Duration) {
	seed(ap, retryHost, hostApproval{state: apPending, approvalID: id, lastPoll: ap.clock.Now().Add(-ago)})
}

func entryOf(ap *approvalClient) hostApproval {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return *ap.hosts[retryHost]
}

// registered returns once every request started so far has finished its locked claim of the host: a request that
// armed its budget did so under a.mu, and joined or started a check before releasing it.
func registered(ap *approvalClient) {
	ap.mu.Lock()
	//lint:ignore SA2001 the empty critical section is the barrier
	ap.mu.Unlock()
}

func awaitEntered(t *testing.T, cp *retryCP, n int) {
	t.Helper()
	for range n {
		select {
		case <-cp.entered:
		case <-time.After(guard):
			t.Fatal("the status check never reached the control plane")
		}
	}
}

func resolveAsync(ap *approvalClient, ctx context.Context) <-chan resolveResult {
	ch := make(chan resolveResult, 1)
	go func() { ch <- ap.Resolve(ctx, retryHost) }()
	return ch
}

// get takes a request's answer, failing the test rather than hanging if it never comes.
func get(t *testing.T, name string, ch <-chan resolveResult) resolveResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(guard):
		t.Fatalf("request %s never answered", name)
		return resolveResult{}
	}
}

// drainAnswers collects every request's answer. A request that lost a once grant re-raises, and one that then
// finds the new approval pending parks on the floor. The clock moves on only when every request still missing
// is parked there, so a request that is merely slow to run is never mistaken for a parked one. Which request
// wins the grant is not fixed, so no step depends on it.
func drainAnswers(t *testing.T, fc *fakeClock, chs []<-chan resolveResult) []resolveResult {
	t.Helper()
	out := make([]resolveResult, len(chs))
	done := make([]bool, len(chs))
	left := len(chs)
	deadline := time.After(guard)
	for left > 0 {
		for i, ch := range chs {
			if done[i] {
				continue
			}
			select {
			case out[i] = <-ch:
				done[i] = true
				left--
			default:
			}
		}
		select {
		case <-deadline:
			t.Fatalf("%d of %d requests never answered", left, len(chs))
		default:
		}
		if left > 0 && fc.armed(pendingPollFloor) >= left {
			fc.Advance(pendingPollFloor)
		}
		runtime.Gosched()
	}
	return out
}

// Raise, a person decides, a retry 100 ms later: the request waits out the rest of the floor (150 ms of the
// 250 ms), checks, and is allowed with the releasing approval's id.
func TestApprovalRetryAfterDecisionWaitsOutFloorAndIsAllowed(t *testing.T) {
	cp := newRetryCP(t)
	ap, fc := cp.client()

	first := ap.Resolve(context.Background(), retryHost)
	if first.State != apPending || first.ApprovalID != cp.id {
		t.Fatalf("raise -> %+v, want pending on %s", first, cp.id)
	}
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	fc.Advance(100 * time.Millisecond)

	ch := resolveAsync(ap, context.Background())
	fc.awaitTimers(t, pendingPollFloor-100*time.Millisecond, 1) // parked on the rest of the floor
	if got := cp.gets.Load(); got != 0 {
		t.Fatalf("control plane saw %d checks inside the floor, want 0", got)
	}
	fc.Advance(pendingPollFloor - 100*time.Millisecond - time.Nanosecond)
	if got := cp.gets.Load(); got != 0 {
		t.Fatalf("control plane saw %d checks a tick before the floor ended, want 0", got)
	}
	fc.Advance(time.Nanosecond)

	res := get(t, "retry", ch)
	if res.State != apApproved || res.ApprovalID != cp.id {
		t.Fatalf("retry -> %+v, want apApproved naming %s", res, cp.id)
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
	cp := newRetryCP(t)
	ap, fc := cp.client()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached")
	}))
	defer upstream.Close()
	p, buf := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains:   []string{"known.test"},
		FirstUseApproval: types.FirstUseDenyWithReview,
	}, upstreamAddr(upstream), ap, nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://retry-host.test/"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("first request status = %d, want 403", rec.Code)
	}
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	fc.Advance(100 * time.Millisecond)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://retry-host.test/again"))
		done <- rec
	}()
	fc.awaitTimers(t, pendingPollFloor-100*time.Millisecond, 1)
	fc.Advance(pendingPollFloor - 100*time.Millisecond)
	select {
	case rec = <-done:
	case <-time.After(guard):
		t.Fatal("the retry never answered")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if d := lastDecision(t, buf); d.Decision != egress.Allow || d.RuleSource != "approval:"+cp.id.String() {
		t.Fatalf("retry decision = %q from %q, want allow from approval:%s", d.Decision, d.RuleSource, cp.id)
	}
}

// joinersOf starts a check on a pending host that has passed its floor, parks it at the control plane, then
// starts n more requests and returns once every one of them has joined it. The n+1 budgets and the check's own
// timeout are the 2+n armed timers that say so.
func joinersOf(t *testing.T, ap *approvalClient, fc *fakeClock, cp *retryCP, n int) (starter <-chan resolveResult, joiners []<-chan resolveResult) {
	t.Helper()
	starter = resolveAsync(ap, context.Background())
	awaitEntered(t, cp, 1)
	for range n {
		joiners = append(joiners, resolveAsync(ap, context.Background()))
	}
	fc.awaitArmed(t, 2+n)
	registered(ap)
	return starter, joiners
}

// A retry that arrives while a check is in flight joins it and makes no second call: it is answered only when
// the check finishes.
func TestApprovalRetryJoinsTheCheckInFlight(t *testing.T) {
	cp := newRetryCP(t)
	ap, fc := cp.client()
	seedPending(ap, cp.id, time.Second)
	release := cp.hold(t)

	a, joiners := joinersOf(t, ap, fc, cp, 2)
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("control plane saw %d checks with 3 requests waiting, want 1", got)
	}
	release()

	for name, ch := range map[string]<-chan resolveResult{"a": a, "b": joiners[0], "c": joiners[1]} {
		if res := get(t, name, ch); res.State != apPending || res.ApprovalID != cp.id {
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
// answered pending. The next retry starts a fresh check, after the floor, and is allowed.
func TestApprovalJoinerOfAPendingSampleIsRefusedThenNextRetryIsAllowed(t *testing.T) {
	cp := newRetryCP(t)
	ap, fc := cp.client()
	seedPending(ap, cp.id, time.Second)
	release := cp.hold(t)

	t0 := resolveAsync(ap, context.Background())
	awaitEntered(t, cp, 1)                                 // the check has sampled pending
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil) // t1
	t2 := resolveAsync(ap, context.Background())
	fc.awaitArmed(t, 3)
	registered(ap)
	release()

	for name, ch := range map[string]<-chan resolveResult{"t0": t0, "t2": t2} {
		if res := get(t, name, ch); res.State != apPending {
			t.Fatalf("request %s -> %+v, want pending (the check sampled pending)", name, res)
		}
	}
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("control plane saw %d checks before the retry, want 1", got)
	}

	retry := resolveAsync(ap, context.Background()) // the next retry
	fc.awaitTimers(t, pendingPollFloor, 1)          // the new check waits the floor since the t0 check
	fc.Advance(pendingPollFloor)
	res := get(t, "next retry", retry)
	if res.State != apApproved || res.ApprovalID != cp.id {
		t.Fatalf("next retry -> %+v, want apApproved naming %s", res, cp.id)
	}
	if got := cp.gets.Load(); got != 2 {
		t.Fatalf("control plane saw %d checks, want 2 (a fresh check for the retry)", got)
	}
}

// A joiner of a check that sampled APPROVED is answered approved by the check's answer, and makes no second call.
func TestApprovalJoinerOfAnApprovedSampleIsAllowed(t *testing.T) {
	cp := newRetryCP(t)
	ap, fc := cp.client()
	seedPending(ap, cp.id, time.Second)
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	release := cp.hold(t)

	a, joiners := joinersOf(t, ap, fc, cp, 2)
	release()

	for name, ch := range map[string]<-chan resolveResult{"a": a, "b": joiners[0], "c": joiners[1]} {
		if res := get(t, name, ch); res.State != apApproved || res.ApprovalID != cp.id {
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
	cp := newRetryCP(t)
	ap, fc := cp.client()
	seedPending(ap, cp.id, time.Second)
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	release := cp.hold(t)

	ctx, cancel := context.WithCancel(context.Background())
	starter := resolveAsync(ap, ctx)
	awaitEntered(t, cp, 1)
	joiner := resolveAsync(ap, context.Background())
	fc.awaitArmed(t, 3)
	registered(ap)
	cancel()
	if res := get(t, "starter", starter); res.State != apPending {
		t.Fatalf("starter -> %+v, want pending (it was cancelled)", res)
	}
	release()
	if res := get(t, "joiner", joiner); res.State != apApproved || res.ApprovalID != cp.id {
		t.Fatalf("joiner -> %+v, want apApproved: the starter's cancel must not end the check", res)
	}
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("control plane saw %d checks, want 1", got)
	}
}

// Cancelling the request, or running out of its one budget, during the floor wait or the check answers
// pending and changes nothing.
func TestApprovalRetryWaitEndsPendingWithNoStateChange(t *testing.T) {
	unchanged := func(t *testing.T, ap *approvalClient, before hostApproval) {
		t.Helper()
		after := entryOf(ap)
		if after.state != before.state || after.approvalID != before.approvalID || !after.lastPoll.Equal(before.lastPoll) {
			t.Fatalf("entry changed: before %+v, after %+v", before, after)
		}
	}

	t.Run("cancelled during the floor wait", func(t *testing.T) {
		cp := newRetryCP(t)
		ap, fc := cp.client()
		seedPending(ap, cp.id, 0)
		before := entryOf(ap)
		ctx, cancel := context.WithCancel(context.Background())
		ch := resolveAsync(ap, ctx)
		fc.awaitTimers(t, pendingPollFloor, 1)
		cancel()
		if res := get(t, "cancelled", ch); res.State != apPending || res.ApprovalID != cp.id {
			t.Fatalf("cancelled -> %+v, want pending on %s", res, cp.id)
		}
		if cp.gets.Load() != 0 {
			t.Fatalf("control plane saw %d checks, want 0", cp.gets.Load())
		}
		unchanged(t, ap, before)
	})
	t.Run("budget ends during the floor wait", func(t *testing.T) {
		setFloor(t, time.Hour)
		cp := newRetryCP(t)
		ap, fc := cp.client()
		seedPending(ap, cp.id, 0)
		before := entryOf(ap)
		ch := resolveAsync(ap, context.Background())
		fc.awaitTimers(t, time.Hour, 1)
		fc.Advance(approvalCallTimeout)
		if res := get(t, "budget end", ch); res.State != apPending || res.ApprovalID != cp.id {
			t.Fatalf("budget end -> %+v, want pending on %s", res, cp.id)
		}
		if cp.gets.Load() != 0 {
			t.Fatalf("control plane saw %d checks, want 0", cp.gets.Load())
		}
		unchanged(t, ap, before)
	})
	t.Run("budget ends during the check", func(t *testing.T) {
		// The request waits out the floor first, so its budget (armed at its start) ends before the check's own
		// timeout (armed when the check starts): the request answers while the check is still in flight.
		cp := newRetryCP(t)
		ap, fc := cp.client()
		seedPending(ap, cp.id, 0)
		cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
		release := cp.hold(t)
		defer release()
		ch := resolveAsync(ap, context.Background())
		fc.awaitTimers(t, pendingPollFloor, 1)
		fc.Advance(pendingPollFloor) // the floor ends; the check starts
		awaitEntered(t, cp, 1)
		fc.Advance(approvalCallTimeout - pendingPollFloor) // the budget ends; the check has 250 ms left
		if res := get(t, "budget end", ch); res.State != apPending || res.ApprovalID != cp.id {
			t.Fatalf("budget end -> %+v, want pending on %s", res, cp.id)
		}
		if e := entryOf(ap); e.state != apPending || e.approvalID != cp.id || e.pending == nil {
			t.Fatalf("entry after the budget ended = %+v, want the same pending approval with its check still in flight", e)
		}
	})
	t.Run("cancelled during the check", func(t *testing.T) {
		cp := newRetryCP(t)
		ap, _ := cp.client()
		seedPending(ap, cp.id, time.Second)
		release := cp.hold(t)
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		ch := resolveAsync(ap, ctx)
		awaitEntered(t, cp, 1)
		cancel()
		if res := get(t, "cancelled", ch); res.State != apPending || res.ApprovalID != cp.id {
			t.Fatalf("cancelled -> %+v, want pending on %s", res, cp.id)
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
			cp := newRetryCP(t)
			cp.freshRaise.Store(true)
			ap, _ := cp.client()
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
		cp := newRetryCP(t)
		ap, fc := cp.client()
		seed(ap, retryHost, hostApproval{state: apApproved, approvalID: cp.id, lastPoll: fc.Now().Add(-2 * approvalTTL), verifiedAt: time.Now()})
		cp.decide(types.ApprovalCancelled, "", nil)
		if res := ap.Resolve(context.Background(), retryHost); res.State != apExpired {
			t.Fatalf("revoked grant -> %+v, want apExpired", res)
		}
	})
}

// A check whose approval was replaced while it ran is dropped: it never writes onto the new approval.
func TestApprovalStaleCheckResultAfterReplacementIsDropped(t *testing.T) {
	cp := newRetryCP(t)
	ap, fc := cp.client()
	seedPending(ap, cp.id, time.Second)
	cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
	release := cp.hold(t)

	ch := resolveAsync(ap, context.Background())
	awaitEntered(t, cp, 1)
	newID := uuid.New()
	seed(ap, retryHost, hostApproval{state: apPending, approvalID: newID, lastPoll: fc.Now()})
	release()

	res := get(t, "stale", ch)
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
	cp := newRetryCP(t)
	cp.freshRaise.Store(true)
	ap, fc := cp.client()
	seedPending(ap, cp.id, time.Second)
	cp.decide(types.ApprovalApproved, types.ScopeOnce, nil)
	release := cp.hold(t)

	const n = 16
	starter, joiners := joinersOf(t, ap, fc, cp, n-1)
	release()

	allowed := 0
	for i, res := range drainAnswers(t, fc, append(joiners, starter)) {
		switch {
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

// N concurrent requests on a pending host cost one control-plane call per floor window: one check serves them
// all, and until the floor ends no request makes another.
func TestApprovalConcurrentPendingRetriesCallOncePerFloorWindow(t *testing.T) {
	cp := newRetryCP(t)
	ap, fc := cp.client()
	seedPending(ap, cp.id, time.Second)
	release := cp.hold(t)

	const n = 20
	starter, joiners := joinersOf(t, ap, fc, cp, n-1)
	release()
	for i, ch := range append(joiners, starter) {
		if res := get(t, "first window", ch); res.State != apPending {
			t.Fatalf("request %d state = %v, want pending", i, res.State)
		}
	}
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("%d checks for %d concurrent requests, want 1", got, n)
	}

	// The same window again: every request parks on the floor, and none makes a call.
	ctx, cancel := context.WithCancel(context.Background())
	second := make([]<-chan resolveResult, n)
	for i := range second {
		second[i] = resolveAsync(ap, ctx)
	}
	fc.awaitTimers(t, pendingPollFloor, n)
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("%d checks inside the floor window, want still 1", got)
	}
	cancel()
	for i, ch := range second {
		if res := get(t, "second window", ch); res.State != apPending {
			t.Fatalf("request %d state = %v, want pending", i, res.State)
		}
	}
	if got := cp.gets.Load(); got != 1 {
		t.Fatalf("%d checks after the second window's requests ended, want still 1", got)
	}
}

// A request that waited on a check, and then meets a spent once grant and re-raises, still ends within its one
// budget: the re-raise is bounded by what is left of it, not by a fresh approvalCallTimeout.
func TestApprovalRetryNeverTakesMoreThanOneBudget(t *testing.T) {
	cp := newRetryCP(t)
	cp.stallPost.Store(true)
	ap, fc := cp.client()
	seedPending(ap, cp.id, time.Second)
	cp.decide(types.ApprovalApproved, types.ScopeOnce, nil)
	release := cp.hold(t)

	starter, joiners := joinersOf(t, ap, fc, cp, 1)
	slow := 3 * time.Second // the check is slow: 3 s of the request's 5 s budget
	fc.Advance(slow)
	release()

	// One request spends the grant; the other re-raises with what is left of its budget, 2 s, and the raise stalls.
	fc.awaitTimers(t, approvalCallTimeout-slow, 1) // the re-raise is armed for what is left, not a fresh 5 s
	fc.Advance(approvalCallTimeout - slow)

	allowed := 0
	for _, ch := range []<-chan resolveResult{starter, joiners[0]} {
		if res := get(t, "request", ch); res.State == apApproved {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("%d requests were let through, want exactly 1 (the once grant)", allowed)
	}
}

// A held (wait_for_review) request's own first resolve checks a pending host inline at most once per
// holdInlinePollInterval (2 s): ResolveWait's throttle, not the floor deny_with_review waits out.
func TestApprovalHoldInlineCheckKeepsItsTwoSecondThrottle(t *testing.T) {
	t.Run("one second since the last check: not checked", func(t *testing.T) {
		cp := newRetryCP(t)
		ap, _ := cp.client()
		seedPending(ap, cp.id, time.Second)
		cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
		if res := ap.resolve(context.Background(), retryHost, true); res.State != apPending {
			t.Fatalf("held -> %+v, want pending: a check only 1 s after the last is inside the 2 s throttle", res)
		}
		if got := cp.gets.Load(); got != 0 {
			t.Fatalf("control plane saw %d checks, want 0", got)
		}
	})
	t.Run("three seconds since the last check: checked", func(t *testing.T) {
		cp := newRetryCP(t)
		ap, _ := cp.client()
		seedPending(ap, cp.id, 3*time.Second)
		cp.decide(types.ApprovalApproved, types.ScopeRun, nil)
		if res := ap.resolve(context.Background(), retryHost, true); res.State != apApproved {
			t.Fatalf("held -> %+v, want apApproved from the inline check", res)
		}
		if got := cp.gets.Load(); got != 1 {
			t.Fatalf("control plane saw %d checks, want 1", got)
		}
	})
}

// A request whose budget ended must not spend a once grant that a check published: it answers pending and
// the grant stays for a request still waiting, which spends it exactly once.
func TestApprovalEndedBudgetLeavesAPublishedOnceGrantForALiveWaiter(t *testing.T) {
	ended := func(ap *approvalClient) *pendingBudget {
		pb := &pendingBudget{clock: ap.clock}
		ctx, cancel := context.WithCancel(context.Background())
		pb.get(ctx)
		cancel()
		return pb
	}
	live := func(t *testing.T, ap *approvalClient, id uuid.UUID) {
		t.Helper()
		if e := entryOf(ap); e.state != apApproved || e.scope != types.ScopeOnce || e.approvalID != id {
			t.Fatalf("the published grant was touched: %+v", e)
		}
		if res := ap.Resolve(context.Background(), retryHost); res.State != apApproved || res.ApprovalID != id {
			t.Fatalf("live waiter -> %+v, want the once grant", res)
		}
		if res := ap.Resolve(context.Background(), retryHost); res.State == apApproved {
			t.Fatal("the once grant was served twice")
		}
	}
	published := func(t *testing.T) (*approvalClient, uuid.UUID) {
		cp := newRetryCP(t)
		cp.freshRaise.Store(true)
		ap, fc := cp.client()
		id := uuid.New()
		seed(ap, retryHost, hostApproval{state: apApproved, approvalID: id, scope: types.ScopeOnce, lastPoll: fc.Now(), verifiedAt: time.Now()})
		return ap, id
	}

	// A check finishes and the budget has ended: the completed check does not win over the ended budget. Both
	// arms of waitPending's select are ready, so it runs many times to catch a coin flip.
	t.Run("a completed check and an ended budget", func(t *testing.T) {
		ap, id := published(t)
		p := &pendingPoll{done: make(chan struct{}), decided: true, applied: true, state: apApproved}
		close(p.done)
		pb := ended(ap)
		for i := range 200 {
			if res := ap.waitPending(pb.ctx, retryHost, p, 0, id); res.again || res.State != apPending {
				t.Fatalf("pass %d: waitPending -> %+v, want pending from the ended budget", i, res)
			}
		}
		live(t, ap, id)
	})
	// The budget ends after the check completed but before the waiter re-reads the entry under the lock.
	t.Run("the budget ends before the locked re-read", func(t *testing.T) {
		ap, id := published(t)
		res := ap.resolveOnce(context.Background(), retryHost, ended(ap), false)
		if res.again || res.State != apPending {
			t.Fatalf("resolveOnce -> %+v, want pending without touching the entry", res)
		}
		live(t, ap, id)
	})
}
