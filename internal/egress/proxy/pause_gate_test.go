// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
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

// pgState is the gate's state read under its lock and without expiring it, so a test can tell whether the
// watcher, not the read, moved it.
func pgState(g *lapseGate) lapseState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// pgTunnel is a tracked client connection and the peer end a test reads to see whether it was closed.
func pgTunnel(t *testing.T) (client, peer net.Conn) {
	t.Helper()
	client, peer = net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	return client, peer
}

// pgClosed reports whether the client end of a pgTunnel was closed.
func pgClosed(peer net.Conn) bool {
	_ = peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := peer.Read(make([]byte, 1))
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)
}

// pgRequest sends a plain proxy request through p and returns the recorded answer.
func pgRequest(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://example.com/"))
	return rec
}

// pgPausable is an open gate that pauses at its allowance of 30 minutes.
func pgPausable(clock bootClock) *lapseGate {
	return newLapseGate(clock, 30*time.Minute, gateOpts{pausable: true, startOpen: true})
}

// TestPauseGate_PausesAtAllowanceByWatcher: with no request at all, the watcher moves an open gate to paused within
// a tick of its allowance, and not before; the pause is not a lapse, and the next request is refused as retryable.
func TestPauseGate_PausesAtAllowanceByWatcher(t *testing.T) {
	clock := &fakeBoot{}
	g := pgPausable(clock)
	paused := make(chan struct{}, 4)
	g.onPause = func() { paused <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go g.watch(ctx, time.Millisecond)

	clock.set(30*time.Minute - time.Nanosecond)
	select {
	case <-paused:
		t.Fatal("the gate paused before its allowance")
	case <-time.After(100 * time.Millisecond):
	}
	if pgState(g) != lapseOpen {
		t.Fatalf("state %v before the allowance, want open", pgState(g))
	}
	clock.set(30 * time.Minute)
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not pause an idle gate at its allowance")
	}
	if pgState(g) != lapsePaused {
		t.Fatalf("state %v after the allowance, want paused", pgState(g))
	}
	select {
	case <-g.Done():
		t.Fatal("a pause lapsed the gate")
	case <-g.exited():
		t.Fatal("a pause signalled the process to end")
	default:
	}

	p, _ := newTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, "", nil, nil)
	p.lapse = g
	rec := pgRequest(t, p)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "60" || !strings.Contains(rec.Body.String(), pausedBody) {
		t.Fatalf("paused request: %d, Retry-After %q, body %q", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if len(paused) != 0 {
		t.Fatalf("onPause ran %d more times than the one pause", len(paused))
	}
}

// TestPauseGate_PauseClosesTrackedTunnels: a pause closes every tunnel the proxy registered with the gate, with no
// traffic on it, leaves one the proxy had already forgotten alone, and closes a tunnel registered while paused at
// once. The registration is the proxy's own hijack hook.
func TestPauseGate_PauseClosesTrackedTunnels(t *testing.T) {
	clock := &fakeBoot{}
	g := pgPausable(clock)
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, "", nil, nil)
	p.lapse = g
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go g.watch(ctx, time.Millisecond)

	liveClient, livePeer := pgTunnel(t)
	forgottenClient, forgottenPeer := pgTunnel(t)
	p.trackTunnel(liveClient)
	forget := p.trackTunnel(forgottenClient)
	forget()
	if pgClosed(livePeer) {
		t.Fatal("a tunnel was closed while the gate was open")
	}
	clock.set(30 * time.Minute)
	if !gateEventually(5*time.Second, func() bool { return pgState(g) == lapsePaused }) {
		t.Fatal("the gate never paused")
	}
	if !pgClosed(livePeer) {
		t.Fatal("a tracked tunnel survived the pause")
	}
	if pgClosed(forgottenPeer) {
		t.Fatal("a tunnel the proxy had forgotten was closed by the pause")
	}
	lateClient, latePeer := pgTunnel(t)
	p.trackTunnel(lateClient)
	if !pgClosed(latePeer) {
		t.Fatal("a tunnel registered on a paused gate was left open")
	}
}

