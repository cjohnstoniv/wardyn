// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
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

// An erasure of the masking corpus on its own (fence first, then eviction) that lands while an emit is in
// flight must not let the raw id out: the masker the emit used may already be empty.
func TestEnrichRuns_AFenceDuringTheReadOrTheMaskingExportsNothing(t *testing.T) {
	const secret = "zq-throwaway-barrier-6"
	for _, tc := range []struct {
		name     string
		warm     bool
		blockIn  string // "read" or "mask"
		wantRead int32
	}{
		{"cold cache, erased during the read", false, "read", 1},
		{"warm cache, erased during the masking", true, "mask", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := uuid.New()
			reg := secretmask.NewRegistry()
			if err := reg.Add(run, []byte(secret)); err != nil {
				t.Fatal(err)
			}
			var covered atomic.Bool
			covered.Store(true)
			rows := &erasableRows{ids: map[uuid.UUID]string{run: secret}}
			inside, release := make(chan struct{}), make(chan struct{})
			var block atomic.Bool
			wait := func() {
				if block.Load() {
					close(inside)
					<-release
				}
			}
			sink := &lastEventSink{}
			f := NewFanout(sink)
			f.EnrichRuns(func(ctx context.Context, id uuid.UUID) (string, bool, error) {
				v, ok, err := rows.lookup(ctx, id) // the raw value is read before the erasure
				if tc.blockIn == "read" {
					wait()
				}
				return v, ok, err
			}, Witnessed(func(id uuid.UUID, b []byte) []byte {
				if tc.blockIn == "mask" {
					wait()
				}
				return reg.JSONVariantMasker(id).Mask(b) // read after the eviction below
			}, func(uuid.UUID) bool { return covered.Load() }, reg.Generation), func(uuid.UUID) bool { return covered.Load() })

			if tc.warm {
				emitFor(t, f, run)
				if got, _ := sink.correlation(t); got != "<secret-hidden>" {
					t.Fatalf("warm-up: %q", got)
				}
			}
			block.Store(true)
			done := make(chan struct{})
			go func() { defer close(done); emitFor(t, f, run) }()
			<-inside
			covered.Store(false) // the fence, durable first
			reg.Evict(run)       // then the corpus goes
			close(release)
			<-done

			if got, ok := sink.correlation(t); ok {
				t.Errorf("the sink got correlation_id %q after the fence", got)
			}
			sink.mu.Lock()
			defer sink.mu.Unlock()
			if strings.Contains(string(sink.ev.Data), secret) {
				t.Errorf("the raw value reached the sink: %s", sink.ev.Data)
			}
		})
	}
}

// Another replica's erasure reaches the registry as a dropped value before anything else tells the run's
// coverage: the drop guard has the coverage answer false first, so an emit in flight exports nothing.
func TestEnrichRuns_ADropAppliedFromAnotherReplicaUnholdsBeforeTheValueGoes(t *testing.T) {
	const secret = "zq-throwaway-replica-8"
	for _, global := range []bool{false, true} {
		name := map[bool]string{false: "a run value", true: "a process-wide value"}[global]
		t.Run(name, func(t *testing.T) {
			run, rowID := uuid.New(), uuid.New()
			reg := secretmask.NewRegistry()
			if global {
				reg.ApplyGlobal(rowID, "alice", "sso", []byte(secret), time.Time{}, time.Time{})
			} else if err := reg.Add(run, []byte(secret)); err != nil {
				t.Fatal(err)
			}
			var covered atomic.Bool
			covered.Store(true)
			reg.SetDropGuard(func(uuid.UUID) func() { covered.Store(false); return func() {} }) // what maskmanifest registers
			rows := &erasableRows{ids: map[uuid.UUID]string{run: secret}}
			inside, release := make(chan struct{}), make(chan struct{})
			sink := &lastEventSink{}
			f := NewFanout(sink)
			f.EnrichRuns(func(ctx context.Context, id uuid.UUID) (string, bool, error) {
				v, ok, err := rows.lookup(ctx, id)
				close(inside)
				<-release
				return v, ok, err
			}, Witnessed(func(id uuid.UUID, b []byte) []byte { return reg.JSONVariantMasker(id).Mask(b) },
				func(uuid.UUID) bool { return covered.Load() }, reg.Generation),
				func(uuid.UUID) bool { return covered.Load() })

			done := make(chan struct{})
			go func() { defer close(done); emitFor(t, f, run) }()
			<-inside
			if global {
				reg.DropGlobalValue(rowID) // the tombstone another replica committed, applied here
			} else {
				reg.DropRunValue(run, []byte(secret))
			}
			close(release)
			<-done

			sink.mu.Lock()
			defer sink.mu.Unlock()
			if strings.Contains(string(sink.ev.Data), secret) || strings.Contains(string(sink.ev.Data), CorrelationKey) {
				t.Errorf("the sink got %s after the drop", sink.ev.Data)
			}
		})
	}
}

// A run is evicted after the coverage check and before the id is masked (the masker reads an empty corpus),
// then reloaded and held again while the masking finishes. Each answer taken alone says covered; the
// registry's generation moved between them, so the id is left off.
func TestEnrichRuns_AnEvictionAndAReloadAroundTheMaskingExportNothing(t *testing.T) {
	const secret = "zq-throwaway-evict-reload-15"
	run := uuid.New()
	reg := secretmask.NewRegistry()
	if err := reg.Add(run, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	var covered atomic.Bool
	covered.Store(true)
	reg.SetDropGuard(func(uuid.UUID) func() { covered.Store(false); return func() {} })
	rows := &erasableRows{ids: map[uuid.UUID]string{run: secret}}
	inLookup, releaseLookup := make(chan struct{}), make(chan struct{})
	inMask, releaseMask := make(chan struct{}), make(chan struct{})
	sink := &lastEventSink{}
	f := NewFanout(sink)
	f.EnrichRuns(func(ctx context.Context, id uuid.UUID) (string, bool, error) {
		v, ok, err := rows.lookup(ctx, id)
		close(inLookup)
		<-releaseLookup
		return v, ok, err
	}, Witnessed(func(id uuid.UUID, b []byte) []byte {
		out := reg.JSONVariantMasker(id).Mask(b) // the corpus is empty here
		close(inMask)
		<-releaseMask
		return out
	}, func(uuid.UUID) bool { return covered.Load() }, reg.Generation),
		func(uuid.UUID) bool { return covered.Load() })

	done := make(chan struct{})
	go func() { defer close(done); emitFor(t, f, run) }()
	<-inLookup // the first coverage answer, true, is behind us
	reg.Evict(run)
	close(releaseLookup)
	<-inMask                                             // masked with nothing registered
	if err := reg.Add(run, []byte(secret)); err != nil { // the reload
		t.Fatal(err)
	}
	covered.Store(true) // and publishes the run held again
	close(releaseMask)
	<-done

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if strings.Contains(string(sink.ev.Data), secret) || strings.Contains(string(sink.ev.Data), CorrelationKey) {
		t.Errorf("the sink got %s", sink.ev.Data)
	}
}
