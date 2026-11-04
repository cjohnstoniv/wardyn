// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
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
	fan.EnrichRuns(func(_ context.Context, id uuid.UUID) (string, bool, error) { return ids[id], true, nil }, runValueMasker(reg, func(uuid.UUID) bool { return true }), func(uuid.UUID) bool { return true })
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

// TestCorrelationIDIsOmittedOnceTheRunsMaskingIsFenced: the corpus is erased on its own (the mask_copies
// scope, not run_tasks), so the column still holds the raw value. The masking recorder labels the rows
// globals_only, and the sinks get no correlation id after that, whether the cache was cold or warm.
func TestCorrelationIDIsOmittedOnceTheRunsMaskingIsFenced(t *testing.T) {
	const secret = "zq-throwaway-fenced-5"
	warm, cold := uuid.New(), uuid.New()
	ids := map[uuid.UUID]string{warm: secret, cold: secret}
	reg := secretmask.NewRegistry()
	var heldMu sync.Mutex
	held := map[uuid.UUID]bool{}
	scope := &maskScope{}
	scope.arm(func(id uuid.UUID) bool { heldMu.Lock(); defer heldMu.Unlock(); return held[id] })
	setHeld := func(id uuid.UUID, v bool) { heldMu.Lock(); defer heldMu.Unlock(); held[id] = v }
	for _, id := range []uuid.UUID{warm, cold} {
		if err := reg.Add(id, []byte(secret)); err != nil {
			t.Fatal(err)
		}
		setHeld(id, true)
	}
	capture := &captureSink{}
	fan := sinks.NewFanout(capture)
	fan.EnrichRuns(func(_ context.Context, id uuid.UUID) (string, bool, error) { return ids[id], true, nil }, runValueMasker(reg, scope.covers), scope.covers)
	rec := maskingRecorder{inner: &fanOnly{fan: fan}, reg: reg, scope: scope}

	emit := func(run uuid.UUID, action string) map[string]any {
		t.Helper()
		ev := types.AuditEvent{ID: uuid.New(), RunID: &run, ActorType: types.ActorSystem, Actor: "wardynd", Action: action, Outcome: "success",
			Data: json.RawMessage(`{"agent":"claude-code"}`)}
		if err := rec.Record(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
		evs := capture.events()
		var d map[string]any
		if err := json.Unmarshal(evs[len(evs)-1].Data, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	if d := emit(warm, "run.create"); d["correlation_id"] != "<secret-hidden>" {
		t.Fatalf("while covered the id is masked: %v", d)
	}
	// mask_copies erased on its own: the corpus is gone and the run is no longer held; the column is not touched.
	for _, id := range []uuid.UUID{warm, cold} {
		reg.Evict(id)
		setHeld(id, false)
	}
	for name, run := range map[string]uuid.UUID{"warm cache": warm, "cold cache": cold} {
		d := emit(run, "run.kill")
		if _, ok := d["correlation_id"]; ok {
			t.Errorf("%s: run.kill reached the sink with a correlation id: %v", name, d)
		}
		if d["mask_scope"] != "globals_only" {
			t.Errorf("%s: the row is not labelled globals_only: %v", name, d)
		}
	}
	for _, ev := range capture.events() {
		if strings.Contains(string(ev.Data), secret) {
			t.Errorf("the raw value reached the sink: %s", ev.Data)
		}
	}
}
