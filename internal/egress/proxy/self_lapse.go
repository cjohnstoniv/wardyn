// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// bootClock is the self-lapse clock seam (design P7). The production clock is
// CLOCK_BOOTTIME: it counts suspended time and the pod cannot set it. Go's
// monotonic reading (time.Since) is CLOCK_MONOTONIC, which stops in suspend,
// so it must never measure the deadline.
type bootClock interface{ Now() time.Duration }

type lapseState uint8

const (
	lapseClosed lapseState = iota // from start until the first successful renew
	lapseOpen
	lapseLapsed // terminal
)

// Lapse reasons sent to runnerd in the lapsed frame.
const (
	LapseDeadline     = "deadline"
	LapseRenewRefused = "renew_refused"
)

// lapseGate is a k8s runner proxy's terminal self-lapse (D6, §6.2): closed
// until the first successful renew, open until the last successful renew's
// SEND time plus RunTokenTTL, then lapsed for good. The renew result and the
// lapse take the same lock, so a result arriving after the lapse is dropped.
// A nil gate (Docker, server) always serves.
type lapseGate struct {
	mu       sync.Mutex
	clock    bootClock
	ttl      time.Duration
	state    lapseState
	deadline time.Duration
	reason   string
	done     chan struct{}
	onLapse  func(reason string)
}

func newLapseGate(clock bootClock, ttl time.Duration) *lapseGate {
	// Closed from start: until a renew succeeds the bound is process start + TTL.
	return &lapseGate{clock: clock, ttl: ttl, deadline: addSat(clock.Now(), ttl), done: make(chan struct{})}
}

// addSat adds without wrapping: a far-future clock reading (the fail-closed
// answer of a clock that cannot be read) gives the far-future deadline, which
// that same reading has already reached.
func addSat(a, b time.Duration) time.Duration {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// serving reports whether agent requests may be served now, lapsing first when
// the deadline has passed (a request after a suspend sees it at once).
func (g *lapseGate) serving() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked()
	return g.state == lapseOpen
}

// check is the watchdog's tick.
func (g *lapseGate) check() {
	g.mu.Lock()
	g.expireLocked()
	g.mu.Unlock()
}

func (g *lapseGate) expireLocked() {
	if g.state != lapseLapsed && g.clock.Now() >= g.deadline {
		g.lapseLocked(LapseDeadline)
	}
}

// renewed applies a successful renew sent at sent. It returns false, applying
// nothing, when the gate has lapsed or the deadline passed before the result.
func (g *lapseGate) renewed(sent time.Duration, apply func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked()
	if g.state == lapseLapsed {
		return false
	}
	apply()
	g.state, g.deadline = lapseOpen, addSat(sent, g.ttl)
	return true
}

func (g *lapseGate) lapse(reason string) {
	g.mu.Lock()
	g.lapseLocked(reason)
	g.mu.Unlock()
}

func (g *lapseGate) lapseLocked(reason string) {
	if g.state == lapseLapsed {
		return
	}
	g.state, g.reason = lapseLapsed, reason
	close(g.done)
	if g.onLapse != nil {
		go g.onLapse(reason)
	}
}

// Done closes on the lapse; nil for a gate-less proxy (never fires).
func (g *lapseGate) Done() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.done
}

// watch lapses the gate within a tick of the deadline even with no traffic.
func (g *lapseGate) watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.done:
			return
		case <-t.C:
			g.check()
		}
	}
}

func refuseClosed(w http.ResponseWriter) {
	http.Error(w, "wardyn: this run's egress is closed", http.StatusServiceUnavailable)
}

// lapseRenewTuning is the self-lapse renew cadence (N1): every 10 min ±1 min
// after a success, every 30 s ±5 s after a failure, until the deadline. Nothing
// gives up before it; a permanent refusal lapses at once.
type lapseRenewTuning struct {
	success, successJitter time.Duration
	failure, failureJitter time.Duration
	watchEvery             time.Duration
	jitter                 func(d time.Duration) time.Duration
}

func defaultLapseRenewTuning() lapseRenewTuning {
	return lapseRenewTuning{
		success: 10 * time.Minute, successJitter: time.Minute,
		failure: 30 * time.Second, failureJitter: 5 * time.Second,
		watchEvery: time.Second,
		jitter: func(d time.Duration) time.Duration {
			if d <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(2*d+1))) - d
		},
	}
}

// runSelfLapseRenewer renews until the gate lapses: each success moves the
// deadline to its send time + RunTokenTTL, under the gate's lock.
func runSelfLapseRenewer(ctx context.Context, ts *tokenSource, base string, client *http.Client, g *lapseGate, tune lapseRenewTuning) {
	for {
		sent := g.clock.Now()
		rctx, cancel := context.WithTimeout(ctx, renewTimeout)
		tok, _, err := renewToken(rctx, base, ts.Get(), client)
		cancel()
		next := tune.failure + tune.jitter(tune.failureJitter)
		var se *renewStatusError
		switch {
		case err == nil:
			if !g.renewed(sent, func() { ts.Set(tok) }) {
				return
			}
			next = tune.success + tune.jitter(tune.successJitter)
		case errors.As(err, &se) && se.permanent():
			slog.ErrorContext(ctx, "wardyn-proxy: run token renew refused permanently, lapsing", slog.Int("status", se.Status))
			g.lapse(LapseRenewRefused)
			return
		default:
			slog.ErrorContext(ctx, "wardyn-proxy: run token renew failed, retrying", slog.Duration("retry_in", next), slog.Any("err", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-g.done:
			return
		case <-time.After(next):
		}
	}
}

// selfLapseTTL is the deadline's span: the run token's lifetime.
const selfLapseTTL = identity.RunTokenTTL
