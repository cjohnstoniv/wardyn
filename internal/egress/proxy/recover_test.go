// Copyright 2026 The Wardyn Authors
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
)

// rcDrop is a recover answer the plane never sends: it cuts the connection, which the proxy sees as a transport error.
const rcDrop = -1

// rcPlane is a control plane whose renew and recover doors answer what the test says. A renewal that succeeds returns
// a token expiring at renewExp; a recovery that succeeds returns "recovered", expiring in an hour.
type rcPlane struct {
	srv           *httptest.Server
	renews        atomic.Int32
	recovers      atomic.Int32
	renewStatus   atomic.Int32
	recoverStatus atomic.Int32 // rcDrop cuts the connection
	renewExp      atomic.Int64 // unix seconds
	mu            sync.Mutex
	recoverAt     []time.Time // when each recover ask arrived
}

func rcNewPlane(t *testing.T, renewStatus, recoverStatus int) *rcPlane {
	t.Helper()
	p := &rcPlane{}
	p.renewStatus.Store(int32(renewStatus))
	p.recoverStatus.Store(int32(recoverStatus))
	p.renewExp.Store(time.Now().Add(time.Hour).Unix())
	answer := func(w http.ResponseWriter, status int, token string, exp time.Time) {
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]string{"token": token, "expires_at": exp.UTC().Format(time.RFC3339)})
			return
		}
		http.Error(w, fmt.Sprintf("answered %d", status), status)
	}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/internal/token/renew"):
			p.renews.Add(1)
			answer(w, int(p.renewStatus.Load()), "renewed", time.Unix(p.renewExp.Load(), 0))
		case strings.HasSuffix(r.URL.Path, "/internal/token/recover"):
			p.recovers.Add(1)
			p.mu.Lock()
			p.recoverAt = append(p.recoverAt, time.Now())
			p.mu.Unlock()
			status := int(p.recoverStatus.Load())
			if status == rcDrop {
				panic(http.ErrAbortHandler)
			}
			answer(w, status, "recovered", time.Now().Add(time.Hour))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *rcPlane) recoverTimes() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.recoverAt...)
}

// rcLoop is one renew loop under test.
type rcLoop struct {
	g     *lapseGate
	ts    *tokenSource
	ended chan struct{} // closes when the loop returns
}

// rcStart runs a renewer against p until the test ends. kind "gated" is runTokenRenewerGated on a plain gate that
// serves from its start, with a renew that keeps answering 401 so the loop enters recover mode once the token has
// been held for 20 ms; kind "self-lapse" is runSelfLapseRenewer on a k8s runner's gate, whose first renew opens it
// with a token that has already expired, so the next try is a recovery. p's renew door answers accordingly.
func rcStart(t *testing.T, kind string, p *rcPlane) *rcLoop {
	t.Helper()
	l := &rcLoop{ts: newTokenSource("seed"), ended: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		select {
		case <-l.ended:
		case <-time.After(5 * time.Second):
			t.Errorf("%s: the renewer did not stop with its context", kind)
		}
	})
	switch kind {
	case "gated":
		p.renewStatus.Store(http.StatusUnauthorized)
		l.g = newLapseGate(&fakeBoot{}, testLapseTTL, gateOpts{pausable: true, startOpen: true})
		tune := renewerTuning{firstRetry: 5 * time.Millisecond, maxInterval: 10 * time.Millisecond, giveUpAfter: 20 * time.Millisecond,
			expiryMargin: time.Minute, closedRetry: 5 * time.Millisecond, recoverEvery: 5 * time.Millisecond, now: time.Now}
		go func() {
			runTokenRenewerGated(ctx, l.ts, p.srv.URL, p.srv.Client(), tune, l.g)
			close(l.ended)
		}()
	case "self-lapse":
		p.renewStatus.Store(http.StatusOK)
		p.renewExp.Store(time.Now().Add(-2 * time.Second).Unix())
		l.g = newLapseGate(&fakeBoot{}, testLapseTTL, gateOpts{pausable: true, exitOnLapse: true})
		tune := lapseRenewTuning{success: 5 * time.Millisecond, failure: 5 * time.Millisecond, recover: 5 * time.Millisecond,
			watchEvery: time.Millisecond, jitter: func(time.Duration) time.Duration { return 0 }}
		go func() {
			runSelfLapseRenewer(ctx, l.ts, p.srv.URL, p.srv.Client(), l.g, tune)
			close(l.ended)
		}()
	default:
		t.Fatalf("unknown loop %q", kind)
	}
	return l
}

