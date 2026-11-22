// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package dispatchgate

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errStopped = errors.New("test: the exec was stopped")

// testClock is the gate's clock and timer: the test moves the time and fires the expiry.
type testClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*testTimer
}

type testTimer struct {
	mu      sync.Mutex
	f       func()
	stopped bool
}

func newClock() *testClock { return &testClock{now: time.Unix(1_000_000, 0)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *testClock) AfterFunc(_ time.Duration, f func()) func() bool {
	t := &testTimer{f: f}
	c.mu.Lock()
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	return func() bool {
		t.mu.Lock()
		defer t.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// expire moves the clock to by and fires every live timer on a goroutine of its own, as time.AfterFunc does.
func (c *testClock) expire(by time.Time) {
	c.set(by)
	c.mu.Lock()
	ts := append([]*testTimer(nil), c.timers...)
	c.mu.Unlock()
	for _, t := range ts {
		t.mu.Lock()
		live := !t.stopped
		t.stopped = true
		t.mu.Unlock()
		if live {
			go t.f()
		}
	}
}

// events is an ordered log of what the fakes and the gate's callbacks did, with wall times.
type events struct {
	mu   sync.Mutex
	list []string
	at   []time.Time
}

func (e *events) add(name string) {
	e.mu.Lock()
	e.list, e.at = append(e.list, name), append(e.at, time.Now())
	e.mu.Unlock()
}

// index is name's first position, or -1.
func (e *events) index(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, n := range e.list {
		if n == name {
			return i
		}
	}
	return -1
}

// last is name's last position, or -1.
func (e *events) last(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.list) - 1; i >= 0; i-- {
		if e.list[i] == name {
			return i
		}
	}
	return -1
}

func (e *events) when(name string) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, n := range e.list {
		if n == name {
			return e.at[i]
		}
	}
	return time.Time{}
}

func (e *events) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.list, ",")
}

// model is one driver's stdin path as the gate sees it: conn is the uiConn (Write is the driver write, Close
// the usual close), stop the hard stop. Every model logs driver-enter, driver-return, stop and usual-close.
type model struct {
	conn net.Conn
	stop func()
	ev   *events
}

// fakeConn is a net.Conn over a driver write and a usual close; Read waits for the close.
type fakeConn struct {
	write  func([]byte) (int, error)
	close  func()
	ev     *events
	once   sync.Once
	closed chan struct{}
}

func newFakeConn(ev *events, write func([]byte) (int, error), closeFn func()) *fakeConn {
	return &fakeConn{write: write, close: closeFn, ev: ev, closed: make(chan struct{})}
}

func (c *fakeConn) Write(p []byte) (int, error) {
	c.ev.add("driver-enter")
	n, err := c.write(p)
	c.ev.add("driver-return")
	return n, err
}

func (c *fakeConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *fakeConn) Close() error {
	c.once.Do(func() {
		c.ev.add("usual-close")
		c.close()
		close(c.closed)
		c.ev.add("slot-release")
	})
	return nil
}

func (c *fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// socketModel is Docker's: a socket whose far end reads nothing; Close wakes a blocked Write.
func socketModel(t *testing.T) *model {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	far := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			far <- c
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case f := <-far:
			_ = f.Close()
		default:
		}
	})
	ev := &events{}
	return &model{ev: ev, stop: func() { ev.add("stop"); _ = c.Close() },
		conn: newFakeConn(ev, c.Write, func() { _ = c.Close() })}
}

// pipeModel is Kubernetes': an io.Pipe read by another goroutine (the stream copier); PipeWriter.Close wakes a
// blocked Write. read, when started, counts the bytes that crossed the pipe.
type pipeModel struct {
	*model
	pr   *io.PipeReader
	pw   *io.PipeWriter
	got  atomic.Int64
	done chan struct{}
}

func newPipeModel() *pipeModel {
	pr, pw := io.Pipe()
	ev := &events{}
	m := &pipeModel{pr: pr, pw: pw, done: make(chan struct{})}
	m.model = &model{ev: ev, stop: func() { ev.add("stop"); _ = pw.CloseWithError(errStopped) },
		conn: newFakeConn(ev, pw.Write, func() { _ = pw.Close() })}
	return m
}

