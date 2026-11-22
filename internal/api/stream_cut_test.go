// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive streamCut against real servers: HTTP/1 on a TCP listener and HTTP/2 over TLS. A client
// that has stopped reading is either a real one (the HTTP/2 client never reads the body, so flow control
// blocks the server) or a stalled connection (stallConn: every server write waits, honouring its deadline,
// as a socket whose peer stopped reading does).

// stallConn is a server-side connection whose writes can be stalled. A stalled write waits until the write
// deadline passes, the connection closes or the stall lifts, and every write that reaches the socket is
// recorded with its time.
type stallConn struct {
	net.Conn
	mu       sync.Mutex
	stalled  bool
	slow     time.Duration // while not stalled, each write first waits this long, honouring its deadline
	deadline time.Time
	changed  chan struct{} // closed and replaced on every change
	closed   bool
	writes   []stallWrite
}

type stallWrite struct {
	at time.Time // when the bytes went to the socket
	n  int
}

func newStallConn(c net.Conn) *stallConn { return &stallConn{Conn: c, changed: make(chan struct{})} }

func (c *stallConn) bump() { close(c.changed); c.changed = make(chan struct{}) }

func (c *stallConn) stall(on bool) { c.mu.Lock(); c.stalled = on; c.bump(); c.mu.Unlock() }

// setSlow makes every later write take d, as a reader making slow progress does.
func (c *stallConn) setSlow(d time.Duration) { c.mu.Lock(); c.slow = d; c.bump(); c.mu.Unlock() }

func (c *stallConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.bump()
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *stallConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.bump()
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *stallConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.bump()
	c.mu.Unlock()
	return c.Conn.Close()
}

func (c *stallConn) Write(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		if !c.deadline.IsZero() && !time.Now().Before(c.deadline) {
			c.mu.Unlock()
			return 0, errStallTimeout
		}
		if !c.stalled && c.slow > 0 {
			slow, dl := c.slow, c.deadline
			c.mu.Unlock()
			wait := slow
			if !dl.IsZero() && time.Until(dl) < wait {
				time.Sleep(max(time.Until(dl), 0))
				return 0, errStallTimeout
			}
			time.Sleep(wait)
			c.mu.Lock()
		}
		if !c.stalled {
			c.mu.Unlock()
			at := time.Now()
			n, err := c.Conn.Write(p)
			c.mu.Lock()
			c.writes = append(c.writes, stallWrite{at, n})
			c.mu.Unlock()
			return n, err
		}
		ch, dl := c.changed, c.deadline
		c.mu.Unlock()
		var timer <-chan time.Time
		if !dl.IsZero() {
			t := time.NewTimer(time.Until(dl))
			timer = t.C
			defer t.Stop()
		}
		select {
		case <-ch:
		case <-timer:
		}
	}
}

// deliveredAfter counts the bytes that went to the socket at or after t.
func (c *stallConn) deliveredAfter(t time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, w := range c.writes {
		if !w.at.Before(t) {
			n += w.n
		}
	}
	return n
}

var errStallTimeout = &net.OpError{Op: "write", Err: timeoutErr{}}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type stallListener struct {
	net.Listener
	conns chan *stallConn
}

func (l *stallListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	sc := newStallConn(c)
	select {
	case l.conns <- sc:
	default:
	}
	return sc, nil
}

// cutTestServer serves h over HTTP/1, or HTTP/2 over TLS, on a listener whose connections can be stalled.
// Each accepted connection is sent on conns while it has room.
func cutTestServer(t *testing.T, h2 bool, conns chan *stallConn, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = &stallListener{Listener: srv.Listener, conns: conns}
	if h2 {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv
}

func protoName(h2 bool) string {
	if h2 {
		return "HTTP2"
	}
	return "HTTP1"
}

// cutStream builds a stream's cut in a handler: its context, the cut and the outer writer.
func cutStream(w http.ResponseWriter, r *http.Request, end time.Time) (context.Context, *streamCut, http.ResponseWriter) {
	ctx, cancel := context.WithCancel(r.Context())
	c := newStreamCut(w, end, cancel)
	return ctx, c, c.Writer(w)
}

func passCheck(context.Context) error { return nil }

var errTestRevoked = errors.New("revoked")

// writeUntilError writes 32 KiB chunks through w, flushing each, until one fails, and returns when it did.
func writeUntilError(w http.ResponseWriter) (time.Time, error) {
	chunk := []byte(strings.Repeat("x", 32<<10))
	rc := http.NewResponseController(w)
	for {
		if _, err := w.Write(chunk); err != nil {
			return time.Now(), err
		}
		if err := rc.Flush(); err != nil {
			return time.Now(), err
		}
	}
}

// openUnread starts a request and never reads its body. HTTP/2's flow control then blocks the server.
func openUnread(t *testing.T, srv *httptest.Server) {
	t.Helper()
	var mu sync.Mutex
	var body io.Closer
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if body != nil {
			_ = body.Close()
		}
	})
	go func() {
		if resp, err := srv.Client().Get(srv.URL); err == nil {
			mu.Lock()
			body = resp.Body
			mu.Unlock()
		}
	}()
}

