// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
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

// The HOLD, from the sandbox's side. Every case below is about one of three
// promises: a non-423 grant is untouched, one lapse is one workflow, and a hold
// always ends.

// fakeApprovalReader answers a scripted sequence of (state, status) pairs,
// repeating the last one forever. Optionally, notifyAfter closes notifyCh
// once the poll at that 1-based index has been answered, so a test can wait
// for a specific poll instead of sleeping a guessed wall-clock duration.
type fakeApprovalReader struct {
	mu          sync.Mutex
	steps       []approvalStep
	reads       int
	lastID      uuid.UUID
	notifyAfter int
	notifyCh    chan struct{}
}

type approvalStep struct {
	state  types.ApprovalState
	status int
	err    error
}

func (f *fakeApprovalReader) readApproval(_ context.Context, id uuid.UUID) (types.ApprovalState, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID = id
	i := f.reads
	f.reads++
	if i >= len(f.steps) {
		i = len(f.steps) - 1
	}
	s := f.steps[i]
	if f.notifyCh != nil && f.reads == f.notifyAfter {
		close(f.notifyCh)
	}
	return s.state, s.status, s.err
}

func (f *fakeApprovalReader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func pending(n int) []approvalStep {
	out := make([]approvalStep, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, approvalStep{state: types.ApprovalPending, status: http.StatusOK})
	}
	return out
}

// injectionServer is a control plane that answers 423 for the first n resolves
// and 200 after, counting every hit.
type injectionServer struct {
	srv        *httptest.Server
	approvalID uuid.UUID
	lockedFor  int32
	hits       atomic.Int32
	token      atomic.Value // the token the LAST resolve presented
	// requireToken, when set, refuses any resolve AFTER the locked ones that
	// presents a different token — i.e. the FINAL re-resolve, the one whose
	// token was captured before the wait in the bug this pins.
	requireToken string
}

func newInjectionServer(t *testing.T, lockedFor int32) *injectionServer {
	t.Helper()
	is := &injectionServer{approvalID: uuid.New(), lockedFor: lockedFor}
	is.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := is.hits.Add(1)
		is.token.Store(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if n > is.lockedFor && is.requireToken != "" && r.Header.Get("Authorization") != "Bearer "+is.requireToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if n <= is.lockedFor {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusLocked)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"state": "reauth_pending", "approval_id": is.approvalID.String(),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Host: "portal.sso.eu-west-2.amazonaws.com", Header: "x-amz-sso_bearer_token",
			Value: "fresh-session-token", JTI: "jti-2",
		})
	}))
	t.Cleanup(is.srv.Close)
	return is
}

// holdInjector builds the injector the MITM lane actually uses, with ONE
// dynamic entry already past its refresh margin so the next resolveCtx
// re-resolves.
//
// EVERY test below drives `inj.resolveCtx` — the call serveMITMRequest makes —
// and never the hold helper directly. The first shape of this file called the
// hold function itself, which is a call shape production never makes: it
// skipped the entry's reMu entirely, so "64 resolvers share one workflow" was
// green against a path where no follower ever shared one (security BLOCKER-1 /
// general B1). A test that cannot see the lock discipline cannot see the bug.
func holdInjector(t *testing.T, is *injectionServer, reader approvalReader) *injector {
	t.Helper()
	tok := &tokenSource{}
	tok.Set("t")
	return holdInjectorWithToken(t, is, reader, tok)
}

// holdInjectorCoord is holdInjector with an optional shared coordinator, for the
// cases that assert across two resolves.
func holdInjectorCoord(t *testing.T, is *injectionServer, reader approvalReader, coord ...*reauthCoordinator) *injector {
	t.Helper()
	inj := holdInjector(t, is, reader)
	if len(coord) == 1 && coord[0] != nil {
		inj.reauth = coord[0]
	}
	return inj
}

func holdInjectorWithToken(t *testing.T, is *injectionServer, reader approvalReader, tok *tokenSource) *injector {
	t.Helper()
	inj := &injector{
		byHost: map[string]*injEntry{}, base: is.srv.URL, token: tok, client: is.srv.Client(),
		reauth: newReauthCoordinator(), approvals: reader,
	}
	// A workflow OUTLIVES its first caller by design, so a test that leaves one
	// running would have its goroutine racing the test's own cleanup (it did,
	// under -race). Ending them is what the proxy's shutdown does too.
	t.Cleanup(inj.reauth.stop)
	inj.byHost[holdHost] = &injEntry{
		grantID:   uuid.New(),
		header:    injectedHeader{name: "x-amz-sso_bearer_token", value: "stale"},
		expiresAt: time.Now().Add(-time.Minute).UnixMilli(),
	}
	return inj
}