// pgRecoveryPlane answers the recover door with a fresh token and the renew door with 503. It counts the recover asks
// and the renews that came before the first of them.
func pgRecoveryPlane(t *testing.T) (srv *httptest.Server, early, recovers *atomic.Int32) {
	t.Helper()
	early, recovers = new(atomic.Int32), new(atomic.Int32)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/internal/token/recover"):
			recovers.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "recovered", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case strings.HasSuffix(r.URL.Path, "/internal/token/renew"):
			if recovers.Load() == 0 {
				early.Add(1)
			}
			http.Error(w, "blip", http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, early, recovers
}

// TestPauseGate_RecoverReopens: a renew or recovery reopens a paused gate for one more allowance from its send
// time, applies its token, and moves the lapse bound with it; a tunnel registered after it stays open. A paused
// self-lapse loop goes straight to the recover door and reopens on its answer.
func TestPauseGate_RecoverReopens(t *testing.T) {
	t.Run("renewed", func(t *testing.T) {
		clock := &fakeBoot{}
		g := pgPausable(clock)
		clock.set(31 * time.Minute)
		if !g.paused() {
			t.Fatal("the gate did not pause past its allowance")
		}
		sent := clock.Now()
		clock.advance(time.Minute) // the answer lands a minute after the send
		applied := false
		if !g.renewed(sent, func() { applied = true }) || !applied {
			t.Fatalf("a recovery on a paused gate: applied=%v", applied)
		}
		if !g.serving() || g.paused() {
			t.Fatal("the gate did not reopen")
		}
		if g.deadline != sent+30*time.Minute || g.hardEnd != sent+recoverHardEnd {
			t.Fatalf("deadline %v hardEnd %v, want %v and %v from the send time", g.deadline, g.hardEnd, sent+30*time.Minute, sent+recoverHardEnd)
		}
		client, peer := pgTunnel(t)
		g.track(client)
		clock.set(sent + 29*time.Minute)
		if !g.serving() || pgClosed(peer) {
			t.Fatal("the reopened gate closed a tunnel inside its new allowance")
		}
		clock.set(sent + 30*time.Minute)
		if !g.paused() || !pgClosed(peer) {
			t.Fatal("the reopened gate did not pause again at its new allowance")
		}
	})

	t.Run("by the self-lapse loop", func(t *testing.T) {
		srv, early, recovers := pgRecoveryPlane(t)
		clock := &fakeBoot{}
		g := newLapseGate(clock, 30*time.Minute, gateOpts{pausable: true, exitOnLapse: true, startOpen: true})
		clock.set(30 * time.Minute)
		if !g.paused() {
			t.Fatal("the gate did not pause")
		}
		ts := newTokenSource("seed")
		tune := lapseRenewTuning{success: 20 * time.Millisecond, failure: 5 * time.Millisecond, recover: 5 * time.Millisecond,
			watchEvery: time.Millisecond, jitter: func(time.Duration) time.Duration { return 0 }}
		ctx, cancel := context.WithCancel(t.Context())
		ended := make(chan struct{})
		go func() {
			runSelfLapseRenewer(ctx, ts, srv.URL, srv.Client(), g, tune)
			close(ended)
		}()
		defer func() { cancel(); <-ended }()
		if !gateEventually(10*time.Second, func() bool { return ts.Get() == "recovered" && g.serving() }) {
			t.Fatalf("the paused gate did not reopen: token %q state %v", ts.Get(), pgState(g))
		}
		if recovers.Load() == 0 || early.Load() != 0 {
			t.Fatalf("%d recover asks and %d renews before the first, want the recover door first", recovers.Load(), early.Load())
		}
		if g.deadline != 60*time.Minute {
			t.Fatalf("deadline %v, want the allowance from the recovery's send time (60m)", g.deadline)
		}
	})
}

// TestPauseGate_StrictLapses: a gate that is not pausable (strict terms) lapses at its allowance instead: the
// refusal is the closed one, with no Retry-After, its tunnels are closed, it never pauses, and it never reopens.
func TestPauseGate_StrictLapses(t *testing.T) {
	clock := &fakeBoot{}
	g := newLapseGate(clock, 30*time.Minute, gateOpts{startOpen: true})
	g.onPause = func() { t.Error("a strict gate paused") }
	client, peer := pgTunnel(t)
	g.track(client)
	clock.set(30*time.Minute - time.Nanosecond)
	if !g.serving() || pgClosed(peer) {
		t.Fatal("the strict gate closed before its allowance")
	}
	clock.set(30 * time.Minute)
	if g.serving() || g.paused() || pgState(g) != lapseLapsed || g.reason != LapseDeadline {
		t.Fatalf("state %v reason %q, want lapsed as %q", pgState(g), g.reason, LapseDeadline)
	}
	select {
	case <-g.Done():
	default:
		t.Fatal("the lapse did not close Done")
	}
	if !pgClosed(peer) {
		t.Fatal("a strict lapse left a tunnel open")
	}
	rec := httptest.NewRecorder()
	g.refuse(rec)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "" || strings.Contains(rec.Body.String(), "paused") {
		t.Fatalf("a lapsed gate answered %d, Retry-After %q, %q; want the closed refusal", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if g.renewed(clock.Now(), func() { t.Error("a lapsed gate applied a renewal") }) {
		t.Fatal("a recovery reopened a lapsed gate")
	}
}

// TestPauseGate_HardEndLapses: a paused gate lapses at its hard end, the last good renew's send time + the token's
// life + 24 h + 5 min, however it is reached; a recovery moves that bound with it.
func TestPauseGate_HardEndLapses(t *testing.T) {
	t.Run("paused, then the hard end", func(t *testing.T) {
		clock := &fakeBoot{}
		g := pgPausable(clock)
		clock.set(30 * time.Minute)
		if !g.paused() {
			t.Fatal("the gate did not pause")
		}
		clock.set(recoverHardEnd - time.Nanosecond)
		if !g.paused() {
			t.Fatalf("state %v just before the hard end, want paused", pgState(g))
		}
		clock.set(recoverHardEnd)
		if g.paused() || g.serving() || pgState(g) != lapseLapsed || g.reason != LapseDeadline {
			t.Fatalf("state %v reason %q at the hard end, want lapsed as %q", pgState(g), g.reason, LapseDeadline)
		}
		select {
		case <-g.Done():
		default:
			t.Fatal("the hard end did not close Done")
		}
	})

	t.Run("a suspend past the hard end", func(t *testing.T) {
		clock := &fakeBoot{}
		g := pgPausable(clock)
		pauses := 0
		g.onPause = func() { pauses++ }
		client, peer := pgTunnel(t)
		g.track(client)
		clock.set(recoverHardEnd + time.Hour)
		if g.serving() || pgState(g) != lapseLapsed || g.reason != LapseDeadline {
			t.Fatalf("state %v reason %q, want lapsed as %q", pgState(g), g.reason, LapseDeadline)
		}
		if pauses != 1 || !pgClosed(peer) {
			t.Fatalf("pauses %d, tunnel closed %v: the jump should pause, then lapse, and close the tunnel", pauses, pgClosed(peer))
		}
	})

	t.Run("a recovery moves the hard end", func(t *testing.T) {
		clock := &fakeBoot{}
		g := pgPausable(clock)
		clock.set(30 * time.Minute)
		if !g.paused() {
			t.Fatal("the gate did not pause")
		}
		if !g.renewed(clock.Now(), func() {}) {
			t.Fatal("recovery refused")
		}
		clock.set(recoverHardEnd + time.Minute) // past the first hard end, inside the one the recovery set
		if !g.paused() || pgState(g) == lapseLapsed {
			t.Fatalf("state %v: the recovery did not move the hard end", pgState(g))
		}
		clock.set(30*time.Minute + recoverHardEnd)
		if g.paused() || pgState(g) != lapseLapsed {
			t.Fatalf("state %v at the moved hard end, want lapsed", pgState(g))
		}
	})
}

// pgServer builds a started server (no listener) against cp on clock, as cmd/wardyn-proxy does, with the self-lapse
// renewer at millisecond cadence. mutate edits the config first.
func pgServer(t *testing.T, cp string, clock *fakeBoot, mutate func(*Config)) *Server {
	t.Helper()
	cfg := &Config{RunID: uuid.New(), RunToken: "seed", Listen: "127.0.0.1:0", DecisionBufferSize: defaultBufferSize,
		ControlPlaneURL: cp, Policy: types.RunPolicySpec{AllowAllEgress: true}}
	if mutate != nil {
		mutate(cfg)
	}
	tune := lapseRenewTuning{success: 20 * time.Millisecond, failure: 5 * time.Millisecond, recover: 5 * time.Millisecond,
		watchEvery: time.Millisecond, jitter: func(time.Duration) time.Duration { return 0 }}
	srv, err := newServer(context.Background(), cfg, http.DefaultClient, io.Discard, serverHooks{lapseClock: clock, lapseTune: &tune})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

// pgKubernetes makes cfg a k8s runner's proxy: self-lapsing, with a decision sink nothing answers.
func pgKubernetes(cfg *Config) {
	cfg.SelfLapse = true
	cfg.DecisionSink = &DecisionSinkConfig{URL: "tls://127.0.0.1:1", SPKISHA256: strings.Repeat("00", 32), Key: strings.Repeat("00", 32)}
}

// pgTerms sets the run's outage terms.
func pgTerms(allowanceS int64, strict bool) func(*Config) {
	return func(cfg *Config) { cfg.Outage = &OutageConfig{AllowanceS: allowanceS, Strict: strict} }
}

// pgExited reports whether the server's process-end signal has fired.
func pgExited(s *Server) bool {
	select {
	case <-s.Lapsed():
		return true
	default:
		return false
	}
}

// TestServerGate_KubernetesRunnerUsesSelfLapseLoop: a k8s runner's proxy starts closed, on the self-lapse loop (it
// retries at the loop's own cadence, where the gated loop retries once a second), pauses at its allowance with no
// frame and no process end, and ends its process on a lapse even after serving. The gate takes the run's terms.
func TestServerGate_KubernetesRunnerUsesSelfLapseLoop(t *testing.T) {
	rp := newRenewPlane(t, http.StatusServiceUnavailable, false)
	clock := &fakeBoot{}
	srv := pgServer(t, rp.srv.URL, clock, pgKubernetes)
	g := srv.proxy.lapse
	if g == nil || pgState(g) != lapseClosed || !g.pausable || !g.exitOnLapse || g.span != time.Hour {
		t.Fatalf("gate (state %v pausable %v exitOnLapse %v span %v), want closed, pausable, ending the process, an hour", pgState(g), g.pausable, g.exitOnLapse, g.span)
	}
	if !gateEventually(3*time.Second, func() bool { return rp.renews.Load() >= 8 }) {
		t.Fatalf("%d renews in 3 s: the gated loop's once-a-second retry, not the self-lapse loop's", rp.renews.Load())
	}
	rp.status.Store(http.StatusOK)
	if !gateEventually(5*time.Second, g.serving) {
		t.Fatal("the gate never opened on a renewal")
	}
	rp.status.Store(http.StatusServiceUnavailable)
	before := rp.renews.Load()
	if !gateEventually(5*time.Second, func() bool { return rp.renews.Load() >= before+2 }) {
		t.Fatal("the loop stopped renewing")
	}
	clock.advance(time.Hour)
	if !gateEventually(5*time.Second, func() bool { return pgState(g) == lapsePaused }) {
		t.Fatalf("state %v past the allowance, want paused", pgState(g))
	}
	if pgExited(srv) || g.reason != "" {
		t.Fatalf("a pause ended the process (%v) or lapsed the gate (%q)", pgExited(srv), g.reason)
	}
	g.lapse(LapseRenewRefused)
	if !pgExited(srv) {
		t.Fatal("a lapse after serving did not end a k8s runner's process")
	}

	t.Run("terms", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			mutate   func(*Config)
			span     time.Duration
			pausable bool
		}{
			{"default", nil, time.Hour, true},
			{"an allowance", pgTerms(7200, false), 2 * time.Hour, true},
			{"strict", pgTerms(7200, true), 2 * time.Hour, false},
		} {
			cfg := func(c *Config) {
				pgKubernetes(c)
				if tc.mutate != nil {
					tc.mutate(c)
				}
			}
			g := pgServer(t, rp.srv.URL, &fakeBoot{}, cfg).proxy.lapse
			if g.span != tc.span || g.pausable != tc.pausable || !g.exitOnLapse {
				t.Errorf("%s: span %v pausable %v exitOnLapse %v, want %v, %v, true", tc.name, g.span, g.pausable, g.exitOnLapse, tc.span, tc.pausable)
			}
		}
	})
}

// TestServerGate_ReplacementStartsClosed: a proxy the runner started as a replacement serves nothing until its
// first renew under its own token has landed; it does not end the process once it has served, pauses at its
// allowance by the watcher with no traffic, and closes its tunnels then.
func TestServerGate_ReplacementStartsClosed(t *testing.T) {
	t.Setenv(FirstRenewGateEnv, "1")
	rp := newRenewPlane(t, http.StatusOK, true)
	clock := &fakeBoot{}
	srv := pgServer(t, rp.srv.URL, clock, nil)
	g := srv.proxy.lapse
	if g == nil || pgState(g) != lapseClosed || !g.isClosed() || g.serving() || !g.pausable || g.exitOnLapse || g.span != time.Hour {
		t.Fatalf("gate (pausable %v exitOnLapse %v span %v), want closed, pausable, not ending the process, an hour", g.pausable, g.exitOnLapse, g.span)
	}
	if !gateEventually(5*time.Second, func() bool { return rp.renews.Load() > 0 }) {
		t.Fatal("the proxy never asked for its first renewal")
	}
	if pgState(g) != lapseClosed || pgExited(srv) {
		t.Fatalf("state %v exited %v while the first renewal was unanswered", pgState(g), pgExited(srv))
	}
	close(rp.release)
	if !gateEventually(5*time.Second, g.serving) {
		t.Fatal("the proxy did not open on its first renewal")
	}
	client, peer := pgTunnel(t)
	srv.proxy.trackTunnel(client)
	rp.status.Store(http.StatusServiceUnavailable)
	clock.advance(time.Hour)
	if !gateEventually(5*time.Second, func() bool { return pgState(g) == lapsePaused }) {
		t.Fatalf("state %v past the allowance, want the watcher to have paused it", pgState(g))
	}
	if !pgClosed(peer) {
		t.Fatal("the pause left a tracked tunnel open")
	}
	if pgExited(srv) {
		t.Fatal("a pause ended a replacement proxy's process")
	}
}

// TestServerGate_PlainStartsOpen: any other proxy with a control plane serves from its start, pauses at its
// allowance by the watcher, and takes the run's terms; strict terms make its gate lapse instead of pause.
func TestServerGate_PlainStartsOpen(t *testing.T) {
	t.Setenv(FirstRenewGateEnv, "")
	for _, tc := range []struct {
		name     string
		mutate   func(*Config)
		span     time.Duration
		pausable bool
	}{
		{"default terms", nil, time.Hour, true},
		{"an allowance", pgTerms(7200, false), 2 * time.Hour, true},
		{"strict", pgTerms(7200, true), 2 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rp := newRenewPlane(t, http.StatusServiceUnavailable, false)
			clock := &fakeBoot{}
			srv := pgServer(t, rp.srv.URL, clock, tc.mutate)
			g := srv.proxy.lapse
			if g == nil || pgState(g) != lapseOpen || g.isClosed() || !g.serving() || g.pausable != tc.pausable || g.exitOnLapse || g.span != tc.span {
				t.Fatalf("gate (pausable %v exitOnLapse %v span %v), want open from the start, pausable %v, not ending the process, %v", g.pausable, g.exitOnLapse, g.span, tc.pausable, tc.span)
			}
			select {
			case <-g.open:
			default:
				t.Fatal("the gate's open signal did not fire at start")
			}
			clock.advance(tc.span - time.Second)
			if gateEventually(100*time.Millisecond, func() bool { return pgState(g) != lapseOpen }) {
				t.Fatalf("state %v before the allowance", pgState(g))
			}
			clock.advance(time.Second)
			want := lapsePaused
			if !tc.pausable {
				want = lapseLapsed
			}
			if !gateEventually(5*time.Second, func() bool { return pgState(g) == want }) {
				t.Fatalf("state %v at the allowance with no request, want the watcher to have moved it to %v", pgState(g), want)
			}
			if pgExited(srv) {
				t.Fatal("the allowance ended a plain proxy's process")
			}
		})
	}
}

// TestLapsed_PlainProxyDoesNotExitAfterServing: Lapsed, the signal that the process must end, fires for a k8s
// runner's proxy on any lapse and for any proxy that lapses before its first renew; a proxy that lapses after
// serving and is not a k8s runner's refuses everything and stays up.
func TestLapsed_PlainProxyDoesNotExitAfterServing(t *testing.T) {
	strict := pgTerms(1800, true)
	// lapseAfterServing moves the clock past the strict allowance and waits for the gate to lapse. Whether a renewal
	// lands before or after the move, a strict gate past its allowance lapses.
	lapseAfterServing := func(t *testing.T, clock *fakeBoot, srv *Server) {
		t.Helper()
		g := srv.proxy.lapse
		clock.advance(30 * time.Minute)
		select {
		case <-g.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the strict gate did not lapse at its allowance")
		}
		if g.reason != LapseDeadline {
			t.Fatalf("reason %q, want %q", g.reason, LapseDeadline)
		}
		if rec := pgRequest(t, srv.proxy); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("a lapsed proxy answered %d, want 503", rec.Code)
		}
	}

	t.Run("plain proxy", func(t *testing.T) {
		t.Setenv(FirstRenewGateEnv, "")
		rp := newRenewPlane(t, http.StatusOK, false)
		clock := &fakeBoot{}
		srv := pgServer(t, rp.srv.URL, clock, strict)
		lapseAfterServing(t, clock, srv)
		if gateEventually(300*time.Millisecond, func() bool { return pgExited(srv) }) {
			t.Fatal("a plain proxy that lapsed after serving ended its process")
		}
	})

	t.Run("replacement after serving", func(t *testing.T) {
		t.Setenv(FirstRenewGateEnv, "1")
		rp := newRenewPlane(t, http.StatusOK, false)
		clock := &fakeBoot{}
		srv := pgServer(t, rp.srv.URL, clock, strict)
		if !gateEventually(5*time.Second, srv.proxy.lapse.serving) {
			t.Fatal("the replacement never opened")
		}
		lapseAfterServing(t, clock, srv)
		if gateEventually(300*time.Millisecond, func() bool { return pgExited(srv) }) {
			t.Fatal("a replacement that lapsed after serving ended its process")
		}
	})

	t.Run("replacement before its first renew", func(t *testing.T) {
		t.Setenv(FirstRenewGateEnv, "1")
		rp := newRenewPlane(t, http.StatusForbidden, false)
		srv := pgServer(t, rp.srv.URL, &fakeBoot{}, strict)
		select {
		case <-srv.Lapsed():
		case <-time.After(5 * time.Second):
			t.Fatal("a replacement refused before it ever served did not end its process")
		}
		if g := srv.proxy.lapse; g.reason != LapseRenewRefused || g.serving() {
			t.Fatalf("reason %q serving %v", g.reason, g.serving())
		}
	})

	t.Run("k8s runner after serving", func(t *testing.T) {
		rp := newRenewPlane(t, http.StatusOK, false)
		clock := &fakeBoot{}
		srv := pgServer(t, rp.srv.URL, clock, func(cfg *Config) { pgKubernetes(cfg); strict(cfg) })
		if !gateEventually(5*time.Second, srv.proxy.lapse.serving) {
			t.Fatal("the k8s runner's proxy never opened")
		}
		lapseAfterServing(t, clock, srv)
		select {
		case <-srv.Lapsed():
		case <-time.After(5 * time.Second):
			t.Fatal("a k8s runner's proxy that lapsed after serving did not end its process")
		}
	})
}
