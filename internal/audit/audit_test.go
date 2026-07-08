// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The audit package is a pure contract: Recorder is the append-only system of
// record, and Sink fans events out to external destinations. There is no
// concrete logic to exercise directly, so these tests assert the INTERFACE
// CONTRACT documented in sink.go using hand-rolled fakes (no real OTLP/syslog):
//
//   - a Recorder appends every event it is given (append-only, no overwrite);
//   - a Sink is identified by a stable Name() used to discriminate destinations;
//   - a Sink failure surfaces to the caller (so it can be logged/retried/counted)
//     rather than being swallowed.

// ---- fakes (no external destinations) ------------------------------------

// fakeRecorder is an in-memory append-only Recorder.
type fakeRecorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (r *fakeRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

// fakeSink records what it was asked to emit and can be wired to fail.
type fakeSink struct {
	name    string
	emitErr error
	mu      sync.Mutex
	emitted []types.AuditEvent
	emitCnt int
}

func (s *fakeSink) Name() string { return s.name }

func (s *fakeSink) Emit(_ context.Context, ev types.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitCnt++
	if s.emitErr != nil {
		return s.emitErr
	}
	s.emitted = append(s.emitted, ev)
	return nil
}

// Compile-time assertions that the fakes actually satisfy the contracts under
// test. If the interface signatures drift, this fails to compile.
var (
	_ Recorder = (*fakeRecorder)(nil)
	_ Sink     = (*fakeSink)(nil)
)

func sampleEvent() types.AuditEvent {
	return types.AuditEvent{
		ID:        uuid.New(),
		ActorType: types.ActorAgent,
		Actor:     "spiffe://wardyn/agent-run/x",
		Action:    "credential.mint",
		Outcome:   "success",
	}
}

// TestRecorderIsAppendOnly verifies the documented append-only semantics: every
// Record call adds a new event in order; nothing is overwritten or reordered.
func TestRecorderIsAppendOnly(t *testing.T) {
	rec := &fakeRecorder{}
	ctx := context.Background()

	want := []types.AuditEvent{sampleEvent(), sampleEvent(), sampleEvent()}
	for _, ev := range want {
		if err := rec.Record(ctx, ev); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	if len(rec.events) != len(want) {
		t.Fatalf("recorded %d events, want %d", len(rec.events), len(want))
	}
	for i := range want {
		if rec.events[i].ID != want[i].ID {
			t.Errorf("event[%d].ID = %v, want %v (append-only ordering broken)", i, rec.events[i].ID, want[i].ID)
		}
	}
}

// TestSinkNameDiscriminates checks the Name() contract: each sink reports a
// stable name (e.g. "otlp", "syslog") that the fan-out layer uses to tell
// destinations apart. Two distinct sinks must report distinct names.
func TestSinkNameDiscriminates(t *testing.T) {
	otlp := &fakeSink{name: "otlp"}
	syslog := &fakeSink{name: "syslog"}

	if otlp.Name() != "otlp" {
		t.Errorf("otlp.Name() = %q, want %q", otlp.Name(), "otlp")
	}
	if syslog.Name() != "syslog" {
		t.Errorf("syslog.Name() = %q, want %q", syslog.Name(), "syslog")
	}
	if otlp.Name() == syslog.Name() {
		t.Errorf("distinct sinks share a name %q; cannot discriminate destinations", otlp.Name())
	}
}

// TestSinkEmitDeliversEvent verifies the happy path: a healthy sink receives the
// exact event handed to it.
func TestSinkEmitDeliversEvent(t *testing.T) {
	s := &fakeSink{name: "otlp"}
	ev := sampleEvent()

	if err := s.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(s.emitted) != 1 {
		t.Fatalf("emitted %d events, want 1", len(s.emitted))
	}
	if s.emitted[0].ID != ev.ID {
		t.Errorf("emitted ID = %v, want %v", s.emitted[0].ID, ev.ID)
	}
}

// TestSinkEmitErrorSurfaces guards the documented invariant that sink failures
// are NOT silently dropped: Emit returns its error so the caller can log, retry,
// and count it. We also confirm the call was actually attempted.
func TestSinkEmitErrorSurfaces(t *testing.T) {
	boom := errors.New("destination unreachable")
	s := &fakeSink{name: "syslog", emitErr: boom}

	err := s.Emit(context.Background(), sampleEvent())
	if !errors.Is(err, boom) {
		t.Fatalf("Emit error = %v, want %v (failures must surface, not be swallowed)", err, boom)
	}
	if s.emitCnt != 1 {
		t.Errorf("emit attempts = %d, want 1", s.emitCnt)
	}
	if len(s.emitted) != 0 {
		t.Errorf("failed emit should not record a delivered event; got %d", len(s.emitted))
	}
}