// holdHost is the entry every hold test resolves.
const holdHost = "portal.sso.eu-west-2.amazonaws.com"

// fastPolls shrinks the 1s poll for a test and restores it.
func fastPolls(t *testing.T, d time.Duration) {
	t.Helper()
	prev := holdPollInterval
	holdPollInterval = d
	t.Cleanup(func() { holdPollInterval = prev })
}

// shortBudget sets the knob for a test.
func shortBudget(t *testing.T, v string) {
	t.Helper()
	t.Setenv(envCredentialReauthTimeout, v)
}

// shrinkReauthFloor lowers minCredentialReauthTimeout to d for one test,
// restoring the real 10s production floor after. Used only by tests that wait
// out a hold's FULL budget to observe its expiry, so they wait d instead of
// the real 10s — pair it with shortBudget(t, v) where v is below d, so the
// clamp still lands on the (now-shrunk) floor exactly as it does in
// production. See TestMinCredentialReauthTimeout_ProductionFloorUnchanged for
// the guard that pins the real floor's production default.
func shrinkReauthFloor(t *testing.T, d time.Duration) {
	t.Helper()
	prev := minCredentialReauthTimeout
	minCredentialReauthTimeout = d
	t.Cleanup(func() { minCredentialReauthTimeout = prev })
}

// TestMinCredentialReauthTimeout_ProductionFloorUnchanged pins the production
// default of minCredentialReauthTimeout. It does not check that a shrinking
// test restored it — shrinkReauthFloor's t.Cleanup does that.
func TestMinCredentialReauthTimeout_ProductionFloorUnchanged(t *testing.T) {
	if minCredentialReauthTimeout != 10*time.Second {
		t.Fatalf("minCredentialReauthTimeout = %v, want the production 10s floor", minCredentialReauthTimeout)
	}
}

// The happy path: 423, two PENDING polls, APPROVED, exactly one re-resolve, and
// the header the SDK needed.
func TestResolveInjectionHolding_HoldsThenResolvesOnce(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "10s")
	is := newInjectionServer(t, 1)
	reader := &fakeApprovalReader{steps: append(pending(2), approvalStep{state: types.ApprovalApproved, status: http.StatusOK})}
	tok := &tokenSource{}
	tok.Set("run-token-1")

	out, _, err := holdInjectorCoord(t, is, reader).resolveCtx(context.Background(), holdHost)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if out.name != "x-amz-sso_bearer_token" || out.value != "fresh-session-token" {
		t.Errorf("resolved = %+v, want the injected session header", out)
	}
	if got := is.hits.Load(); got != 2 {
		t.Errorf("injection URL hit %d times, want exactly 2 (the 423, then ONE re-resolve) — each resolve re-mints and writes a credential.mint audit row", got)
	}
	if reader.count() < 3 {
		t.Errorf("approval reads = %d, want at least 3 (two pending polls then the answer)", reader.count())
	}
	if reader.lastID != is.approvalID {
		t.Errorf("polled approval %s, want the id the 423 named %s", reader.lastID, is.approvalID)
	}
}

// The case that keeps every other grant untouched: a non-423 error returns
// immediately, with no hold, no poll and no second resolve.
func TestResolveInjectionHolding_NonLockedErrorIsUnchanged(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	// The budget is set even though this test must never reach it: without it a
	// mistake here (a 424 mistaken for a 423) parks for the 600 s default and
	// reds as a ten-minute test-binary panic instead of an assertion. The clamp's
	// floor makes the same mistake fail in ~11 s with the message below.
	shortBudget(t, "10s")
	is := &injectionServer{approvalID: uuid.New()}
	is.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		is.hits.Add(1)
		http.Error(w, "secret is not in the store", http.StatusFailedDependency)
	}))
	defer is.srv.Close()
	reader := &fakeApprovalReader{steps: pending(1)}
	tok := &tokenSource{}
	tok.Set("t")

	start := time.Now()
	_, _, err := holdInjectorCoord(t, is, reader).resolveCtx(context.Background(), holdHost)
	if err == nil {
		t.Fatal("want the resolve error")
	}
	// errReauthNoCredential, not errReauthTimedOut: the sentinel split means a
	// hold can now end in ways that are NOT an expiry, and a 424 mistaken for a
	// 423 that then ended on a shutdown would slip past the narrower check.
	if errors.Is(err, errReauthNoCredential) {
		t.Fatalf("a 424 was treated as a hold: %v", err)
	}
	if reader.count() != 0 {
		t.Errorf("approval reads = %d, want 0 — nothing but a 423 may poll", reader.count())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want an immediate return", elapsed)
	}
}

