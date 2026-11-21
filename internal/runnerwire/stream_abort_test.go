// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// wireEvent is one entry of a wireLog: a frame the org admitted (it passed its check and entered the
// conn's WriteFrame), a frame the runner's read loop took, or a mark the test made between them.
type wireEvent struct {
	what   string // "admit", "arrive" or a mark
	typ    Type
	stream uint32
	n      int
}

type wireLog struct {
	mu sync.Mutex
	ev []wireEvent
}

func (l *wireLog) add(e wireEvent) {
	l.mu.Lock()
	l.ev = append(l.ev, e)
	l.mu.Unlock()
}

func (l *wireLog) mark(what string, stream uint32) { l.add(wireEvent{what: what, stream: stream}) }

func (l *wireLog) events() []wireEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]wireEvent(nil), l.ev...)
}

// index is the position of the first event matching what, typ and stream at or after from, or -1.
func (l *wireLog) index(from int, what string, typ Type, stream uint32) int {
	ev := l.events()
	for i := max(from, 0); i < len(ev); i++ {
		if ev[i].what == what && ev[i].typ == typ && ev[i].stream == stream {
			return i
		}
	}
	return -1
}

func (l *wireLog) count(what string, typ Type, stream uint32) int {
	n := 0
	for _, e := range l.events() {
		if e.what == what && e.typ == typ && (stream == 0 || e.stream == stream) {
			n++
		}
	}
	return n
}

// heldConn is the org's end of a loopback whose writes the test can stall, as a far end that stops
// reading does: the next frame that matches enters WriteFrame, is logged as admitted, and then waits
// until the test releases it or the link closes.
type heldConn struct {
	*LoopConn
	log     *wireLog
	mu      sync.Mutex
	hold    func(Frame) bool
	entered chan Frame
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
}

// stall holds the next frame that matches; release lets it go.
func (c *heldConn) stall(match func(Frame) bool) (entered <-chan Frame, release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hold, c.entered, c.release = match, make(chan Frame, 1), make(chan struct{})
	rel := c.release
	var once sync.Once
	return c.entered, func() { once.Do(func() { close(rel) }) }
}

func (c *heldConn) WriteFrame(ctx context.Context, f Frame) error {
	c.log.add(wireEvent{what: "admit", typ: f.Type, stream: f.Stream, n: len(f.Payload)})
	c.mu.Lock()
	held := c.hold != nil && c.hold(f)
	entered, release := c.entered, c.release
	if held {
		c.hold = nil
	}
	c.mu.Unlock()
	if held {
		entered <- f
		select {
		case <-release:
		case <-c.closed:
			return ErrLinkDown
		}
	}
	return c.LoopConn.WriteFrame(ctx, f)
}

func (c *heldConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.LoopConn.Close()
}

// arrivalConn logs every frame the runner's read loop takes.
type arrivalConn struct {
	*LoopConn
	log *wireLog
}

func (c *arrivalConn) ReadFrame(ctx context.Context) (Frame, error) {
	f, err := c.LoopConn.ReadFrame(ctx)
	if err == nil {
		c.log.add(wireEvent{what: "arrive", typ: f.Type, stream: f.Stream, n: len(f.Payload)})
	}
	return f, err
}

// abortRig is a real Peer pair whose org writes the test can stall and whose wire it logs. Streams are
// opened by the org; the runner accepts each and hands the test its end.
type abortRig struct {
	org, run *Peer
	oc       *heldConn
	log      *wireLog
	accepted chan *Stream
}

func newAbortRig(t *testing.T, runCfg PeerConfig) *abortRig {
	t.Helper()
	oc, rc := Loopback()
	r := &abortRig{log: &wireLog{}, accepted: make(chan *Stream, MaxStreams+8)}
	r.oc = &heldConn{LoopConn: oc, log: r.log, closed: make(chan struct{})}
	runCfg.OnOpen = func(s *Stream, _ Open) error { r.accepted <- s; return nil }
	r.org = NewPeer(r.oc, PeerConfig{Org: true})
	r.run = NewPeer(&arrivalConn{LoopConn: rc, log: r.log}, runCfg)
	go r.org.Run(context.Background())
	go r.run.Run(context.Background())
	t.Cleanup(func() { r.org.Close(); r.run.Close() })
	return r
}

