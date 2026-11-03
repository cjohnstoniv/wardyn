// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lastSink keeps the last event it was handed.
type lastSink struct {
	mu sync.Mutex
	ev types.AuditEvent
}

func (*lastSink) Name() string { return "last" }
func (l *lastSink) Emit(_ context.Context, ev types.AuditEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = ev
	return nil
}

func runEvent(runID *uuid.UUID, data string) types.AuditEvent {
	return types.AuditEvent{
		ID: uuid.New(), RunID: runID, Actor: "wardynd", Action: "run.create", Outcome: "success",
		Data: json.RawMessage(data), PrevHash: "prev", RowHash: "row",
	}
}

func dataOf(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("data %q: %v", ev.Data, err)
	}
	return m
}

func TestFanoutEnrichRuns_StampsTheSinksCopyAndLeavesTheStoredEventAlone(t *testing.T) {
	runID := uuid.New()
	sink := &lastSink{}
	f := sinks.NewFanout(sink)
	f.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) { return "build-42/a:b", true, nil })

	stored := runEvent(&runID, `{"agent":"claude-code","n":1}`)
	before := stored
	before.Data = append(json.RawMessage(nil), stored.Data...)
	if err := f.Emit(context.Background(), stored); err != nil {
		t.Fatal(err)
	}

	got := dataOf(t, sink.ev)
	if got[sinks.CorrelationKey] != "build-42/a:b" || got["agent"] != "claude-code" || got["n"] != float64(1) {
		t.Errorf("the sink's data = %v; want the original keys plus correlation_id", got)
	}
	if sinks.CorrelationKey != "correlation_id" {
		t.Errorf("the key is %q; the sinks and docs name correlation_id", sinks.CorrelationKey)
	}
	// What the store took is the value the caller still holds: data bytes and both chain hashes.
	if string(stored.Data) != string(before.Data) || stored.PrevHash != before.PrevHash || stored.RowHash != before.RowHash {
		t.Errorf("the caller's event changed: %+v, was %+v", stored, before)
	}
	if sink.ev.RowHash != "row" || sink.ev.PrevHash != "prev" {
		t.Errorf("the sink's chain hashes = %q/%q; enrichment must not touch them", sink.ev.PrevHash, sink.ev.RowHash)
	}
}

func TestFanoutEnrichRuns_DataShapes(t *testing.T) {
	runID := uuid.New()
	for name, tc := range map[string]struct {
		data   string
		wantID bool
	}{
		"no data":         {"", true},
		"null data":       {"null", true},
		"empty object":    {"{}", true},
		"existing value":  {`{"correlation_id":"forged"}`, true},
		"not an object":   {`[1,2]`, false},
		"scalar":          {`"text"`, false},
		"malformed bytes": {`{"a":`, false},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &lastSink{}
			f := sinks.NewFanout(sink)
			f.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) { return "abc", true, nil })
			if err := f.Emit(context.Background(), runEvent(&runID, tc.data)); err != nil {
				t.Fatal(err)
			}
			if !tc.wantID {
				if string(sink.ev.Data) != tc.data {
					t.Errorf("data = %q; a payload that is not an object goes out as it was (%q)", sink.ev.Data, tc.data)
				}
				return
			}
			if got := dataOf(t, sink.ev)[sinks.CorrelationKey]; got != "abc" {
				t.Errorf("correlation_id = %v; the run's own value must win over one already in data", got)
			}
		})
	}
}

func TestFanoutEnrichRuns_ReadsEachRunOnceAndSkipsWhatNeedsNoRead(t *testing.T) {
	withID, plain, missing := uuid.New(), uuid.New(), uuid.New()
	var reads atomic.Int32
	sink := &lastSink{}
	f := sinks.NewFanout(sink)
	f.EnrichRuns(func(_ context.Context, id uuid.UUID) (string, bool, error) {
		reads.Add(1)
		switch id {
		case withID:
			return "ref-1", true, nil
		case plain:
			return "", true, nil
		}
		return "", false, nil
	})
	emit := func(id *uuid.UUID) types.AuditEvent {
		t.Helper()
		if err := f.Emit(context.Background(), runEvent(id, `{"k":1}`)); err != nil {
			t.Fatal(err)
		}
		return sink.ev
	}

	emit(nil) // no run: no read
	if reads.Load() != 0 {
		t.Errorf("an event with no run read %d times", reads.Load())
	}
	for i := 0; i < 3; i++ {
		emit(&withID)
		emit(&plain)
	}
	if reads.Load() != 2 {
		t.Errorf("%d reads for two runs over six events; each run is read once", reads.Load())
	}
	if _, ok := dataOf(t, emit(&plain))[sinks.CorrelationKey]; ok {
		t.Error("a run with no correlation id got the key")
	}
	// A run the table does not hold yet is read again next time (it may be inserted by then).
	emit(&missing)
	emit(&missing)
	if reads.Load() != 4 {
		t.Errorf("%d reads; an absent run is not cached", reads.Load())
	}
}

func TestFanoutEnrichRuns_AReadFailureStillDeliversTheEvent(t *testing.T) {
	runID := uuid.New()
	sink := &lastSink{}
	f := sinks.NewFanout(sink)
	f.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) { return "", false, errors.New("db down") })
	if err := f.Emit(context.Background(), runEvent(&runID, `{"k":1}`)); err != nil {
		t.Fatalf("emit = %v; a failed read must not fail the fan-out", err)
	}
	if got := dataOf(t, sink.ev); got["k"] != float64(1) || got[sinks.CorrelationKey] != nil {
		t.Errorf("data = %v; want the event as it was", got)
	}
}

func TestFanoutWithoutEnrichRuns_PassesTheEventThrough(t *testing.T) {
	runID := uuid.New()
	sink := &lastSink{}
	if err := sinks.NewFanout(sink).Emit(context.Background(), runEvent(&runID, `{"k":1}`)); err != nil {
		t.Fatal(err)
	}
	if string(sink.ev.Data) != `{"k":1}` {
		t.Errorf("data = %s", sink.ev.Data)
	}
}