// Nobody signs in: the hold ends inside its advertised bound (+10%, at least
// 100ms of slack for the 423 round trip and polling) with the sentinel that
// earns the 401 body.
func TestResolveInjectionHolding_TimesOutInsideItsBudget(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shrinkReauthFloor(t, 200*time.Millisecond)
	shortBudget(t, "1ms") // clamped UP to the (shrunk) floor either way
	is := newInjectionServer(t, 1000)
	reader := &fakeApprovalReader{steps: pending(1)}
	tok := &tokenSource{}
	tok.Set("t")

	// The BUDGET, not a caller, is what expires a hold. Clamped to the floor,
	// so the wait is real; the assertion is the sentinel and the bound.
	inj := holdInjector(t, is, reader)
	start := time.Now()
	_, _, err := inj.resolveCtx(context.Background(), holdHost)
	if !errors.Is(err, errReauthTimedOut) {
		t.Fatalf("err = %v, want errReauthTimedOut", err)
	}
	elapsed := time.Since(start)
	if elapsed < minCredentialReauthTimeout {
		t.Errorf("the hold ended after %v, before its %v budget", elapsed, minCredentialReauthTimeout)
	}
	if slack := max(minCredentialReauthTimeout/10, 100*time.Millisecond); elapsed > minCredentialReauthTimeout+slack {
		t.Errorf("the hold ran %v, past its advertised bound +%v", elapsed, slack)
	}
	// The FIRST live observer gets the reportable sentinel — it writes the one
	// decision row — and a later caller of the same workflow does not.
	if errors.Is(err, errReauthTimedOutAgain) {
		t.Error("the first observer got the already-recorded sentinel; it must be the one that reports")
	}
}

// A CALLER whose own ctx ends is released at once and is told so — ctx.Err(),
// NOT the hold's expiry. That distinction is what stops the MITM lane writing a
// 401 and a deny row for a client that hung up (security SHOULD-1), and it is
// what the workflow's independent deadline buys: the hold continues for whoever
// is left.
func TestResolveCtx_ACallerThatHangsUpIsReleasedAndTheHoldContinues(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "10s")
	is := newInjectionServer(t, 1000)
	reader := &fakeApprovalReader{steps: pending(1)}
	inj := holdInjector(t, is, reader)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := inj.resolveCtx(ctx, holdHost)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's own ctx error — an expiry that did not happen must not be reported", err)
	}
	if errors.Is(err, errReauthTimedOut) {
		t.Fatal("a hung-up caller was told the hold expired; the MITM lane would write a 401 and a deny row for a client that is gone")
	}
	if elapsed > 2*time.Second {
		t.Errorf("the caller took %v to be released, want ~its own 300ms deadline", elapsed)
	}
	// The hold is still running: the workflow owns its deadline, not the caller.
	inj.reauth.mu.Lock()
	wf := inj.reauth.workflows[is.approvalID]
	inj.reauth.mu.Unlock()
	if wf == nil {
		t.Fatal("the workflow vanished with its first caller")
	}
	if wf.finished() {
		t.Error("the first caller's disconnect ended the hold — the next retry would open a second one for the same lapse")
	}
}

// wantEndedNotExpired is the honest classification, asserted on both halves
// (security NIT-B): the sandbox IS refused — errReauthNoCredential, so the
// modelled 401 — but the hold did NOT expire, so errors.Is(err,
// errReauthTimedOut) must be FALSE. That is what keeps the
// credential:reauth-timeout decision row and outcome=timeout for a spent budget
// and nothing else, so a trail reading "expired" never describes a killed run,
// a shut-down proxy or an answered request.
func wantEndedNotExpired(t *testing.T, err error, reason string) {
	t.Helper()
	if !errors.Is(err, errReauthNoCredential) {
		t.Fatalf("err = %v, want errReauthNoCredential (the sandbox still gets the modelled 401)", err)
	}
	if errors.Is(err, errReauthTimedOut) {
		t.Errorf("err = %v is classified as an EXPIRY — it would write a credential:reauth-timeout row and count outcome=timeout for a hold whose budget never ran out", err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("err = %v does not name the reason %q", err, reason)
	}
}

// Codex #4 — TERMINAL DENIAL IS CLASSIFIED. After a run is killed the internal
// middleware can answer 401 before the CANCELLED row is readable; the hold must
// end within one poll, not run its whole budget.
func TestResolveInjectionHolding_UnauthorizedPollIsTerminal(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "1800s")
	is := newInjectionServer(t, 1000)
	reader := &fakeApprovalReader{steps: []approvalStep{{status: http.StatusUnauthorized}}}
	tok := &tokenSource{}
	tok.Set("t")

	start := time.Now()
	_, _, err := holdInjectorCoord(t, is, reader).resolveCtx(context.Background(), holdHost)
	wantEndedNotExpired(t, err, "the run has ended")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a 401 from the approval read took %v to end the hold, want one poll", elapsed)
	}
}

