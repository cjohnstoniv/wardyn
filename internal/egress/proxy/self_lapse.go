// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
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
	lapsePaused // past the allowance on a pausable gate: refuses, closes tunnels, reopens on a renew or recovery
	lapseLapsed // terminal
)

// Lapse reasons sent to runnerd in the lapsed frame.
const (
	LapseDeadline     = "deadline"
	LapseRenewRefused = "renew_refused"
)

// lapseGate is every proxy's egress gate (#4082): open until the
// last successful renew's SEND time plus the run's outage allowance (span).
// A gate that starts closed (a k8s runner's proxy, a replacement) serves
// nothing until its first renew. Past the deadline a pausable gate pauses: it
// refuses with a retryable answer, closes its tunnels and reopens on the next
// renew or recovery, until its hardEnd; any other gate lapses for good. The
// renew result and the lapse take the same lock, so a result arriving after
// the lapse is dropped. A nil gate (no control plane, the via_org engine)
// always serves.
type lapseGate struct {
	mu          sync.Mutex
	clock       bootClock
	span        time.Duration
	state       lapseState
	start       time.Duration
	deadline    time.Duration
	hardEnd     time.Duration // the last good renew's send time + RunTokenTTL + 24 h + 5 min: a pause lapses here
	pausable    bool          // pause at the deadline instead of lapsing
	exitOnLapse bool          // the process ends when the gate lapses after serving (a k8s runner's proxy)
	reason      string
	cause       string        // the last renew error, redacted (renewCause)
	open        chan struct{} // closes on the first successful renew
	pause       chan struct{} // closes when the gate pauses; a reopen replaces it
	done        chan struct{}
	exit        chan struct{} // closes on a lapse when exitOnLapse holds, or on a lapse while still closed
	tunnels     map[net.Conn]struct{}
	onLapse     func(reason string)
	onPause     func() // called under the gate's lock; it must not call the gate
}

// gateOpts are a gate's case (server.go): pausable unless the run's terms are strict, exitOnLapse only on a k8s
// runner, and startOpen for a proxy that serves from its start.
type gateOpts struct {
	pausable, exitOnLapse, startOpen bool
}

// recoverHardEnd is how long after a renew's send time a paused gate may still be reopened: the token's lifetime, the
// 24 h within which a lapsed token may be recovered, and the stamp margin.
const recoverHardEnd = identity.RunTokenTTL + 24*time.Hour + 5*time.Minute

func newLapseGate(clock bootClock, span time.Duration, opts gateOpts) *lapseGate {
	// Closed from start (unless startOpen): until a renew succeeds the bound is process start + span.
	now := clock.Now()
	g := &lapseGate{clock: clock, span: span, start: now, deadline: addSat(now, span), hardEnd: addSat(now, recoverHardEnd),
		pausable: opts.pausable, exitOnLapse: opts.exitOnLapse,
		open: make(chan struct{}), pause: make(chan struct{}), done: make(chan struct{}), exit: make(chan struct{}),
		tunnels: map[net.Conn]struct{}{}}
	if opts.startOpen {
		g.state = lapseOpen
		close(g.open)
	}
	return g
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
	now := g.clock.Now()
	switch {
	case g.state == lapseOpen && now >= g.deadline && g.pausable:
		g.state = lapsePaused
		close(g.pause)
		for c := range g.tunnels {
			_ = c.Close()
		}
		if g.onPause != nil {
			g.onPause()
		}
		if now >= g.hardEnd {
			g.lapseLocked(LapseDeadline)
		}
	case (g.state == lapseOpen || g.state == lapseClosed) && now >= g.deadline:
		g.lapseLocked(LapseDeadline)
	case g.state == lapsePaused && now >= g.hardEnd:
		g.lapseLocked(LapseDeadline)
	}
}

// renewed applies a successful renew or recovery sent at sent: a closed or
// paused gate opens until sent + span. It returns false, applying nothing,
// when the gate has lapsed or a non-pausable deadline passed before the result.
func (g *lapseGate) renewed(sent time.Duration, apply func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked()
	if g.state == lapseLapsed {
		return false
	}
	apply()
	switch g.state {
	case lapseClosed:
		close(g.open)
	case lapsePaused:
		g.pause = make(chan struct{})
	}
	g.state, g.deadline, g.hardEnd = lapseOpen, addSat(sent, g.span), addSat(sent, recoverHardEnd)
	return true
}