func cutWithin(t *testing.T, what string, got, want time.Time, slack time.Duration) {
	t.Helper()
	if got.Before(want.Add(-slack)) || got.After(want.Add(slack)) {
		t.Errorf("%s at %v from the mark, want within %v of it", what, got.Sub(want), slack)
	}
}

// TestStreamCut_AbsoluteDeadlineCutsBlockedWriter: the stream's end passes while its client is not reading,
// and the blocked write ends at it, on both protocols.
func TestStreamCut_AbsoluteDeadlineCutsBlockedWriter(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(protoName(h2), func(t *testing.T) {
			end := time.Now().Add(400 * time.Millisecond)
			failed := make(chan time.Time, 1)
			conns := make(chan *stallConn, 64)
			srv := cutTestServer(t, h2, conns, func(w http.ResponseWriter, r *http.Request) {
				ctx, c, cw := cutStream(w, r, end)
				c.watch(ctx, time.Now().Add(time.Hour), time.Hour, time.Second, passCheck)
				defer c.End()
				if !h2 {
					(<-conns).stall(true)
				}
				at, _ := writeUntilError(cw)
				failed <- at
			})
			openUnread(t, srv)
			select {
			case at := <-failed:
				cutWithin(t, "the blocked write failed", at, end, 150*time.Millisecond)
			case <-time.After(5 * time.Second):
				t.Fatal("the blocked write outlived the stream's end")
			}
		})
	}
}

// TestStreamCut_FallbackWriteBeforeEndStopsAtEnd: with the concurrent interrupt off, a write started 1 s
// before the end to a client that is not reading fails at the end, and nothing is written after it.
func TestStreamCut_FallbackWriteBeforeEndStopsAtEnd(t *testing.T) {
	streamCutNoInterrupt.Store(true)
	t.Cleanup(func() { streamCutNoInterrupt.Store(false) })
	for _, h2 := range []bool{false, true} {
		t.Run(protoName(h2), func(t *testing.T) {
			end := time.Now().Add(time.Second)
			failed := make(chan time.Time, 1)
			var sc *stallConn
			conns := make(chan *stallConn, 64)
			srv := cutTestServer(t, h2, conns, func(w http.ResponseWriter, r *http.Request) {
				ctx, c, cw := cutStream(w, r, end)
				c.watch(ctx, time.Now().Add(time.Hour), time.Hour, time.Second, passCheck)
				defer c.End()
				if !h2 {
					sc = <-conns
					sc.stall(true)
				}
				at, _ := writeUntilError(cw)
				failed <- at
				if _, err := cw.Write([]byte("late")); !errors.Is(err, errStreamCut) {
					t.Errorf("a write after the end = %v, want errStreamCut", err)
				}
			})
			openUnread(t, srv)
			select {
			case at := <-failed:
				cutWithin(t, "the fallback write failed", at, end, 150*time.Millisecond)
			case <-time.After(5 * time.Second):
				t.Fatal("the fallback write outlived the stream's end")
			}
			if sc != nil {
				if n := sc.deliveredAfter(end.Add(time.Millisecond)); n != 0 {
					t.Errorf("%d bytes were written after the end", n)
				}
			}
		})
	}
}