// …and a 5xx is TRANSIENT: a control plane that is down is not a decision.
func TestResolveInjectionHolding_ServerErrorKeepsWaiting(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "1800s")
	is := newInjectionServer(t, 1)
	reader := &fakeApprovalReader{steps: []approvalStep{
		{status: http.StatusServiceUnavailable},
		{status: http.StatusServiceUnavailable},
		{status: http.StatusBadGateway},
		{state: types.ApprovalApproved, status: http.StatusOK},
	}}
	tok := &tokenSource{}
	tok.Set("t")

	out, _, err := holdInjectorCoord(t, is, reader).resolveCtx(context.Background(), holdHost)
	if err != nil {
		t.Fatalf("three transient errors ended the hold: %v", err)
	}
	if out.value != "fresh-session-token" {
		t.Errorf("resolved = %+v, want the session that arrived after the blip", out)
	}
}

// A row that has really gone ends the hold — but only after three consecutive
// reads, so a poll racing the row's own insert does not.
func TestResolveInjectionHolding_PersistentNotFoundIsTerminal(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "1800s")
	is := newInjectionServer(t, 1000)
	reader := &fakeApprovalReader{steps: []approvalStep{{status: http.StatusNotFound}}}
	tok := &tokenSource{}
	tok.Set("t")

	start := time.Now()
	_, _, err := holdInjectorCoord(t, is, reader).resolveCtx(context.Background(), holdHost)
	wantEndedNotExpired(t, err, "the sign-in request is gone")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a persistent 404 took %v to end the hold", elapsed)
	}
	if reader.count() < reauth404Reads {
		t.Errorf("ended after %d reads, want %d — one 404 can be a read racing the insert", reader.count(), reauth404Reads)
	}
}

// A DENIED/CANCELLED/EXPIRED row gives up at once.
func TestResolveInjectionHolding_TerminalRowGivesUpAtOnce(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "1800s")
	for _, state := range []types.ApprovalState{types.ApprovalDenied, types.ApprovalCancelled, types.ApprovalExpired} {
		t.Run(string(state), func(t *testing.T) {
			is := newInjectionServer(t, 1000)
			reader := &fakeApprovalReader{steps: []approvalStep{{state: state, status: http.StatusOK}}}
			tok := &tokenSource{}
			tok.Set("t")
			start := time.Now()
			_, _, err := holdInjectorCoord(t, is, reader).resolveCtx(context.Background(), holdHost)
			wantEndedNotExpired(t, err, "answered without an approval")
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("a %s row took %v to end the hold", state, elapsed)
			}
		})
	}
}

// Codex #2 — 64 concurrent resolvers, ONE workflow, one terminal result, and
// every one of them wakes. A mutex would have given each queued caller a fresh
// full budget against the same approval.
func TestResolveInjectionHolding_SixtyFourResolversShareOneWorkflow(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "10s")
	is := newInjectionServer(t, 1)
	reader := &fakeApprovalReader{steps: append(pending(4), approvalStep{state: types.ApprovalApproved, status: http.StatusOK})}
	inj := holdInjector(t, is, reader)

	var wg sync.WaitGroup
	errs := make([]error, 64)
	start := time.Now()
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = inj.resolveCtx(context.Background(), holdHost)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("resolver %d: %v", i, err)
		}
	}
	coord := inj.reauth
	coord.mu.Lock()
	counted := coord.counted
	coord.mu.Unlock()
	if counted != 1 {
		t.Errorf("counted workflows = %d, want 1 for 64 resolvers of ONE lapse", counted)
	}
	// The assertion the first shape lacked (general B1): the injection URL is a
	// MINT — each resolve writes a credential.mint audit row — so "one workflow"
	// is only true if 64 callers made exactly TWO calls between them: the 423
	// that opened the hold, and the ONE re-resolve that ended it.
	if hits := is.hits.Load(); hits != 2 {
		t.Errorf("injection URL hit %d times for 64 resolvers of one lapse, want exactly 2 (the 423 + ONE re-resolve) — every extra hit is a re-mint and an audit row", hits)
	}
	if elapsed > 8*time.Second {
		t.Errorf("64 resolvers took %v — they did not share one bounded workflow", elapsed)
	}
}

