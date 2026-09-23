// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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
// shells out to WSL interop (powershell.exe, then netsh.exe) — and probeTimeout
// bounds each CHILD, not the call (see the package comment on hostproxy_cache.go
// and the setup package's own grandchild-pipe pin), so a host whose interop is
// wedged turned the FIRST GET /setup/status after every daemon boot into a call
// of six seconds at best and unbounded at worst. The console's first paint waits
// on it, and Playwright's 5s expect times out before any heading renders. The
// memo above only ever helped the SECOND caller.
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
	// sweep/2 rather than a literal bound, so the two numbers cannot drift apart.
	if elapsed := time.Since(start); elapsed > sweep/2 {
		t.Errorf("GET /setup/status took %s with a %s host sweep in flight — the poll is waiting on a subprocess", elapsed, sweep)
	}
}

// the sweep's failure modes
//
// Everything below is about a sweep that does NOT simply answer: one abandoned
// mid-flight by a reset, one that never returns, one that panics, and the one an
// operator asked for on purpose. Each is a state the memo can be left in, and
// each of them used to leave it blind for the life of the process.

// blockingHostProxyDetect installs a detector that counts its calls and blocks
// until the returned release func is called (or the test ends). It returns the
// counter and the release.
func blockingHostProxyDetect(t *testing.T) (*atomic.Int64, chan struct{}) {
	t.Helper()
	var calls atomic.Int64
	gate := make(chan struct{})
	real := hostProxyDetect
	hostProxyDetect = func() setup.HostProxyDetection {
		calls.Add(1)
		<-gate
		return setup.HostProxyDetection{}
	}
	t.Cleanup(func() {
		hostProxyDetect = real
		select {
		case <-gate:
		default:
			close(gate)
		}
		hostProxyCacheReset()
	})
	hostProxyCacheReset()
	return &calls, gate
}

// waitHostProxyCalls blocks until the detector has been called n times.
func waitHostProxyCalls(t *testing.T, calls *atomic.Int64, n int64, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("host-proxy detector was called %d time(s), want %d — %s", calls.Load(), n, why)
}

// TestHostProxy_ResetWhileSweepingLetsTheNextPollReDetect is the pin for the one
// thing hostProxyCacheReset promises: "the next caller re-detects".
//
// It did not, while a sweep was in flight — the reset dropped the memo but left
// hostProxySweeping set, so startHostProxySweepLocked early-returned on a flag
// whose goroutine belonged to a memo that no longer existed, and nothing ever
// swept again. The lane's own memo test failed on exactly this whenever a
// neighbouring test had left a real 6s WSL sweep running.
func TestHostProxy_ResetWhileSweepingLetsTheNextPollReDetect(t *testing.T) {
	calls, gate := blockingHostProxyDetect(t)
	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))

	if w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("first poll: code = %d", w.Code)
	}
	waitHostProxyCalls(t, calls, 1, "the first poll must start a sweep")

	hostProxyCacheReset() // taken WHILE the first sweep is still blocked in detect()

	if w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("second poll: code = %d", w.Code)
	}
	waitHostProxyCalls(t, calls, 2, "a reset taken mid-sweep must not leave the in-flight flag set")
	close(gate)
}

// TestHostProxy_HangingSweepIsAbandonedAndRetried pins the real ceiling.
//
// probeTimeout never bounded the CALL (exec's Output() waits on a pipe a killed
// child's grandchild can still hold — see the setup package's own pin), so a
// wedged host could hang a sweep indefinitely. With the flag held for the whole
// hang, every later poll returned the zero value and started nothing: fast,
// silent and confidently wrong, forever. The deadline must retire it and let the
// next poll try again.
func TestHostProxy_HangingSweepIsAbandonedAndRetried(t *testing.T) {
	realDeadline := setHostProxySweepDeadline(50 * time.Millisecond)
	t.Cleanup(func() { setHostProxySweepDeadline(realDeadline) })

	calls, gate := blockingHostProxyDetect(t)
	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))

	start := time.Now()
	if w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("first poll: code = %d", w.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("first poll took %s against a sweep that never returns — the handler is waiting on it", elapsed)
	}
	waitHostProxyCalls(t, calls, 1, "the first poll must start a sweep")

	// Past the deadline the sweep is abandoned, so a later poll gets its own.
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, "")
		time.Sleep(10 * time.Millisecond)
	}
	if n := calls.Load(); n < 2 {
		t.Errorf("after the deadline the detector had been called %d time(s), want >= 2 — a hung sweep still holds the memo blind forever", n)
	}
	close(gate)
}

