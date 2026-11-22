// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// A stream opened through an issuer portal (a UI relay's streaming answer or 101, a terminal, a desktop, the
// run-events stream) has one streamCut. Its authority is asked again on a goroutine of its own, beside the
// stream, and a failed check, a lookup that panics or the stream's known end cuts it at once, whether or not a
// write is blocked on a client that stopped reading:
//
//   - a stream that has not switched protocols gets a write deadline in the past, so a blocked write returns
//     and the handler ends with http.ErrAbortHandler, which makes the server close the connection;
//   - a switched connection (HTTP/1 101, a websocket) gets a past deadline in both directions, then its raw
//     transport is closed, with no TLS close_notify to wait for.
//
// Every operation that can block on the client runs under a write deadline no later than the stream's end,
// so nothing started before the end runs past it even without the concurrent interrupt. The mutex guards
// state and deadline calls only, never a write, a flush, a hijack or a close.
//
// One goroutine, the authority watch, calls Cut; End joins it before the handler returns, so no Cut and no
// ResponseController call follows the return (a controller used after its handler returns is a nil
// dereference on HTTP/2).

// errStreamCut is what an operation on a cut, ended or expired stream answers.
var errStreamCut = errors.New("api: this stream was cut: its authority ended or it reached its end")

// errStreamLookupPanic is a lookup that panicked: a failed check, so the stream is cut.
var errStreamLookupPanic = errors.New("api: a stream's authority check panicked")

const (
	// streamOpWrite bounds one write, flush, interim header or hijack flush, and never past the stream's end.
	streamOpWrite = 30 * time.Second
	// streamFinishWrite bounds what the server writes after the handler returns (the chunked terminator,
	// trailers, the last buffered bytes), and never past the stream's end.
	streamFinishWrite = 5 * time.Second
)

// streamOpWriteNS replaces streamOpWrite when set, and streamCutNoInterrupt turns the concurrent interrupt
// off so the per-operation deadlines alone bound a blocked client: tests only.
var (
	streamOpWriteNS      atomic.Int64
	streamCutNoInterrupt atomic.Bool
	// streamCutHijackHook runs as a switch begins, before anything is checked: tests only, so a cut can be
	// landed between a backend's 101 and the hijack.
	streamCutHijackHook atomic.Pointer[func(*streamCut)]
)

// streamPast is a deadline already passed: a pending or later write returns at once.
var streamPast = time.Unix(1, 0)

type streamCut struct {
	rc      *http.ResponseController // the server's writer's, never used once End has joined the watch
	cancel  context.CancelFunc       // the stream's context
	hardEnd time.Time                // the stream's known end, wall clock, fixed at admission

	mu       sync.Mutex
	live     bool // the handler has not started returning
	cut      bool // sticky
	inflight int  // wrapped operations running
	deadline time.Time
	batch    time.Time // the handler's own deadline (SetWriteDeadline), zero for none; operations never outlast it
	ending   bool
	switched bool // the server's Hijack returned without error; sticky
	watching bool
	conn     net.Conn // the switched connection, once registered
	closers  []func()

	done   chan struct{} // closed by End: the watch starts no new lookup
	exited chan struct{} // closed when the watch returns
}

// newStreamCut is the cut for a stream whose handler writes to w, ends at hardEnd and runs under a context
// cancel ends. The watch is not running until watch is called.
func newStreamCut(w http.ResponseWriter, hardEnd time.Time, cancel context.CancelFunc) *streamCut {
	return &streamCut{rc: http.NewResponseController(w), cancel: cancel, hardEnd: hardEnd, live: true,
		done: make(chan struct{}), exited: make(chan struct{})}
}

// watch runs the stream's authority on its own goroutine: check at first, then every every after it, each
// lookup bounded by bound and never past the end, and the end itself on a timer. The first check that does
// not pass, a lookup that panics or outlives its bound, and the end each cut the stream once.
func (c *streamCut) watch(ctx context.Context, first time.Time, every, bound time.Duration, check func(context.Context) error) {
	c.mu.Lock()
	c.watching = true
	c.mu.Unlock()
	go func() {
		defer close(c.exited)
		end := time.NewTimer(time.Until(c.hardEnd))
		defer end.Stop()
		tick := time.NewTimer(time.Until(first))
		defer tick.Stop()
		next := first
		for {
			select {
			case <-c.done:
				if !time.Now().Before(c.hardEnd) {
					c.Cut()
				}
				return
			case <-end.C:
				c.Cut()
				return
			case <-tick.C:
			}
			if c.isEnding() {
				return // End began: it waits only for a lookup already running
			}
			if !c.lookup(ctx, bound, check) {
				c.Cut()
				return
			}
			next = next.Add(every) // a tick already passed runs at once
			tick.Reset(time.Until(next))
		}
	}()
}

