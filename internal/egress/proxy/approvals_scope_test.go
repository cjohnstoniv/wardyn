// Copyright 2025 The Wardyn Authors
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

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file locks down decision SCOPES (once / run / until / always). Every test
// here is a fail-OPEN regression guard: the failure mode each one catches is a
// host reaching the internet with no human decision behind it, which is exactly
// the class a green build cannot see.
//
// The load-bearing invariant is consumeIfOnce's: a terminal `once` entry never
// survives an unlock. There are four returns that can carry apApproved — the
// snapshot fast path, the needPoll branch, ResolveWait's publish, and the
// consumed early return — so there are four ways to leak a `once` grant, and a
// test per way.

// decidedCP serves a control plane that answers every GET with one decided
// approval, and every raise with a fresh PENDING one. gets counts GETs so a test
// can assert an expiry was honored from memory with NO network call.
func decidedCP(t *testing.T, apID uuid.UUID, state types.ApprovalState, scope types.ApprovalScope, expires *time.Time) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/internal/approvals/"):
			gets.Add(1)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{
				ID: apID, State: state, DecisionScope: scope, DecisionExpiresAt: expires,
			})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

// seed installs a cache entry directly, which is how a test reaches a state the
// proxy would otherwise need a whole approval round trip to arrive at.
func seed(a *approvalClient, host string, st hostApproval) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hosts[host] = &st
}