// TestHostProxy_SweepPanicIsContainedAndReleasesTheFlag: the sweep is a DETACHED
// goroutine now, so an unrecovered panic under it takes wardynd with it — where
// before the fix the very same panic ran on the request goroutine and net/http
// recovered it. DetectHostProxy parses whatever a host .exe printed, which is
// exactly the shape sshGo's and the run watcher's recovers exist for. And the
// release must be on the panic path too, or one panic strands the flag and the
// memo is blind for the life of the process.
func TestHostProxy_SweepPanicIsContainedAndReleasesTheFlag(t *testing.T) {
	var calls atomic.Int64
	real := hostProxyDetect
	hostProxyDetect = func() setup.HostProxyDetection {
		if calls.Add(1) == 1 {
			panic("host proxy detector blew up")
		}
		return setup.HostProxyDetection{}
	}
	t.Cleanup(func() {
		hostProxyDetect = real
		hostProxyCacheReset()
	})
	hostProxyCacheReset()

	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))
	if w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("poll over a panicking sweep: code = %d", w.Code)
	}
	waitHostProxyCalls(t, &calls, 1, "the first poll must start a sweep")

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, "")
		time.Sleep(10 * time.Millisecond)
	}
	if n := calls.Load(); n < 2 {
		t.Errorf("after a contained panic the detector had been called %d time(s), want >= 2 — the in-flight flag was stranded", n)
	}
}

// TestHostProxy_SeededInstallResolvesOnTheFirstRead pins the honesty escape
// hatch. A compose/`make setup` install carries its host reading in
// WARDYN_HOST_PROXY_B64 and DetectHostProxy decodes it in-process with no exec
// at all — so there is nothing to be asynchronous ABOUT, and answering the first
// poll with an empty detection would hand the operator hostProxyCheck's
// CONFIDENT "nothing is there" copy (blind is false on a seeded install) for a
// host whose proxy this process already knows.
func TestHostProxy_SeededInstallResolvesOnTheFirstRead(t *testing.T) {
	seed := setup.HostProxyDetection{
		HTTPProxy: &setup.HostProxySetting{Value: "http://proxy.corp.example:3128", Source: setup.ProxySourceOS},
	}
	blob, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARDYN_HOST_PROXY_B64", base64.StdEncoding.EncodeToString(blob))
	if !setup.HostProxySeeded() {
		t.Fatal("test setup: the seed env did not take")
	}
	hostProxyCacheReset()
	t.Cleanup(hostProxyCacheReset)

	got := cachedHostProxy()
	if got.HTTPProxy == nil || got.HTTPProxy.Value != seed.HTTPProxy.Value {
		t.Fatalf("first read of a SEEDED install = %+v, want the seeded proxy resolved in line", got.HTTPProxy)
	}
}

// TestSetupStatus_RecheckForcesAReDetectForOperatorsOnly answers the question the
// review asked outright: does Re-check re-check? It is a client-side refetch
// unless the server is told, so ?recheck=1 is the telling — and it is
// operator-only, because a member must not be able to make the daemon sweep the
// host on demand.
func TestSetupStatus_RecheckForcesAReDetectForOperatorsOnly(t *testing.T) {
	var calls atomic.Int64
	real := hostProxyDetect
	hostProxyDetect = func() setup.HostProxyDetection {
		calls.Add(1)
		return setup.HostProxyDetection{}
	}
	t.Cleanup(func() {
		hostProxyDetect = real
		hostProxyCacheReset()
	})
	hostProxyCacheReset()

	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))
	do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, "")
	waitHostProxyMemo(t)
	if n := calls.Load(); n != 1 {
		t.Fatalf("warm-up left calls = %d, want 1", n)
	}

	// A MEMBER's recheck is ignored: inside the TTL a plain poll would not sweep
	// either, so a second call here proves the param, not the clock.
	memberReq := httptest.NewRequest(http.MethodGet, "/api/v1/setup/status?recheck=1", nil)
	ctx := withOIDCRole(withOIDCHuman(memberReq.Context(), "sub-bob"), oidc.RoleMember)
	srv.handleSetupStatus(httptest.NewRecorder(), memberReq.WithContext(ctx))
	if n := calls.Load(); n != 1 {
		t.Errorf("a member's ?recheck=1 swept the host (calls = %d, want 1)", n)
	}

	do(t, srv, http.MethodGet, "/api/v1/setup/status?recheck=1", adminToken, "")
	waitHostProxyCalls(t, &calls, 2, "an operator's ?recheck=1 must force a re-detect")
}

