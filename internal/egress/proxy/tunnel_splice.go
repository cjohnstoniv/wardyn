// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// A tunnel's copies hand the bytes to the kernel once the guards are done with them. Until then every byte goes
// through the wrappers the legs wear (tunnelWatch's first-bytes and SSH guards, readerConn's buffered bytes,
// activityConn's mark), as it always did. After that the wrappers would only pass bytes on, which hides
// *net.TCPConn's ReadFrom from io.Copy and keeps the copy in user space: socket to socket, ReadFrom between two TCP
// sockets is splice(2) on Linux.

// tunnelSpliceTick is how often a spliced copy comes back to mark the run active and count what it moved, as the
// wrappers did on every read. A var only so a test can shorten it.
var tunnelSpliceTick = 5 * time.Second

// tunnelReadFrom moves src to dst until src's read deadline, its end or an error. A var only so a test can see
// that the copy reaches it.
var tunnelReadFrom = (*net.TCPConn).ReadFrom

// relayHalf is one direction of tunnel: io.Copy's loop through the wrappers, checking before each read whether
// the direction may go socket to socket (spliceHalfOf), and then splicedHalf.relay.
func relayHalf(w *writeFailed, dst, src net.Conn) error {
	buf := make([]byte, 32<<10)
	for {
		if s, ok := spliceHalfOf(dst, src); ok {
			return s.relay()
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			if ew != nil {
				return ew
			}
			if nw != nr {
				return io.ErrShortWrite
			}
		}
		if er == io.EOF {
			return nil
		}
		if er != nil {
			return er
		}
	}
}

// splicedHalf is one direction of a tunnel the wrappers are done with: the two sockets beneath them, and what the
// wrappers still owe the bytes: the client leg's activity mark (nil on the sign-in lane), and the upstream's
// relayed count when the upstream is the source.
type splicedHalf struct {
	dst, src *net.TCPConn
	moved    *atomic.Bool
	relayed  *atomic.Int64
}

// spliceHalfOf peels the copy's two legs down to their sockets. It reports false while a wrapper still has work
// on this direction's bytes, or for a leg it does not know.
func spliceHalfOf(dst, src net.Conn) (splicedHalf, bool) {
	var s splicedHalf
	var ok bool
	if s.dst, ok = s.peel(dst, false); !ok {
		return s, false
	}
	s.src, ok = s.peel(src, true)
	return s, ok
}

// peel unwraps c, the copy's source when read is set and its destination otherwise. Only the copy that reads a
// leg may look at that leg's read state: readerConn's buffer (bytes the socket no longer holds) and the guard's.
func (s *splicedHalf) peel(c net.Conn, read bool) (*net.TCPConn, bool) {
	for {
		switch v := c.(type) {
		case *net.TCPConn:
			return v, true
		case activityConn:
			s.moved, c = v.moved, v.Conn
		case *readerConn:
			if br, ok := v.r.(*bufio.Reader); read && (!ok || br.Buffered() > 0) {
				return nil, false
			}
			c = v.Conn
		case *tunnelWatch:
			if !v.settled(read) {
				return nil, false
			}
			if read {
				s.relayed = &v.relayed
			}
			c = v.Conn
		case *trackedConn:
			c = v.Conn
		case *signinConn:
			c = v.Conn
		case *vmnetConn:
			c = v.Conn
		default:
			return nil, false
		}
	}
}

// settled reports whether the guard is done with the bytes one of tunnel's copies carries. Both wait until more
// of the upstream's answer was relayed than an alert record: its first record was vetted, and failureClass and
// answeredNothing read nothing past that, so the tunnel's row no longer depends on which side ends it or how. The
// upstream-to-client copy (read) also waits for the vetted flight to leave br; the client-to-upstream copy waits
// until Write has judged the client's opening: its first byte and any SSH identification. Each looks only at
// what its own goroutine owns, beside the atomics.
func (w *tunnelWatch) settled(read bool) bool {
	if w.relayed.Load() <= tlsAlertRecordLen {
		return false
	}
	if read {
		return w.vetted && w.br.Buffered() == 0
	}
	return w.client.Load() != clientSilent &&
		(w.onSSH == nil || len(w.clientHead) >= len(sshIdent) || !strings.HasPrefix(sshIdent, string(w.clientHead)))
}

// relay copies src to dst socket to socket until src ends, coming back every tunnelSpliceTick to mark the run
// active and count the upstream's bytes. A failed copy cannot say whether its read or its write failed; tunnel's
// row no longer depends on it (tunnelWatch.settled).
func (s splicedHalf) relay() error {
	for {
		if err := s.src.SetReadDeadline(time.Now().Add(tunnelSpliceTick)); err != nil {
			return err
		}
		n, err := tunnelReadFrom(s.dst, s.src)
		if n > 0 {
			if s.moved != nil {
				s.moved.Store(true)
			}
			if s.relayed != nil {
				s.relayed.Add(n)
			}
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			return err
		}
	}
}