// lookup runs one check under its own deadline, the earlier of bound and the end, on a context End never
// cancels. A panic is recovered and logged, and is a failed check.
func (c *streamCut) lookup(parent context.Context, bound time.Duration, check func(context.Context) error) bool {
	dl := time.Now().Add(bound)
	if c.hardEnd.Before(dl) {
		dl = c.hardEnd
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(parent), dl)
	defer cancel()
	res := make(chan error, 1) // buffered: a check that outlives its bound still has somewhere to answer
	go func() {
		defer func() {
			if p := recover(); p != nil {
				slog.ErrorContext(ctx, "api: a stream's authority check panicked; the stream is cut", "panic", p)
				res <- errStreamLookupPanic
			}
		}()
		res <- check(ctx)
	}()
	select {
	case err := <-res:
		return err == nil
	case <-ctx.Done():
		return false
	}
}

func (c *streamCut) isEnding() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ending
}

// Cut ends the stream: once, and only while its handler runs. The watch calls it, and so does an issuer
// portal terminal's own check before an opening its ticket's check does not cover (attachPromotionCheck).
func (c *streamCut) Cut() {
	c.mu.Lock()
	if c.cut || !c.live {
		c.mu.Unlock()
		return
	}
	c.cut = true
	interrupt := !streamCutNoInterrupt.Load()
	if interrupt && !c.switched {
		_ = c.rc.SetWriteDeadline(streamPast)
	}
	conn, closers := c.conn, c.closers
	c.closers = nil
	c.mu.Unlock()
	if !interrupt {
		c.cancel()
		return
	}
	c.cutSwitched(conn, closers)
}

// cutSwitched is Cut's steps after the state change: a switched connection stops in both directions, the
// stream's context ends (the relay then closes its backend connection), the raw transport closes, and the
// closers start on their own goroutine, never waited for.
func (c *streamCut) cutSwitched(conn net.Conn, closers []func()) {
	if conn != nil {
		_ = conn.SetDeadline(streamPast)
	}
	c.cancel()
	if conn != nil {
		closeRawConn(conn)
	}
	if len(closers) > 0 {
		go func() {
			for _, f := range closers {
				f()
			}
		}()
	}
}

// netConner is a connection that wraps another and gives it up: *tls.Conn, and a wrapper of the server's
// hijacked connection (the desktop's frame-checked reader) that must not hide the transport under it.
type netConner interface{ NetConn() net.Conn }

// closeRawConn closes the transport under conn, through every wrapper that gives up its connection: for TLS the
// connection beneath it, because tls.Conn.Close first sends close_notify under a fresh write deadline, and the
// peer could still deliver input meanwhile.
func closeRawConn(conn net.Conn) {
	for {
		w, ok := conn.(netConner)
		if !ok {
			break
		}
		conn = w.NetConn()
	}
	_ = conn.Close()
}

// addCloser registers f to run, unwaited, when the stream is cut; at once if it already was.
func (c *streamCut) addCloser(f func()) {
	c.mu.Lock()
	if !c.cut {
		c.closers = append(c.closers, f)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	go f()
}

// isCut reports whether the stream was cut.
func (c *streamCut) isCut() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cut
}

// End is the handler's last step, deferred: a stream that was not cut and did not switch flushes under its
// authority, the watch is joined (a lookup already running finishes and may still cut), and then a cut stream
// that did not switch aborts the handler so the server closes its connection, while any other gets a short
// deadline, never past the end, for what the server writes after the handler returns. A switched stream does
// neither: the server no longer owns its connection.
func (c *streamCut) End() {
	c.mu.Lock()
	flush := !c.cut && c.live && !c.switched
	c.mu.Unlock()
	if flush {
		_ = c.op(c.rc.Flush)
	}
	c.mu.Lock()
	c.ending = true
	watching := c.watching
	c.mu.Unlock()
	close(c.done)
	if watching {
		<-c.exited
	}
	c.mu.Lock()
	c.live = false
	cut, switched := c.cut, c.switched
	c.mu.Unlock()
	if switched {
		return
	}
	if cut {
		panic(http.ErrAbortHandler)
	}
	d := time.Now().Add(streamFinishWrite)
	if c.hardEnd.Before(d) {
		d = c.hardEnd
	}
	_ = c.rc.SetWriteDeadline(d)
}

// op runs f, one operation that can block on the client, under the per-operation rule: refused once the
// stream is cut, ended or past its end; otherwise under a write deadline of min(now + streamOpWrite, end),
// never later than the one an operation already in flight runs under nor the handler's own (SetWriteDeadline);
// and once no other operation runs and the stream was not cut meanwhile, the deadline goes back to the
// handler's own, or none.
func (c *streamCut) op(f func() error) error {
	if err := c.begin(); err != nil {
		return err
	}
	defer c.finish()
	return f()
}