// read starts the reader that drains the pipe until it closes.
func (m *pipeModel) read() {
	go func() {
		defer close(m.done)
		buf := make([]byte, 4096)
		for {
			n, err := m.pr.Read(buf)
			m.got.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
}

// vmModel is the VM's: Stdin.Write sends frames into the guest agent's request-body pipe under upMu, the
// graceful Stdin.Close sends its frame under the same upMu, and Session.Close (the hard stop) cancels and closes
// the pipe without it. atH counts the bytes the pipe took (exact); Write reports whole payload frames only.
type vmModel struct {
	*model
	upMu    sync.Mutex
	pr      *io.PipeReader
	pw      *io.PipeWriter
	atH     atomic.Int64
	frame   int
	closing atomic.Bool
}

func newVMModel(frame int) *vmModel {
	pr, pw := io.Pipe()
	ev := &events{}
	m := &vmModel{pr: pr, pw: pw, frame: frame}
	sessClose := func() { m.closing.Store(true); _ = pw.CloseWithError(errStopped) }
	m.model = &model{ev: ev, stop: func() { ev.add("stop"); sessClose() },
		conn: newFakeConn(ev, m.stdinWrite, func() {
			m.stdinClose()
			sessClose()
		})}
	return m
}

func (m *vmModel) stdinWrite(p []byte) (int, error) {
	m.upMu.Lock()
	defer m.upMu.Unlock()
	done := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), m.frame)]
		buf := append([]byte{byte(len(chunk) >> 8), byte(len(chunk))}, chunk...)
		n, err := m.pw.Write(buf)
		m.atH.Add(int64(n))
		if err != nil {
			return done, err // a partial frame's count is dropped
		}
		done += len(chunk)
		p = p[len(chunk):]
	}
	return done, nil
}

// stdinClose is the graceful close: one frame under upMu, so it waits behind a blocked stdinWrite.
func (m *vmModel) stdinClose() {
	m.ev.add("stdin-close-enter")
	m.upMu.Lock()
	defer m.upMu.Unlock()
	n, _ := m.pw.Write([]byte{0, 0})
	m.atH.Add(int64(n))
	m.ev.add("stdin-close")
}

// remoteModel is the remote runner's: a DATA frame waits for the link's write lock and checks the stream's
// reset there; Abort marks the reset with no lock; the usual close's ExecClose call also waits for the lock.
type remoteModel struct {
	*model
	link    *sync.Mutex
	aborted atomic.Bool
	atH     atomic.Int64
	abortAt atomic.Pointer[time.Time]
	// closeFirst: the ExecClose call gets the link's lock before the gate's frame does.
	closeFirst atomic.Bool
	lastH      atomic.Pointer[time.Time]
}

func newRemoteModel(link *sync.Mutex) *remoteModel {
	ev := &events{}
	m := &remoteModel{link: link}
	m.model = &model{ev: ev, stop: func() {
		ev.add("stop")
		m.aborted.Store(true)
		now := time.Now()
		m.abortAt.Store(&now)
	}, conn: newFakeConn(ev, m.dataWrite, func() {
		m.aborted.Store(true)
		if !m.closeFirst.Load() {
			link.Lock() // the ExecClose call
			link.Unlock()
		}
	})}
	return m
}

func (m *remoteModel) dataWrite(p []byte) (int, error) {
	m.link.Lock()
	defer m.link.Unlock()
	if m.aborted.Load() {
		return 0, errStopped
	}
	m.atH.Add(int64(len(p)))
	now := time.Now()
	m.lastH.Store(&now)
	return len(p), nil
}

// harness is one gate over one model, with the callbacks recorded.
type harness struct {
	t        *testing.T
	clock    *testClock
	by       time.Time
	g        *Gate
	m        *model
	refusals atomic.Int32
	refuse   func() // the 503 record; a test may replace it before newGate
	outcome  chan Outcome
	record   func(Outcome)
}