func (l *rcLoop) lapsed() bool {
	select {
	case <-l.g.Done():
		return true
	default:
		return false
	}
}

func (l *rcLoop) stopped() bool {
	select {
	case <-l.ended:
		return true
	default:
		return false
	}
}

// TestRenewer_RecoverRetryableKeepsAsking: once a renewer is in recover mode, every answer but a final 403 or 501
// (a 401, 503, a transport error, 500, 502, 504, 429, 404) leaves it asking, with the gate unlapsed, on both renew
// loops; the first recovery then installs the token and the loop goes on.
func TestRenewer_RecoverRetryableKeepsAsking(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"401", http.StatusUnauthorized},
		{"503", http.StatusServiceUnavailable},
		{"transport error", rcDrop},
		{"500", http.StatusInternalServerError},
		{"502", http.StatusBadGateway},
		{"504", http.StatusGatewayTimeout},
		{"429", http.StatusTooManyRequests},
		{"404", http.StatusNotFound},
	} {
		for _, kind := range []string{"gated", "self-lapse"} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				p := rcNewPlane(t, http.StatusOK, tc.status)
				l := rcStart(t, kind, p)
				if !gateEventually(10*time.Second, func() bool { return p.recovers.Load() >= 5 }) {
					t.Fatalf("recover mode asked %d times, want it to keep asking", p.recovers.Load())
				}
				if l.lapsed() || l.stopped() || !l.g.serving() {
					t.Fatalf("a retryable recover answer ended the loop: lapsed=%v stopped=%v reason=%q", l.lapsed(), l.stopped(), l.g.reason)
				}
				if kind == "self-lapse" && p.renews.Load() != 1 {
					t.Fatalf("%d renews, want the one that opened the gate: recover mode asks the recover door instead", p.renews.Load())
				}
				if got := l.ts.Get(); got != "seed" && got != "renewed" {
					t.Fatalf("token %q after refused recoveries", got)
				}
				p.renewStatus.Store(http.StatusServiceUnavailable) // so a renewal does not replace the recovered token
				p.recoverStatus.Store(http.StatusOK)
				if !gateEventually(10*time.Second, func() bool { return l.ts.Get() == "recovered" }) {
					t.Fatalf("a recovery after %s answers did not install its token", tc.name)
				}
				if l.lapsed() || !l.g.serving() {
					t.Fatalf("lapsed=%v serving=%v after a recovery", l.lapsed(), l.g.serving())
				}
			})
		}
	}
}

// TestRenewer_Recover403And501Lapse: a 403 (the run may never recover) or a 501 (the server cannot recover) in recover
// mode ends the loop at once, after one ask, lapsing the gate with its cause; on a k8s runner's gate the process ends
// too, on any other gate it stays up.
func TestRenewer_Recover403And501Lapse(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotImplemented} {
		for _, kind := range []string{"gated", "self-lapse"} {
			t.Run(fmt.Sprintf("%s/%d", kind, status), func(t *testing.T) {
				p := rcNewPlane(t, http.StatusOK, status)
				l := rcStart(t, kind, p)
				select {
				case <-l.ended:
				case <-time.After(10 * time.Second):
					t.Fatalf("the loop kept running after a %d from the recover door", status)
				}
				if !l.lapsed() || l.g.serving() || l.g.reason != LapseRenewRefused {
					t.Fatalf("lapsed=%v serving=%v reason=%q, want the gate lapsed as %q", l.lapsed(), l.g.serving(), l.g.reason, LapseRenewRefused)
				}
				if got := p.recovers.Load(); got != 1 {
					t.Fatalf("%d recover asks, want 1", got)
				}
				if cause := l.g.lastCause(); !strings.Contains(cause, fmt.Sprintf("renew status %d", status)) || strings.Contains(cause, "seed") {
					t.Fatalf("cause %q", cause)
				}
				exited := false
				select {
				case <-l.g.exited():
					exited = true
				default:
				}
				if want := kind == "self-lapse"; exited != want {
					t.Fatalf("process exit signalled = %v, want %v", exited, want)
				}
			})
		}
	}
}