// TestStreamCut_LookupPanicCutsBlocked200And101: a lookup that panics while a streaming 200's client is not
// reading, and after a 101, cuts each stream within one tick plus the bound, and the process stays up.
func TestStreamCut_LookupPanicCutsBlocked200And101(t *testing.T) {
	const tick, bound = 150 * time.Millisecond, 100 * time.Millisecond
	panicky := func(context.Context) error { panic("lookup failed hard") }
	t.Run("streaming 200", func(t *testing.T) {
		failed := make(chan time.Time, 1)
		var opened time.Time
		conns := make(chan *stallConn, 64)
		srv := cutTestServer(t, false, conns, func(w http.ResponseWriter, r *http.Request) {
			ctx, c, cw := cutStream(w, r, time.Now().Add(time.Hour))
			opened = time.Now()
			c.watch(ctx, opened.Add(tick), tick, bound, panicky)
			defer c.End()
			(<-conns).stall(true)
			at, _ := writeUntilError(cw)
			failed <- at
		})
		openUnread(t, srv)
		select {
		case at := <-failed:
			if at.Sub(opened) > tick+bound+100*time.Millisecond {
				t.Errorf("cut %v after open, want within %v", at.Sub(opened), tick+bound)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a panicking lookup did not cut the blocked 200")
		}
	})
	t.Run("after a 101", func(t *testing.T) {
		closed := make(chan time.Time, 1)
		var opened time.Time
		conns := make(chan *stallConn, 64)
		srv := cutTestServer(t, false, conns, func(w http.ResponseWriter, r *http.Request) {
			ctx, c, cw := cutStream(w, r, time.Now().Add(time.Hour))
			opened = time.Now()
			c.watch(ctx, opened.Add(tick), tick, bound, panicky)
			defer c.End()
			sc := <-conns
			conn, brw, err := cw.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\n")
			_ = brw.Flush()
			sc.stall(true)
			chunk := []byte(strings.Repeat("y", 32<<10))
			for {
				if _, err := conn.Write(chunk); err != nil {
					closed <- time.Now()
					return
				}
			}
		})
		raw := dialUpgrade(t, srv)
		defer raw.Close()
		select {
		case at := <-closed:
			if at.Sub(opened) > tick+bound+100*time.Millisecond {
				t.Errorf("cut %v after open, want within %v", at.Sub(opened), tick+bound)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a panicking lookup did not cut the switched stream")
		}
	})
}

// dialUpgrade opens a raw HTTP/1 connection asking to switch protocols and never reads.
func dialUpgrade(t *testing.T, srv *httptest.Server) net.Conn {
	t.Helper()
	raw, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(raw, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	return raw
}

// switchedCut is a cut already switched onto conn, as Hijack leaves it.
func switchedCut(conn net.Conn, end time.Time, cancel context.CancelFunc) *streamCut {
	c := newStreamCut(httptest.NewRecorder(), end, cancel)
	c.switched, c.conn = true, conn
	return c
}

// TestStreamCut_SlowCloserDoesNotDelayCut: a closer that blocks 15 s does not hold Cut: the socket is closed
// and the context cancelled at once.
func TestStreamCut_SlowCloserDoesNotDelayCut(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := switchedCut(a, time.Now().Add(time.Hour), cancel)
	c.addCloser(func() { time.Sleep(15 * time.Second) })
	start := time.Now()
	c.Cut()
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Cut took %v with a slow closer", d)
	}
	if ctx.Err() == nil {
		t.Error("the stream's context was not cancelled")
	}
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := b.Read(make([]byte, 1)); err == nil || errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		t.Errorf("the socket is still open after Cut: read = %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// deadlineConn records the write deadline each write runs under; a write whose first byte is 'B' blocks
// until released or until its deadline passes.
type deadlineConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
	seen     map[byte]time.Time
	release  chan struct{}
	changed  chan struct{}
}

func (d *deadlineConn) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	d.deadline = t
	close(d.changed)
	d.changed = make(chan struct{})
	d.mu.Unlock()
	return nil
}

func (d *deadlineConn) SetDeadline(t time.Time) error { return d.SetWriteDeadline(t) }
func (d *deadlineConn) Close() error                  { return nil }

func (d *deadlineConn) current() time.Time { d.mu.Lock(); defer d.mu.Unlock(); return d.deadline }

func (d *deadlineConn) Write(p []byte) (int, error) {
	d.mu.Lock()
	d.seen[p[0]] = d.deadline
	d.mu.Unlock()
	if p[0] != 'B' {
		return len(p), nil
	}
	for {
		d.mu.Lock()
		dl, ch := d.deadline, d.changed
		d.mu.Unlock()
		if !dl.IsZero() && !time.Now().Before(dl) {
			return 0, errStallTimeout
		}
		select {
		case <-d.release:
			return len(p), nil
		case <-ch:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestStreamCut_ConcurrentWritesKeepTheirDeadline: two writers on one switched connection. One finishing
// while the other is blocked leaves the blocked one's deadline in place; a third that starts meanwhile never
// moves that deadline later; and a cut during a write is not undone by that write's clean-up.
func TestStreamCut_ConcurrentWritesKeepTheirDeadline(t *testing.T) {
	dc := &deadlineConn{seen: map[byte]time.Time{}, release: make(chan struct{}), changed: make(chan struct{})}
	c := switchedCut(dc, time.Now().Add(time.Hour), func() {})
	cc := &cutConn{Conn: dc, c: c}
	streamOpWriteNS.Store(int64(2 * time.Second))
	t.Cleanup(func() { streamOpWriteNS.Store(0) })

	blocked := make(chan error, 1)
	go func() { _, err := cc.Write([]byte("B")); blocked <- err }()
	for dc.current().IsZero() {
		time.Sleep(time.Millisecond)
	}
	held := dc.current()
	time.Sleep(50 * time.Millisecond) // the third operation's own min(now + 2 s, end) is later than held
	if _, err := cc.Write([]byte("A")); err != nil {
		t.Fatalf("a short write: %v", err)
	}
	if got := dc.current(); !got.Equal(held) {
		t.Errorf("a write that finished while another was blocked left the deadline at %v, want %v", got, held)
	}
	if got := dc.seen['A']; got.After(held) {
		t.Errorf("an operation that started while another was blocked ran under %v, later than %v", got, held)
	}
	c.Cut()
	if err := <-blocked; err == nil {
		t.Fatal("the blocked write survived the cut")
	}
	if got := dc.current(); got.IsZero() || got.After(time.Now()) {
		t.Errorf("the cut's past deadline was undone by the blocked write's clean-up: deadline %v", got)
	}
	if _, err := cc.Write([]byte("C")); !errors.Is(err, errStreamCut) {
		t.Errorf("a write after the cut = %v, want errStreamCut", err)
	}
}

// probeWriter records every controller call that reaches the server's writer after the handler returned.
type probeWriter struct {
	http.ResponseWriter
	returned *atomic.Bool
	late     *atomic.Int64
}

func (p probeWriter) note() {
	if p.returned.Load() {
		p.late.Add(1)
	}
}

func (p probeWriter) SetWriteDeadline(t time.Time) error {
	p.note()
	return http.NewResponseController(p.ResponseWriter).SetWriteDeadline(t)
}

func (p probeWriter) FlushError() error {
	p.note()
	return http.NewResponseController(p.ResponseWriter).Flush()
}

func (p probeWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }

// TestStreamCut_HandlerEndRacesCut: the handler returns at the instant the authority fails, 1,000 times, on
// HTTP/1 and HTTP/2. No panic but the abort escapes, no controller call follows the return, a cut HTTP/1
// connection is closed, and a connection reused after an uncut stream has no deadline and a live context.
func TestStreamCut_HandlerEndRacesCut(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(protoName(h2), func(t *testing.T) {
			var late atomic.Int64
			var escaped atomic.Value
			srv := cutTestServer(t, h2, nil, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/after" {
					if r.Context().Err() != nil {
						t.Error("a reused connection's request started cancelled")
					}
					_, _ = io.WriteString(w, "ok")
					return
				}
				var returned atomic.Bool
				pw := probeWriter{ResponseWriter: w, returned: &returned, late: &late}
				defer func() {
					returned.Store(true)
					if p := recover(); p != nil {
						if p != http.ErrAbortHandler {
							escaped.Store(p)
						}
						panic(p)
					}
				}()
				ctx, c, cw := cutStream(pw, r, time.Now().Add(time.Hour))
				c.watch(ctx, time.Now(), time.Hour, time.Second, func(context.Context) error { return errTestRevoked })
				defer c.End()
				_, _ = io.WriteString(cw, "body")
			})
			cl := srv.Client()
			for i := 0; i < 1000; i++ {
				resp, err := cl.Get(srv.URL + "/race")
				if err != nil {
					continue // cut: the connection was closed
				}
				_, rerr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if rerr != nil {
					continue
				}
				resp, err = cl.Get(srv.URL + "/after")
				if err != nil {
					t.Fatalf("a request after an uncut stream failed: %v", err)
				}
				b, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || string(b) != "ok" {
					t.Fatalf("a reused connection after an uncut stream answered %q, %v", b, err)
				}
			}
			if p := escaped.Load(); p != nil {
				t.Errorf("a panic escaped the handler: %v", p)
			}
			if n := late.Load(); n != 0 {
				t.Errorf("%d controller calls followed the handler's return", n)
			}
		})
	}
	t.Run("HTTP1 cut connection closed", func(t *testing.T) {
		srv := cutTestServer(t, false, nil, func(w http.ResponseWriter, r *http.Request) {
			ctx, c, cw := cutStream(w, r, time.Now().Add(time.Hour))
			c.watch(ctx, time.Now(), time.Hour, time.Second, func(context.Context) error { return errTestRevoked })
			defer c.End()
			for !c.isCut() {
				time.Sleep(time.Millisecond)
			}
			_, _ = io.WriteString(cw, "body")
		})
		raw, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		_, _ = io.WriteString(raw, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		_ = raw.SetReadDeadline(time.Now().Add(3 * time.Second))
		if b, err := io.ReadAll(raw); isTimeout(err) || len(b) != 0 {
			t.Errorf("a cut connection was not closed with nothing sent: %q, %v", b, err)
		}
	})
}

// TestStreamCut_EndWaitsForRunningLookup: the handler returns while a lookup that will fail is running. End
// waits for it, the stream is cut, and the server's finish never starts: the client never gets a complete
// answer.
func TestStreamCut_EndWaitsForRunningLookup(t *testing.T) {
	aborted := make(chan bool, 1)
	srv := cutTestServer(t, false, nil, func(w http.ResponseWriter, r *http.Request) {
		started := make(chan struct{})
		ctx, c, cw := cutStream(w, r, time.Now().Add(time.Hour))
		c.watch(ctx, time.Now(), time.Hour, time.Second, func(context.Context) error {
			close(started)
			time.Sleep(200 * time.Millisecond)
			return errTestRevoked
		})
		defer func() {
			p := recover()
			aborted <- p == http.ErrAbortHandler
			if p != nil {
				panic(p)
			}
		}()
		defer c.End()
		<-started
		_, _ = io.WriteString(cw, "short") // chunked: complete only with the server's terminator
	})
	resp, err := srv.Client().Get(srv.URL)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Error("the client got a complete answer from a stream cut during End")
	}
	if !<-aborted {
		t.Error("End returned without cutting while a failing lookup ran")
	}
}

// TestStreamCut_FinishAfterReturnBoundedByEnd: the end falls while the server finishes, after the handler,
// a header-only answer, a buffered body and a chunked answer with trailers, to a client that stops reading as
// the handler returns: nothing goes to the socket after the end. A reading client gets each answer complete,
// on one reused connection, on HTTP/1 and HTTP/2.
func TestStreamCut_FinishAfterReturnBoundedByEnd(t *testing.T) {
	shapes := map[string]func(w http.ResponseWriter){
		"header only":   func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
		"buffered body": func(w http.ResponseWriter) { _, _ = io.WriteString(w, strings.Repeat("b", 1000)) },
		"chunked with trailers": func(w http.ResponseWriter) {
			w.Header().Set("Trailer", "X-Done")
			_, _ = io.WriteString(w, "chunk")
			_ = http.NewResponseController(w).Flush()
			w.Header().Set("X-Done", "yes")
		},
	}
	for name, shape := range shapes {
		t.Run("HTTP1/"+name+"/not reading", func(t *testing.T) {
			end := time.Now().Add(300 * time.Millisecond)
			stalled := make(chan *stallConn, 1)
			conns := make(chan *stallConn, 64)
			srv := cutTestServer(t, false, conns, func(w http.ResponseWriter, r *http.Request) {
				sc := <-conns
				ctx, c, cw := cutStream(w, r, end)
				c.watch(ctx, time.Now().Add(time.Hour), time.Hour, time.Second, passCheck)
				defer func() { sc.stall(true); stalled <- sc }() // after End: the client stops reading for the finish
				defer c.End()
				shape(cw)
			})
			go func() {
				if resp, err := srv.Client().Get(srv.URL); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			}()
			var sc *stallConn
			select {
			case sc = <-stalled:
			case <-time.After(5 * time.Second):
				t.Fatal("the handler did not return")
			}
			time.Sleep(time.Until(end) + 300*time.Millisecond)
			sc.stall(false)
			time.Sleep(100 * time.Millisecond)
			if n := sc.deliveredAfter(end.Add(time.Millisecond)); n != 0 {
				t.Errorf("%d bytes went to the socket after the end", n)
			}
		})
	}
	for _, h2 := range []bool{false, true} {
		t.Run(protoName(h2)+"/reading client reuses the connection", func(t *testing.T) {
			srv := cutTestServer(t, h2, nil, func(w http.ResponseWriter, r *http.Request) {
				if r.Context().Err() != nil {
					t.Error("a request on a reused connection started cancelled")
				}
				ctx, c, cw := cutStream(w, r, time.Now().Add(time.Hour))
				c.watch(ctx, time.Now().Add(time.Hour), time.Hour, time.Second, passCheck)
				defer c.End()
				shapes[r.URL.Query().Get("shape")](cw)
			})
			cl := srv.Client()
			var conns atomic.Int64
			for i := 0; i < 2; i++ {
				for name := range shapes {
					ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
						if !i.Reused {
							conns.Add(1)
						}
					}})
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?shape="+strings.ReplaceAll(name, " ", "+"), nil)
					resp, err := cl.Do(req)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if _, err := io.ReadAll(resp.Body); err != nil {
						t.Fatalf("%s: the body was cut short: %v", name, err)
					}
					_ = resp.Body.Close()
					if name == "chunked with trailers" && resp.Trailer.Get("X-Done") != "yes" {
						t.Errorf("%s: trailer lost: %v", name, resp.Trailer)
					}
				}
			}
			if n := conns.Load(); n != 1 {
				t.Errorf("%d connections for six answers, want one reused", n)
			}
		})
	}
}

