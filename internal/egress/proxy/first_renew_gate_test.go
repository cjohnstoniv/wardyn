// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// renewPlane is a control plane whose token renewal answers what the test says; every other path
// answers 204, so the sidecar's other control-plane calls neither fail nor matter.
type renewPlane struct {
	srv       *httptest.Server
	renews    atomic.Int32
	hangFirst atomic.Bool  // the first renewal request never answers
	status    atomic.Int32 // the renewal's status
	release   chan struct{}
}

func newRenewPlane(t *testing.T, status int, hold bool) *renewPlane {
	t.Helper()
	rp := &renewPlane{release: make(chan struct{})}
	rp.status.Store(int32(status))
	rp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/internal/token/renew") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if rp.renews.Add(1) == 1 && rp.hangFirst.Load() {
			<-r.Context().Done()
			return
		}
		if hold {
			select {
			case <-rp.release:
			case <-r.Context().Done():
				return
			}
		}
		switch st := int(rp.status.Load()); st {
		case http.StatusOK:
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "fresh", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case http.StatusServiceUnavailable:
			http.Error(w, "token_lineage_retired: retired by a revive; retry", st)
		default:
			http.Error(w, "refused", st)
		}
	}))
	t.Cleanup(rp.srv.Close)
	return rp
}

// freeAddr is a loopback address nothing listens on yet.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func isListening(addr string, within time.Duration) bool {
	for end := time.Now().Add(within); ; time.Sleep(10 * time.Millisecond) {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

type sidecar struct {
	srv       *Server
	addr      string
	clock     *fakeBoot
	listening atomic.Bool // OnListening ran
}

// startSidecar builds a server-mode sidecar (no self_lapse) against cp, as cmd/wardyn-proxy does,
// and runs ListenAndServe. flag sets FirstRenewGateEnv, as a runner does for a replacement proxy.
func startSidecar(t *testing.T, cp string, controlPlane, flag bool) *sidecar {
	t.Helper()
	if flag {
		t.Setenv(FirstRenewGateEnv, "1")
	} else {
		t.Setenv(FirstRenewGateEnv, "")
	}
	sc := &sidecar{addr: freeAddr(t), clock: &fakeBoot{}}
	cfg := &Config{RunID: uuid.New(), RunToken: "seed", Listen: sc.addr, DecisionBufferSize: defaultBufferSize,
		Policy: types.RunPolicySpec{AllowAllEgress: true}}
	if controlPlane {
		cfg.ControlPlaneURL = cp
	}
	srv, err := newServer(context.Background(), cfg, http.DefaultClient, io.Discard, serverHooks{lapseClock: sc.clock})
	if err != nil {
		t.Fatal(err)
	}
	sc.srv = srv
	srv.OnListening(func() { sc.listening.Store(true) })
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return sc
}

func gateEventually(d time.Duration, cond func() bool) bool {
	for end := time.Now().Add(d); ; time.Sleep(5 * time.Millisecond) {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// TestSidecar_ClosedUntilFirstRenew: a server-mode sidecar with a control plane that the runner started as
// a replacement (FirstRenewGateEnv, set by withFirstRenewGate) binds its port and runs OnListening only
// once its first renewal has answered 200.
func TestSidecar_ClosedUntilFirstRenew(t *testing.T) {
	rp := newRenewPlane(t, http.StatusOK, true)
	sc := startSidecar(t, rp.srv.URL, true, true)
	if !gateEventually(5*time.Second, func() bool { return rp.renews.Load() > 0 }) {
		t.Fatal("the sidecar never asked for its first renewal")
	}
	if isListening(sc.addr, 300*time.Millisecond) || sc.listening.Load() {
		t.Fatal("the sidecar bound its port before its first renewal answered")
	}
	close(rp.release)
	if !isListening(sc.addr, 5*time.Second) || !gateEventually(5*time.Second, sc.listening.Load) {
		t.Fatal("the sidecar never served after its first renewal landed")
	}
	if !sc.srv.proxy.lapse.serving() {
		t.Fatal("the gate is not open after the first renewal")
	}
}

// TestSidecar_RetiredOrRevokedTokenNeverServes: a renewal refused with the retired-lineage 503, a
// 401 or a 403 never opens the sidecar: it never binds, never reports ready, and its gate lapses.
// On a tuned renewer, the lapse waits for firstRenewWithin on the clock.
func TestSidecar_RetiredOrRevokedTokenNeverServes(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rp := newRenewPlane(t, status, false)
			sc := startSidecar(t, rp.srv.URL, true, true)
			g := sc.srv.proxy.lapse
			if status != http.StatusForbidden {
				// A refusal that may pass keeps being retried until firstRenewWithin.
				if !gateEventually(5*time.Second, func() bool { return rp.renews.Load() >= 1 }) {
					t.Fatal("the sidecar never asked for its renewal")
				}
				sc.clock.set(firstRenewWithin)
			}
			select {
			case <-sc.srv.Lapsed():
			case <-time.After(10 * time.Second):
				t.Fatal("the gate never lapsed")
			}
			want := LapseDeadline
			if status == http.StatusForbidden {
				want = LapseRenewRefused
			}
			if g.reason != want || g.serving() {
				t.Fatalf("reason %q serving %v; want %q, closed", g.reason, g.serving(), want)
			}
			if isListening(sc.addr, 300*time.Millisecond) || sc.listening.Load() {
				t.Fatal("a sidecar whose renewal was refused bound its port")
			}
		})
	}

	t.Run("lapses at firstRenewWithin", func(t *testing.T) {
		rp := newRenewPlane(t, http.StatusServiceUnavailable, false)
		clock := &fakeBoot{}
		g := newLapseGate(clock, 1<<62)
		tune := renewerTuning{firstRetry: 5 * time.Millisecond, maxInterval: 20 * time.Millisecond, giveUpAfter: time.Hour,
			expiryMargin: time.Minute, closedRetry: 5 * time.Millisecond, now: time.Now}
		go runTokenRenewerGated(t.Context(), newTokenSource("seed"), rp.srv.URL, http.DefaultClient, tune, g)
		if !gateEventually(5*time.Second, func() bool { return rp.renews.Load() >= 3 }) {
			t.Fatal("the renewer did not keep trying")
		}
		select {
		case <-g.Done():
			t.Fatal("the gate lapsed before firstRenewWithin")
		default:
		}
		clock.set(firstRenewWithin)
		select {
		case <-g.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the gate did not lapse at firstRenewWithin")
		}
		if g.serving() || g.reason != LapseDeadline || !strings.Contains(g.lastCause(), "token_lineage_retired") {
			t.Fatalf("serving %v reason %q cause %q", g.serving(), g.reason, g.lastCause())
		}
	})
}