// open opens one stream from the org and returns both of its ends.
func (r *abortRig) open(t *testing.T) (org, far *Stream) {
	t.Helper()
	s, err := r.org.Open(ctxT(t), Open{Kind: KindRelay})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	select {
	case far = <-r.accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("the runner never accepted the stream")
	}
	if far.ID() != s.ID() {
		t.Fatalf("runner end %d, want %d", far.ID(), s.ID())
	}
	return s, far
}

func (r *abortRig) due() int {
	r.org.mu.Lock()
	defer r.org.mu.Unlock()
	return len(r.org.resetDue)
}

func (r *abortRig) held() int {
	r.org.mu.Lock()
	defer r.org.mu.Unlock()
	return r.org.heldLocked()
}

func dataOf(id uint32) func(Frame) bool {
	return func(f Frame) bool { return f.Type == TypeData && f.Stream == id }
}

// within fails the test when fn has not returned after d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

type writeResult struct {
	n   int
	err error
}

func writeAsync(s *Stream, b []byte) <-chan writeResult {
	ch := make(chan writeResult, 1)
	go func() { n, err := s.Write(b); ch <- writeResult{n, err} }()
	return ch
}

func awaitWrite(t *testing.T, ch <-chan writeResult, what string) writeResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatalf("%s never returned", what)
		return writeResult{}
	}
}

func readN(t *testing.T, s *Stream, n int) []byte {
	t.Helper()
	_ = s.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer func() { _ = s.SetReadDeadline(time.Time{}) }()
	b := make([]byte, n)
	if _, err := io.ReadFull(s, b); err != nil {
		t.Fatalf("read %d bytes of stream %d: %v", n, s.ID(), err)
	}
	return b
}

// openOrStall opens a stream from p and reports whether Open returned within d: a refusal at capacity
// returns at once, while an Open that sent its OPEN waits for a writer the test holds.
func openOrStall(p *Peer, d time.Duration) (bool, error) {
	ch := make(chan error, 1)
	go func() { _, err := p.Open(context.Background(), Open{Kind: KindRelay}); ch <- err }()
	select {
	case err := <-ch:
		return true, err
	case <-time.After(d):
		return false, nil
	}
}

func isReset(err error, code uint32) bool {
	var r *ResetError
	return errors.As(err, &r) && r.Code == code
}

// abortAndMark aborts s and logs the moment Abort returned. Abort must not wait on the stalled writer.
func (r *abortRig) abortAndMark(t *testing.T, s *Stream) {
	t.Helper()
	within(t, time.Second, "Abort", func() { s.Abort(ResetCancelled) })
	r.log.mark("aborted", s.ID())
}

// checkAbortOrder asserts what every drop case shares: no DATA frame of x admitted after its Abort
// returned, its RESET arrived, and no DATA frame of x arrived after that RESET.
func (r *abortRig) checkAbortOrder(t *testing.T, x *Stream) {
	t.Helper()
	m := r.log.index(0, "aborted", 0, x.ID())
	if i := r.log.index(m, "admit", TypeData, x.ID()); i >= 0 {
		t.Fatalf("a DATA frame of stream %d was admitted after Abort returned (event %d, mark %d)", x.ID(), i, m)
	}
	rst := r.log.index(0, "arrive", TypeReset, x.ID())
	if rst < 0 {
		t.Fatalf("no RESET for stream %d arrived", x.ID())
	}
	if i := r.log.index(rst, "arrive", TypeData, x.ID()); i >= 0 {
		t.Fatalf("a DATA frame of stream %d arrived after its RESET", x.ID())
	}
	if !r.org.Online() || !r.run.Online() {
		t.Fatalf("the link went down: org %v, runner %v", r.org.Err(), r.run.Err())
	}
}

// creditBack closes ys, which returns what the far end withheld for their reads, and waits for the
// connection window to be whole again.
func (r *abortRig) creditBack(t *testing.T, ys ...*Stream) {
	t.Helper()
	for _, y := range ys {
		_ = y.Close() // returns what the far end withheld for y's reads
	}
	waitFor(t, 3*time.Second, "the connection window to come back whole", func() bool { return avail(r.org.connSend) == ConnWindow })
}