func newHarness(t *testing.T, m *model) *harness {
	h := &harness{t: t, clock: newClock(), m: m, outcome: make(chan Outcome, 1)}
	h.by = h.clock.Now().Add(25 * time.Second)
	h.refuse = func() { h.m.ev.add("refuse") }
	h.record = func(o Outcome) { h.outcome <- o }
	return h
}

func (h *harness) config() Config {
	return Config{By: h.by, Now: h.clock.Now, AfterFunc: h.clock.AfterFunc, Stop: h.m.stop,
		Refuse: func() { h.refusals.Add(1); h.refuse() }, Record: h.record}
}

func (h *harness) gate() *Gate {
	h.g = New(h.m.conn, h.config())
	return h.g
}

func (h *harness) wait() Outcome {
	h.t.Helper()
	select {
	case o := <-h.outcome:
		return o
	case <-time.After(5 * time.Second):
		h.t.Fatalf("no outcome; events %s", h.m.ev)
		return Outcome{}
	}
}

type writeResult struct {
	n   int
	err error
	at  time.Time
}

func goWrite(g *Gate, p []byte) chan writeResult {
	ch := make(chan writeResult, 1)
	go func() {
		n, err := g.Write(p)
		ch <- writeResult{n, err, time.Now()}
	}()
	return ch
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func within(t *testing.T, what string, ch chan writeResult, d time.Duration) writeResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
		return writeResult{}
	}
}

// checkCommon asserts what every expiry subtest shares: the slot is released after the driver write returned
// (the writer's exit), the hard stop ran before the usual close, and an admitted write started before By.
func checkCommon(t *testing.T, h *harness, o Outcome) {
	t.Helper()
	ev := h.m.ev
	if ret, rel := ev.last("driver-return"), ev.index("slot-release"); ret >= 0 && rel < ret {
		t.Errorf("the slot was released before the writer's driver write returned: %s", ev)
	}
	if stop, uc := ev.index("stop"), ev.index("usual-close"); stop < 0 || uc < stop {
		t.Errorf("the usual close did not follow the hard stop: %s", ev)
	}
	if !o.StartedAt.IsZero() && !o.StartedAt.Before(h.by) {
		t.Errorf("started_at %v is not before By %v", o.StartedAt, h.by)
	}
}

