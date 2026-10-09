// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
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
	unacked      int64 // consumed by Read, not yet returned to the sender as WINDOW
}

func (p *Peer) newStream(id uint32, kind string) *Stream {
	ctx, cancel := context.WithCancel(p.ctx)
	s := &Stream{p: p, id: id, kind: kind, send: NewStreamCredit(p.connSend), ctx: ctx, cancel: cancel, opened: make(chan struct{})}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *Stream) ID() uint32   { return s.id }
func (s *Stream) Kind() string { return s.kind }

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
	s.unacked += int64(n)
	grant := int64(0)
	if s.unacked >= StreamWindow/4 {
		grant, s.unacked = s.unacked, 0
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
		if err := s.p.send(s.ctx, Frame{Type: TypeData, Stream: s.id, Payload: b[written : written+n]}); err != nil {
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
	s.mu.Unlock()
	err := s.p.send(s.ctx, Frame{Type: TypeClose, Stream: s.id})
	if both {
		s.p.forget(s.id)
	}
	return err
}

// Reset aborts the stream with code and tells the peer.
func (s *Stream) Reset(code uint32) error {
	if !s.abort(code) {
		return nil
	}
	s.p.forget(s.id)
	return s.p.send(context.Background(), Frame{Type: TypeReset, Stream: s.id, Payload: EncodeReset(code)})
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
	s.buf = nil
	s.cond.Broadcast()
	s.mu.Unlock()
	s.send.Close()
	s.cancel()
	return true
}

func (s *Stream) deliver(b []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reset != nil {
		return true
	}
	if int64(len(s.buf)+len(b)) > StreamWindow {
		return false
	}
	s.buf = append(s.buf, b...)
	s.cond.Broadcast()
	return true
}

func (s *Stream) remoteClose() (both bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remoteClosed = true
	s.cond.Broadcast()
	return s.localClosed
}
