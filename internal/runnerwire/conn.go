// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Conn carries whole frames between the two peers. The WebSocket transport (one
// frame per binary message) and the loopback both implement it. WriteFrame and
// ReadFrame may be used from one goroutine each concurrently.
type Conn interface {
	ReadFrame(ctx context.Context) (Frame, error)
	WriteFrame(ctx context.Context, f Frame) error
	Close() error
}

// ErrLinkDown is what a Conn answers once its link is closed or dropped.
var ErrLinkDown = errors.New("runnerwire: link down")

// link is the state shared by the two ends of a loopback.
type link struct {
	mu     sync.Mutex
	delay  time.Duration
	down   bool
	sever  bool
	closed chan struct{}
}

// LoopConn is one end of an in-memory link. It carries encoded bytes, so the
// codec and its size limits are exercised exactly as on a socket, and the link
// carries the fault injection both ends share.
type LoopConn struct {
	l    *link
	in   chan []byte
	out  chan []byte
	once sync.Once
}

// Loopback returns the two ends of an in-memory link: the org's and the runner's.
func Loopback() (org, run *LoopConn) {
	l := &link{closed: make(chan struct{})}
	a, b := make(chan []byte, 64), make(chan []byte, 64)
	return &LoopConn{l: l, in: a, out: b}, &LoopConn{l: l, in: b, out: a}
}

// SetDelay delays every frame written, on either end, by d.
func (c *LoopConn) SetDelay(d time.Duration) {
	c.l.mu.Lock()
	c.l.delay = d
	c.l.mu.Unlock()
}

// Drop takes the link down: every blocked and later read or write on either end
// fails with ErrLinkDown.
func (c *LoopConn) Drop() { c.l.drop() }

// SeverMidFrame makes the next written frame arrive truncated, and then drops
// the link: the peer's ReadFrame answers io.ErrUnexpectedEOF.
func (c *LoopConn) SeverMidFrame() {
	c.l.mu.Lock()
	c.l.sever = true
	c.l.mu.Unlock()
}

func (l *link) drop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.down {
		l.down = true
		close(l.closed)
	}
}

func (c *LoopConn) WriteFrame(ctx context.Context, f Frame) error {
	b, err := f.Encode(nil)
	if err != nil {
		return err
	}
	c.l.mu.Lock()
	down, delay, sever := c.l.down, c.l.delay, c.l.sever
	if sever {
		c.l.sever = false
	}
	c.l.mu.Unlock()
	if down {
		return ErrLinkDown
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-c.l.closed:
			return ErrLinkDown
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if sever {
		b = b[:len(b)/2]
	}
	select {
	case c.out <- b:
	case <-c.l.closed:
		return ErrLinkDown
	case <-ctx.Done():
		return ctx.Err()
	}
	if sever {
		c.l.drop()
		return ErrLinkDown
	}
	return nil
}

func (c *LoopConn) ReadFrame(ctx context.Context) (Frame, error) {
	select {
	case b := <-c.in:
		return Decode(b)
	default:
	}
	select {
	case b := <-c.in:
		return Decode(b)
	case <-c.l.closed:
		// Frames the peer wrote before the link dropped are still delivered.
		select {
		case b := <-c.in:
			return Decode(b)
		default:
			return Frame{}, ErrLinkDown
		}
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

// Close takes the whole link down, as closing a socket does for the peer.
func (c *LoopConn) Close() error {
	c.once.Do(c.l.drop)
	return nil
}

var _ Conn = (*LoopConn)(nil)