// TestStreamCut_CleanSwitchedCloseSkipsFinish: an unrevoked HTTP/1 101 that the client closes normally. No
// flush and no deadline reaches the server's writer after the hijack, nothing panics, and the watch is joined.
func TestStreamCut_CleanSwitchedCloseSkipsFinish(t *testing.T) {
	var late atomic.Int64
	joined := make(chan bool, 1)
	srv := cutTestServer(t, false, nil, func(w http.ResponseWriter, r *http.Request) {
		var hijacked atomic.Bool
		pw := probeWriter{ResponseWriter: w, returned: &hijacked, late: &late}
		ctx, c, cw := cutStream(pw, r, time.Now().Add(time.Hour))
		c.watch(ctx, time.Now().Add(time.Hour), time.Hour, time.Second, passCheck)
		defer func() {
			select {
			case <-c.exited:
				joined <- recover() == nil
			default:
				joined <- false
			}
		}()
		defer c.End()
		conn, brw, err := cw.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		hijacked.Store(true)
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(io.Discard, brw)
	})
	raw := dialUpgrade(t, srv)
	line, _ := bufio.NewReader(raw).ReadString('\n')
	if !strings.Contains(line, "101") {
		t.Fatalf("no 101: %q", line)
	}
	_ = raw.Close()
	select {
	case ok := <-joined:
		if !ok {
			t.Error("End panicked or returned before joining the watch")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never returned")
	}
	if n := late.Load(); n != 0 {
		t.Errorf("%d flush or deadline calls reached the server's writer after the hijack", n)
	}
}

// TestUIRelayIssuerPortal_CutDuringHijackFlushClosesSocket: the cut lands while the server's Hijack is
// flushing the buffered 101 (as websocket.Accept leaves it) to a client that is not reading. The flush ends at
// once, the connection is closed, and it is never registered.
func TestUIRelayIssuerPortal_CutDuringHijackFlushClosesSocket(t *testing.T) {
	conns := make(chan *stallConn, 4)
	result := make(chan error, 1)
	cuts := make(chan *streamCut, 1)
	srv := cutTestServer(t, false, conns, func(w http.ResponseWriter, r *http.Request) {
		sc := <-conns
		ctx, c, cw := cutStream(w, r, time.Now().Add(time.Hour))
		cuts <- c
		c.watch(ctx, time.Now().Add(200*time.Millisecond), time.Hour, time.Second, func(context.Context) error { return errTestRevoked })
		defer c.End()
		sc.stall(true)
		cw.WriteHeader(http.StatusSwitchingProtocols)
		_, _, err := cw.(http.Hijacker).Hijack()
		result <- err
	})
	raw := dialUpgrade(t, srv)
	defer raw.Close()
	start := time.Now()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a hijack whose flush the cut interrupted returned a connection")
		}
		if d := time.Since(start); d > 600*time.Millisecond {
			t.Errorf("the hijack's flush ended %v after the request, want at the cut (200ms)", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hijack's flush outlived the cut")
	}
	cut := <-cuts
	cut.mu.Lock()
	registered := cut.conn != nil
	cut.mu.Unlock()
	if registered {
		t.Error("a connection switched after the cut was registered")
	}
	_ = raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.Copy(io.Discard, raw); isTimeout(err) {
		t.Error("the connection is still open")
	}
}

// TestStreamCut_BatchDeadlineSharedAcrossWrites: a handler sets one deadline for a batch (as the run-events
// loop does). Two writes make slow progress and succeed; then the client stops reading and the batch's flush
// blocks. The flush has only what is left of the batch's one budget, not a fresh per-operation one, and a batch
// with no successful write fails the test.
func TestStreamCut_BatchDeadlineSharedAcrossWrites(t *testing.T) {
	conns := make(chan *stallConn, 4)
	type result struct {
		start, flushed time.Time
		writes         int
		flushErr, err  error
	}
	done := make(chan result, 1)
	const budget, slow = 600 * time.Millisecond, 50 * time.Millisecond // a chunk takes a few socket writes
	srv := cutTestServer(t, false, conns, func(w http.ResponseWriter, r *http.Request) {
		sc := <-conns
		_, c, cw := cutStream(w, r, time.Now().Add(time.Hour))
		defer c.End()
		if err := http.NewResponseController(cw).Flush(); err != nil {
			done <- result{err: err}
			return
		}
		sc.setSlow(slow)
		start := time.Now()
		if err := c.SetWriteDeadline(start.Add(budget)); err != nil {
			done <- result{err: err}
			return
		}
		res := result{start: start}
		chunk := []byte(strings.Repeat("x", 32<<10))
		for ; res.writes < 2; res.writes++ {
			if _, err := cw.Write(chunk); err != nil {
				break
			}
		}
		sc.stall(true) // the client stops reading
		_, _ = cw.Write([]byte("tail"))
		res.flushErr = http.NewResponseController(cw).Flush()
		res.flushed = time.Now()
		done <- res
		_ = c.SetWriteDeadline(time.Time{})
	})
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() { _, _ = io.Copy(io.Discard, resp.Body) }()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.writes != 2 {
			t.Fatalf("%d of 2 slow writes succeeded inside the %v batch: no progress before the blocked flush", r.writes, budget)
		}
		if r.flushErr == nil {
			t.Fatal("the blocked flush succeeded")
		}
		if d := r.flushed.Sub(r.start); d < budget-50*time.Millisecond || d > budget+250*time.Millisecond {
			t.Errorf("the blocked flush returned %v after the batch began, want at its %v deadline", d, budget)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked flush outlived its batch's deadline by seconds: it had a fresh budget")
	}
}

// wrappedConn is a connection wrapper that gives up its connection, as the desktop's frame-checked reader does.
type wrappedConn struct {
	net.Conn
	closed bool
}

func (w *wrappedConn) Close() error      { w.closed = true; return w.Conn.Close() }
func (w *wrappedConn) NetConn() net.Conn { return w.Conn }

// TestStreamCut_CloseRawConnUnwrapsWrappers: a cut closes the transport under every wrapper that gives up its
// connection, never through a wrapper's own Close (for TLS, the one that sends close_notify first): a wrapper
// over a wrapper over the socket, and a TLS connection over the socket.
func TestStreamCut_CloseRawConnUnwrapsWrappers(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	raw := &wrappedConn{Conn: a} // the socket: records its Close
	mid := &wrappedConn{Conn: struct{ net.Conn }{raw}}
	outer := &wrappedConn{Conn: mid}
	closeRawConn(outer)
	if outer.closed || mid.closed {
		t.Errorf("a wrapper's own Close ran (outer %v, middle %v)", outer.closed, mid.closed)
	}
	if !raw.closed {
		t.Error("the transport under both wrappers was not closed")
	}
	c, d := net.Pipe()
	defer d.Close()
	raw2 := &wrappedConn{Conn: c}
	closeRawConn(&wrappedConn{Conn: tls.Client(struct{ net.Conn }{raw2}, &tls.Config{InsecureSkipVerify: true})}) // #nosec G402 -- never handshakes
	if !raw2.closed {
		t.Error("the transport under a TLS connection was not closed")
	}
}

// recWriter is a server writer that records the controller's calls: each write deadline, the flushes and writes
// that reach it, and, on Hijack, the deadline in force for the server's flush.
type recWriter struct {
	header http.Header
	conn   net.Conn

	mu        sync.Mutex
	deadline  time.Time
	atHijack  time.Time
	flushes   int
	writes    int
	hijacked  bool
	deadlines int
}

func newRecWriter(conn net.Conn) *recWriter { return &recWriter{header: http.Header{}, conn: conn} }

func (w *recWriter) Header() http.Header { return w.header }
func (w *recWriter) WriteHeader(int)     {}
func (w *recWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	w.mu.Unlock()
	return len(p), nil
}

func (w *recWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadline = t
	w.deadlines++
	w.mu.Unlock()
	return nil
}

func (w *recWriter) FlushError() error {
	w.mu.Lock()
	w.flushes++
	w.mu.Unlock()
	return nil
}

func (w *recWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.mu.Lock()
	w.atHijack, w.hijacked = w.deadline, true
	w.mu.Unlock()
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func (w *recWriter) state() (deadline, atHijack time.Time, flushes, writes int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deadline, w.atHijack, w.flushes, w.writes
}

// TestStreamCut_HijackFlushUnderDeadlineAndFlushRefusedAfter: the server's flush of the buffered header runs under
// the per-operation deadline, and once the stream has switched a Flush is refused without reaching the server.
func TestStreamCut_HijackFlushUnderDeadlineAndFlushRefusedAfter(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	rw := newRecWriter(a)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newStreamCut(rw, time.Now().Add(time.Hour), cancel)
	cw := c.Writer(rw)
	start := time.Now()
	if _, _, err := cw.(http.Hijacker).Hijack(); err != nil {
		t.Fatalf("hijack: %v", err)
	}
	_, at, _, _ := rw.state()
	if at.IsZero() || at.After(start.Add(streamOpWrite+time.Second)) {
		t.Errorf("the server's hijack flush ran under deadline %v, want one within %v", at, streamOpWrite)
	}
	if err := cw.(interface{ FlushError() error }).FlushError(); !errors.Is(err, errStreamCut) {
		t.Errorf("a flush after the switch: %v, want refused", err)
	}
	if _, _, flushes, _ := rw.state(); flushes != 0 {
		t.Errorf("a flush after the switch reached the server's writer %d times", flushes)
	}
}

// TestStreamCut_RefusedOnceCutOrPastEnd: once cut, the handler's own deadline is refused and the past deadline
// stands; past the end, a write is refused before it reaches the server.
func TestStreamCut_RefusedOnceCutOrPastEnd(t *testing.T) {
	rw := newRecWriter(nil)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newStreamCut(rw, time.Now().Add(time.Hour), cancel)
	c.Cut()
	if err := c.SetWriteDeadline(time.Now().Add(time.Minute)); !errors.Is(err, errStreamCut) {
		t.Errorf("a handler deadline after the cut: %v, want refused", err)
	}
	if d, _, _, _ := rw.state(); !d.Equal(streamPast) {
		t.Errorf("the write deadline after the cut is %v, want the past one", d)
	}

	rw2 := newRecWriter(nil)
	c2 := newStreamCut(rw2, time.Now().Add(-time.Millisecond), cancel)
	if _, err := c2.Writer(rw2).Write([]byte("x")); !errors.Is(err, errStreamCut) {
		t.Errorf("a write past the end: %v, want refused", err)
	}
	if _, _, _, writes := rw2.state(); writes != 0 {
		t.Errorf("a write past the end reached the server's writer %d times", writes)
	}
}

// TestStreamCut_LookupDeadlineClampedToEnd: a lookup's context never outlasts the stream's end, whatever its bound.
func TestStreamCut_LookupDeadlineClampedToEnd(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	end := time.Now().Add(150 * time.Millisecond)
	c := newStreamCut(newRecWriter(nil), end, cancel)
	var dl time.Time
	c.lookup(context.Background(), time.Hour, func(ctx context.Context) error { dl, _ = ctx.Deadline(); return nil })
	if dl.IsZero() || dl.After(end) {
		t.Errorf("the lookup ran under deadline %v, past the end %v", dl, end)
	}
}

// TestStreamCut_EndTimerCutsWithNoCheckDue: with no check due before it, the end alone cuts the stream and ends
// its context.
func TestStreamCut_EndTimerCutsWithNoCheckDue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	end := time.Now().Add(100 * time.Millisecond)
	c := newStreamCut(newRecWriter(nil), end, cancel)
	c.watch(ctx, time.Now().Add(time.Hour), time.Hour, time.Second, passCheck)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the stream outlived its end by 2 s with no check due")
	}
	if !c.isCut() {
		t.Error("the stream's context ended but it was not cut")
	}
	cutWithin(t, "the end timer", time.Now(), end, 300*time.Millisecond)
	func() {
		defer func() { _ = recover() }() // a cut unswitched stream aborts its handler
		c.End()
	}()
}