// TestOnceServedToExactlyOneConcurrentResolver is the headline guard: a cached
// `once` grant must be spent by exactly ONE of N racing callers. The pre-fix
// code snapshotted state under the lock and returned AFTER unlocking, so every
// racer observed apApproved and the "one connection" promise was a lie bounded
// only by how many goroutines happened to arrive.
func TestOnceServedToExactlyOneConcurrentResolver(t *testing.T) {
	apID := uuid.New()
	cp, _ := decidedCP(t, apID, types.ApprovalApproved, types.ScopeOnce, nil)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	seed(ap, "egress.test", hostApproval{
		state: apApproved, approvalID: apID, scope: types.ScopeOnce, lastPoll: time.Now(),
	})

	const racers = 24
	var granted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ap.Resolve(context.Background(), "egress.test").State == apApproved {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := granted.Load(); got != 1 {
		t.Fatalf("%d of %d racers got the `once` grant, want exactly 1", got, racers)
	}
}

// TestOnceConsumedOnFirstObservation covers the needPoll branch — the NORMAL
// path under deny_with_review, not a race. The grant is decided while the entry
// is still apPending, so the snapshot consume is a no-op and the poll is what
// discovers it. If that branch serves the grant without consuming, `once` is
// served exactly twice: once from the poll, once from the cache afterwards.
func TestOnceConsumedOnFirstObservation(t *testing.T) {
	apID := uuid.New()
	cp, _ := decidedCP(t, apID, types.ApprovalApproved, types.ScopeOnce, nil)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	seed(ap, "egress.test", hostApproval{
		state: apPending, approvalID: apID, lastPoll: time.Now().Add(-2 * pollInterval),
	})

	if got := ap.Resolve(context.Background(), "egress.test").State; got != apApproved {
		t.Fatalf("first observation state = %v, want apApproved", got)
	}
	// The entry must be spent, so the very next caller re-raises instead of being
	// handed the same grant off the fast path.
	if got := ap.Resolve(context.Background(), "egress.test").State; got == apApproved {
		t.Fatal("the `once` grant survived the needPoll branch and was served twice")
	}
}

// TestConsumedOnceDoesNotReapproveViaStaleID is the fail-open blocker: clearing
// only `state` on consume leaves approvalID pointing at the already-APPROVED
// row. The raise path then refuses to overwrite a non-nil id, discards the fresh
// approval's id, and the next poll re-reads the OLD approved row — the host
// re-approves itself with no human involved. Reset must clear the WHOLE entry.
func TestConsumedOnceDoesNotReapproveViaStaleID(t *testing.T) {
	apID := uuid.New()
	cp, _ := decidedCP(t, apID, types.ApprovalApproved, types.ScopeOnce, nil)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	seed(ap, "egress.test", hostApproval{
		state: apApproved, approvalID: apID, scope: types.ScopeOnce, lastPoll: time.Now(),
	})

	if got := ap.Resolve(context.Background(), "egress.test").State; got != apApproved {
		t.Fatalf("first resolve = %v, want apApproved", got)
	}
	ap.mu.Lock()
	st := ap.hosts["egress.test"]
	gotID, gotScope, gotExpiry := st.approvalID, st.scope, st.expiresAt
	ap.mu.Unlock()
	if gotID != uuid.Nil || gotScope != "" || !gotExpiry.IsZero() {
		t.Fatalf("consume left residue: id=%v scope=%q expiresAt=%v — the whole entry must clear",
			gotID, gotScope, gotExpiry)
	}
	// Drive several more requests; none may come back approved off the stale id.
	for i := 0; i < 4; i++ {
		if got := ap.Resolve(context.Background(), "egress.test").State; got == apApproved {
			t.Fatalf("request %d re-approved with no human decision (stale-id resurrection)", i+2)
		}
	}
}

// TestStalePollerCannotResurrectConsumedEntry covers the fifth leak, which only
// became reachable BECAUSE consume introduced apNone resets: two callers can both
// pass needPoll whenever a round trip outlives pollInterval, and the slow one
// used to relock and write its result onto an entry a sibling had already spent.
// The id discriminator is what stops it.
func TestStalePollerCannotResurrectConsumedEntry(t *testing.T) {
	apID := uuid.New()
	release := make(chan struct{})
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/internal/approvals/") {
			<-release // hold this poll open so the "sibling" wins the race
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{
				ID: apID, State: types.ApprovalApproved, DecisionScope: types.ScopeOnce,
			})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
	}))
	defer cp.Close()

	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	seed(ap, "egress.test", hostApproval{
		state: apPending, approvalID: apID, lastPoll: time.Now().Add(-2 * pollInterval),
	})

	done := make(chan resolveResult, 1)
	go func() { done <- ap.Resolve(context.Background(), "egress.test") }()

	// While that poll is parked, a sibling consumes the grant and a fresh raise
	// takes the slot — the exact interleaving the discriminator exists for.
	time.Sleep(20 * time.Millisecond)
	seed(ap, "egress.test", hostApproval{state: apPending, approvalID: uuid.New(), lastPoll: time.Now()})
	close(release)

	if got := (<-done).State; got == apApproved {
		t.Fatal("a stale in-flight poller resurrected a spent grant and served it")
	}
	ap.mu.Lock()
	st := ap.hosts["egress.test"]
	resultState := st.state
	ap.mu.Unlock()
	if resultState == apApproved {
		t.Fatal("a stale poller wrote apApproved over a fresh PENDING entry — the next fast path would serve it")
	}
}

// TestResolveWaitOnceReleasesExactlyOneHolder: each held connection runs its own
// ticker and polls the same approval, so before the fix every holder returned its
// OWN poll result and up to maxHolds callers consumed a single `once`. The
// control plane cannot help — it does not know about consumption — so the cache
// is the only place the spend is recorded.
func TestResolveWaitOnceReleasesExactlyOneHolder(t *testing.T) {
	saved := holdPollInterval
	holdPollInterval = 5 * time.Millisecond
	defer func() { holdPollInterval = saved }()

	apID := uuid.New()
	var raised atomic.Bool
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			raised.Store(true)
			w.WriteHeader(http.StatusCreated)
			// A FRESH id per raise, mirroring the real control plane: a re-raise
			// after a consume creates a NEW approval row, because the PENDING dedup
			// index cannot match an already-decided one. Returning the same id here
			// would let a scheduling-delayed holder re-raise, land back on the id it
			// is still polling, and be granted a second time — a false failure that
			// blames the code for the fake's unrealistic behavior.
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
			return
		}
		// Every holder polls this and is told APPROVED — the cache, not the CP, is
		// what must make the grant singular.
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{
			ID: apID, State: types.ApprovalApproved, DecisionScope: types.ScopeOnce,
		})
	}))
	defer cp.Close()

	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	ap.configureHold(types.FirstUseWaitForReview, 2*time.Second, 8)
	// Seed the pending entry directly so every holder parks on the SAME approval
	// id: the cache is then the only thing that can make the grant singular,
	// which is the property under test.
	seed(ap, "egress.test", hostApproval{state: apPending, approvalID: apID, lastPoll: time.Now()})
	raised.Store(true) // the raise is pre-seeded; the hold path is what we exercise

	const holders = 8
	var granted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < holders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ap.ResolveWait(context.Background(), "egress.test").State == apApproved {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()

	if !raised.Load() {
		t.Fatal("no approval was raised — the test never exercised the hold path")
	}
	if got := granted.Load(); got != 1 {
		t.Fatalf("%d of %d holders got the `once` grant, want exactly 1", got, holders)
	}
}

