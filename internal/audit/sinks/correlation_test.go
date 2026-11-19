// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
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
	f.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) { return "build-42/a:b", true, nil }, nil, nil)

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
			f.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) { return "abc", true, nil }, nil, nil)
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
	}, nil, nil)
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
	f.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) { return "", false, errors.New("db down") }, nil, nil)
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

// registryMasker is the mask the daemon hands EnrichRuns (cmd/wardynd runValueMasker).
func registryMasker(reg *secretmask.Registry) sinks.RunValueMasker {
	return sinks.Witnessed(func(runID uuid.UUID, b []byte) []byte { return reg.JSONVariantMasker(runID).Mask(b) },
		func(uuid.UUID) bool { return true }, reg.Generation)
}

// TestFanoutEnrichRuns_MasksTheIDWithTheRunsRegistryOnEveryEmit: the id comes from the run row after the
// recorder has masked the event, so it is masked here: against a global value, against the run's own
// value and not another run's, and, on a warm cache, against a value registered after the first emit.
func TestFanoutEnrichRuns_MasksTheIDWithTheRunsRegistryOnEveryEmit(t *testing.T) {
	const (
		globalSecret = "zq-throwaway-global-1"
		runSecret    = "zq-throwaway-run-2"
		lateSecret   = "zq-throwaway-late-3"
	)
	ids := map[uuid.UUID]string{}
	runA, runB, runC, runD := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ids[runA], ids[runB], ids[runC], ids[runD] = globalSecret, runSecret, "plain-ref/7", lateSecret

	reg := secretmask.NewRegistry()
	if err := reg.AddGlobal(0, "", "throwaway", time.Now(), []byte(globalSecret)); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(runB, []byte(runSecret)); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	sink := &lastSink{}
	f := sinks.NewFanout(sink)
	f.EnrichRuns(func(_ context.Context, id uuid.UUID) (string, bool, error) {
		reads.Add(1)
		return ids[id], true, nil
	}, registryMasker(reg), nil)
	emit := func(id uuid.UUID) string {
		t.Helper()
		if err := f.Emit(context.Background(), runEvent(&id, `{"k":1}`)); err != nil {
			t.Fatal(err)
		}
		got, _ := dataOf(t, sink.ev)[sinks.CorrelationKey].(string)
		return got
	}
	const hidden = "<secret-hidden>"

	if got := emit(runA); got != hidden {
		t.Errorf("a global value went out as %q", got)
	}
	if got := emit(runB); got != hidden {
		t.Errorf("the run's own value went out as %q", got)
	}
	if got := emit(runC); got != "plain-ref/7" {
		t.Errorf("an id no secret covers went out as %q", got)
	}
	// Another run's value is not this run's to hide.
	if err := reg.Add(runC, []byte("zq-throwaway-other-4")); err != nil {
		t.Fatal(err)
	}
	if got := emit(runC); got != "plain-ref/7" {
		t.Errorf("an unrelated registered value changed the id to %q", got)
	}
	// Warm cache, then the registry learns the value: the next emit hides it, with no second read.
	if got := emit(runD); got != lateSecret {
		t.Fatalf("before it was registered the id went out as %q", got)
	}
	before := reads.Load()
	if err := reg.Add(runD, []byte(lateSecret)); err != nil {
		t.Fatal(err)
	}
	if got := emit(runD); got != hidden {
		t.Errorf("a value registered after the cache was warm went out as %q", got)
	}
	if reads.Load() != before {
		t.Errorf("%d more reads; the cache should have served the raw id", reads.Load()-before)
	}
	if strings.Contains(string(sink.ev.Data), lateSecret) {
		t.Errorf("the sink's data still holds the secret: %s", sink.ev.Data)
	}
}
