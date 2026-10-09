// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrReplayFull: a frame larger than the whole buffer can never be buffered.
	ErrReplayFull = errors.New("runnerwire: frame larger than the replay buffer")
	// ErrResumeGap: the frames a resuming peer needs have already been dropped.
	ErrResumeGap = errors.New("runnerwire: resume point is no longer buffered")
)

// ReplayBuffer holds sequenced frames, up to ReplayBytes of payload, until the
// peer ACKs them. A resumed session replays what Since returns.
type ReplayBuffer struct {
	mu      sync.Mutex
	frames  []Frame
	bytes   int // buffered plus reserved
	acked   uint64
	next    uint64
	changed chan struct{}
}

// NewReplayBuffer starts numbering sequenced frames at 1.
func NewReplayBuffer() *ReplayBuffer {
	return &ReplayBuffer{next: 1, changed: make(chan struct{})}
}

// Reserve waits until n payload bytes fit, then holds that room for the Assign
// that follows. It is separate from Assign so a sender can wait for the peer's
// ACK without holding the lock that orders its writes.
func (b *ReplayBuffer) Reserve(ctx context.Context, n int) error {
	if n > ReplayBytes {
		return ErrReplayFull
	}
	for {
		b.mu.Lock()
		if b.bytes+n <= ReplayBytes {
			b.bytes += n
			b.mu.Unlock()
			return nil
		}
		wait := b.changed
		b.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Assign stamps f, whose room Reserve already holds, with the next sequence
// number and buffers it.
func (b *ReplayBuffer) Assign(f Frame) Frame {
	b.mu.Lock()
	defer b.mu.Unlock()
	f.Seq = b.next
	b.next++
	b.frames = append(b.frames, f)
	return f
}

// ErrAckAhead: the peer acknowledged a sequence number never sent. It would make
// every later Since fail, so it is refused and the session ends.
var ErrAckAhead = errors.New("runnerwire: ACK ahead of anything sent")

// Ack drops every frame up to and including seq (cumulative). A seq at or
// before the last ACK is ignored; one ahead of what was sent is ErrAckAhead.
func (b *ReplayBuffer) Ack(seq uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if seq >= b.next {
		return ErrAckAhead
	}
	if seq <= b.acked {
		return nil
	}
	b.acked = seq
	i := 0
	for i < len(b.frames) && b.frames[i].Seq <= seq {
		b.bytes -= len(b.frames[i].Payload)
		i++
	}
	b.frames = append([]Frame(nil), b.frames[i:]...)
	close(b.changed)
	b.changed = make(chan struct{})
	return nil
}

// Since returns the buffered frames after lastSeq, the peer's last received
// sequence number, oldest first.
func (b *ReplayBuffer) Since(lastSeq uint64) ([]Frame, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if lastSeq < b.acked || lastSeq >= b.next {
		return nil, ErrResumeGap
	}
	var out []Frame
	for _, f := range b.frames {
		if f.Seq > lastSeq {
			out = append(out, f)
		}
	}
	return out, nil
}