// A late arrival, after the workflow's terminal result, does not open a second
// hold for the same lapse: it re-resolves and takes whatever the control plane
// now says (here: another 423, which is a NEW, separately counted workflow).
func TestResolveInjectionHolding_LateArrivalStartsANewCountedWorkflow(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "10s")
	is := newInjectionServer(t, 1)
	reader := &fakeApprovalReader{steps: []approvalStep{{state: types.ApprovalApproved, status: http.StatusOK}}}
	inj := holdInjector(t, is, reader)
	coord := inj.reauth

	if _, _, err := inj.resolveCtx(context.Background(), holdHost); err != nil {
		t.Fatalf("first: %v", err)
	}
	coord.mu.Lock()
	first := coord.counted
	coord.mu.Unlock()
	if first != 1 {
		t.Fatalf("counted = %d after one lapse, want 1", first)
	}
	// Force the entry stale again so the next call really re-resolves; the
	// control plane now answers 200 (lockedFor was 1), so no workflow opens.
	inj.byHost[holdHost].reMu.Lock()
	inj.byHost[holdHost].expiresAt = time.Now().Add(-time.Minute).UnixMilli()
	inj.byHost[holdHost].reMu.Unlock()
	if _, _, err := inj.resolveCtx(context.Background(), holdHost); err != nil {
		t.Fatalf("late arrival: %v", err)
	}
	coord.mu.Lock()
	after := coord.counted
	coord.mu.Unlock()
	if after != first {
		t.Errorf("counted = %d, want %d — a caller arriving after the result must not open a second hold", after, first)
	}
}

// The per-run cap: the ninth workflow is refused rather than held.
func TestResolveInjectionHolding_WorkflowCapRefusesTheNinth(t *testing.T) {
	coord := newReauthCoordinator()
	id := uuid.New()
	for i := 0; i < maxReauthHolds; i++ {
		wf, fresh, ok := coord.admit(uuid.New(), time.Second)
		if !ok || !fresh {
			t.Fatalf("workflow %d: ok=%v fresh=%v", i, ok, fresh)
		}
		// End it the way a workflow ends: its own goroutine publishes and closes.
		wf.err = errReauthTimedOut
		close(wf.done)
	}
	if _, _, ok := coord.admit(id, time.Second); ok {
		t.Fatalf("the %dth workflow was admitted; want the cap to refuse it", maxReauthHolds+1)
	}
	// A repeat of an ALREADY-COUNTED approval id is always joinable — it is the
	// same lapse, and refusing it would punish a caller for its SDK's cadence.
	coord.mu.Lock()
	var known uuid.UUID
	for k := range coord.workflows {
		known = k
		break
	}
	coord.mu.Unlock()
	if _, fresh, ok := coord.admit(known, time.Second); !ok || fresh {
		t.Errorf("re-admitting a known approval id gave ok=%v fresh=%v, want ok=true fresh=false", ok, fresh)
	}
}

// Codex #4 — the CURRENT run token on every call. A token captured before the
// wait would let the polls succeed on a renewed token while the final resolve
// presents the obsolete one and is refused.
func TestResolveInjectionHolding_UsesTheCurrentRunToken(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "10s")
	is := newInjectionServer(t, 1)
	is.requireToken = "rotated-token"
	// Enough PENDING polls (at 5ms each) that the rotation below lands well
	// before the APPROVED one — the point is which token the FINAL resolve
	// presents, not how fast the answer arrives.
	reader := &fakeApprovalReader{steps: append(pending(20), approvalStep{state: types.ApprovalApproved, status: http.StatusOK})}
	tok := &tokenSource{}
	tok.Set("original-token")

	// The run token rotates while the hold is waiting.
	go func() {
		time.Sleep(20 * time.Millisecond)
		tok.Set("rotated-token")
	}()
	_, _, err := holdInjectorWithToken(t, is, reader, tok).resolveCtx(context.Background(), holdHost)
	if err != nil {
		t.Fatalf("the final resolve presented a stale run token: %v", err)
	}
	if got, _ := is.token.Load().(string); got != "rotated-token" {
		t.Errorf("the last resolve presented %q, want the rotated token", got)
	}
}