func TestRunnerwireStream_AbortDropsPendingData(t *testing.T) {
	t.Run("CreditWait", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		x, xFar := r.open(t)
		y, yFar := r.open(t)
		if _, err := y.Write([]byte("a")); err != nil {
			t.Fatal(err)
		}
		// The far end reads nothing of x, so its window is spent and the next Write waits in send.Take.
		if n, err := x.Write(make([]byte, StreamWindow)); err != nil || n != StreamWindow {
			t.Fatalf("fill x's window: %d, %v", n, err)
		}
		res := writeAsync(x, []byte("late"))
		time.Sleep(20 * time.Millisecond)
		r.abortAndMark(t, x)
		w := awaitWrite(t, res, "the credit-waiting Write")
		if w.n != 0 || !isReset(w.err, ResetCancelled) {
			t.Fatalf("Write after Abort = %d, %v; want 0 and the reset", w.n, w.err)
		}
		if _, err := y.Write([]byte("b")); err != nil {
			t.Fatal(err)
		}
		if got := string(readN(t, yFar, 2)); got != "ab" {
			t.Fatalf("y carried %q, want \"ab\"", got)
		}
		waitFor(t, 3*time.Second, "the far end to see x's RESET", func() bool { return xFar.Err() != nil })
		r.checkAbortOrder(t, x)
		r.creditBack(t, y)
	})

	t.Run("WriterWait", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		x, xFar := r.open(t)
		y, yFar := r.open(t)
		entered, release := r.oc.stall(dataOf(y.ID()))
		defer release()
		yRes := writeAsync(y, []byte("a"))
		<-entered // y's frame holds the link's writer, as on a socket the far end stopped reading
		const size = 1000
		res := writeAsync(x, make([]byte, size))
		waitFor(t, 3*time.Second, "x's Write to take its credit", func() bool { return avail(x.send) == StreamWindow-size })
		before := avail(r.org.connSend)
		r.abortAndMark(t, x) // returns while the far end is still stalled
		select {
		case <-yRes:
			t.Fatal("the stalled write finished before the test released it")
		default:
		}
		release()
		w := awaitWrite(t, res, "the writer-waiting Write")
		if w.n != 0 || !isReset(w.err, ResetCancelled) {
			t.Fatalf("Write after Abort = %d, %v; want 0 and the reset", w.n, w.err)
		}
		if got := avail(r.org.connSend); got != before+size {
			t.Fatalf("connection window %d after the drop, want %d: the dropped frame's credit was not returned", got, before+size)
		}
		if yw := awaitWrite(t, yRes, "y's Write"); yw.err != nil {
			t.Fatal(yw.err)
		}
		if _, err := y.Write([]byte("b")); err != nil {
			t.Fatal(err)
		}
		if got := string(readN(t, yFar, 2)); got != "ab" {
			t.Fatalf("y carried %q, want \"ab\"", got)
		}
		waitFor(t, 3*time.Second, "the far end to see x's RESET", func() bool { return xFar.Err() != nil })
		r.checkAbortOrder(t, x)
		if n := r.log.count("arrive", TypeData, x.ID()); n != 0 {
			t.Fatalf("%d DATA frames of x arrived, want none", n)
		}
		r.creditBack(t, y)
	})

	t.Run("FrameAlreadyAdmitted", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		x, xFar := r.open(t)
		y, yFar := r.open(t)
		entered, release := r.oc.stall(dataOf(x.ID()))
		defer release()
		res := writeAsync(x, make([]byte, MaxDataFrame+100))
		<-entered // x's first frame passed its check and is being written
		r.abortAndMark(t, x)
		release()
		w := awaitWrite(t, res, "the two-frame Write")
		if w.n != MaxDataFrame || !isReset(w.err, ResetCancelled) {
			t.Fatalf("Write = %d, %v; want %d (the first frame) and the reset", w.n, w.err, MaxDataFrame)
		}
		if _, err := y.Write([]byte("ab")); err != nil {
			t.Fatal(err)
		}
		if got := string(readN(t, yFar, 2)); got != "ab" {
			t.Fatalf("y carried %q, want \"ab\"", got)
		}
		waitFor(t, 3*time.Second, "the far end to see x's RESET", func() bool { return xFar.Err() != nil })
		first := r.log.index(0, "arrive", TypeData, x.ID())
		rst := r.log.index(0, "arrive", TypeReset, x.ID())
		if first < 0 || first > rst || r.log.events()[first].n != MaxDataFrame {
			t.Fatalf("x's first frame (event %d) must arrive whole before its RESET (event %d)", first, rst)
		}
		if n := r.log.count("arrive", TypeData, x.ID()); n != 1 {
			t.Fatalf("%d DATA frames of x arrived, want only the first", n)
		}
		r.checkAbortOrder(t, x)
		r.creditBack(t, y)
	})

	t.Run("ManyAbortsAtCapacity", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		y, yFar := r.open(t)
		others := make([]*Stream, 0, MaxStreams-1)
		for len(others) < MaxStreams-1 {
			s, _ := r.open(t)
			others = append(others, s)
		}
		entered, release := r.oc.stall(dataOf(y.ID()))
		defer release()
		yRes := writeAsync(y, []byte("a"))
		<-entered
		r.org.mu.Lock()
		quiet := len(r.org.answers) == 0 && r.org.pongDue == nil && r.org.owed == 0
		r.org.mu.Unlock()
		if !quiet {
			t.Fatal("the reply worker has other work queued; this test needs the aborts' own wakes only")
		}
		base := runtime.NumGoroutine()
		within(t, 2*time.Second, "the aborts", func() {
			for _, s := range others {
				s.Abort(ResetCancelled)
			}
		})
		r.log.mark("aborts done", 0)
		if grown := runtime.NumGoroutine() - base; grown > 4 {
			t.Fatalf("%d goroutines started for %d aborts", grown, len(others))
		}
		if d, h := r.due(), r.held(); d != MaxStreams-1 || h != MaxStreams {
			t.Fatalf("%d RESETs due and %d counted, want %d and %d", d, h, MaxStreams-1, MaxStreams)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := r.org.Open(ctx, Open{Kind: KindRelay}); !isReset(err, ResetCapacity) {
			t.Fatalf("Open with every place held = %v, want the capacity reset", err)
		}
		release()
		waitFor(t, 5*time.Second, "every due RESET to arrive", func() bool { return r.log.count("arrive", TypeReset, 0) == len(others) })
		var order []uint32
		for _, e := range r.log.events() {
			if e.what == "arrive" && e.typ == TypeReset {
				order = append(order, e.stream)
			}
		}
		for i, s := range others {
			if order[i] != s.ID() {
				t.Fatalf("RESET %d arrived for stream %d, want %d: not in abort order", i, order[i], s.ID())
			}
		}
		if n := r.log.count("admit", TypeOpen, 0); n != MaxStreams {
			t.Fatalf("%d OPENs written, want %d: the refused Open must send none", n, MaxStreams)
		}
		if yw := awaitWrite(t, yRes, "y's Write"); yw.err != nil {
			t.Fatal(yw.err)
		}
		if _, err := y.Write([]byte("b")); err != nil {
			t.Fatal(err)
		}
		if got := string(readN(t, yFar, 2)); got != "ab" {
			t.Fatalf("y carried %q, want \"ab\"", got)
		}
		waitFor(t, 3*time.Second, "the due set to empty", func() bool { return r.due() == 0 })
		if _, err := r.org.Open(ctxT(t), Open{Kind: KindRelay}); err != nil {
			t.Fatalf("Open once the RESETs are written: %v", err)
		}
	})

	t.Run("PeerResetCrossesADueReset", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		x, xFar := r.open(t)
		z, _ := r.open(t)
		r.org.wmu.Lock() // the writer is held
		x.Abort(ResetCancelled)
		before := r.held()
		_ = xFar.Reset(ResetCancelled) // the peer's RESET for x arrives while ours is due
		waitFor(t, 3*time.Second, "the peer's RESET to retire x's", func() bool { return r.due() == 0 })
		if got := r.held(); got != before-1 {
			t.Fatalf("%d counted after the crossing, want %d", got, before-1)
		}
		r.org.wmu.Unlock()
		z.Abort(ResetCancelled) // the worker writes in order: z's RESET follows any RESET of x's
		waitFor(t, 3*time.Second, "z's RESET", func() bool { return r.log.index(0, "arrive", TypeReset, z.ID()) >= 0 })
		if n := r.log.count("admit", TypeReset, x.ID()); n != 0 {
			t.Fatalf("x's retired RESET was written %d times", n)
		}
	})

	t.Run("LocalOpenAtCapacity", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		x, _ := r.open(t)
		for range MaxStreams - 1 {
			r.open(t)
		}
		r.org.wmu.Lock()
		x.Abort(ResetCancelled) // MaxStreams-1 live and one RESET due
		returned, local := openOrStall(r.org, time.Second)
		r.org.wmu.Unlock()
		if !returned {
			t.Fatal("Open at capacity waited for the writer: it sent an OPEN instead of refusing")
		}
		if !isReset(local, ResetCapacity) {
			t.Fatalf("local Open at capacity = %v, want the capacity reset", local)
		}
		// The same value a full peer's RESET gives the opener.
		var fromPeer error = &ResetError{Code: ResetCapacity}
		if local.Error() != fromPeer.Error() {
			t.Fatalf("local refusal %q, a full peer's %q", local, fromPeer)
		}
		waitFor(t, 3*time.Second, "x's RESET to be written", func() bool { return r.due() == 0 })
		if _, err := r.org.Open(ctxT(t), Open{Kind: KindRelay}); err != nil {
			t.Fatalf("Open once the RESET is written: %v", err)
		}
		if n := r.log.count("admit", TypeOpen, 0); n != MaxStreams+1 {
			t.Fatalf("%d OPENs written, want %d: the refused Open must send none", n, MaxStreams+1)
		}
	})

	t.Run("IncomingOpenAtCapacity", func(t *testing.T) {
		accepted := make(chan *Stream, MaxStreams+8)
		raw := newRaw(t, PeerConfig{Org: true, OnOpen: func(s *Stream, _ Open) error { accepted <- s; return nil }})
		var mu sync.Mutex
		var frames []Frame // every frame the org writes, as the raw end reads them
		go func() {
			for {
				f, err := raw.raw.ReadFrame(context.Background())
				if err != nil {
					return
				}
				mu.Lock()
				frames = append(frames, f)
				mu.Unlock()
			}
		}()
		find := func(what string, match func(Frame) bool) Frame {
			t.Helper()
			var a Frame
			waitFor(t, 3*time.Second, what, func() bool {
				mu.Lock()
				defer mu.Unlock()
				for _, f := range frames {
					if match(f) {
						a = f
						return true
					}
				}
				return false
			})
			return a
		}
		answer := func(id uint32) Frame {
			t.Helper()
			return find("the answer to an OPEN", func(f Frame) bool { return (f.Type == TypeOpenOK || f.Type == TypeReset) && f.Stream == id })
		}
		ids := NewIDAllocator(false)
		relay, _ := json.Marshal(Open{Kind: KindRelay})
		var x *Stream
		for i := range MaxStreams {
			id := ids.Next()
			raw.write(t, Frame{Type: TypeOpen, Stream: id, Payload: relay})
			if f := answer(id); f.Type != TypeOpenOK {
				t.Fatalf("OPEN %d answered %s", i, f.Type)
			}
			s := <-accepted
			if i == 0 {
				x = s
			}
		}
		// The worker stops once it has chosen x's RESET, so the OPEN below is judged with that RESET due
		// and its answer stays queued until the worker moves on.
		entered, release := make(chan struct{}), make(chan struct{})
		hook := func(id uint32) {
			if id == x.ID() {
				close(entered)
				<-release
			}
		}
		dueSelected.Store(&hook)
		t.Cleanup(func() { dueSelected.Store(nil) })
		x.Abort(ResetCancelled) // MaxStreams-1 live and one RESET due
		<-entered
		late := ids.Next()
		raw.write(t, Frame{Type: TypeOpen, Stream: late, Payload: relay})
		waitFor(t, 3*time.Second, "the OPEN's answer to be queued", func() bool {
			raw.p.mu.Lock()
			defer raw.p.mu.Unlock()
			return len(raw.p.answers) == 1
		})
		close(release)
		if f := answer(late); f.Type != TypeReset {
			t.Fatalf("OPEN with one RESET due answered %s, want RESET", f.Type)
		} else if code, _ := DecodeReset(f.Payload); code != ResetCapacity {
			t.Fatalf("OPEN with one RESET due answered code %d, want capacity", code)
		}
		find("x's due RESET", func(f Frame) bool { return f.Type == TypeReset && f.Stream == x.ID() })
		retry := ids.Next()
		raw.write(t, Frame{Type: TypeOpen, Stream: retry, Payload: relay})
		if f := answer(retry); f.Type != TypeOpenOK {
			t.Fatalf("a retry once the RESET is written answered %s, want OPEN_OK", f.Type)
		}
	})

	t.Run("LinkFailsWithResetsDue", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		x, _ := r.open(t)
		z, _ := r.open(t)
		r.org.wmu.Lock()
		x.Abort(ResetCancelled)
		z.Abort(ResetCancelled)
		if d := r.due(); d != 2 {
			t.Fatalf("%d RESETs due, want 2", d)
		}
		r.org.Close()
		if d := r.due(); d != 0 {
			t.Fatalf("%d RESETs still due after the link failed", d)
		}
		r.org.wmu.Unlock()
		time.Sleep(50 * time.Millisecond)
		if n := r.log.count("admit", TypeReset, 0); n != 0 {
			t.Fatalf("%d RESETs written after the link failed", n)
		}
	})

	t.Run("AbortInOnResetsWindow", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		x, xFar := r.open(t)
		z, _ := r.open(t)
		entered, release := make(chan struct{}), make(chan struct{})
		hook := func(id uint32) {
			if id == x.ID() {
				close(entered)
				<-release
			}
		}
		resetUnlocked.Store(&hook)
		t.Cleanup(func() { resetUnlocked.Store(nil) })
		before := r.held()
		_ = xFar.Reset(ResetCancelled)
		<-entered // onReset has removed x and not yet aborted it
		x.Abort(ResetCancelled)
		if d, h := r.due(), r.held(); d != 0 || h != before-1 {
			t.Fatalf("%d due and %d counted after an Abort in onReset's window, want 0 and %d", d, h, before-1)
		}
		close(release)
		z.Abort(ResetCancelled)
		waitFor(t, 3*time.Second, "z's RESET", func() bool { return r.log.index(0, "arrive", TypeReset, z.ID()) >= 0 })
		if n := r.log.count("admit", TypeReset, x.ID()); n != 0 {
			t.Fatalf("a RESET was written for x, which the peer had reset")
		}
	})

	t.Run("AbortAfterThePeersReset", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		x, xFar := r.open(t)
		z, _ := r.open(t)
		_ = xFar.Reset(ResetCancelled)
		waitFor(t, 3*time.Second, "the peer's RESET to reach x", func() bool { return x.Err() != nil })
		before := r.held()
		x.Abort(ResetCancelled)
		if d, h := r.due(), r.held(); d != 0 || h != before {
			t.Fatalf("%d due and %d counted after aborting a stream the peer reset, want 0 and %d", d, h, before)
		}
		z.Abort(ResetCancelled)
		waitFor(t, 3*time.Second, "z's RESET", func() bool { return r.log.index(0, "arrive", TypeReset, z.ID()) >= 0 })
		if n := r.log.count("admit", TypeReset, x.ID()); n != 0 {
			t.Fatalf("a RESET was written for x, which the peer had reset")
		}
	})

	for _, failLink := range []bool{false, true} {
		name := "BarrierAfterSelection"
		if failLink {
			name = "BarrierAfterSelectionThenLinkFails"
		}
		t.Run(name, func(t *testing.T) {
			r := newAbortRig(t, PeerConfig{})
			x, xFar := r.open(t)
			var spare *Stream
			for i := range MaxStreams - 1 {
				s, _ := r.open(t)
				if i == 0 {
					spare = s
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			hook := func(id uint32) {
				if id == x.ID() {
					close(entered)
					<-release
				}
			}
			dueSelected.Store(&hook)
			t.Cleanup(func() { dueSelected.Store(nil) })
			r.org.wmu.Lock() // the writer is held too
			x.Abort(ResetCancelled)
			<-entered // the worker has chosen x's RESET and not yet taken the writer
			returned, err := openOrStall(r.org, time.Second)
			if !returned {
				close(release)
				r.org.wmu.Unlock()
				t.Fatal("Open while x's RESET waits for the writer sent an OPEN: x no longer counted")
			}
			if !isReset(err, ResetCapacity) {
				t.Fatalf("Open while x's RESET waits for the writer = %v, want the capacity reset", err)
			}
			if failLink {
				r.org.Close()
			} else {
				_ = xFar.Reset(ResetCancelled)
			}
			waitFor(t, 3*time.Second, "x's RESET to be retired", func() bool { return r.due() == 0 })
			close(release)
			r.org.wmu.Unlock()
			if failLink {
				time.Sleep(50 * time.Millisecond)
			} else {
				spare.Abort(ResetCancelled)
				waitFor(t, 3*time.Second, "the spare's RESET", func() bool { return r.log.index(0, "arrive", TypeReset, spare.ID()) >= 0 })
			}
			if n := r.log.count("admit", TypeReset, x.ID()); n != 0 {
				t.Fatalf("x's retired RESET was written")
			}
		})
	}

	t.Run("CallBehindAStalledWriter", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{OnCall: echoHandler})
		const every = 100 * time.Millisecond
		go r.org.keepalive(every)
		y, _ := r.open(t)
		entered, release := r.oc.stall(dataOf(y.ID()))
		defer release()
		writeAsync(y, []byte("a"))
		<-entered
		// The far end reads nothing but sends a PONG every interval, so only the stalled-write check can
		// fail the link.
		stopPongs := make(chan struct{})
		defer close(stopPongs)
		go func() {
			tick := time.NewTicker(every)
			defer tick.Stop()
			for {
				select {
				case now := <-tick.C:
					_ = r.run.Send(context.Background(), Frame{Type: TypePong, Payload: EncodePing(now)})
				case <-stopPongs:
					return
				}
			}
		}()
		failed := make(chan time.Time, 1)
		go func() { <-r.org.Done(); failed <- time.Now() }()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		type callResult struct {
			at     time.Time
			online bool
			err    error
		}
		res := make(chan callResult, 1)
		go func() {
			err := r.org.Call(ctx, "echo", map[string]string{"a": "b"}, nil)
			res <- callResult{time.Now(), r.org.Online(), err}
		}()
		var got callResult
		select {
		case got = <-res:
		case <-time.After(5 * time.Second):
			t.Fatal("the call behind the stalled writer never returned")
		}
		failedAt := <-failed
		if !errors.Is(got.err, runner.ErrRunnerOffline) || got.online {
			t.Fatalf("call = %v with the link online %v; want offline, after the link failed", got.err, got.online)
		}
		if d := got.at.Sub(failedAt); d > every {
			t.Fatalf("the call returned %s after the link failed, want within %s", d, every)
		}
		if err := r.org.Err(); err == nil || !strings.Contains(err.Error(), "write stalled") {
			t.Fatalf("the link failed with %v, want the stalled-write check", err)
		}
	})

	t.Run("CreditAfterManyDrops", func(t *testing.T) {
		r := newAbortRig(t, PeerConfig{})
		y, yFar := r.open(t)
		go func() { _, _ = io.Copy(io.Discard, yFar) }()
		start := avail(r.org.connSend)
		for i := range ConnWindow/MaxDataFrame + 4 {
			x, _ := r.open(t)
			entered, release := r.oc.stall(dataOf(y.ID()))
			yRes := writeAsync(y, []byte{1})
			<-entered
			res := writeAsync(x, make([]byte, MaxDataFrame))
			waitFor(t, 3*time.Second, "x's Write to take its credit", func() bool { return avail(x.send) == StreamWindow-MaxDataFrame })
			x.Abort(ResetCancelled)
			release()
			if w := awaitWrite(t, res, "the dropped Write"); w.n != 0 || !isReset(w.err, ResetCancelled) {
				t.Fatalf("drop %d: Write = %d, %v", i, w.n, w.err)
			}
			if yw := awaitWrite(t, yRes, "y's Write"); yw.err != nil {
				t.Fatalf("drop %d: y's Write: %v", i, yw.err)
			}
		}
		yRes := writeAsync(y, make([]byte, MaxDataFrame))
		if yw := awaitWrite(t, yRes, "y's Write after the drops"); yw.err != nil || yw.n != MaxDataFrame {
			t.Fatalf("y's Write after the drops = %d, %v", yw.n, yw.err)
		}
		_ = y.Close()
		waitFor(t, 3*time.Second, "the connection window to come back whole", func() bool { return avail(r.org.connSend) == start })
	})
}