// paused reports a gate that has paused at its allowance; false for nil.
func (g *lapseGate) paused() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked()
	return g.state == lapsePaused
}

// pausing is a channel that closes when an open pausable gate pauses, so a renew loop that takes it before a try ends
// that try's wait at the pause and asks the recover door from then on. It is nil, never closing, for a nil gate or one
// that is not open and pausable (a paused gate's loop already asks at the recover cadence).
func (g *lapseGate) pausing() <-chan struct{} {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked()
	if g.state != lapseOpen || !g.pausable {
		return nil
	}
	return g.pause
}

// track registers a hijacked tunnel's client conn, so a pause closes it; the returned func forgets it. A gate that
// is not open closes it at once. A nil gate tracks nothing.
func (g *lapseGate) track(c net.Conn) func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked()
	if g.state != lapseOpen {
		_ = c.Close()
		return func() {}
	}
	g.tunnels[c] = struct{}{}
	return func() {
		g.mu.Lock()
		delete(g.tunnels, c)
		g.mu.Unlock()
	}
}

func (g *lapseGate) lapse(reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.lapseLocked(reason)
	g.mu.Unlock()
}

// failed records a failed renew's cause. When the gate is still closed and its next try, at
// retryAt, would land past start + firstRenewWithin, it lapses now: the proxy never served, so the
// runner's start fails naming this cause instead of waiting out its own deadline with none.
func (g *lapseGate) failed(cause string, retryAt time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cause = cause
	if g.state == lapseClosed && retryAt >= addSat(g.start, firstRenewWithin) {
		g.lapseLocked(LapseDeadline)
	}
}

// lastCause is the last renew error recorded, redacted; "" when none failed.
func (g *lapseGate) lastCause() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cause
}

