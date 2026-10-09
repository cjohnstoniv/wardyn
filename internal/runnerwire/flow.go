// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"errors"
	"sync"
)

// ErrCreditClosed is what a blocked writer gets when its stream or connection
// ends.
var ErrCreditClosed = errors.New("runnerwire: credit closed")

// Credit is a send window: a writer takes bytes of credit before sending DATA
// and blocks when there is none, and WINDOW increments give it back. Nothing is
// dropped; a writer simply waits. The connection window and each stream's
// window are both held, so a send needs credit in both.
type Credit struct {
	mu      sync.Mutex
	avail   int64
	closed  bool
	changed chan struct{}
	parent  *Credit
}

// NewConnCredit is the connection-level window.
func NewConnCredit() *Credit { return newCredit(ConnWindow, nil) }

// NewStreamCredit is one byte stream's window, bounded also by the connection's.
func NewStreamCredit(conn *Credit) *Credit { return newCredit(StreamWindow, conn) }

func newCredit(n int64, parent *Credit) *Credit {
	return &Credit{avail: n, changed: make(chan struct{}), parent: parent}
}

// Add grants n more bytes (a WINDOW frame's increment).
func (c *Credit) Add(n int64) {
	c.mu.Lock()
	c.avail += n
	c.wake()
	c.mu.Unlock()
}

// Close fails every blocked and future Take.
func (c *Credit) Close() {
	c.mu.Lock()
	c.closed = true
	c.wake()
	c.mu.Unlock()
}

func (c *Credit) wake() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Take blocks until some credit is available, then consumes and returns up to
// max bytes of it from this window and, when it has one, the connection's.
func (c *Credit) Take(ctx context.Context, max int) (int, error) {
	for {
		// The parent's wake channel is read before the attempt, so a grant that
		// lands in between is not missed.
		parentWake := c.parent.wakeChan()
		n, wait, err := c.tryTake(max)
		if err != nil || n > 0 {
			return n, err
		}
		select {
		case <-wait:
		case <-parentWake:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (c *Credit) wakeChan() <-chan struct{} {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

func (c *Credit) tryTake(max int) (int, <-chan struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, nil, ErrCreditClosed
	}
	n := int64(max)
	if c.avail < n {
		n = c.avail
	}
	if n <= 0 {
		return 0, c.changed, nil
	}
	if c.parent != nil {
		got, err := c.parent.takeUpTo(n)
		if err != nil {
			return 0, nil, err
		}
		n = got
		if n == 0 {
			return 0, c.changed, nil
		}
	}
	c.avail -= n
	return int(n), nil, nil
}

func (c *Credit) takeUpTo(n int64) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, ErrCreditClosed
	}
	if c.avail < n {
		n = c.avail
	}
	c.avail -= n
	return n, nil
}
