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
// write timer, so a write deadline is kept here: when it passes with a Write waiting,
// the stream is reset, which unblocks the Write with os.ErrDeadlineExceeded and
// ends the relay; with none waiting, the next Write fails and the stream is untouched. A relay whose peer stalls with no write deadline ends with its
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
	expired atomic.Bool  // the stream was reset because a Write was waiting past the deadline
	passed  atomic.Bool  // the deadline passed with no Write waiting: the next Write fails, the stream is untouched
	writing atomic.Int32 // Writes in flight
}

func newRelayConn(st *runnerwire.Stream, runner string) relayConn {
	return relayConn{Stream: st, runner: runner, wd: &writeDeadline{}}
}

// armWrite replaces the write deadline with t; a zero t clears it. When t passes, a Write waiting at
// that moment is released by resetting the stream; with none waiting, the stream is left alone and the
// next Write fails, because a deadline in the past is how tls.Conn.Close sends its alert without
// blocking, and the Close that follows must still be able to finish the answer the org just wrote.
func (c relayConn) armWrite(t time.Time) {
	c.wd.mu.Lock()
	defer c.wd.mu.Unlock()
	c.wd.passed.Store(false)
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

// expire is the write deadline passing: it marks the next Write timed out, and resets a stream still
// open when a Write is waiting on it.
func (c relayConn) expire() {
	c.wd.passed.Store(true)
	if c.wd.writing.Load() == 0 {
		return
	}
	select {
	case <-c.Done():
	default:
		c.wd.expired.Store(true)
		_ = c.Stream.Close()
	}
}

func (c relayConn) Write(b []byte) (int, error) {
	c.wd.writing.Add(1)
	defer c.wd.writing.Add(-1)
	if c.wd.passed.Load() {
		return 0, os.ErrDeadlineExceeded
	}
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

// relayCloseGrace is how long a closed relay waits for the runner to finish reading and close its end
// before the stream is reset.
const relayCloseGrace = 10 * time.Second

// Close stops the write deadline and ends this end. A Write still in flight is released by resetting
// the stream (net.Conn requires it, and tls.Conn.Close relies on it to break a stalled Write). With none
// in flight, the write half is closed and the stream is left to the runner, so it reads what the org
// wrote (an error answer written and closed at once) to its end; resetting there would discard bytes the
// runner has received and not yet read. The read half is closed at once, so bytes the runner keeps
// sending are discarded and cannot hold the runner link's shared window. A runner that never closes its
// end is reset after relayCloseGrace. A stream that already ended needs nothing.
func (c relayConn) Close() error {
	c.armWrite(time.Time{})
	select {
	case <-c.Done():
		return nil
	default:
	}
	if err := c.Stream.CloseWrite(); err != nil {
		return c.Stream.Close()
	}
	if c.wd.writing.Load() > 0 { // a Write is still waiting: only a reset releases it
		return c.Stream.Close()
	}
	_ = c.Stream.CloseRead()
	time.AfterFunc(relayCloseGrace, func() {
		select {
		case <-c.Done():
		default:
			_ = c.Stream.Close()
		}
	})
	return nil
}
