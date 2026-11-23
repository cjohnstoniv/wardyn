// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package dispatchgate bounds when one relayed request may start its first write into a sandbox. A Gate wraps
// the one connection a request is dialed (NewTransport gives each request its own), and admits that request's
// first driver write only before a fixed time, By. A first write still in progress at By is hard-stopped, and
// the request is answered at once, never after the driver write returns.
//
// The gate holds no runner or server type: the caller hands it the connection, the hard stop, the refusal
// record and the outcome recorder as plain values.
package dispatchgate

import (
	"errors"
	"net"
	"sync"
	"time"
)

// ErrExpired is a Write refused, or cut, because the request's dispatch time passed or its connection closed
// before its first write returned, or because an earlier first write failed or came back short. A later write
// that returns after the connection closed also gets it.
var ErrExpired = errors.New("dispatchgate: the request's dispatch time passed")

// Kinds of Outcome. Refused: no driver write had started, so zero bytes passed. Interrupted: the first driver
// write had started and had not returned in full before By or the close. Dispatched: the first driver write
// returned in full before By. Unused: the connection closed before any write.
const (
	KindRefused     = "refused"
	KindInterrupted = "interrupted"
	KindDispatched  = "dispatched"
	KindUnused      = "unused"
)

// Outcome is one gated connection's first write, published once its writer has exited. BytesDone is what the
// driver write reported complete; Partial is true when that write failed, because the bytes that passed the
// driver are then not always observable. StopReturned is when the hard stop returned (zero if none ran).
type Outcome struct {
	Kind          string
	By            time.Time
	BytesDone     int
	Partial       bool
	StartedAt     time.Time
	WriteReturned time.Time
	StopReturned  time.Time
}

// Config is one Gate's fixed inputs.
type Config struct {
	By  time.Time
	Now func() time.Time
	// AfterFunc arms the expiry timer, as time.AfterFunc does; its result stops it.
	AfterFunc func(d time.Duration, f func()) (stop func() bool)
	// Stop is the exec's hard stop: it wakes a blocked driver write and never waits on it.
	Stop func()
	// Refuse records the request's typed refusal. It runs before any waiting Write is woken.
	Refuse func()
	// Record receives the outcome on a goroutine of its own, after the connection's usual close returned.
	Record func(Outcome)
	// BeforeAdmit, when set, runs on the writer goroutine after a Write queued its copy and before the
	// admission check. Nil in production; a test holds the writer there.
	BeforeAdmit func()
	// beforeWait, when set, runs on the Write caller's goroutine after it queued its copy and before it waits for
	// the reply. Only this package's tests set it, to hold a caller there.
	beforeWait func()
}

type state int

const (
	idle state = iota
	queued
	started
	open
)

type result struct {
	n   int
	err error
}

type job struct {
	p   []byte
	res chan result
}

// Gate is a net.Conn whose first Write is admitted to the wrapped conn only before Config.By. Read and the
// addresses pass through; Close never blocks and never passes through directly (see Close).
type Gate struct {
	net.Conn
	cfg Config

	writeMu sync.Mutex // one Write at a time

	mu        sync.Mutex
	st        state
	expired   bool
	closed    bool
	cleaning  bool
	out       Outcome
	done      chan struct{} // closed at expiry or Close: wakes Write and the writer
	q         chan job
	writerUp  bool
	writerEnd chan struct{}
	stopTimer func() bool
}

// New wraps conn. The expiry timer is armed for cfg.By.
func New(conn net.Conn, cfg Config) *Gate {
	g := &Gate{Conn: conn, cfg: cfg, done: make(chan struct{}), q: make(chan job, 1), writerEnd: make(chan struct{})}
	g.out.By = cfg.By
	// A By at or before now fires the timer at once, on its own goroutine. expire takes mu before anything else,
	// so its cleanup cannot read stopTimer before it is stored here.
	g.mu.Lock()
	g.stopTimer = cfg.AfterFunc(cfg.By.Sub(cfg.Now()), g.expire)
	g.mu.Unlock()
	return g
}

// wake closes done once; mu is held.
func (g *Gate) wake() {
	select {
	case <-g.done:
	default:
		close(g.done)
	}
}