// rcJitter is a jitter func that answers a script in order, repeating it, and records the bound it was asked for.
type rcJitter struct {
	mu     sync.Mutex
	script []time.Duration
	n      int
	bounds []time.Duration
}

func (j *rcJitter) fn(bound time.Duration) time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.bounds = append(j.bounds, bound)
	d := j.script[j.n%len(j.script)]
	j.n++
	return d
}

func (j *rcJitter) asked() []time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]time.Duration(nil), j.bounds...)
}

// TestRenewer_RecoverModeCadence: recover mode asks every minute, spread by up to half a minute either way, and the
// gap does not grow across failures. The gated loop's gap is checked on the clock-free answer function with the
// jitter scripted, then on the default jitter; the self-lapse loop's on real, millisecond-scale gaps.
func TestRenewer_RecoverModeCadence(t *testing.T) {
	if recoverEvery != time.Minute || recoverJitter != 30*time.Second {
		t.Fatalf("recover cadence %v ± %v, want 1m ± 30s", recoverEvery, recoverJitter)
	}
	if d := defaultRenewerTuning(); d.recoverEvery != recoverEvery || d.recoverJitter != recoverJitter {
		t.Fatalf("gated loop's default tuning %v ± %v", d.recoverEvery, d.recoverJitter)
	}
	if d := defaultLapseRenewTuning(); d.recover != recoverEvery || d.recoverJitter != recoverJitter || d.jitter == nil {
		t.Fatalf("self-lapse default tuning %v ± %v", d.recover, d.recoverJitter)
	}

	t.Run("default jitter stays in the band", func(t *testing.T) {
		tune := defaultRenewerTuning()
		lo, hi := time.Duration(1<<62), time.Duration(0)
		for range 2000 {
			gap := tune.recoverGap()
			if gap < recoverEvery-recoverJitter || gap > recoverEvery+recoverJitter {
				t.Fatalf("gap %v outside 60s ± 30s", gap)
			}
			lo, hi = min(lo, gap), max(hi, gap)
		}
		if lo > 40*time.Second || hi < 80*time.Second {
			t.Fatalf("gaps spread only %v to %v: the jitter is not spreading proxies", lo, hi)
		}
	})

	t.Run("gated loop's gap does not grow", func(t *testing.T) {
		script := []time.Duration{-30 * time.Second, 30 * time.Second, 0, 17 * time.Second, -3 * time.Second}
		j := &rcJitter{script: script}
		tune := defaultRenewerTuning()
		tune.jitter = j.fn
		g := newLapseGate(&fakeBoot{}, testLapseTTL, gateOpts{pausable: true, startOpen: true})
		ts := newTokenSource("seed")
		statuses := []int{http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusBadGateway, http.StatusNotFound, http.StatusTooManyRequests}
		for i := range 60 {
			err := error(&renewStatusError{Status: statuses[i%len(statuses)]})
			if i%7 == 6 {
				err = fmt.Errorf("renew request: %w", context.DeadlineExceeded)
			}
			next, stop := recoverAnswer(t.Context(), ts, g, 0, "", err, tune)
			if want := recoverEvery + script[i%len(script)]; stop || next != want {
				t.Fatalf("failure %d: next %v stop %v, want %v and not stopped", i, next, stop, want)
			}
		}
		for _, bound := range j.asked() {
			if bound != recoverJitter {
				t.Fatalf("jitter asked for a bound of %v, want %v", bound, recoverJitter)
			}
		}
		if g.state != lapseOpen {
			t.Fatalf("gate state %v after 60 retryable answers", g.state)
		}
	})

	t.Run("self-lapse loop's gap does not grow", func(t *testing.T) {
		p := rcNewPlane(t, http.StatusOK, http.StatusServiceUnavailable)
		p.renewExp.Store(time.Now().Add(-2 * time.Second).Unix())
		const every, spread = 30 * time.Millisecond, 10 * time.Millisecond
		j := &rcJitter{script: []time.Duration{spread, -spread, 0}}
		tune := lapseRenewTuning{success: 5 * time.Millisecond, failure: 5 * time.Millisecond, recover: every, recoverJitter: spread,
			watchEvery: time.Millisecond, jitter: j.fn}
		g := newLapseGate(&fakeBoot{}, testLapseTTL, gateOpts{pausable: true, exitOnLapse: true})
		ctx, cancel := context.WithCancel(t.Context())
		ended := make(chan struct{})
		go func() {
			runSelfLapseRenewer(ctx, newTokenSource("seed"), p.srv.URL, p.srv.Client(), g, tune)
			close(ended)
		}()
		defer func() { cancel(); <-ended }()
		if !gateEventually(10*time.Second, func() bool { return p.recovers.Load() >= 12 }) {
			t.Fatalf("%d recover asks", p.recovers.Load())
		}
		times := p.recoverTimes()
		for i := 1; i < len(times); i++ {
			// The wait starts after the answer, so a gap is never shorter than its own wait: every ± spread.
			gap := times[i].Sub(times[i-1])
			if gap < every-spread || gap > every+spread+time.Second {
				t.Fatalf("gap %d is %v, want %v ± %v (plus scheduling)", i, gap, every, spread)
			}
		}
		for _, bound := range j.asked() {
			if bound != 0 && bound != spread {
				t.Fatalf("jitter asked for a bound of %v, want the recover spread %v", bound, spread)
			}
		}
	})
}