// TestSidecar_OpenStaysOpenAfterLaterFailures: once the first renewal landed, no later failure and
// no passage of time closes the gate: a server-mode proxy has no deadline and keeps today's give-up.
func TestSidecar_OpenStaysOpenAfterLaterFailures(t *testing.T) {
	t.Run("through the server", func(t *testing.T) {
		rp := newRenewPlane(t, http.StatusOK, false)
		sc := startSidecar(t, rp.srv.URL, true, true)
		if !isListening(sc.addr, 5*time.Second) {
			t.Fatal("the sidecar never served")
		}
		rp.status.Store(http.StatusServiceUnavailable)
		sc.clock.advance(24 * time.Hour)
		if !sc.srv.proxy.lapse.serving() {
			t.Fatalf("the gate closed after a day (reason %q)", sc.srv.proxy.lapse.reason)
		}
		select {
		case <-sc.srv.Lapsed():
			t.Fatal("an open server-mode gate lapsed on time alone")
		default:
		}
	})

	t.Run("failures and a permanent refusal", func(t *testing.T) {
		for _, status := range []int{http.StatusServiceUnavailable, http.StatusForbidden} {
			rp := newRenewPlane(t, http.StatusOK, false)
			clock := &fakeBoot{}
			g := newLapseGate(clock, 1<<62)
			ts := newTokenSource("seed")
			tune := renewerTuning{firstRetry: 5 * time.Millisecond, maxInterval: 10 * time.Millisecond, giveUpAfter: time.Hour,
				expiryMargin: time.Minute, closedRetry: 5 * time.Millisecond, now: time.Now}
			ended := make(chan struct{})
			go func() {
				runTokenRenewerGated(t.Context(), ts, rp.srv.URL, http.DefaultClient, tune, g)
				close(ended)
			}()
			if !gateEventually(5*time.Second, g.serving) {
				t.Fatal("the gate never opened")
			}
			rp.status.Store(int32(status))
			before := rp.renews.Load()
			if !gateEventually(5*time.Second, func() bool {
				return rp.renews.Load() >= before+2 || status == http.StatusForbidden && rp.renews.Load() > before
			}) {
				t.Fatalf("status %d: no later renewal was tried", status)
			}
			clock.advance(24 * time.Hour)
			if !g.serving() {
				t.Fatalf("status %d closed an open gate (reason %q)", status, g.reason)
			}
			if status == http.StatusForbidden {
				select {
				case <-ended:
				case <-time.After(5 * time.Second):
					t.Fatal("the renewer kept running after a permanent refusal")
				}
			}
		}
	})
}