func (c *streamCut) begin() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.cut || !c.live || !now.Before(c.hardEnd) {
		return errStreamCut
	}
	d := now.Add(time.Duration(cmp.Or(streamOpWriteNS.Load(), int64(streamOpWrite))))
	if c.hardEnd.Before(d) {
		d = c.hardEnd
	}
	if c.inflight > 0 && c.deadline.Before(d) {
		d = c.deadline
	}
	if !c.batch.IsZero() && c.batch.Before(d) {
		d = c.batch
	}
	c.inflight++
	c.deadline = d
	c.setWriteDeadlineLocked(d)
	return nil
}

func (c *streamCut) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight--
	if c.inflight == 0 && !c.cut {
		c.deadline = c.batch // the handler's own deadline stands until it clears it
		c.setWriteDeadlineLocked(c.batch)
	}
}

// setWriteDeadlineLocked sets the write deadline on the switched connection, or through the controller.
func (c *streamCut) setWriteDeadlineLocked(t time.Time) {
	if c.switched {
		if c.conn != nil {
			_ = c.conn.SetWriteDeadline(t)
		}
		return
	}
	_ = c.rc.SetWriteDeadline(t)
}

// SetWriteDeadline is a handler's own deadline (the run-events loop's per batch), never past the end: refused
// once the stream is cut or ended, and a deadline set at or past the end is refused too, so no batch starts
// after it. It stands until the handler clears it with the zero time: every operation meanwhile runs under it as
// well as its own, so the writes and the flush of one batch share one budget.
func (c *streamCut) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cut || !c.live || c.switched {
		return errStreamCut
	}
	if !t.IsZero() {
		if !time.Now().Before(c.hardEnd) {
			return errStreamCut
		}
		if c.hardEnd.Before(t) {
			t = c.hardEnd
		}
	}
	c.batch = t
	if c.inflight > 0 && !c.deadline.IsZero() && (t.IsZero() || c.deadline.Before(t)) {
		return nil // an operation in flight keeps its earlier deadline; finish restores this one
	}
	c.deadline = t
	return c.rc.SetWriteDeadline(t)
}

// Writer is the stream's outer writer over w, the writer newStreamCut was given.
func (c *streamCut) Writer(w http.ResponseWriter) http.ResponseWriter {
	return &cutWriter{ResponseWriter: w, c: c}
}

// cutWriter takes every operation that can block on the client through the per-operation rule: Write,
// Flush, a 1xx other than 101 (the server writes and flushes one at once) and Hijack (the server flushes the
// buffered header first: for a websocket, that is the 101). A final header only buffers, and passes.
type cutWriter struct {
	http.ResponseWriter
	c *streamCut
}

func (w *cutWriter) Write(p []byte) (int, error) {
	n := 0
	err := w.c.op(func() error {
		var err error
		n, err = w.ResponseWriter.Write(p)
		return err
	})
	return n, err
}

func (w *cutWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		_ = w.c.op(func() error { w.ResponseWriter.WriteHeader(code); return nil })
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

// FlushError refuses on a switched stream: the server has released the response's buffer.
func (w *cutWriter) FlushError() error {
	w.c.mu.Lock()
	switched := w.c.switched
	w.c.mu.Unlock()
	if switched {
		return errStreamCut
	}
	return w.c.op(w.c.rc.Flush)
}

func (w *cutWriter) Flush() { _ = w.FlushError() }

func (w *cutWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack switches the stream without holding the mutex across the server's Hijack: refused once cut or past
// the end; the server's flush runs under the per-operation deadline; a cut that landed meanwhile closes the
// connection at once and is never registered. The connection returned is wrapped, and so is the writer of the
// bufio.ReadWriter, whose reader stays the server's (it may hold bytes already read).
func (w *cutWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c := w.c
	if hook := streamCutHijackHook.Load(); hook != nil {
		(*hook)(c)
	}
	if err := c.begin(); err != nil {
		return nil, nil, err
	}
	conn, brw, err := c.rc.Hijack()
	c.mu.Lock()
	c.inflight--
	if err != nil {
		if c.inflight == 0 && !c.cut {
			c.deadline = time.Time{}
			_ = c.rc.SetWriteDeadline(time.Time{})
		}
		c.mu.Unlock()
		return nil, nil, err
	}
	c.switched = true
	if c.cut {
		c.mu.Unlock()
		if streamCutNoInterrupt.Load() {
			closeRawConn(conn)
		} else {
			c.cutSwitched(conn, nil)
		}
		return nil, nil, errStreamCut
	}
	c.conn = conn
	c.deadline = time.Time{}
	c.mu.Unlock()
	cc := &cutConn{Conn: conn, c: c}
	return cc, bufio.NewReadWriter(brw.Reader, bufio.NewWriter(cc)), nil
}

// cutConn is a switched connection whose every write takes the per-operation rule.
type cutConn struct {
	net.Conn
	c *streamCut
}

func (cc *cutConn) Write(p []byte) (int, error) {
	n := 0
	err := cc.c.op(func() error {
		var err error
		n, err = cc.Conn.Write(p)
		return err
	})
	return n, err
}