// deadlineFirstConn records whether a past deadline was set on it before it was closed.
type deadlineFirstConn struct {
	net.Conn
	mu        sync.Mutex
	past      bool
	pastFirst bool
}

func (d *deadlineFirstConn) SetDeadline(t time.Time) error {
	d.mu.Lock()
	d.past = d.past || !t.After(time.Now())
	d.mu.Unlock()
	return d.Conn.SetDeadline(t)
}

func (d *deadlineFirstConn) Close() error {
	d.mu.Lock()
	d.pastFirst = d.past
	d.mu.Unlock()
	return d.Conn.Close()
}

// TestStreamCut_SwitchedCutStopsBothDirectionsBeforeClose: a cut gives a switched connection a past deadline in
// both directions before it closes the transport, so a read or write in progress returns at the cut.
func TestStreamCut_SwitchedCutStopsBothDirectionsBeforeClose(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	conn := &deadlineFirstConn{Conn: a}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	switchedCut(conn, time.Now().Add(time.Hour), cancel).Cut()
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !conn.pastFirst {
		t.Error("the switched connection was closed with no past deadline set first")
	}
}

// tlsNetConn is a wrapper of the server's hijacked TLS connection that gives it up, as the desktop's frame-checked
// reader does.
type tlsNetConn struct{ net.Conn }

