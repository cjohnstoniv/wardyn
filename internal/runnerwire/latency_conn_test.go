// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// latencyConn is one end of an in-memory link with a pipelined one-way delay: a write stamps the frame
// with its delivery time and returns at once, and the far end's read waits for that time. Many frames are
// in flight at once, as on a long link (SetDelay sleeps inside the write, so it serialises the writer).
type latencyConn struct {
	d      time.Duration
	in     <-chan delayed
	out    chan<- delayed
	closed chan struct{}
	once   *sync.Once
}

type delayed struct {
	b  []byte
	at time.Time
}

// latencyPair returns the two ends of a link that delays every frame by d each way (2d round trip).
func latencyPair(d time.Duration) (org, run Conn) {
	a, b := make(chan delayed, 4096), make(chan delayed, 4096)
	closed, once := make(chan struct{}), &sync.Once{}
	return &latencyConn{d: d, in: a, out: b, closed: closed, once: once},
		&latencyConn{d: d, in: b, out: a, closed: closed, once: once}
}

func (c *latencyConn) WriteFrame(ctx context.Context, f Frame) error {
	b, err := f.Encode(nil)
	if err != nil {
		return err
	}
	select {
	case c.out <- delayed{b, time.Now().Add(c.d)}:
		return nil
	case <-c.closed:
		return ErrLinkDown
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *latencyConn) ReadFrame(ctx context.Context) (Frame, error) {
	var m delayed
	select {
	case m = <-c.in:
	case <-c.closed:
		return Frame{}, ErrLinkDown
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
	if wait := time.Until(m.at); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-c.closed:
			return Frame{}, ErrLinkDown
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
	return Decode(m.b)
}

func (c *latencyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// firstWindowConn counts the DATA bytes its side sends until the first WINDOW frame reaches it.
type firstWindowConn struct {
	Conn
	sent    atomic.Int64
	atFirst atomic.Int64 // -1 until the first WINDOW arrives
}

func (c *firstWindowConn) WriteFrame(ctx context.Context, f Frame) error {
	if f.Type == TypeData {
		c.sent.Add(int64(len(f.Payload)))
	}
	return c.Conn.WriteFrame(ctx, f)
}

func (c *firstWindowConn) ReadFrame(ctx context.Context) (Frame, error) {
	f, err := c.Conn.ReadFrame(ctx)
	if err == nil && f.Type == TypeWindow {
		c.atFirst.CompareAndSwap(-1, c.sent.Load())
	}
	return f, err
}

// relayOverLatency copies total bytes through one relay stream from the org to the runner over conns, both
// peers using window (0: the legacy window), and returns how long the copy took.
func relayOverLatency(tb testing.TB, org, run Conn, window, total int64) time.Duration {
	tb.Helper()
	accepted := make(chan *Stream, 1)
	op := NewPeer(org, PeerConfig{Org: true, StreamWindow: window})
	rp := NewPeer(run, PeerConfig{StreamWindow: window, OnOpen: func(s *Stream, o Open) error { accepted <- s; return nil }})
	go op.Run(context.Background())
	go rp.Run(context.Background())
	defer func() { op.Close(); rp.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s, err := op.Open(ctx, Open{Kind: KindRelay})
	if err != nil {
		tb.Fatal(err)
	}
	var r *Stream
	select {
	case r = <-accepted:
	case <-ctx.Done():
		tb.Fatal("the runner never accepted the stream")
	}
	got := make(chan int64, 1)
	go func() { n, _ := io.Copy(io.Discard, r); got <- n }()
	start := time.Now()
	buf := make([]byte, MaxDataFrame)
	for sent := int64(0); sent < total; sent += int64(len(buf)) {
		if _, err := s.Write(buf); err != nil {
			tb.Fatalf("write: %v", err)
		}
	}
	if err := s.CloseWrite(); err != nil {
		tb.Fatal(err)
	}
	select {
	case n := <-got:
		if n != total {
			tb.Fatalf("relayed %d bytes, want %d", n, total)
		}
	case <-ctx.Done():
		tb.Fatal("the copy did not finish")
	}
	return time.Since(start)
}

const relayBenchBytes = 64 << 20

// The relay copy rate over a 50 ms round trip, with the legacy and the negotiated stream window.
func BenchmarkRelayCopyOverLatency(b *testing.B) {
	for _, bc := range []struct {
		name   string
		window int64
	}{{"window=legacy", 0}, {"window=negotiated", StreamWindowLarge}} {
		b.Run(bc.name, func(b *testing.B) {
			var elapsed time.Duration
			for range b.N {
				org, run := latencyPair(25 * time.Millisecond)
				elapsed += relayOverLatency(b, org, run, bc.window, relayBenchBytes)
			}
			b.ReportMetric(float64(relayBenchBytes)*float64(b.N)/(1<<20)/elapsed.Seconds(), "MiB/s")
		})
	}
}

// A sender sends at most one stream window per round trip: before the first WINDOW comes back it has sent
// no more than the window, and with the negotiated window it sends more than the legacy one.
func TestNegotiatedWindowFillsTheRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name     string
		window   int64
		min, max int64
	}{{"legacy", 0, 1, StreamWindow}, {"negotiated", StreamWindowLarge, StreamWindow + 1, StreamWindowLarge}} {
		t.Run(tc.name, func(t *testing.T) {
			org, run := latencyPair(50 * time.Millisecond)
			spy := &firstWindowConn{Conn: org}
			spy.atFirst.Store(-1)
			relayOverLatency(t, spy, run, tc.window, 2*tc.max)
			if got := spy.atFirst.Load(); got < tc.min || got > tc.max {
				t.Fatalf("%d bytes sent before the first WINDOW, want %d to %d", got, tc.min, tc.max)
			}
		})
	}
}
