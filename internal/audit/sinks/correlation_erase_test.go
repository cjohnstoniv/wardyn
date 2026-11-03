// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

type lastEventSink struct {
	mu sync.Mutex
	ev types.AuditEvent
}

func (*lastEventSink) Name() string { return "last" }
func (l *lastEventSink) Emit(_ context.Context, ev types.AuditEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = ev
	return nil
}

func (l *lastEventSink) correlation(t *testing.T) (string, bool) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var d map[string]any
	if err := json.Unmarshal(l.ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	v, ok := d[CorrelationKey].(string)
	return v, ok
}

// erasableRows is the run table as the lookup sees it: a person's erasure blanks a run's id.
type erasableRows struct {
	mu    sync.Mutex
	ids   map[uuid.UUID]string
	reads atomic.Int32
}

func (r *erasableRows) lookup(_ context.Context, id uuid.UUID) (string, bool, error) {
	r.reads.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.ids[id]
	return v, ok, nil
}

func (r *erasableRows) erase(ids ...uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		r.ids[id] = ""
	}
}

func emitFor(t *testing.T, f *Fanout, run uuid.UUID) {
	t.Helper()
	ev := types.AuditEvent{ID: uuid.New(), RunID: &run, Action: "run.create", Outcome: "success", Data: json.RawMessage(`{"k":1}`)}
	if err := f.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func TestForgetRuns_AWarmCacheStopsCarryingAnErasedID(t *testing.T) {
	erased, other := uuid.New(), uuid.New()
	rows := &erasableRows{ids: map[uuid.UUID]string{erased: "person-ref-1", other: "other-ref-2"}}
	sink := &lastEventSink{}
	f := NewFanout(sink)
	f.EnrichRuns(rows.lookup, nil, nil)

	emitFor(t, f, erased)
	emitFor(t, f, other)
	if got, _ := sink.correlation(t); got != "other-ref-2" {
		t.Fatalf("warm-up: %q", got)
	}
	rows.erase(erased)
	emitFor(t, f, erased)
	if got, ok := sink.correlation(t); !ok || got != "person-ref-1" {
		t.Fatalf("before forget the warm cache should still answer (%q, %v): this replica has not been told", got, ok)
	}

	f.ForgetRuns([]uuid.UUID{erased})
	emitFor(t, f, erased)
	if got, ok := sink.correlation(t); ok {
		t.Errorf("after the erasure the sink still got %q", got)
	}
	before := rows.reads.Load()
	emitFor(t, f, other)
	if got, _ := sink.correlation(t); got != "other-ref-2" || rows.reads.Load() != before {
		t.Errorf("another person's run: %q after %d more reads; its entry must stay warm", got, rows.reads.Load()-before)
	}
}

// A read that began before the erasure and finished after it may hold the id the erasure cleared; it must
// neither be cached nor reach the sink.
func TestForgetRuns_ARacingReadDoesNotRepopulateTheCache(t *testing.T) {
	run := uuid.New()
	rows := &erasableRows{ids: map[uuid.UUID]string{run: "person-ref-1"}}
	inRead, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sink := &lastEventSink{}
	f := NewFanout(sink)
	f.EnrichRuns(func(ctx context.Context, id uuid.UUID) (string, bool, error) {
		v, ok, err := rows.lookup(ctx, id) // reads the row before the erasure
		once.Do(func() { close(inRead); <-release })
		return v, ok, err
	}, nil, nil)

	done := make(chan struct{})
	go func() { defer close(done); emitFor(t, f, run) }()
	<-inRead
	rows.erase(run)
	f.ForgetRuns([]uuid.UUID{run})
	close(release)
	<-done
	if got, ok := sink.correlation(t); ok {
		t.Errorf("the racing read delivered %q after the erasure", got)
	}
	emitFor(t, f, run)
	if got, ok := sink.correlation(t); ok {
		t.Errorf("the next emit carried %q: the racing read repopulated the cache", got)
	}
}

func TestCorrelationCache_EntriesExpireAfterTheTTL(t *testing.T) {
	run := uuid.New()
	rows := &erasableRows{ids: map[uuid.UUID]string{run: "person-ref-1"}}
	sink := &lastEventSink{}
	f := NewFanout(sink)
	f.EnrichRuns(rows.lookup, nil, nil)
	clock := time.Now()
	f.enrich.now = func() time.Time { return clock }

	emitFor(t, f, run)
	clock = clock.Add(correlationCacheTTL - time.Second)
	rows.erase(run) // another replica's erasure: this one is not told
	emitFor(t, f, run)
	if got, _ := sink.correlation(t); got != "person-ref-1" || rows.reads.Load() != 1 {
		t.Fatalf("inside the TTL: %q after %d reads, want the cached id after 1", got, rows.reads.Load())
	}
	clock = clock.Add(2 * time.Second)
	emitFor(t, f, run)
	if got, ok := sink.correlation(t); ok || rows.reads.Load() != 2 {
		t.Errorf("past the TTL: %q (%v) after %d reads, want no id after a second read", got, ok, rows.reads.Load())
	}
}

func TestCorrelationCache_TheTTLClockStartsBeforeTheRead(t *testing.T) {
	run := uuid.New()
	rows := &erasableRows{ids: map[uuid.UUID]string{run: "person-ref-1"}}
	clock := time.Now()
	sink := &lastEventSink{}
	f := NewFanout(sink)
	f.EnrichRuns(func(ctx context.Context, id uuid.UUID) (string, bool, error) {
		v, ok, err := rows.lookup(ctx, id)
		clock = clock.Add(correlationCacheTTL + time.Second) // a read slower than the whole TTL
		return v, ok, err
	}, nil, nil)
	f.enrich.now = func() time.Time { return clock }

	emitFor(t, f, run)
	emitFor(t, f, run)
	if rows.reads.Load() != 2 {
		t.Errorf("%d reads; an entry whose read took longer than the TTL must already be stale", rows.reads.Load())
	}
}

func TestEnrichRuns_NoIDForARunTheCoverageGateDoesNotVouchFor(t *testing.T) {
	run := uuid.New()
	rows := &erasableRows{ids: map[uuid.UUID]string{run: "person-ref-1"}}
	var covered atomic.Bool
	sink := &lastEventSink{}
	f := NewFanout(sink)
	f.EnrichRuns(rows.lookup, nil, func(uuid.UUID) bool { return covered.Load() })

	emitFor(t, f, run) // cold, not covered
	if _, ok := sink.correlation(t); ok || rows.reads.Load() != 0 {
		t.Fatalf("an uncovered run: id sent %v after %d reads, want neither", ok, rows.reads.Load())
	}
	covered.Store(true)
	emitFor(t, f, run) // warms the cache
	if got, _ := sink.correlation(t); got != "person-ref-1" {
		t.Fatalf("a covered run: %q", got)
	}
	covered.Store(false) // fenced or erased: the warm entry must not be used
	emitFor(t, f, run)
	if _, ok := sink.correlation(t); ok {
		t.Error("a warm cache still sent the id after the run stopped being covered")
	}
}
