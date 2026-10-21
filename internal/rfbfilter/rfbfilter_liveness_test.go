// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbfilter

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// closingBackend: Close ends later I/O, like the exec stream does.
type closingBackend struct {
	mu      sync.Mutex
	chunks  chan []byte
	pending []byte
	closed  chan struct{}
	once    sync.Once
	written []byte
	block   chan struct{} // when non-nil, Write blocks until it is closed
}

func newClosingBackend() *closingBackend {
	return &closingBackend{chunks: make(chan []byte, 1024), closed: make(chan struct{})}
}

func (b *closingBackend) Read(p []byte) (int, error) {
	select {
	case <-b.closed:
		return 0, errors.New("backend closed")
	default:
	}
	if len(b.pending) == 0 {
		select {
		case c := <-b.chunks:
			b.pending = c
		case <-b.closed:
			return 0, errors.New("backend closed")
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *closingBackend) Write(p []byte) (int, error) {
	if b.block != nil {
		<-b.block
	}
	select {
	case <-b.closed:
		return 0, errors.New("backend closed")
	default:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.written = append(b.written, p...)
	return len(p), nil
}

func (b *closingBackend) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

// A browser Write blocked in the backend (the desktop is not reading its input) must not stall the
// server's bytes: a desktop blocked writing a large frame and a person pasting a large text would
// otherwise wait on each other forever.
func TestReadIsNotHeldUpByABlockedWrite(t *testing.T) {
	b := newClosingBackend()
	c := Wrap(b, Rules{PasteIn: true, CopyOut: true}, func(Event) {}, func(err error) { t.Logf("refused: %v", err) })
	step := func(fromServer bool, msg []byte) {
		t.Helper()
		if fromServer {
			b.chunks <- frames(msg, false, false)
			if _, err := c.Read(make([]byte, 1024)); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := c.Write(frames(msg, true, false)); err != nil {
			t.Fatal(err)
		}
	}
	step(true, []byte("RFB 003.008\n"))
	step(false, []byte("RFB 003.008\n"))
	step(true, []byte{1, 1})
	step(false, []byte{1})
	step(true, be32(0))
	step(false, []byte{1})
	step(true, serverInit())

	b.block = make(chan struct{})
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		c.Write(frames(cat([]byte{4, 1, 0, 0}, be32(0x61)), true, false)) // a KeyEvent, blocked in the backend
	}()
	time.Sleep(20 * time.Millisecond)
	b.chunks <- frames([]byte{2}, false, false) // a Bell from the desktop
	readDone := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 100))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read waited on a Write blocked in the backend")
	}
	close(b.block)
	<-writeDone
}
