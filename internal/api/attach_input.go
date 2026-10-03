// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
)

// attachInputQueue holds the keystroke frames the socket reader has parsed but
// the drain goroutine has not yet written into the exec. The reader only parses
// frames, so pong, close and resize are handled at once even while the exec is
// still being opened or a paste is blocked in Session.Write; the drain goroutine
// is the only one that writes, and it re-tests write authority per chunk
// (attachHolder.writeGated).
//
// Bounded by attachReadLimit bytes: a queue that grows without limit during a
// slow attach is a buffer a client controls, and a full queue closes the socket
// (1009) rather than blocking the reader or dropping a chunk without saying so.
type attachInputQueue struct {
	mu    sync.Mutex
	items [][]byte
	bytes int
	wake  chan struct{} // 1-slot: a push signals the drain goroutine
}

func newAttachInputQueue() *attachInputQueue {
	return &attachInputQueue{wake: make(chan struct{}, 1)}
}

// push queues one chunk, and reports false when it would take the queue past
// attachReadLimit. A single message is at most attachReadLimit (the socket's own
// read limit), so an empty queue always accepts one.
func (q *attachInputQueue) push(b []byte) bool {
	q.mu.Lock()
	if q.bytes+len(b) > attachReadLimit {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, b)
	q.bytes += len(b)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// pop returns the oldest chunk, waiting for one; false when ctx ended first.
func (q *attachInputQueue) pop(ctx context.Context) ([]byte, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			b := q.items[0]
			q.items[0] = nil
			q.items = q.items[1:]
			q.bytes -= len(b)
			q.mu.Unlock()
			return b, true
		}
		q.mu.Unlock()
		select {
		case <-q.wake:
		case <-ctx.Done():
			return nil, false
		}
	}
}