// The budget knob: parsed, clamped, and never turned off by a typo.
func TestCredentialReauthBudget_ClampsAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", defaultCredentialReauthTimeout},
		{"nonsense", defaultCredentialReauthTimeout},
		{"0s", defaultCredentialReauthTimeout},
		{"-5m", defaultCredentialReauthTimeout},
		{"1s", minCredentialReauthTimeout},
		{"90s", 90 * time.Second},
		{"9h", maxCredentialReauthTimeout},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(envCredentialReauthTimeout, tc.raw)
			if got := credentialReauthBudget(); got != tc.want {
				t.Errorf("credentialReauthBudget(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// The 423 body decode: an id or nothing, never a guess.
func TestReauthPendingFrom(t *testing.T) {
	id := uuid.New()
	body, _ := json.Marshal(map[string]string{"state": "reauth_pending", "approval_id": id.String()})
	got, ok := reauthPendingFrom(body)
	if !ok || got.approvalID != id {
		t.Errorf("reauthPendingFrom = (%v, %v), want the id", got, ok)
	}
	for _, bad := range []string{``, `{}`, `{"approval_id":"not-a-uuid"}`, `not json`,
		`{"approval_id":"00000000-0000-0000-0000-000000000000"}`} {
		if _, ok := reauthPendingFrom([]byte(bad)); ok {
			t.Errorf("reauthPendingFrom(%q) accepted a body with no usable id", bad)
		}
	}
}

// The 401 body is the modelled AWS error, and it carries Wardyn's sentence
// rather than a bare status — and no credential.
func TestWriteSSOUnauthorized_IsTheModelledAWSError(t *testing.T) {
	// BOTH sentences: an expiry and an end-that-was-not-an-expiry get the same
	// modelled shape, and each says only what is true of it (security NIT-B).
	for _, tc := range []struct {
		name     string
		sentence string
		says     string
	}{
		{"budget expired", reauthTimedOutSentence, "nobody signed in before the hold expired"},
		{"ended, not expired", reauthEndedSentence, "the request ended before a sign-in arrived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeSSOUnauthorized(w, tc.sentence)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("code = %d, want 401 — a 502 is a transport error both SDKs RETRY", w.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if body["__type"] != "UnauthorizedException" {
				t.Errorf("__type = %q, want UnauthorizedException (modelled, non-retryable)", body["__type"])
			}
			if body["message"] != tc.sentence {
				t.Errorf("message = %q, want the hold's own sentence", body["message"])
			}
			if !strings.Contains(body["message"], tc.says) {
				t.Errorf("message = %q does not say %q — a body that misstates WHY is the one place a person would look", body["message"], tc.says)
			}
			if !strings.Contains(body["message"], "nothing was substituted") {
				t.Error("the body does not say that nothing was substituted — it is the one place a reader could conclude otherwise")
			}
		})
	}
	// The expiry sentence must not be reachable for a hold that did not expire.
	if strings.Contains(reauthEndedSentence, "expired") {
		t.Error("the ended sentence claims an expiry")
	}
}

// Codex #4, the other half of the substrate pass-through: the forwarded value
// must actually CHANGE the wait, not merely arrive. The substrate tests
// (internal/runner/docker, internal/runner/k8s) pin that a non-default value
// reaches the sidecar's environment; this pins that the sidecar's own hold is
// bounded by it.
func TestResolveInjectionHolding_TheKnobBoundsTheLiveWait(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	for _, tc := range []struct{ raw string }{{"30s"}, {"120s"}} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(envCredentialReauthTimeout, tc.raw)
			want, _ := time.ParseDuration(tc.raw)
			coord := newReauthCoordinator()
			is := newInjectionServer(t, 1000)
			reader := &fakeApprovalReader{steps: pending(1)}
			tok := &tokenSource{}
			tok.Set("t")

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _, _ = holdInjectorCoord(t, is, reader, coord).resolveCtx(ctx, holdHost)
			}()
			// Let the leader open its workflow, then read the deadline it set.
			deadline := waitForWorkflowDeadline(t, coord)
			cancel()
			<-done

			if got := time.Until(deadline).Round(time.Second); got != want {
				t.Errorf("the hold's deadline is %v out with the knob at %s, want %v — a forwarded knob that does not bound the wait is a control that does not exist", got, tc.raw, want)
			}
		})
	}
}