// TestExpiredUntilResetsToUnknownWithoutPolling: an `until` shorter than
// approvalTTL must be honored from memory, on the very next request, with no
// control-plane round trip — and it must reset to UNKNOWN (re-ask the operator),
// never to denied, because "until T" means the grant lapsed, not that the host
// became forbidden.
func TestExpiredUntilResetsToUnknownWithoutPolling(t *testing.T) {
	apID := uuid.New()
	past := time.Now().Add(-time.Minute)
	cp, gets := decidedCP(t, apID, types.ApprovalApproved, types.ScopeUntil, &past)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	// lastPoll is FRESH, so nothing but the expiry check can invalidate this.
	seed(ap, "egress.test", hostApproval{
		state: apApproved, approvalID: apID, scope: types.ScopeUntil,
		expiresAt: past, lastPoll: time.Now(),
	})

	res := ap.Resolve(context.Background(), "egress.test")
	if res.State == apApproved {
		t.Fatal("an expired `until` grant was still served")
	}
	if res.State == apDenied {
		t.Fatal("an expired `until` resolved to DENIED; it must return to unknown so the operator is re-asked")
	}
	if got := gets.Load(); got != 0 {
		t.Fatalf("expiry cost %d control-plane GETs, want 0 (it must be honored from memory)", got)
	}
}

// TestUnknownScopeFloorsToOnce: an unrecognised scope can only come from a NEWER
// control plane, and treating it as `run` would be fail-open across versions.
// Normalize floors it to the tightest scope, so it is spent like a `once`.
func TestUnknownScopeFloorsToOnce(t *testing.T) {
	apID := uuid.New()
	cp, _ := decidedCP(t, apID, types.ApprovalApproved, "some-future-scope", nil)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	seed(ap, "egress.test", hostApproval{
		state: apApproved, approvalID: apID, scope: "some-future-scope", lastPoll: time.Now(),
	})

	if got := ap.Resolve(context.Background(), "egress.test").State; got != apApproved {
		t.Fatalf("first resolve = %v, want apApproved", got)
	}
	if got := ap.Resolve(context.Background(), "egress.test").State; got == apApproved {
		t.Fatal("an unrecognised scope behaved like `run` — that is fail-open against a newer control plane")
	}
}

// TestRunAndAlwaysUnchanged pins the compatibility promise: the two scopes that
// mean "today's behavior" must be served from cache indefinitely, with zero
// network calls, exactly as before scopes existed. The empty scope is included
// because it is what every pre-migration row and every older client sends.
func TestRunAndAlwaysUnchanged(t *testing.T) {
	for _, scope := range []types.ApprovalScope{types.ScopeRun, types.ScopeAlways, ""} {
		t.Run(string("scope="+scope), func(t *testing.T) {
			apID := uuid.New()
			cp, gets := decidedCP(t, apID, types.ApprovalApproved, scope, nil)
			ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
			seed(ap, "egress.test", hostApproval{
				state: apApproved, approvalID: apID, scope: scope, lastPoll: time.Now(),
			})
			for i := 0; i < 5; i++ {
				if got := ap.Resolve(context.Background(), "egress.test").State; got != apApproved {
					t.Fatalf("call %d = %v, want apApproved (scope %q must not be consumed)", i+1, got, scope)
				}
			}
			if got := gets.Load(); got != 0 {
				t.Fatalf("fresh grant cost %d GETs, want 0", got)
			}
		})
	}
}

