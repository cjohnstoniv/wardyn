// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package ackcursor is the durable, acknowledgement-gated cursor behind every
// at-least-once delivery of an ordered, hash-chained row stream: federation's
// audit forwarder today, the per-run spool replay over the runner stream and
// acknowledged SIEM delivery next. The cursor moves only after the receiver
// acknowledges; a re-sent row is recognised by seq + row hash (a local table
// reset can reuse a seq, so the position keeps both).
package ackcursor

import (
	"context"
	"slices"
)

// Pos is a position in the stream: the seq of the last acknowledged row and
// that row's hash. The zero Pos is the start of the stream.
type Pos struct {
	Seq  int64
	Hash string
}

// Source reads rows in ascending seq order.
type Source[T any] interface {
	// After returns up to limit rows with seq strictly greater than seq.
	After(ctx context.Context, seq int64, limit int) ([]T, error)
}

// Sink delivers one batch. rows is empty when there is nothing to send (a
// sink with a liveness signal sends it then). It returns the highest seq the
// receiver acknowledged; the cursor never moves past the rows read or
// backwards, so a sink that skips rows it cannot send returns the last seq
// of the batch.
type Sink[T any] interface {
	Deliver(ctx context.Context, rows []T) (ackSeq int64, err error)
}

// Store persists the position durably.
type Store interface {
	Get(ctx context.Context) (Pos, error)
	Set(ctx context.Context, p Pos) error
}

// Cursor runs the batch loop. Not safe for concurrent use.
type Cursor[T any] struct {
	Source Source[T]
	Sink   Sink[T]
	Store  Store
	// Key reads a row's seq and hash.
	Key func(T) Pos
	// Batch is the most rows delivered per Step.
	Batch int
	// OnReset is called, before the position is cleared, when the row at the
	// cursor is gone or changed; its argument is the position that failed.
	OnReset func(Pos)

	pos Pos
}

// Pos is the last acknowledged position.
func (c *Cursor[T]) Pos() Pos { return c.pos }

// Load reads the durable position.
func (c *Cursor[T]) Load(ctx context.Context) error {
	p, err := c.Store.Get(ctx)
	if err != nil {
		return err
	}
	c.pos = p
	return nil
}

// Step delivers the next batch and advances the position past what was
// acknowledged. more is true when the batch was full, so the caller should
// step again without waiting. On error the position is unchanged, except that
// a reset already made durable stays reset.
//
// The row at the position and the batch after it come from one read. If that
// row is gone or carries another hash the stream was reset: rows after it
// would link to nothing the receiver holds, so Step starts over from the
// beginning (the receiver skips what it already holds).
func (c *Cursor[T]) Step(ctx context.Context) (more bool, err error) {
	var rows []T
	if c.pos.Seq > 0 {
		if rows, err = c.Source.After(ctx, c.pos.Seq-1, c.Batch+1); err != nil {
			return false, err
		}
		if len(rows) > 0 && c.Key(rows[0]) == c.pos {
			rows = rows[1:]
		} else {
			if c.OnReset != nil {
				c.OnReset(c.pos)
			}
			if err := c.Store.Set(ctx, Pos{}); err != nil {
				return false, err
			}
			c.pos, rows = Pos{}, nil
		}
	}
	if c.pos.Seq == 0 {
		if rows, err = c.Source.After(ctx, 0, c.Batch); err != nil {
			return false, err
		}
	}

	ack, err := c.Sink.Deliver(ctx, rows)
	if err != nil {
		return false, err
	}
	next := c.pos.Seq
	if len(rows) > 0 {
		next = max(next, min(ack, c.Key(rows[len(rows)-1]).Seq))
	}
	if next != c.pos.Seq {
		// A next that is not a row read here keeps "", which the next Step
		// reads as a reset and resends from the start — the safe direction.
		var hash string
		if i := slices.IndexFunc(rows, func(r T) bool { return c.Key(r).Seq == next }); i >= 0 {
			hash = c.Key(rows[i]).Hash
		}
		p := Pos{Seq: next, Hash: hash}
		if err := c.Store.Set(ctx, p); err != nil {
			return false, err
		}
		c.pos = p
	}
	return len(rows) == c.Batch, nil
}