// rcGo runs loop until the test ends and checks that it stops with its context, as rcStart does.
func rcGo(t *testing.T, loop func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	go func() {
		loop(ctx)
		close(ended)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Error("the renewer did not stop with its context")
		}
	})
}

// rcGatedTune is the gated loop's timing for the tests below: fast first retries, a renew backoff that grows to an
// hour, no give-up within the test, and a recover gap of every (no jitter).
func rcGatedTune(every time.Duration) renewerTuning {
	return renewerTuning{firstRetry: time.Millisecond, maxInterval: time.Hour, giveUpAfter: time.Hour, expiryMargin: time.Minute,
		closedRetry: time.Millisecond, recoverEvery: every, jitter: func(time.Duration) time.Duration { return 0 }, now: time.Now}
}

// TestRenewer_PausedLiveToken409RenewsOnceAndReopens: on both loops, a paused gate whose token is still live asks the
// recover door first; its 409 sends the loop to one renew a first retry later, not a recover gap later, and that renew
// reopens the gate. The recover gap here is 5 s, so a loop that waits it after the 409, or asks the door again instead
// of renewing, does not reopen within the second the test allows.
func TestRenewer_PausedLiveToken409RenewsOnceAndReopens(t *testing.T) {
	for _, kind := range []string{"gated", "self-lapse"} {
		t.Run(kind, func(t *testing.T) {
			p := rcNewPlane(t, http.StatusOK, http.StatusConflict)
			clock := &fakeBoot{}
			g := newLapseGate(clock, 30*time.Minute, gateOpts{pausable: true, startOpen: true, exitOnLapse: kind == "self-lapse"})
			clock.set(31 * time.Minute)
			if !g.paused() {
				t.Fatal("the gate did not pause")
			}
			ts := newTokenSource("seed")
			rcGo(t, func(ctx context.Context) {
				if kind == "gated" {
					runTokenRenewerGated(ctx, ts, p.srv.URL, p.srv.Client(), rcGatedTune(5*time.Second), g)
					return
				}
				runSelfLapseRenewer(ctx, ts, p.srv.URL, p.srv.Client(), g, lapseRenewTuning{success: time.Hour, failure: time.Millisecond,
					recover: 5 * time.Second, watchEvery: time.Millisecond, jitter: func(time.Duration) time.Duration { return 0 }})
			})
			if !gateEventually(time.Second, func() bool { return g.serving() && ts.Get() == "renewed" }) {
				t.Fatalf("the paused gate did not reopen through a renew within a second: token %q state %v, %d recover asks, %d renews",
					ts.Get(), pgState(g), p.recovers.Load(), p.renews.Load())
			}
			time.Sleep(50 * time.Millisecond)
			if r, n := p.recovers.Load(), p.renews.Load(); r != 1 || n != 1 {
				t.Fatalf("%d recover asks and %d renews, want the recover door once and then one renew", r, n)
			}
		})
	}
}