func waitForWorkflowDeadline(t *testing.T, coord *reauthCoordinator) time.Time {
	t.Helper()
	for i := 0; i < 200; i++ {
		coord.mu.Lock()
		for _, wf := range coord.workflows {
			d := wf.deadline
			coord.mu.Unlock()
			return d
		}
		coord.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no workflow opened")
	return time.Time{}
}

// The leader disconnects. Codex #2's clause, and the one the first shape got
// exactly backwards: the caller that happened to arrive first ended the
// workflow with a timeout that had not happened, handing every follower a
// terminal 401 and writing a credential:reauth-timeout deny row for a hold that
// still had minutes left.
//
// Now: it is released with its own ctx error, writes nothing, the deadline is
// untouched, and a follower that arrives after it still gets the credential.
func TestResolveCtx_LeaderDisconnectLeavesTheWorkflowAndItsDeadlineAlone(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "10s")
	is := newInjectionServer(t, 1)
	// PENDING long enough that the first caller's ctx dies while the hold runs.
	reader := &fakeApprovalReader{steps: append(pending(30), approvalStep{state: types.ApprovalApproved, status: http.StatusOK})}
	inj := holdInjector(t, is, reader)

	lctx, lcancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer lcancel()
	if _, _, err := inj.resolveCtx(lctx, holdHost); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the first caller got %v, want its own ctx error", err)
	}

	inj.reauth.mu.Lock()
	wf := inj.reauth.workflows[is.approvalID]
	counted := inj.reauth.counted
	inj.reauth.mu.Unlock()
	if wf == nil || wf.finished() {
		t.Fatal("the first caller's disconnect ended the workflow")
	}
	deadline := wf.deadline

	// …AND IT IS STILL ON THE ENTRY (security NIT-A). The coordinator keeping it
	// is not enough: the entry is what the next arrival reads first, and taking
	// a LIVE workflow off it made that arrival call resolveInjection — a broker
	// mint and a credential.mint audit row — and only then join, through the
	// coordinator, the very workflow it should have joined without asking. One
	// spare mint per disconnect, on a hash-chained log, at the measured ~30 s
	// SDK cadence.
	inj.mu.Lock()
	e := inj.byHost[holdHost]
	inj.mu.Unlock()
	e.reMu.Lock()
	onEntry := e.reauth
	e.reMu.Unlock()
	if onEntry != wf {
		t.Fatalf("the entry's workflow after the disconnect = %v, want the live one — the next arrival re-mints before joining it", onEntry)
	}

	// A later arrival joins the SAME workflow — same deadline, no new count —
	// and rides it to the credential.
	out, _, err := inj.resolveCtx(context.Background(), holdHost)
	if err != nil {
		t.Fatalf("the follower that took over: %v", err)
	}
	if out.value != "fresh-session-token" {
		t.Errorf("the follower resolved %q, want the session the hold waited for", out.value)
	}
	inj.reauth.mu.Lock()
	after := inj.reauth.counted
	inj.reauth.mu.Unlock()
	if after != counted {
		t.Errorf("counted %d -> %d: the takeover opened a second workflow for one lapse", counted, after)
	}
	if !wf.deadline.Equal(deadline) {
		t.Errorf("the deadline moved from %v to %v — a takeover must not renew the budget", deadline, wf.deadline)
	}
	if hits := is.hits.Load(); hits != 2 {
		t.Errorf("injection URL hits = %d, want 2 (the 423 + ONE re-resolve) across a disconnect and a takeover", hits)
	}
}