// TestResolveThenResolveWaitDoesNotDoubleConsume: ResolveWait calls Resolve first
// and returns early on a terminal state, so a cached grant is spent by exactly
// one of the two consume sites. Double-consuming fails CLOSED rather than open —
// but it silently eats a grant the operator just gave, which reads in the field
// as "the approval didn't work".
func TestResolveThenResolveWaitDoesNotDoubleConsume(t *testing.T) {
	apID := uuid.New()
	cp, _ := decidedCP(t, apID, types.ApprovalApproved, types.ScopeOnce, nil)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	ap.configureHold(types.FirstUseWaitForReview, time.Second, 4)
	seed(ap, "egress.test", hostApproval{
		state: apApproved, approvalID: apID, scope: types.ScopeOnce, lastPoll: time.Now(),
	})

	if got := ap.ResolveWait(context.Background(), "egress.test").State; got != apApproved {
		t.Fatalf("ResolveWait on a cached `once` grant = %v, want apApproved (the grant must not be dropped)", got)
	}
}

// TestApprovalGrantReleasesEveryPortOnTheHost (F145) is the missing PORT axis of
// this file. Every test above locks a scope down along TIME (once / run / until
// / always); nothing anywhere encoded how WIDE one grant reaches, and neither
// approvals test file contained the string "port" or a port literal at all.
//
// The behaviour is deliberate and now documented (docs/POLICIES.md, "Every scope
// in that table is HOST-wide — on every port"): the human is asked about a bare
// host, the cache is keyed on that bare host, and the durable `always` write
// goes through hostrules.ValidApprovedHost, which refuses a port by
// construction. Nothing pinned it, so a well-meant change to key the cache (or
// evaluate's call into it) on host:port would silently make every already-granted
// approval stop covering the ports the operator was told it covered — and the
// whole existing suite would stay green, because it drives approvalClient
// directly and never goes through evaluate.
//
// So the pin drives evaluate, on the four ports the finding measured, and
// asserts BOTH halves: allowed, and attributed to the SAME approval — an allow
// that re-raised a second approval per port would satisfy a naive "still
// reachable" assertion while breaking the promise the document makes.
func TestApprovalGrantReleasesEveryPortOnTheHost(t *testing.T) {
	apID := uuid.New()
	cp, _ := decidedCP(t, apID, types.ApprovalApproved, types.ScopeRun, nil)
	ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
	seed(ap, "mirror.test", hostApproval{
		state: apApproved, approvalID: apID, scope: types.ScopeRun, lastPoll: time.Now(),
	})

	p, _ := newTestProxy(t, types.RunPolicySpec{
		FirstUseApproval: types.FirstUseDenyWithReview,
	}, "127.0.0.1:1", ap, nil)

	// 443 is the port the approval would have been raised by; the other three are
	// ports no human was ever asked about.
	for _, port := range []int{443, 22, 3306, 6379} {
		decision, target, log := p.evaluate(context.Background(), "mirror.test", port, "CONNECT", "")
		if decision != egress.Allow {
			t.Fatalf("mirror.test:%d = %q (rule %q), want allow — one approval is HOST-wide, "+
				"and docs/POLICIES.md's scope table tells the operator so", port, decision, log.RuleSource)
		}
		if target != "93.184.216.34:"+strconv.Itoa(port) {
			t.Errorf("mirror.test:%d target = %q, want the vetted address on the requested port", port, target)
		}
		if log.ApprovalID == nil || *log.ApprovalID != apID {
			t.Errorf("mirror.test:%d attributed to %v, want the ONE approval %s — a per-port re-raise would "+
				"ask the operator again for a decision the document says they already made", port, log.ApprovalID, apID)
		}
	}
}
