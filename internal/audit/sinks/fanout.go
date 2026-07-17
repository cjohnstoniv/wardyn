// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// childState tracks per-child drop statistics for a Fanout.
type childState struct {
	sink  audit.Sink
	drops atomic.Int64
}

// Fanout multiplexes a single audit event stream to multiple audit.Sink
// children. Per-child failures are isolated: an error from one child is logged
// (and counted) but never propagates to sibling sinks or to the caller.
//
// Emit returns nil unless ALL children fail, in which case it returns the last
// observed error. This preserves the recorder's ability to detect a total
// fan-out failure while satisfying the isolation requirement.
type Fanout struct {
	children []*childState
}

// NewFanout creates a Fanout over the supplied sinks. Each sink in children
// is wrapped in its own childState with an independent drop counter.
func NewFanout(children ...audit.Sink) *Fanout {
	cs := make([]*childState, len(children))
	for i, s := range children {
		cs[i] = &childState{sink: s}
	}
	return &Fanout{children: cs}
}

// Name implements audit.Sink.
func (f *Fanout) Name() string { return "fanout" }

// Emit delivers ev to every child sink concurrently (one goroutine per child).
// Per-child panics are recovered; errors are logged and counted. Emit blocks
// until every child has returned.
//
// If every child returns an error Emit returns the last error seen; if at
// least one child succeeds Emit returns nil.
func (f *Fanout) Emit(ctx context.Context, ev types.AuditEvent) error {
	type result struct {
		name string
		err  error
	}

	results := make(chan result, len(f.children))

	for _, cs := range f.children {
		cs := cs // capture
		go func() {
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						err = panicErr(r)
					}
				}()
				err = cs.sink.Emit(ctx, ev)
			}()
			if err != nil {
				cs.drops.Add(1)
				slog.ErrorContext(ctx, "sinks.fanout: child sink error",
					slog.String("child", cs.sink.Name()),
					slog.Int64("drops", cs.drops.Load()),
					slog.Any("err", err))
			}
			results <- result{name: cs.sink.Name(), err: err}
		}()
	}

	var lastErr error
	failures := 0
	for range f.children {
		r := <-results
		if r.err != nil {
			lastErr = r.err
			failures++
		}
	}
	if failures == len(f.children) && len(f.children) > 0 {
		return lastErr
	}
	return nil
}

// dropper is implemented by sinks that track their own drop counter for events
// lost asynchronously (after Emit returns), e.g. WebhookSink and SyslogSink
// buffer in the background, so overflow and retry-exhaustion losses never
// surface as an Emit error.
type dropper interface{ Drops() int64 }

// Drops returns the total drop count for the named child sink. Returns -1 if no
// child with that name is found.
//
// The total aggregates two independent sources of loss:
//   - the fanout-local counter, incremented when the child's Emit returns an
//     error (synchronous failure), and
//   - the child's own Drops() counter, if it implements dropper — buffering
//     sinks (webhook, syslog) drop asynchronously and report nil from Emit, so
//     without this their losses would be structurally invisible (Drops was
//     always 0 for them).
func (f *Fanout) Drops(name string) int64 {
	for _, cs := range f.children {
		if cs.sink.Name() == name {
			total := cs.drops.Load()
			if d, ok := cs.sink.(dropper); ok {
				total += d.Drops()
			}
			return total
		}
	}
	return -1
}

// Close closes every child sink that implements io.Closer, returning the first
// error encountered (after attempting to close all of them). Buffering sinks
// (webhook, syslog) block in Close until their final batch has been flushed, so
// calling Fanout.Close on graceful shutdown ensures the last events are drained
// and awaited rather than abandoned (finding: sinks were never Closed on
// shutdown, so the webhook drain goroutine was never awaited).
func (f *Fanout) Close() error {
	var firstErr error
	for _, cs := range f.children {
		if c, ok := cs.sink.(io.Closer); ok {
			if err := c.Close(); err != nil {
				slog.Error("sinks.fanout: closing child sink failed",
					slog.String("child", cs.sink.Name()),
					slog.Any("err", err))
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// panicErr converts a recovered panic value to an error string.
func panicErr(v any) error {
	type stringer interface{ Error() string }
	if e, ok := v.(stringer); ok {
		return e.(error)
	}
	return fmt.Errorf("panic: %v", v)
}
