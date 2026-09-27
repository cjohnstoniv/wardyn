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
// children; a per-child error is logged and counted but never propagates to
// siblings or the caller. Emit returns nil unless ALL children fail, in which
// case it returns the last observed error.
type Fanout struct {
	children []*childState
}

// NewFanout creates a Fanout over the supplied sinks.
func NewFanout(children ...audit.Sink) *Fanout {
	cs := make([]*childState, len(children))
	for i, s := range children {
		cs[i] = &childState{sink: s}
	}
	return &Fanout{children: cs}
}

// Name implements audit.Sink.
func (f *Fanout) Name() string { return "fanout" }

// Emit delivers ev to every child sink concurrently, recovering per-child
// panics, and blocks until all have returned. Returns nil unless every child
// failed, in which case it returns the last error seen.
func (f *Fanout) Emit(ctx context.Context, ev types.AuditEvent) error {
	results := make(chan error, len(f.children))

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
			results <- err
		}()
	}

	var lastErr error
	failures := 0
	for range f.children {
		if err := <-results; err != nil {
			lastErr = err
			failures++
		}
	}
	if failures == len(f.children) && len(f.children) > 0 {
		return lastErr
	}
	return nil
}

// dropper is implemented by sinks (webhook, syslog) that track their own drop
// counter for events lost asynchronously, after Emit has already returned nil.
type dropper interface{ Drops() int64 }

// Drops returns the total drop count for the named child sink (fanout-local
// sync failures plus the child's own dropper count, if it has one), or -1 if
// no child with that name is found.
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

// DropsByName returns the total drop count per child sink name, aggregating
// across children sharing a name, for the wardyn_audit_sink_drops_total metric.
func (f *Fanout) DropsByName() map[string]int64 {
	out := make(map[string]int64, len(f.children))
	for _, cs := range f.children {
		total := cs.drops.Load()
		if d, ok := cs.sink.(dropper); ok {
			total += d.Drops()
		}
		out[cs.sink.Name()] += total
	}
	return out
}

// Close closes every child sink that implements io.Closer, returning the first
// error after attempting all of them. Buffering sinks block in Close until
// their final batch is flushed.
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

// panicErr converts a recovered panic value to an error, returning it as-is if
// it already is one, else wrapping its %v rendering.
func panicErr(v any) error {
	if e, ok := v.(error); ok {
		return e
	}
	return fmt.Errorf("panic: %v", v)
}