// TestRenewer_PauseEndsTheGatedLoopsBackoffWait: a gated loop that the control plane's absence has backed off to a
// wait of seconds asks the recover door as soon as its gate pauses, and a live token is renewed and the gate reopened
// within a second of the pause, not at the end of that wait.
func TestRenewer_PauseEndsTheGatedLoopsBackoffWait(t *testing.T) {
	p := rcNewPlane(t, http.StatusServiceUnavailable, http.StatusConflict)
	clock := &fakeBoot{}
	g := newLapseGate(clock, 30*time.Minute, gateOpts{pausable: true, startOpen: true})
	ts := newTokenSource("seed")
	rcGo(t, func(ctx context.Context) {
		runTokenRenewerGated(ctx, ts, p.srv.URL, p.srv.Client(), rcGatedTune(5*time.Second), g)
	})
	// The twelfth failed renew backs the loop off for about two seconds.
	if !gateEventually(10*time.Second, func() bool { return p.renews.Load() >= 12 }) {
		t.Fatalf("only %d renews in 10 s", p.renews.Load())
	}
	if p.recovers.Load() != 0 {
		t.Fatalf("%d recover asks while the gate was open with a live token, want none", p.recovers.Load())
	}
	clock.set(31 * time.Minute)
	g.check()
	if !g.paused() {
		t.Fatal("the gate did not pause")
	}
	p.renewStatus.Store(http.StatusOK)
	if !gateEventually(time.Second, func() bool { return g.serving() && ts.Get() == "renewed" }) {
		t.Fatalf("the gate did not reopen within a second of its pause: token %q state %v, %d recover asks",
			ts.Get(), pgState(g), p.recovers.Load())
	}
	if p.recovers.Load() == 0 {
		t.Fatal("the gate reopened without the recover door being asked")
	}
}

// TestRenewer_401PastHeldExpiryRecoversBeforeGivingUp: a gated loop whose held token's known expiry has passed takes
// a renew's 401 as the cue to recover, long before giveUpAfter (an hour here), and the recovery installs its token.
func TestRenewer_401PastHeldExpiryRecoversBeforeGivingUp(t *testing.T) {
	p := rcNewPlane(t, http.StatusOK, http.StatusOK)
	p.renewExp.Store(time.Now().Add(-2 * time.Second).Unix())
	g := newLapseGate(&fakeBoot{}, testLapseTTL, gateOpts{pausable: true, startOpen: true})
	ts := newTokenSource("seed")
	rcGo(t, func(ctx context.Context) {
		runTokenRenewerGated(ctx, ts, p.srv.URL, p.srv.Client(), rcGatedTune(time.Millisecond), g)
	})
	if !gateEventually(5*time.Second, func() bool { return p.renews.Load() >= 1 }) {
		t.Fatal("the loop never renewed")
	}
	p.renewStatus.Store(http.StatusUnauthorized)
	if !gateEventually(time.Second, func() bool { return ts.Get() == "recovered" }) {
		t.Fatalf("a 401 after the held token's expiry did not lead to a recovery within a second: token %q, %d recover asks",
			ts.Get(), p.recovers.Load())
	}
	if !g.serving() {
		t.Fatalf("the gate is %v after the recovery, want open", pgState(g))
	}
}