// TestViaOrgEngine_NeverGated: the via_org engine builds through newServer too and starts no
// renewer, so it must not carry a gate that nothing would ever open.
func TestViaOrgEngine_NeverGated(t *testing.T) {
	t.Setenv(FirstRenewGateEnv, "1")
	cfg := &Config{RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK", DecisionBufferSize: defaultBufferSize,
		Policy: types.RunPolicySpec{AllowAllEgress: true}}
	e, err := NewViaOrgEngine(context.Background(), cfg, ViaOrgEngineOptions{Client: &http.Client{Transport: &fakeOrg{}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	if e.srv.proxy.lapse != nil || e.srv.Lapsed() != nil {
		t.Fatal("the via_org engine carries a first-renew gate")
	}
}

// TestSidecar_NoControlPlaneNoGate: without a control plane there is no renewal to wait for, so the
// sidecar has no gate and binds at once.
func TestSidecar_NoControlPlaneNoGate(t *testing.T) {
	sc := startSidecar(t, "", false, true)
	if sc.srv.proxy.lapse != nil {
		t.Fatal("a sidecar with no control plane has a gate")
	}
	if !isListening(sc.addr, 5*time.Second) || !gateEventually(5*time.Second, sc.listening.Load) {
		t.Fatal("a sidecar with no control plane did not bind")
	}
}

// TestSidecar_OneRefusalThenAnswerOpensQuickly: a closed gate retries every second, not at the
// ordinary one-minute gap, so one refused renewal does not spend a replace's ten-second wait.
func TestSidecar_OneRefusalThenAnswerOpensQuickly(t *testing.T) {
	rp := newRenewPlane(t, http.StatusServiceUnavailable, false)
	sc := startSidecar(t, rp.srv.URL, true, true)
	if !gateEventually(5*time.Second, func() bool { return rp.renews.Load() >= 1 }) {
		t.Fatal("the sidecar never asked for its renewal")
	}
	if isListening(sc.addr, 100*time.Millisecond) {
		t.Fatal("the sidecar bound after a refusal")
	}
	rp.status.Store(http.StatusOK)
	if !isListening(sc.addr, 10*time.Second) || !gateEventually(time.Second, sc.listening.Load) {
		t.Fatal("the sidecar did not open within ten seconds of the renewal answering")
	}
}

// TestSidecar_FlagUnsetServesAtOnce: without FirstRenewGateEnv (a create, or an older runner) a
// sidecar binds before its renewal answers, as before.
func TestSidecar_FlagUnsetServesAtOnce(t *testing.T) {
	rp := newRenewPlane(t, http.StatusOK, true)
	sc := startSidecar(t, rp.srv.URL, true, false)
	if sc.srv.proxy.lapse != nil {
		t.Fatal("a sidecar without the flag has a gate")
	}
	if !isListening(sc.addr, 5*time.Second) || !gateEventually(5*time.Second, sc.listening.Load) {
		t.Fatal("a sidecar without the flag did not bind at once")
	}
	close(rp.release)
}

// TestSidecar_HungFirstRenewThenAnswerOpensQuickly: while the gate is closed a renewal request is
// cut off at about three seconds, so a first request that hangs still leaves room for a retry that
// opens the proxy within a replace's ten-second wait.
func TestSidecar_HungFirstRenewThenAnswerOpensQuickly(t *testing.T) {
	rp := newRenewPlane(t, http.StatusOK, false)
	rp.hangFirst.Store(true)
	sc := startSidecar(t, rp.srv.URL, true, true)
	if !isListening(sc.addr, 10*time.Second) || !gateEventually(time.Second, sc.listening.Load) {
		t.Fatalf("the sidecar did not open within ten seconds after a hung first renewal (%d tries)", rp.renews.Load())
	}
}