func (g *lapseGate) lapseLocked(reason string) {
	if g.state == lapseLapsed {
		return
	}
	wasClosed := g.state == lapseClosed
	g.state, g.reason = lapseLapsed, reason
	for c := range g.tunnels {
		_ = c.Close()
	}
	close(g.done)
	if g.exitOnLapse || wasClosed {
		close(g.exit)
	}
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

// exited closes when the process must end: on a lapse of a k8s runner's gate, or a lapse before the first renew;
// nil for a gate-less proxy (never fires).
func (g *lapseGate) exited() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.exit
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

// pausedBody is a paused gate's refusal.
const pausedBody = "wardyn: this run's network is paused until Wardyn can check the run again"

// refuse answers a request the gate does not serve: a paused gate with a retryable 503, any other with refuseClosed.
func (g *lapseGate) refuse(w http.ResponseWriter) {
	if g.paused() {
		w.Header().Set("Retry-After", "60")
		http.Error(w, pausedBody, http.StatusServiceUnavailable)
		return
	}
	refuseClosed(w)
}

// lapseRenewTuning is the self-lapse renew cadence (N1): every 10 min ±1 min
// after a success, every 30 s ±5 s after a failure, until the deadline. Nothing
// gives up before it, except a proxy not yet renewed by firstRenewWithin; a
// permanent refusal lapses at once.
type lapseRenewTuning struct {
	success, successJitter time.Duration
	failure, failureJitter time.Duration
	recover, recoverJitter time.Duration // recover mode's cadence (recover.go)
	watchEvery             time.Duration
	jitter                 func(d time.Duration) time.Duration
}

func defaultLapseRenewTuning() lapseRenewTuning {
	return lapseRenewTuning{
		success: 10 * time.Minute, successJitter: time.Minute,
		failure: 30 * time.Second, failureJitter: 5 * time.Second,
		recover: recoverEvery, recoverJitter: recoverJitter,
		watchEvery: time.Second,
		jitter:     uniformJitter,
	}
}

// runSelfLapseRenewer renews until the gate lapses: each success moves the
// deadline to its send time + the gate's span, under the gate's lock. Once the
// held token's known expiry has passed, or the gate has paused, it asks the
// recover door instead at the recover cadence (#4082): a recovery reopens the
// gate, a 409 sends it back to one renew, and a 403 or 501 lapses the gate.
func runSelfLapseRenewer(ctx context.Context, ts *tokenSource, base string, client *http.Client, g *lapseGate, tune lapseRenewTuning) {
	var heldExp time.Time // the held token's expiry; unknown for the startup token
	renewNext := false    // a 409 from the recover door: the token is live, so renew it
	recovering := false
	for {
		sent := g.clock.Now()
		recover := !renewNext && (g.paused() || !heldExp.IsZero() && time.Now().After(heldExp))
		renewNext = false
		if recover && !recovering {
			slog.WarnContext(ctx, "wardyn-proxy: the held run token can no longer renew, asking to recover it")
		}
		recovering = recover
		rctx, cancel := context.WithTimeout(ctx, renewTimeout)
		var tok string
		var exp time.Time
		var err error
		if recover {
			tok, exp, err = recoverToken(rctx, base, ts.Get(), client)
		} else {
			tok, exp, err = renewToken(rctx, base, ts.Get(), client)
		}
		cancel()
		next := tune.failure + tune.jitter(tune.failureJitter)
		var se *renewStatusError
		switch {
		case err == nil:
			if !g.renewed(sent, func() { ts.Set(tok) }) {
				return
			}
			if recover {
				slog.InfoContext(ctx, "wardyn-proxy: run token recovered after the control plane came back")
				recovering = false
			}
			heldExp = exp
			next = tune.success + tune.jitter(tune.successJitter)
		case recover && isRecoverStatus(err, http.StatusConflict):
			renewNext = true
		case recover && recoverFinal(err):
			slog.ErrorContext(ctx, "wardyn-proxy: run token recovery refused for good, lapsing", slog.Any("err", err))
			g.mu.Lock()
			g.cause = renewCause(err, ts.Get())
			g.lapseLocked(LapseRenewRefused)
			g.mu.Unlock()
			return
		case recover:
			next = tune.recover + tune.jitter(tune.recoverJitter)
			g.failed(renewCause(err, ts.Get()), addSat(g.clock.Now(), next))
		case errors.As(err, &se) && se.permanent():
			slog.ErrorContext(ctx, "wardyn-proxy: run token renew refused permanently, lapsing", slog.Int("status", se.Status))
			g.mu.Lock()
			g.cause = renewCause(err, ts.Get())
			g.lapseLocked(LapseRenewRefused)
			g.mu.Unlock()
			return
		default:
			slog.ErrorContext(ctx, "wardyn-proxy: run token renew failed, retrying", slog.Duration("retry_in", next), slog.Any("err", err))
			g.failed(renewCause(err, ts.Get()), addSat(g.clock.Now(), next))
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

// firstRenewWithin bounds how long a proxy that has never renewed keeps trying: it never served, so
// no run is lost, and the runner's start (WARDYN_SANDBOX_START_TIMEOUT, 3m by default) can still
// report why. Two or three tries fit at the failure cadence.
const firstRenewWithin = time.Minute

// maxRenewCause caps a recorded renew cause; a termination message holds 4 KiB.
const maxRenewCause = 512

// renewCause is a renew error fit to leave the proxy: one line of at most maxRenewCause bytes,
// keeping the error's class and the organisation's scheme://host:port but never the token, the URL's
// path, query or user info.
func renewCause(err error, token string) string {
	msg := err.Error()
	var ue *url.Error
	var se *renewStatusError
	switch {
	case errors.As(err, &se) && se.permanent():
		msg = "refused for good: " + se.Error()
	case errors.As(err, &se):
		msg = se.Error()
	case errors.As(err, &ue):
		origin := "the organisation"
		if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
			origin = u.Scheme + "://" + u.Host
		}
		msg = ue.Op + " " + origin + ": " + ue.Err.Error()
	}
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "[redacted]")
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > maxRenewCause {
		msg = strings.ToValidUTF8(msg[:maxRenewCause], "") + "…"
	}
	return msg
}

// WriteLapseCause writes a lapsed proxy's last renew error to path, the container's termination
// message on a k8s runner, where the runner names a failed start by it. Nothing when none failed.
func (s *Server) WriteLapseCause(path string) error {
	cause := s.proxy.lapse.lastCause()
	if cause == "" {
		return nil
	}
	return os.WriteFile(path, []byte(cause), 0o644)
}