// Write admits p through the writer goroutine. Before the gate opens: at or after By, after expiry or after
// Close it refuses with ErrExpired (recording the refusal unless the gate is closed); otherwise it waits for the
// driver write's return, expiry or Close. Once open, every Write passes with no clock check, still through the
// writer, so Close wakes it in every state. After a first write that did not open the gate, every Write refuses
// with ErrExpired and records nothing.
func (g *Gate) Write(p []byte) (int, error) {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	g.mu.Lock()
	if g.closed || g.expired {
		g.mu.Unlock()
		return 0, ErrExpired
	}
	if g.st != open {
		if g.st == started {
			// A first write already passed bytes and did not open the gate (it failed or came back short): a later
			// write is never a first write, and no refusal is recorded, since bytes passed. The outcome stays
			// interrupted, with that write's start and bytes.
			g.mu.Unlock()
			return 0, ErrExpired
		}
		if !g.cfg.Now().Before(g.cfg.By) {
			g.mu.Unlock()
			g.cfg.Refuse()
			return 0, ErrExpired
		}
		g.st = queued
	}
	if !g.writerUp {
		g.writerUp = true
		go g.writer()
	}
	g.mu.Unlock()
	// The writer owns its copy, so p is never read after Write returns.
	j := job{p: append([]byte(nil), p...), res: make(chan result, 1)}
	select {
	case g.q <- j:
	case <-g.done:
		return 0, ErrExpired
	}
	if g.cfg.beforeWait != nil {
		g.cfg.beforeWait()
	}
	// The answer is serve's reply, decided under mu, the lock done is closed under: a reply sent before done closed
	// is already in res whichever case select takes, and any later one is ErrExpired, since the gate cannot open
	// once done is closed.
	select {
	case r := <-j.res:
		return r.n, r.err
	case <-g.done:
		select {
		case r := <-j.res:
			return r.n, r.err
		default:
			return 0, ErrExpired
		}
	}
}

// writer serves the queued copies one at a time until expiry or Close, and then publishes the outcome.
func (g *Gate) writer() {
	defer close(g.writerEnd)
	for {
		select {
		case <-g.done:
			return
		case j := <-g.q:
			g.serve(j)
		}
	}
}

// serve admits j to the driver or discards it. The admission is decided under mu: only a queued first write
// read before By, with no expiry and no Close, becomes started, and the driver write follows with no other step.
func (g *Gate) serve(j job) {
	if g.cfg.BeforeAdmit != nil {
		g.cfg.BeforeAdmit()
	}
	g.mu.Lock()
	firstWrite := g.st != open
	now := g.cfg.Now()
	if g.closed || g.expired || firstWrite && (g.st != queued || !now.Before(g.cfg.By)) {
		// A first write refused here stays queued: zero bytes, outcome refused. Refused by the clock ahead of
		// the expiry timer, it expires the gate here, the refusal recorded before the Write is woken.
		run := false
		if !g.closed && !g.expired {
			g.cfg.Refuse()
			g.expired = true
			g.wake()
			run, g.cleaning = !g.cleaning, true
		}
		g.mu.Unlock()
		j.res <- result{0, ErrExpired}
		if run {
			go g.cleanup()
		}
		return
	}
	if firstWrite {
		g.st, g.out.StartedAt = started, now
	}
	g.mu.Unlock()
	n, err := g.Conn.Write(j.p)
	g.mu.Lock()
	defer g.mu.Unlock()
	if firstWrite {
		g.out.BytesDone, g.out.Partial, g.out.WriteReturned = n, err != nil, g.cfg.Now()
		if err == nil && n == len(j.p) && !g.expired && !g.closed {
			g.st = open
			g.out.Kind = KindDispatched
			g.stopTimer()
		}
	}
	// The reply is decided at the same step as the opening: a write that returns after expiry or Close, even in
	// full, answers ErrExpired, while the outcome keeps the driver's own result. A first write that opened the gate
	// returned before both. The send never blocks (res holds one reply).
	if g.expired || g.closed {
		n, err = 0, ErrExpired
	}
	j.res <- result{n, err}
}

// expire runs at By. Unless the gate is open or closed, it records the typed refusal first, then marks expiry
// and wakes the waiting Write, all under mu; the hard stop and the usual close follow on this goroutine.
func (g *Gate) expire() {
	g.mu.Lock()
	if g.st == open || g.closed || g.expired {
		g.mu.Unlock()
		return
	}
	g.cfg.Refuse()
	g.expired = true
	g.wake()
	run := !g.cleaning
	g.cleaning = true
	g.mu.Unlock()
	if run {
		g.cleanup()
	}
}

// Close marks the gate closed and wakes Write, and starts the cleanup on a goroutine of its own unless expiry
// already runs it. net/http calls Close under its connection's mutex, so it never waits here.
func (g *Gate) Close() error {
	g.mu.Lock()
	g.closed = true
	g.wake()
	run := !g.cleaning
	g.cleaning = true
	g.mu.Unlock()
	if run {
		go g.cleanup()
	}
	return nil
}

// cleanup runs once: the hard stop first, then the wait for the writer's exit, then the usual close (which
// frees the run's slot), and only then the outcome, handed to the recorder on a goroutine of its own.
func (g *Gate) cleanup() {
	g.stopTimer()
	g.cfg.Stop()
	stopped := g.cfg.Now()
	g.mu.Lock()
	up := g.writerUp
	g.mu.Unlock()
	if up {
		<-g.writerEnd
	}
	_ = g.Conn.Close()
	g.mu.Lock()
	out, st := g.out, g.st
	g.mu.Unlock()
	out.StopReturned = stopped
	switch st {
	case idle:
		out.Kind = KindUnused
	case queued:
		out.Kind = KindRefused
	case started:
		out.Kind = KindInterrupted
	}
	go g.cfg.Record(out)
}