func (c tlsNetConn) NetConn() net.Conn { return c.Conn }

// wrapHijack hands the cut the server's connection inside tlsNetConn.
type wrapHijack struct{ http.ResponseWriter }

func (v *wrapHijack) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(v.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return tlsNetConn{conn}, brw, nil
}

// countConn counts the bytes read through it.
type countConn struct {
	net.Conn
	n atomic.Int64
}

func (c *countConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// TestStreamCut_IdleTLSCutSendsNothing: a cut of an idle switched TLS stream, the server's own connection and the
// same connection inside a wrapper, closes the socket under TLS: not one byte (no close_notify) reaches the client
// after the cut began.
func TestStreamCut_IdleTLSCutSendsNothing(t *testing.T) {
	for _, wrap := range []bool{false, true} {
		t.Run(map[bool]string{false: "server's conn", true: "wrapped conn"}[wrap], func(t *testing.T) {
			cutNow, cutDone := make(chan struct{}), make(chan struct{})
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var base http.ResponseWriter = w
				if wrap {
					base = &wrapHijack{w}
				}
				_, cancel := context.WithCancel(r.Context())
				defer cancel()
				c := newStreamCut(base, time.Now().Add(time.Hour), cancel)
				cw := c.Writer(base)
				cw.WriteHeader(http.StatusSwitchingProtocols)
				if _, _, err := cw.(http.Hijacker).Hijack(); err != nil {
					t.Errorf("hijack: %v", err)
					close(cutDone)
					return
				}
				<-cutNow
				c.Cut()
				close(cutDone)
				c.End()
			}))
			srv.StartTLS()
			defer srv.Close()
			raw, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			cc := &countConn{Conn: raw}
			conn := tls.Client(cc, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) // #nosec G402 -- the test server's own certificate
			defer conn.Close()
			if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(conn)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("reading the 101: %v", err)
				}
				if line == "\r\n" {
					break
				}
			}
			time.Sleep(300 * time.Millisecond) // session tickets and anything else sent after the handshake
			mark := cc.n.Load()
			close(cutNow)
			<-cutDone
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, _ = io.Copy(io.Discard, br)
			if n := cc.n.Load() - mark; n != 0 {
				t.Errorf("%d bytes reached the client after the cut began", n)
			}
		})
	}
}