func TestDispatchGate_BackpressureAfterOnTimeConnStopped(t *testing.T) {
	const big = 32 << 20
	models := []struct {
		name string
		new  func(t *testing.T) *model
	}{
		{"socket", socketModel},
		{"pipe", func(*testing.T) *model { return newPipeModel().model }},
		{"vm", func(*testing.T) *model { return newVMModel(16 << 10).model }},
	}
	for _, mc := range models {
		t.Run("driver/"+mc.name, func(t *testing.T) {
			h := newHarness(t, mc.new(t))
			g := h.gate()
			res := goWrite(g, make([]byte, big))
			waitFor(t, "the driver write", func() bool { return h.m.ev.index("driver-enter") >= 0 })
			fired := time.Now()
			h.clock.expire(h.by)
			r := within(t, "Write", res, time.Second)
			if r.n != 0 || !errors.Is(r.err, ErrExpired) {
				t.Fatalf("Write = %d, %v; want 0, ErrExpired", r.n, r.err)
			}
			o := h.wait()
			if o.Kind != KindInterrupted || !o.Partial {
				t.Fatalf("outcome %+v; want interrupted, partial", o)
			}
			if w := h.m.ev.when("driver-return").Sub(fired); w > time.Second {
				t.Errorf("W = %s, want <= 1s", w)
			}
			if h.refusals.Load() != 1 {
				t.Errorf("refusals = %d, want 1", h.refusals.Load())
			}
			checkCommon(t, h, o)
		})
	}

	t.Run("driver/remote", func(t *testing.T) {
		var link sync.Mutex
		rm := newRemoteModel(&link)
		h := newHarness(t, rm.model)
		g := h.gate()
		link.Lock()
		res := goWrite(g, []byte("GET / HTTP/1.1\r\n\r\n"))
		waitFor(t, "the driver write", func() bool { return h.m.ev.index("driver-enter") >= 0 })
		fired := time.Now()
		h.clock.expire(h.by)
		r := within(t, "Write, with the link's lock still held,", res, time.Second)
		if !errors.Is(r.err, ErrExpired) {
			t.Fatalf("Write err %v, want ErrExpired", r.err)
		}
		waitFor(t, "the hard stop", func() bool { return rm.abortAt.Load() != nil })
		if w := rm.abortAt.Load().Sub(fired); w > time.Second {
			t.Errorf("W = %s, want <= 1s", w)
		}
		select {
		case o := <-h.outcome:
			t.Fatalf("outcome %+v before the lock freed", o)
		case <-time.After(50 * time.Millisecond):
		}
		link.Unlock()
		o := h.wait()
		if o.Kind != KindInterrupted || rm.atH.Load() != 0 {
			t.Fatalf("outcome %+v, bytes at H %d; want interrupted, 0", o, rm.atH.Load())
		}
		checkCommon(t, h, o)
	})

	t.Run("queued then expired", func(t *testing.T) {
		pm := newPipeModel()
		h := newHarness(t, pm.model)
		hold, entered := make(chan struct{}), make(chan struct{})
		cfg := h.config()
		cfg.BeforeAdmit = func() { close(entered); <-hold }
		g := New(pm.conn, cfg)
		res := goWrite(g, []byte("request"))
		<-entered
		h.clock.expire(h.by.Add(time.Millisecond))
		r := within(t, "Write", res, time.Second)
		if r.n != 0 || !errors.Is(r.err, ErrExpired) {
			t.Fatalf("Write = %d, %v", r.n, r.err)
		}
		close(hold)
		o := h.wait()
		if o.Kind != KindRefused || o.BytesDone != 0 || pm.ev.index("driver-enter") >= 0 {
			t.Fatalf("outcome %+v, events %s; want refused, no driver call", o, pm.ev)
		}
		if h.refusals.Load() != 1 {
			t.Fatalf("refusals = %d, want 1 (the expiry callback)", h.refusals.Load())
		}
	})

	t.Run("start point", func(t *testing.T) {
		pm := newPipeModel()
		pm.read()
		h := newHarness(t, pm.model)
		entry := make(chan time.Time, 1)
		cfg := h.config()
		g := New(newFakeConn(pm.ev, func(p []byte) (int, error) { entry <- h.clock.Now(); return pm.pw.Write(p) }, func() { _ = pm.pw.Close() }), cfg)
		if n, err := g.Write([]byte("hello")); n != 5 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
		at := <-entry
		_ = g.Close()
		o := h.wait()
		if o.Kind != KindDispatched || !o.StartedAt.Before(h.by) || at.Before(o.StartedAt) {
			t.Fatalf("outcome %+v, driver entry %v", o, at)
		}
	})

	t.Run("typed 503 first", func(t *testing.T) {
		for _, st := range []string{"queued", "started"} {
			t.Run(st, func(t *testing.T) {
				pm := newPipeModel()
				h := newHarness(t, pm.model)
				held, release := make(chan struct{}), make(chan struct{})
				h.refuse = func() { close(held); <-release }
				cfg := h.config()
				entered, hold := make(chan struct{}), make(chan struct{})
				if st == "queued" {
					cfg.BeforeAdmit = func() { close(entered); <-hold }
				} else {
					close(entered)
				}
				g := New(pm.conn, cfg)
				res := goWrite(g, []byte("request"))
				<-entered
				if st == "started" {
					waitFor(t, "the driver write", func() bool { return pm.ev.index("driver-enter") >= 0 })
				}
				h.clock.expire(h.by)
				<-held
				select {
				case r := <-res:
					t.Fatalf("Write returned %d, %v while the 503 record was held", r.n, r.err)
				case <-time.After(100 * time.Millisecond):
				}
				close(release)
				if r := within(t, "Write", res, time.Second); !errors.Is(r.err, ErrExpired) {
					t.Fatalf("Write err %v", r.err)
				}
				if st == "queued" {
					close(hold)
				}
				h.wait()
			})
		}
	})

	t.Run("gate already open", func(t *testing.T) {
		vm := newVMModel(8)
		h := newHarness(t, vm.model)
		g := h.gate()
		got := make(chan []byte, 1)
		go func() { buf := make([]byte, 10); _, _ = io.ReadFull(vm.pr, buf); got <- buf }() // one frame of 8
		if n, err := g.Write([]byte("12345678")); n != 8 || err != nil {
			t.Fatalf("first Write = %d, %v", n, err)
		}
		<-got
		res := goWrite(g, []byte("blocked!")) // nothing reads: the VM write blocks under upMu
		waitFor(t, "the second driver write", func() bool {
			vm.ev.mu.Lock()
			defer vm.ev.mu.Unlock()
			c := 0
			for _, n := range vm.ev.list {
				if n == "driver-enter" {
					c++
				}
			}
			return c == 2
		})
		closed := make(chan struct{})
		go func() { _ = g.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Close blocked")
		}
		within(t, "the blocked Write", res, time.Second)
		o := h.wait()
		if o.Kind != KindDispatched {
			t.Fatalf("outcome %+v", o)
		}
		if s, c := vm.ev.index("stop"), vm.ev.index("stdin-close-enter"); s < 0 || c < s {
			t.Fatalf("the graceful close ran before the hard stop: %s", vm.ev)
		}
		checkCommon(t, h, o)
	})

	t.Run("close first", func(t *testing.T) {
		var link sync.Mutex
		var dials, gotConns atomic.Int32
		rm := newRemoteModel(&link)
		h := newHarness(t, rm.model)
		dialed := make(chan *Gate, 4)
		tr := NewTransport(func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			g := New(rm.conn, h.config())
			dialed <- g
			return g, nil
		}, 8)
		link.Lock()
		ctx, cancel := context.WithCancel(context.Background())
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { gotConns.Add(1) }})
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://run:1/", nil)
		done := make(chan error, 1)
		go func() {
			resp, err := tr.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()
		gate := <-dialed
		waitFor(t, "the driver write", func() bool { return rm.ev.index("driver-enter") >= 0 })
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("RoundTrip succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("the handler did not leave RoundTrip within 1s while the lock was held")
		}
		waitFor(t, "the hard stop", func() bool { return rm.abortAt.Load() != nil })
		if _, err := gate.Write([]byte("x")); !errors.Is(err, ErrExpired) || h.refusals.Load() != 0 {
			t.Fatalf("a Write after Close: err %v, refusals %d; want ErrExpired, 0", err, h.refusals.Load())
		}
		link.Unlock()
		o := h.wait()
		if rm.atH.Load() != 0 || dials.Load() != 1 || gotConns.Load() != 1 {
			t.Fatalf("bytes at H %d, dials %d, GotConn %d; want 0, 1, 1 (outcome %+v)", rm.atH.Load(), dials.Load(), gotConns.Load(), o)
		}
		checkCommon(t, h, o)
	})

	t.Run("open then cut", func(t *testing.T) {
		var link sync.Mutex
		rm := newRemoteModel(&link)
		h := newHarness(t, rm.model)
		g := h.gate()
		if _, err := g.Write([]byte("headers")); err != nil {
			t.Fatal(err)
		}
		link.Lock()
		res := goWrite(g, []byte("body"))
		waitFor(t, "the body's driver write", func() bool { return rm.ev.String() == "driver-enter,driver-return,driver-enter" })
		_ = g.Close()
		within(t, "the open gate's Write, with the lock held,", res, time.Second)
		waitFor(t, "the hard stop", func() bool { return rm.abortAt.Load() != nil })
		link.Unlock()
		o := h.wait()
		if rm.atH.Load() != int64(len("headers")) {
			t.Fatalf("bytes at H %d; the body's copy was not dropped", rm.atH.Load())
		}
		checkCommon(t, h, o)
	})

	t.Run("idle then closed", func(t *testing.T) {
		before := runtime.NumGoroutine()
		pm := newPipeModel()
		h := newHarness(t, pm.model)
		g := h.gate()
		_ = g.Close()
		if o := h.wait(); o.Kind != KindUnused {
			t.Fatalf("outcome %+v", o)
		}
		waitFor(t, "the goroutine count", func() bool { return runtime.NumGoroutine() <= before })
		g.mu.Lock()
		up := g.writerUp
		g.mu.Unlock()
		if up {
			t.Fatal("a gate closed before any Write started a writer")
		}

		pm2 := newPipeModel()
		pm2.read()
		h2 := newHarness(t, pm2.model)
		g2 := h2.gate()
		if _, err := g2.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		_ = g2.Close()
		select {
		case <-g2.writerEnd:
		case <-time.After(time.Second):
			t.Fatal("the waiting writer did not exit at Close")
		}
		h2.wait()
	})

	t.Run("slot after writer, recorder held", func(t *testing.T) {
		var link sync.Mutex
		rm := newRemoteModel(&link)
		rm.closeFirst.Store(true)
		h := newHarness(t, rm.model)
		recHeld, recFree := make(chan struct{}), make(chan struct{})
		h.record = func(o Outcome) { close(recHeld); <-recFree; h.outcome <- o }
		g := h.gate()
		link.Lock()
		res := goWrite(g, []byte("request"))
		waitFor(t, "the driver write", func() bool { return rm.ev.index("driver-enter") >= 0 })
		h.clock.expire(h.by)
		within(t, "Write", res, time.Second)
		time.Sleep(20 * time.Millisecond)
		if rm.ev.index("slot-release") >= 0 {
			t.Fatalf("the slot was released while the writer still waited: %s", rm.ev)
		}
		link.Unlock()
		waitFor(t, "the slot's release with the recorder held", func() bool { return rm.ev.index("slot-release") >= 0 })
		<-recHeld
		close(recFree)
		if o := h.wait(); o.Kind != KindInterrupted {
			t.Fatalf("outcome %+v", o)
		}
		checkCommon(t, h, Outcome{})
	})

	t.Run("partial frame", func(t *testing.T) {
		const frame = 16
		vm := newVMModel(frame)
		h := newHarness(t, vm.model)
		g := h.gate()
		read := make(chan int64, 1)
		go func() {
			buf := make([]byte, 2*(frame+2)+2+frame/2) // two whole frames, then half the third's payload
			n, _ := io.ReadFull(vm.pr, buf)
			read <- int64(n)
		}()
		res := goWrite(g, make([]byte, 4*frame))
		got := <-read
		h.clock.expire(h.by)
		within(t, "Write", res, time.Second)
		o := h.wait()
		if o.Kind != KindInterrupted || o.BytesDone != 2*frame || !o.Partial {
			t.Fatalf("outcome %+v; want interrupted, bytes_done %d, partial", o, 2*frame)
		}
		if vm.atH.Load() != got || got != 2*(frame+2)+2+frame/2 {
			t.Fatalf("bytes at H %d, read %d; want the exact count %d", vm.atH.Load(), got, 2*(frame+2)+2+frame/2)
		}
	})

	t.Run("pipe races the close", func(t *testing.T) {
		for _, kind := range []string{"pipe", "vm"} {
			for i := 0; i < 1000; i++ {
				var m *model
				var pr *io.PipeReader
				var atH func() int64
				if kind == "pipe" {
					pm := newPipeModel()
					m, pr = pm.model, pm.pr
					atH = func() int64 { return -1 }
				} else {
					vm := newVMModel(64)
					m, pr, atH = vm.model, vm.pr, vm.atH.Load
				}
				h := newHarness(t, m)
				g := h.gate()
				res := goWrite(g, make([]byte, 256))
				waitFor(t, "the driver write", func() bool { return m.ev.index("driver-enter") >= 0 })
				var got atomic.Int64
				readDone := make(chan struct{})
				go func() { // the reader becomes ready as the close lands
					defer close(readDone)
					n, _ := io.Copy(io.Discard, pr)
					got.Store(n)
				}()
				h.clock.expire(h.by)
				within(t, "Write", res, time.Second)
				_ = g.Close() // as net/http closes the connection after a write in either outcome
				o := h.wait()
				<-readDone
				want := int64(o.BytesDone)
				if kind == "vm" {
					want = atH()
				}
				if got.Load() != want {
					t.Fatalf("%s iteration %d: the reader got %d bytes, the returned writes account for %d", kind, i, got.Load(), want)
				}
			}
		}
	})

	t.Run("another live stream", func(t *testing.T) {
		pm := newPipeModel()
		h := newHarness(t, pm.model)
		other := newPipeModel()
		other.read()
		og := New(other.conn, Config{By: h.by, Now: h.clock.Now, AfterFunc: h.clock.AfterFunc,
			Stop: other.stop, Refuse: func() {}, Record: func(Outcome) {}})
		if _, err := og.Write([]byte("open")); err != nil {
			t.Fatal(err)
		}
		g := h.gate()
		res := goWrite(g, make([]byte, 1024))
		waitFor(t, "the driver write", func() bool { return pm.ev.index("driver-enter") >= 0 })
		h.clock.expire(h.by)
		within(t, "Write", res, time.Second)
		h.wait()
		for i := 0; i < 3; i++ {
			if n, err := og.Write([]byte("more")); n != 4 || err != nil {
				t.Fatalf("the other stream's write %d = %d, %v", i, n, err)
			}
		}
		_ = og.Close()

		vm := newVMModel(64)
		hv := newHarness(t, vm.model)
		second := &vmModel{pr: vm.pr, frame: 64} // a second Session on the same guest client: its own pipe
		spr, spw := io.Pipe()
		second.pw = spw
		go func() { _, _ = io.Copy(io.Discard, spr) }()
		gv := hv.gate()
		res = goWrite(gv, make([]byte, 1024))
		waitFor(t, "the VM driver write", func() bool { return vm.ev.index("driver-enter") >= 0 })
		hv.clock.expire(hv.by)
		within(t, "Write", res, time.Second)
		hv.wait()
		if n, err := second.stdinWrite([]byte("still")); n != 5 || err != nil {
			t.Fatalf("the second VM session's write = %d, %v", n, err)
		}
		_ = spw.Close()
	})
}

