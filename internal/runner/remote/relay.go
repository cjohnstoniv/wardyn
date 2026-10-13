// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// handleRelay hands a runner-opened relay stream to Options.Relay (design
// §4.2). The runner opens a relay toward the org, never the reverse, and names
// no target: the stream carries TLS bytes to the org's own internal listener
// and nothing else.
func (s *Substrate) handleRelay(st *runnerwire.Stream, o runnerwire.Open) error {
	if o.Target != "" || o.Offset != 0 || s.opts.Relay == nil {
		return ErrUnsupportedStream
	}
	return s.opts.Relay(s.id, newRelayConn(st, s.id))
}

// relayConn is one relay stream as a net.Conn, for the org's internal relay
// listener. A read deadline is the stream's own: a Read waiting past it fails
// with os.ErrDeadlineExceeded and the stream stays usable. A stream has no
// write timer, so a write deadline is kept here: when it passes, the stream is
// reset, which unblocks the Write waiting on it with os.ErrDeadlineExceeded and
// ends the relay. A relay whose peer stalls with no write deadline ends with its
// runner's session (every stream is reset then), and runnerwire.MaxStreams
// bounds how many one session holds.
type relayConn struct {
	*runnerwire.Stream
	runner string
	wd     *writeDeadline
}

// writeDeadline is the write timer of one relayConn, shared by its copies.
type writeDeadline struct {
	mu      sync.Mutex
	timer   *time.Timer
	expired atomic.Bool
}

func newRelayConn(st *runnerwire.Stream, runner string) relayConn {
	return relayConn{Stream: st, runner: runner, wd: &writeDeadline{}}
}

// armWrite replaces the write timer with one that resets the stream at t; a zero t clears it.
func (c relayConn) armWrite(t time.Time) {
	c.wd.mu.Lock()
	defer c.wd.mu.Unlock()
	if c.wd.timer != nil {
		c.wd.timer.Stop()
		c.wd.timer = nil
	}
	if t.IsZero() {
		return
	}
	if d := time.Until(t); d > 0 {
		c.wd.timer = time.AfterFunc(d, c.expire)
		return
	}
	c.expire()
}

// expire resets a stream still open, and marks its write timed out.
func (c relayConn) expire() {
	select {
	case <-c.Done():
	default:
		c.wd.expired.Store(true)
		_ = c.Stream.Close()
	}
}

func (c relayConn) Write(b []byte) (int, error) {
	n, err := c.Stream.Write(b)
	if err != nil && c.wd.expired.Load() {
		err = os.ErrDeadlineExceeded
	}
	return n, err
}

// relayAddr names a relay's ends; RemoteAddr is "runner:<id>".
type relayAddr string

func (a relayAddr) Network() string { return "runnerwire" }
func (a relayAddr) String() string  { return string(a) }

func (c relayConn) LocalAddr() net.Addr  { return relayAddr("relay") }
func (c relayConn) RemoteAddr() net.Addr { return relayAddr(placement.RunnerSubstrateName(c.runner)) }

func (c relayConn) SetWriteDeadline(t time.Time) error {
	c.armWrite(t)
	return nil
}

func (c relayConn) SetDeadline(t time.Time) error {
	_ = c.Stream.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

// Close stops the write deadline, then resets a stream that has not already ended;
// one both ends closed gracefully has left the peer's table and needs no RESET.
func (c relayConn) Close() error {
	c.armWrite(time.Time{})
	select {
	case <-c.Done():
		return nil
	default:
		return c.Stream.Close()
	}
}