// A late arrival after the expiry gets that terminal result at once: no second
// hold, no second count, and — because the decision row is claimed once — no
// second deny row. With the measured ~30 s SDK cadence this is the difference
// between one recorded expiry and one per retry for ten minutes.
func TestResolveCtx_LateArrivalAfterTimeoutGetsTheStickyResult(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shrinkReauthFloor(t, 500*time.Millisecond)
	shortBudget(t, "1ms")             // clamped UP to the (shrunk) floor; the first call below waits it out
	is := newInjectionServer(t, 1000) // 423 forever
	reader := &fakeApprovalReader{steps: pending(1)}
	inj := holdInjector(t, is, reader)

	_, _, first := inj.resolveCtx(context.Background(), holdHost)
	if !errors.Is(first, errReauthTimedOut) || errors.Is(first, errReauthTimedOutAgain) {
		t.Fatalf("the first observer got %v, want the reportable expiry", first)
	}
	hitsAfterFirst := is.hits.Load()
	inj.reauth.mu.Lock()
	counted := inj.reauth.counted
	inj.reauth.mu.Unlock()

	start := time.Now()
	_, _, second := inj.resolveCtx(context.Background(), holdHost)
	if !errors.Is(second, errReauthTimedOut) {
		t.Fatalf("the late arrival got %v, want the same expiry", second)
	}
	if !errors.Is(second, errReauthTimedOutAgain) {
		t.Error("the late arrival was handed a REPORTABLE expiry — it would write a second credential:reauth-timeout row for one hold")
	}
	if elapsed := time.Since(start); elapsed > minCredentialReauthTimeout/2 {
		t.Errorf("the late arrival waited %v — it opened a second hold instead of taking the sticky result", elapsed)
	}
	inj.reauth.mu.Lock()
	after := inj.reauth.counted
	inj.reauth.mu.Unlock()
	if after != counted {
		t.Errorf("counted %d -> %d: a retry after the expiry opened a NEW full-budget workflow for the same lapse", counted, after)
	}
	// It DID re-resolve once (it cannot know the approval id without asking),
	// and that is the only extra control-plane call.
	if hits := is.hits.Load(); hits != hitsAfterFirst+1 {
		t.Errorf("injection URL hits %d -> %d, want exactly one more", hitsAfterFirst, hits)
	}
}

// …and a 423 naming a DIFFERENT approval id IS a new lapse: the old row left
// PENDING, the control plane raised a fresh request, and that earns its own
// counted lifecycle and its own budget.
func TestResolveCtx_ANewApprovalIDStartsASecondCountedWorkflow(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shrinkReauthFloor(t, 50*time.Millisecond) // two full-budget waits below
	shortBudget(t, "1ms")                     // clamped UP to the (shrunk) floor
	is := newInjectionServer(t, 1000)
	reader := &fakeApprovalReader{steps: pending(1)}
	inj := holdInjector(t, is, reader)

	if _, _, err := inj.resolveCtx(context.Background(), holdHost); !errors.Is(err, errReauthTimedOut) {
		t.Fatalf("first lapse: %v", err)
	}
	inj.reauth.mu.Lock()
	first := inj.reauth.counted
	inj.reauth.mu.Unlock()

	// The old request left PENDING (cancelled, expired, or resolved and lapsed
	// again); the control plane now names a new one.
	is.approvalID = uuid.New()

	if _, _, err := inj.resolveCtx(context.Background(), holdHost); !errors.Is(err, errReauthTimedOut) {
		t.Fatalf("second lapse: %v", err)
	}
	inj.reauth.mu.Lock()
	after := inj.reauth.counted
	inj.reauth.mu.Unlock()
	if after != first+1 {
		t.Errorf("counted %d -> %d, want one more: a NEW approval id is a new lapse and gets its own budget", first, after)
	}
}

// Two stacked joiners, and the second hangs up. Adopted verbatim from
// REVIEW-2-security's appendix (SHOULD-2), because the brief's own
// counterfactual did not discriminate: with a fake that answers 423 once, the
// follower in every other test re-resolves into a direct 200 and never joins
// the workflow at all, so "a follower waits on its own ctx, not on reMu" was
// asserted on a wait nobody made. A JOINER that blocks under reMu passes the
// whole suite — and pins the mutex for the budget, so the joiner behind it is
// not released when its SDK gives up. That is the half this case owns.
//
// Reviewer-executed: clean tree PASS 0.46 s; with a joiner made to wait under
// reMu, FAIL "joiner 2 was not released by its own ctx (took 9.845s)".
func TestResolveCtx_TwoJoinersTheSecondHangsUpAndIsReleased(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shortBudget(t, "10s")
	is := newInjectionServer(t, 1_000_000) // 423 forever
	inj := holdInjector(t, is, &fakeApprovalReader{steps: pending(1)})
	go func() { _, _, _ = inj.resolveCtx(context.Background(), holdHost) }() // creator, parked
	waitForWorkflowDeadline(t, inj.reauth)
	time.Sleep(50 * time.Millisecond)
	go func() { _, _, _ = inj.resolveCtx(context.Background(), holdHost) }() // joiner 1, patient
	time.Sleep(100 * time.Millisecond)
	fctx, fcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer fcancel()
	start := time.Now()
	_, _, ferr := inj.resolveCtx(fctx, holdHost) // joiner 2, hangs up
	if elapsed := time.Since(start); !errors.Is(ferr, context.DeadlineExceeded) || elapsed > 2*time.Second {
		t.Errorf("joiner 2 was not released by its own ctx (took %v, err=%v)", elapsed.Round(time.Millisecond), ferr)
	}
}
