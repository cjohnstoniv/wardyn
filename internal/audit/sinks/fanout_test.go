// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countSink counts how many events it has received. It can be configured to
// return an error on every Emit call.
type countSink struct {
	name  string
	count atomic.Int64
	err   error // if non-nil, returned from every Emit
}

var _ audit.Sink = (*countSink)(nil)

func (c *countSink) Name() string { return c.name }
func (c *countSink) Emit(_ context.Context, _ types.AuditEvent) error {
	if c.err != nil {
		return c.err
	}
	c.count.Add(1)
	return nil
}

func TestFanout_DeliversToAllChildren(t *testing.T) {
	t.Parallel()

	a := &countSink{name: "a"}
	b := &countSink{name: "b"}
	c := &countSink{name: "c"}

	f := sinks.NewFanout(a, b, c)
	ctx := context.Background()

	const n = 10
	for i := 0; i < n; i++ {
		if err := f.Emit(ctx, makeEvent(fmt.Sprintf("fanout.%d", i))); err != nil {
			t.Errorf("Emit %d: unexpected error: %v", i, err)
		}
	}

	for _, s := range []*countSink{a, b, c} {
		if got := s.count.Load(); got != n {
			t.Errorf("sink %q: got %d events, want %d", s.name, got, n)
		}
	}
}

func TestFanout_IsolatesChildFailure(t *testing.T) {
	t.Parallel()

	bad := &countSink{name: "bad", err: errors.New("always fails")}
	good := &countSink{name: "good"}

	f := sinks.NewFanout(bad, good)
	ctx := context.Background()

	const n = 5
	for i := 0; i < n; i++ {
		// Emit must not return an error when at least one child succeeds.
		if err := f.Emit(ctx, makeEvent(fmt.Sprintf("isolation.%d", i))); err != nil {
			t.Errorf("Emit %d: got error %v; want nil (good sibling succeeded)", i, err)
		}
	}

	if got := good.count.Load(); got != n {
		t.Errorf("good sink received %d events, want %d", got, n)
	}
	if drops := f.Drops("bad"); drops != int64(n) {
		t.Errorf("bad sink drops: got %d, want %d", drops, n)
	}
}

func TestFanout_ReturnsErrorWhenAllFail(t *testing.T) {
	t.Parallel()

	bad1 := &countSink{name: "bad1", err: errors.New("fail1")}
	bad2 := &countSink{name: "bad2", err: errors.New("fail2")}

	f := sinks.NewFanout(bad1, bad2)
	ctx := context.Background()

	if err := f.Emit(ctx, makeEvent("all-fail")); err == nil {
		t.Error("expected error when all children fail; got nil")
	}
}

func TestFanout_EmptyFanoutNoError(t *testing.T) {
	t.Parallel()

	f := sinks.NewFanout()
	ctx := context.Background()

	if err := f.Emit(ctx, makeEvent("empty")); err != nil {
		t.Errorf("empty fanout: unexpected error: %v", err)
	}
}

func TestFanout_DropsUnknownChild(t *testing.T) {
	t.Parallel()

	good := &countSink{name: "good"}
	f := sinks.NewFanout(good)
	if got := f.Drops("nonexistent"); got != -1 {
		t.Errorf("Drops for unknown child: got %d, want -1", got)
	}
}

// asyncDropSink models a buffering sink (like webhook/syslog) whose Emit always
// succeeds but which drops events asynchronously in the background, exposing the
// loss only through its own Drops() method.
type asyncDropSink struct {
	name      string
	selfDrops atomic.Int64
}

var _ audit.Sink = (*asyncDropSink)(nil)

func (s *asyncDropSink) Name() string                                 { return s.name }
func (s *asyncDropSink) Emit(context.Context, types.AuditEvent) error { return nil }
func (s *asyncDropSink) Drops() int64                                 { return s.selfDrops.Load() }

// TestFanout_DropsSurfacesChildCounter covers the finding that Fanout.Drops was
// structurally always 0 for buffering children: their Emit returns nil (no
// synchronous error to count), so the only loss signal is the child's own
// Drops(). The fanout must surface that counter, aggregated with any
// fanout-local synchronous-error drops.
func TestFanout_DropsSurfacesChildCounter(t *testing.T) {
	t.Parallel()

	web := &asyncDropSink{name: "webhook"}
	f := sinks.NewFanout(web)
	ctx := context.Background()

	// Emits all succeed at the fanout level (no per-child error), so without
	// surfacing the child counter Drops would be 0.
	for i := 0; i < 3; i++ {
		if err := f.Emit(ctx, makeEvent("async")); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	if got := f.Drops("webhook"); got != 0 {
		t.Fatalf("Drops before any async loss: got %d, want 0", got)
	}

	// Simulate the background flusher losing 5 events.
	web.selfDrops.Store(5)

	if got := f.Drops("webhook"); got != 5 {
		t.Errorf("Drops after async loss: got %d, want 5 (child counter not surfaced)", got)
	}
}

// closeRecorderSink records that Close was called, so the fanout Close wiring can
// be asserted.
type closeRecorderSink struct {
	name   string
	closed atomic.Bool
}

var _ audit.Sink = (*closeRecorderSink)(nil)

func (s *closeRecorderSink) Name() string                                 { return s.name }
func (s *closeRecorderSink) Emit(context.Context, types.AuditEvent) error { return nil }
func (s *closeRecorderSink) Close() error                                 { s.closed.Store(true); return nil }

// TestFanout_CloseClosesChildren verifies Fanout.Close closes every child that
// implements io.Closer (and tolerates children that do not).
func TestFanout_CloseClosesChildren(t *testing.T) {
	t.Parallel()

	closer := &closeRecorderSink{name: "file"}
	plain := &countSink{name: "plain"} // no Close method
	f := sinks.NewFanout(closer, plain)

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closer.closed.Load() {
		t.Error("Fanout.Close did not Close the io.Closer child")
	}
}
