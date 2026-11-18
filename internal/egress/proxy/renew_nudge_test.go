// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// startNudgeRenewer runs the plain renew loop against a stand-in control plane that refuses with 503 until
// cp.renewErr is cleared. The timed gap is an hour, so only a wake can make it try again in the test.
func startNudgeRenewer(t *testing.T, cp *renewCP, ts *tokenSource, floor time.Duration) {
	t.Helper()
	srv := httptest.NewServer(cp.handler())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTokenRenewerTuned(ctx, ts, srv.URL, srv.Client(), renewerTuning{
			firstRetry: time.Hour, maxInterval: time.Hour, giveUpAfter: 24 * time.Hour,
			expiryMargin: time.Minute, nudgeFloor: floor, now: time.Now,
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func waitAttempts(t *testing.T, cp *renewCP, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for cp.renewAttempts() < n {
		if time.Now().After(deadline) {
			t.Fatalf("renew attempts %d, want %d", cp.renewAttempts(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRenewNudgeTriesAtOnceAfterTheFloor: after a failed renew with a long timed gap, one wake makes the
// next try within the floor, not after the gap, and a success zeroes quiet.
func TestRenewNudgeTriesAtOnceAfterTheFloor(t *testing.T) {
	const floor = 150 * time.Millisecond
	cp := &renewCP{ttl: time.Hour, renewErr: http.StatusServiceUnavailable}
	ts := newTokenSource("startup-token")
	ts.atMs = time.Now().Add(-10 * time.Minute).UnixMilli()
	startNudgeRenewer(t, cp, ts, floor)
	waitAttempts(t, cp, 1)

	cp.mu.Lock()
	cp.renewErr = 0 // the organisation is back
	cp.mu.Unlock()
	woke := time.Now()
	ts.wake()

	deadline := woke.Add(floor + 2*time.Second)
	for ts.quiet(time.Now()) > time.Minute {
		if time.Now().After(deadline) {
			t.Fatalf("no renewal %v after the wake; quiet %v", time.Since(woke), ts.quiet(time.Now()))
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := cp.renewAttempts(); got != 2 {
		t.Fatalf("renew attempts %d, want 2 (the failure and the nudged success)", got)
	}
	if q := ts.quiet(time.Now()); q > time.Second {
		t.Fatalf("quiet %v after a renewal, want near 0", q)
	}
}

// TestRenewNudgeBurstIsRateLimited: 50 wakes inside one floor give one extra try, and not before the floor.
func TestRenewNudgeBurstIsRateLimited(t *testing.T) {
	const floor = 400 * time.Millisecond
	cp := &renewCP{ttl: time.Hour, renewErr: http.StatusServiceUnavailable}
	ts := newTokenSource("startup-token")
	startNudgeRenewer(t, cp, ts, floor)
	waitAttempts(t, cp, 1)

	for range 50 {
		ts.wake()
	}
	time.Sleep(floor / 3)
	if got := cp.renewAttempts(); got != 1 {
		t.Fatalf("renew attempts %d inside the floor, want 1", got)
	}
	waitAttempts(t, cp, 2)
	time.Sleep(floor + floor/2) // the rest of the burst was answered by that try
	if got := cp.renewAttempts(); got != 2 {
		t.Fatalf("renew attempts %d after the burst, want 2", got)
	}
	if gaps := cp.gaps(); len(gaps) != 1 || gaps[0] < floor-20*time.Millisecond {
		t.Fatalf("gaps %v, want one gap of at least %v", gaps, floor)
	}
}

// TestBrokeredTokenOfflineWakesTheRenewer: the offline-bound refusal leaves a wake; a served token or a
// refusal for another reason does not.
func TestBrokeredTokenOfflineWakesTheRenewer(t *testing.T) {
	up := newGitBrokerUpstream(t, "ghs_nudge")
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))
	grant := uuid.New()
	wire := func(string) string { return gitBrokerUsername }
	woken := func() bool {
		select {
		case <-p.runToken.nudged():
			return true
		default:
			return false
		}
	}

	if _, _, err := p.brokeredToken(context.Background(), grant, wire); err != nil || woken() {
		t.Fatalf("served token: err %v, woken %v, want no error and no wake", err, woken())
	}

	p.runToken.atMs = time.Now().Add(-29 * time.Minute).UnixMilli() // inside the bound
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := p.brokeredToken(ctx, uuid.New(), wire); err == nil || woken() {
		t.Fatalf("other refusal: err %v, woken %v, want an error and no wake", err, woken())
	}

	p.runToken.atMs = time.Now().Add(-31 * time.Minute).UnixMilli()
	if _, _, err := p.brokeredToken(context.Background(), grant, wire); err == nil {
		t.Fatal("offline: no error")
	}
	if !woken() {
		t.Fatal("offline refusal left no wake")
	}
}

// TestTokenSourceZeroValueWakeIsSafe: a source with no channel, and a nil source, take a wake in silence.
func TestTokenSourceZeroValueWakeIsSafe(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&tokenSource{}).wake()
		var nilSrc *tokenSource
		nilSrc.wake()
		if nilSrc.nudged() != nil || (&tokenSource{}).nudged() != nil {
			t.Error("nudged() not nil for a nil source or channel")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wake blocked")
	}
}