// hostProxyValue reads the http_proxy value a /setup/status response carries,
// so a test can tell the FRESH answer from the last-known one.
func hostProxyValue(t *testing.T, body string) string {
	t.Helper()
	var st struct {
		HostProxy struct {
			HTTPProxy *struct {
				Value string `json:"value"`
			} `json:"http_proxy"`
		} `json:"host_proxy"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("decode setup status: %v", err)
	}
	if st.HostProxy.HTTPProxy == nil {
		return ""
	}
	return st.HostProxy.HTTPProxy.Value
}

func hostProxyDetection(value string) setup.HostProxyDetection {
	return setup.HostProxyDetection{HTTPProxy: &setup.HostProxySetting{Value: value}}
}

// TestSetupStatus_RecheckAnswersWithTheFreshValue (V1-r2 lens-U2 U2-02).
//
// Re-check forced a re-detect and then answered from the memo it had just
// invalidated, so the console's single forced read was always ONE PRESS BEHIND
// — and stamped "checked just now" over the old value. The FORCED path (only)
// waits a bounded moment for the sweep it started.
func TestSetupStatus_RecheckAnswersWithTheFreshValue(t *testing.T) {
	var calls atomic.Int64
	real := hostProxyDetect
	hostProxyDetect = func() setup.HostProxyDetection {
		if calls.Add(1) == 1 {
			return hostProxyDetection("http://old.proxy:3128")
		}
		time.Sleep(300 * time.Millisecond)
		return hostProxyDetection("http://new.proxy:3128")
	}
	t.Cleanup(func() {
		hostProxyDetect = real
		hostProxyCacheReset()
	})
	hostProxyCacheReset()

	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))
	do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, "")
	waitHostProxyMemo(t)

	w := do(t, srv, http.MethodGet, "/api/v1/setup/status?recheck=1", adminToken, "")
	if got := hostProxyValue(t, w.Body.String()); got != "http://new.proxy:3128" {
		t.Errorf("Re-check answered %q, want the value the re-detect it forced found — the button is one press behind", got)
	}
}

// TestSetupStatus_RecheckStillAnswersWhenTheSweepIsSlow: the bounded half of the
// same wait. A host whose interop is wedged must not hold the console's Re-check
// open — past the bound it answers with last-known, exactly as an unforced poll
// does.
func TestSetupStatus_RecheckStillAnswersWhenTheSweepIsSlow(t *testing.T) {
	var calls atomic.Int64
	gate := make(chan struct{})
	real := hostProxyDetect
	hostProxyDetect = func() setup.HostProxyDetection {
		if calls.Add(1) == 1 {
			return hostProxyDetection("http://old.proxy:3128")
		}
		<-gate
		return hostProxyDetection("http://new.proxy:3128")
	}
	t.Cleanup(func() {
		close(gate)
		hostProxyDetect = real
		hostProxyCacheReset()
	})
	hostProxyCacheReset()

	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))
	do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, "")
	waitHostProxyMemo(t)

	start := time.Now()
	w := do(t, srv, http.MethodGet, "/api/v1/setup/status?recheck=1", adminToken, "")
	elapsed := time.Since(start)
	if elapsed > hostProxyRecheckWait+2*time.Second {
		t.Errorf("Re-check took %s against a sweep that never answers, want ~%s", elapsed, hostProxyRecheckWait)
	}
	if got := hostProxyValue(t, w.Body.String()); got != "http://old.proxy:3128" {
		t.Errorf("Re-check answered %q, want the last-known value rather than an empty detection", got)
	}
}

// TestSetupStatus_RecheckIsBoundedToOnePerSweepDeadline (V1-r2 lens-S2 S2-07).
//
// The forced re-detect yields single-flight on purpose — the case Re-check
// exists for is a WEDGED sweep — but nothing bounded it in aggregate, so N
// presses inside one deadline started N overlapping sweeps, each with up to two
// host subprocesses. One forced re-detect per hostProxySweepDeadline is enough
// to unstick a wedged sweep and is a bound.
func TestSetupStatus_RecheckIsBoundedToOnePerSweepDeadline(t *testing.T) {
	var calls atomic.Int64
	real := hostProxyDetect
	hostProxyDetect = func() setup.HostProxyDetection {
		calls.Add(1)
		return setup.HostProxyDetection{}
	}
	t.Cleanup(func() {
		hostProxyDetect = real
		hostProxyCacheReset()
	})
	hostProxyCacheReset()

	srv := New(baseTestConfig(newHarness(t), &setupStatusCostStore{}))
	do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, "")
	waitHostProxyMemo(t)
	if n := calls.Load(); n != 1 {
		t.Fatalf("warm-up left calls = %d, want 1", n)
	}

	for i := 0; i < 5; i++ {
		do(t, srv, http.MethodGet, "/api/v1/setup/status?recheck=1", adminToken, "")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("5 Re-check presses inside one %s deadline started %d sweeps, want 1 (calls = %d)",
			hostProxySweepDeadline, n-1, n)
	}
}
