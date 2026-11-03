// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fanOnly is the store-less end of fanoutRecorder: what the masking recorder hands on reaches the fan-out.
type fanOnly struct {
	fan  *sinks.Fanout
	seen []types.AuditEvent
}

func (f *fanOnly) Record(ctx context.Context, ev types.AuditEvent) error {
	f.seen = append(f.seen, ev)
	return f.fan.Emit(ctx, ev)
}

var _ audit.Recorder = (*fanOnly)(nil)

// TestCorrelationIDIsMaskedOnItsWayToTheSinks assembles the masking recorder, the fan-out with the
// daemon's own enrichment wiring and a capture sink: a correlation id that is a registered secret never
// reaches the sink, for a global value, a run's own value, and a value registered after the cache is warm,
// while the row the recorder passes on (what the store takes) carries no correlation id at all.
func TestCorrelationIDIsMaskedOnItsWayToTheSinks(t *testing.T) {
	const (
		globalSecret = "zq-throwaway-global-1"
		runSecret    = "zq-throwaway-run-2"
		lateSecret   = "zq-throwaway-late-3"
	)
	runG, runR, runL := uuid.New(), uuid.New(), uuid.New()
	ids := map[uuid.UUID]string{runG: globalSecret, runR: runSecret, runL: lateSecret}
	reg := secretmask.NewRegistry()
	if err := reg.AddGlobal(0, "", "throwaway", time.Now(), []byte(globalSecret)); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(runR, []byte(runSecret)); err != nil {
		t.Fatal(err)
	}
	capture := &captureSink{}
	fan := sinks.NewFanout(capture)
	fan.EnrichRuns(func(_ context.Context, id uuid.UUID) (string, bool, error) { return ids[id], true, nil }, runValueMasker(reg))
	held := &fanOnly{fan: fan}
	rec := maskingRecorder{inner: held, reg: reg}

	last := func(evs []types.AuditEvent) types.AuditEvent { return evs[len(evs)-1] }
	record := func(run uuid.UUID) types.AuditEvent {
		t.Helper()
		ev := types.AuditEvent{ID: uuid.New(), RunID: &run, ActorType: types.ActorSystem, Actor: "wardynd", Action: "run.create", Outcome: "success",
			Data: json.RawMessage(`{"agent":"claude-code"}`)}
		if err := rec.Record(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
		return last(capture.events())
	}
	correlation := func(ev types.AuditEvent) string {
		t.Helper()
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		got, _ := d["correlation_id"].(string)
		return got
	}
	const hidden = "<secret-hidden>"

	if got := correlation(record(runG)); got != hidden {
		t.Errorf("a global value reached the sink as %q", got)
	}
	if got := correlation(record(runR)); got != hidden {
		t.Errorf("the run's own value reached the sink as %q", got)
	}
	if got := correlation(record(runL)); got != lateSecret {
		t.Fatalf("before it was registered the id reached the sink as %q", got)
	}
	if err := reg.Add(runL, []byte(lateSecret)); err != nil {
		t.Fatal(err)
	}
	if got := correlation(record(runL)); got != hidden {
		t.Errorf("a value registered after the cache was warm reached the sink as %q", got)
	}
	for _, ev := range held.seen {
		if string(ev.Data) != `{"agent":"claude-code"}` {
			t.Errorf("the row handed to the store = %s; the id is stamped after it", ev.Data)
		}
	}
}
