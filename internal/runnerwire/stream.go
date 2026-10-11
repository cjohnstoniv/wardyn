// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ResetError is what a read or write on an aborted stream answers.
type ResetError struct{ Code uint32 }

func (e *ResetError) Error() string { return fmt.Sprintf("runnerwire: stream reset (code %d)", e.Code) }

// Stream is one byte stream: a pty, an exec's stdio, a relay or an output
// copy. Read and Write may run in their own goroutines. A writer blocks on
// credit; nothing is dropped.
type Stream struct {
	p    *Peer
	id   uint32
	kind string

	send   *Credit
	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	cond         *sync.Cond
	buf          []byte
	remoteClosed bool
	localClosed  bool
	reset        error
	opened       chan struct{}
	done         chan struct{}
	doneOnce     sync.Once
	unacked      int64 // consumed by Read, not yet returned to the sender as WINDOW
	heldReleased bool  // both ends closed: the unread buffer no longer counts toward Peer.held
}

func (p *Peer) newStream(id uint32, kind string) *Stream {
	ctx, cancel := context.WithCancel(p.ctx)
	s := &Stream{p: p, id: id, kind: kind, send: NewStreamCredit(p.connSend), ctx: ctx, cancel: cancel, opened: make(chan struct{}), done: make(chan struct{})}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *Stream) ID() uint32   { return s.id }
func (s *Stream) Kind() string { return s.kind }

// Context ends on reset or connection loss. Graceful half-closes leave it live
// so a consumer can acknowledge its final buffered bytes.
func (s *Stream) Context() context.Context { return s.ctx }

// Done closes after a reset or both half-closes. A graceful close leaves any
// buffered bytes readable; Done alone does not mean the reader has drained them.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Err is nil on graceful completion or before termination, otherwise the reset.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reset
}

func (s *Stream) finish() { s.doneOnce.Do(func() { close(s.done) }) }

// Read returns buffered bytes, io.EOF once the peer half-closed and the buffer
// is drained, or a *ResetError after an abort.
func (s *Stream) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	for len(s.buf) == 0 && s.reset == nil && !s.remoteClosed {
		s.cond.Wait()
	}
	if s.reset != nil {
		err := s.reset
		s.mu.Unlock()
		return 0, err
	}
	if len(s.buf) == 0 {
		s.mu.Unlock()
		return 0, io.EOF
	}
	n := copy(b, s.buf)
	s.buf = s.buf[n:]
	grant := int64(0)
	if !s.heldReleased { // once released, these bytes were already returned to the sender
		s.p.held.Add(-int64(n))
		s.unacked += int64(n)
		if s.unacked >= StreamWindow/4 {
			grant, s.unacked = s.unacked, 0
		}
	}
	s.mu.Unlock()
	if grant > 0 {
		s.p.grant(s, grant)
	}
	return n, nil
}

// Write sends b as DATA frames of at most MaxDataFrame bytes, waiting for credit.
func (s *Stream) Write(b []byte) (int, error) {
	s.mu.Lock()
	err := s.reset
	closed := s.localClosed
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if closed {
		return 0, io.ErrClosedPipe
	}
	written := 0
	for written < len(b) {
		n, err := s.send.Take(s.ctx, min(len(b)-written, MaxDataFrame))
		if err != nil {
			return written, s.writeErr(err)
		}
		// The frame is written under the link's ctx, not the stream's: a reset
		// that cancels a write in progress would close the shared connection.
		if err := s.p.send(s.p.ctx, Frame{Type: TypeData, Stream: s.id, Payload: b[written : written+n]}); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func (s *Stream) writeErr(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reset != nil {
		return s.reset
	}
	return err
}

// CloseWrite half-closes: the peer's reads reach EOF after the bytes already sent.
func (s *Stream) CloseWrite() error {
	s.mu.Lock()
	if s.localClosed || s.reset != nil {
		s.mu.Unlock()
		return nil
	}
	s.localClosed = true
	both := s.remoteClosed
	var owed int64
	if both {
		owed = s.releaseHeldLocked()
	}
	s.mu.Unlock()
	s.returnCredit(owed)
	err := s.p.send(s.p.ctx, Frame{Type: TypeClose, Stream: s.id})
	if both {
		s.p.forget(s.id)
		s.finish()
	}
	return err
}

// Reset aborts the stream with code and tells the peer.
func (s *Stream) Reset(code uint32) error {
	if !s.abort(code) {
		return nil
	}
	s.p.forget(s.id)
	return s.p.send(s.p.ctx, Frame{Type: TypeReset, Stream: s.id, Payload: EncodeReset(code)})
}

// Close aborts the stream only, never the sandbox, the agent or any sidecar.
func (s *Stream) Close() error { return s.Reset(ResetCancelled) }

// abort marks the stream reset and wakes every waiter; it reports whether this
// call did it.
func (s *Stream) abort(code uint32) bool {
	s.mu.Lock()
	if s.reset != nil {
		s.mu.Unlock()
		return false
	}
	s.reset = &ResetError{Code: code}
	owed := s.releaseHeldLocked()
	s.buf = nil
	s.cond.Broadcast()
	s.mu.Unlock()
	s.returnCredit(owed)
	s.send.Close()
	s.cancel()
	s.finish()
	return true
}

// errStreamOverflow: a peer sent past one stream's window. The stream is reset;
// the connection survives. Past the connection's window it is ErrBadFrame.
var errStreamOverflow = errors.New("runnerwire: stream window exceeded")

func (s *Stream) deliver(b []byte) error {
	s.mu.Lock()
	if s.reset != nil {
		s.mu.Unlock()
		s.returnCredit(int64(len(b))) // discarded, so it is the sender's again
		return nil
	}
	defer s.mu.Unlock()
	if int64(len(s.buf)+len(b)) > StreamWindow {
		return errStreamOverflow
	}
	if s.p.held.Add(int64(len(b))) > ConnWindow {
		s.p.held.Add(-int64(len(b)))
		return fmt.Errorf("%w: peer sent past the connection window", ErrBadFrame)
	}
	s.buf = append(s.buf, b...)
	s.cond.Broadcast()
	return nil
}

func (s *Stream) remoteClose() (both bool) {
	s.mu.Lock()
	s.remoteClosed = true
	s.cond.Broadcast()
	both = s.localClosed
	var owed int64
	if both {
		owed = s.releaseHeldLocked()
		s.finish()
	}
	s.mu.Unlock()
	s.returnCredit(owed)
	return both
}

// releaseHeldLocked stops counting the unread buffer against the connection,
// once. A stream that is closed on both ends leaves the peer's table, so a
// holder that never drains it must not keep its bytes in Peer.held. It returns
// the connection credit the sender is owed for the stream: bytes read and not
// yet granted, and bytes never read.
func (s *Stream) releaseHeldLocked() int64 {
	if s.heldReleased {
		return 0
	}
	s.heldReleased = true
	s.p.held.Add(-int64(len(s.buf)))
	owed := s.unacked + int64(len(s.buf))
	s.unacked = 0
	return owed
}

// returnCredit gives n bytes of an ended stream back to the sender's connection
// window, with whatever grant is holding in connPend, so nothing a finished
// stream received stays withheld. It does not block: the read loop calls it.
func (s *Stream) returnCredit(n int64) {
	if n <= 0 {
		return
	}
	p := s.p
	p.mu.Lock()
	n, p.connPend = n+p.connPend, 0
	p.mu.Unlock()
	go p.send(p.ctx, Frame{Type: TypeWindow, Payload: EncodeWindow(uint32(n))}) //nolint:errcheck // a failed WINDOW is the link going down
}
