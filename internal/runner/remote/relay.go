// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"net"
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
	return s.opts.Relay(s.id, relayConn{Stream: st, runner: s.id})
}

// relayConn is one relay stream as a net.Conn, for the org's internal relay
// listener. Deadlines are no-ops: a stream has no timer, so the listener's
// server timeouts do not apply to it. A stalled relay ends with its runner's
// session (every stream is reset then), and runnerwire.MaxStreams bounds how
// many one session holds.
type relayConn struct {
	*runnerwire.Stream
	runner string
}

// relayAddr names a relay's ends; RemoteAddr is "runner:<id>".
type relayAddr string

func (a relayAddr) Network() string { return "runnerwire" }
func (a relayAddr) String() string  { return string(a) }

func (c relayConn) LocalAddr() net.Addr              { return relayAddr("relay") }
func (c relayConn) RemoteAddr() net.Addr             { return relayAddr(placement.RunnerSubstrateName(c.runner)) }
func (c relayConn) SetDeadline(time.Time) error      { return nil }
func (c relayConn) SetReadDeadline(time.Time) error  { return nil }
func (c relayConn) SetWriteDeadline(time.Time) error { return nil }

// Close resets a stream that has not already ended; one both ends closed
// gracefully has left the peer's table and needs no RESET.
func (c relayConn) Close() error {
	select {
	case <-c.Done():
		return nil
	default:
		return c.Stream.Close()
	}
}