func TestDispatchGate_SingleUseTransport(t *testing.T) {
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	var dials atomic.Int32
	refuseFirst := atomic.Bool{}
	tr := NewTransport(func(ctx context.Context, network, _ string) (net.Conn, error) {
		dials.Add(1)
		c, err := (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		if err != nil || !refuseFirst.Load() {
			return c, err
		}
		clock := newClock()
		return New(c, Config{By: clock.Now(), Now: clock.Now, AfterFunc: clock.AfterFunc,
			Stop: func() { _ = c.Close() }, Refuse: func() {}, Record: func(Outcome) {}}), nil
	}, 8)
	t.Cleanup(tr.CloseIdleConnections)
	var gotConns, reused atomic.Int32
	get := func() error {
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
			gotConns.Add(1)
			if i.Reused {
				reused.Add(1)
			}
		}})
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://run:1/", nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Body.Close()
	}
	for i := 0; i < 3; i++ {
		if err := get(); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := get(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if dials.Load() != 6 || gotConns.Load() != 6 || reused.Load() != 0 {
		t.Fatalf("dials %d, GotConn %d, reused %d; want 6, 6, 0", dials.Load(), gotConns.Load(), reused.Load())
	}

	refuseFirst.Store(true)
	dials.Store(0)
	gotConns.Store(0)
	if err := get(); err == nil {
		t.Fatal("a refused first write succeeded")
	}
	if dials.Load() != 1 || gotConns.Load() != 1 {
		t.Fatalf("a refused GET: dials %d, GotConn %d; want 1, 1 (no retry)", dials.Load(), gotConns.Load())
	}
}
