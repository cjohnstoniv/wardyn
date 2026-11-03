// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.correlation_id (migration 0254): the caller's reference for a
// run. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunCorrelationID_RoundTrips: the id survives create, read and list, and a run with none reads "".
func TestPG_RunCorrelationID_RoundTrips(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	for _, id := range []string{"", "build-42/a:b"} {
		r := newRun(types.RunRunning)
		r.CorrelationID = id
		created := persistRun(t, ctx, pool, r)
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if created.CorrelationID != id || got.CorrelationID != id {
			t.Errorf("a run created with %q read back as %q (create) and %q (get)", id, created.CorrelationID, got.CorrelationID)
		}
		found, ok, err := store.RunCorrelationID(ctx, pool, r.ID)
		if err != nil || !ok || found != id {
			t.Errorf("RunCorrelationID = %q, %v, %v; want %q", found, ok, err, id)
		}
	}
	if _, ok, err := store.RunCorrelationID(ctx, pool, uuid.New()); err != nil || ok {
		t.Errorf("an absent run: found %v, err %v; want found false", ok, err)
	}
}

// TestPG_RunCorrelationID_QueuedRunKeepsItThroughPromotion: a waiting run is written with its id, and the
// promotion that moves it to PENDING keeps it.
func TestPG_RunCorrelationID_QueuedRunKeepsItThroughPromotion(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 1)
	now := time.Now().UTC()
	r := poolRun(p, types.RunQueued, "alice")
	r.CorrelationID = "queued-1"
	if queued, err := pg.CreateQueuedRun(ctx, r, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0); err != nil || queued.CorrelationID != "queued-1" {
		t.Fatalf("queued run = %q, %v; want correlation id queued-1", queued.CorrelationID, err)
	}
	promoted, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll)
	if err != nil || len(promoted) != 1 || promoted[0].Run.CorrelationID != "queued-1" {
		t.Fatalf("promote = %+v, %v; want the one waiting run with its id", promoted, err)
	}
	if got := runState(t, pg, r.ID); got.State != types.RunPending || got.CorrelationID != "queued-1" {
		t.Errorf("promoted run is %s with id %q, want PENDING with queued-1", got.State, got.CorrelationID)
	}
}

type heldSink struct {
	mu sync.Mutex
	ev types.AuditEvent
}

func (*heldSink) Name() string { return "held" }
func (h *heldSink) Emit(_ context.Context, ev types.AuditEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ev = ev
	return nil
}

// TestPG_RunCorrelationID_SinkEnrichmentLeavesTheStoredRowAndItsChainAlone: a sink receives a run's row with
// data.correlation_id, while the row Postgres holds, and the hashes it computed, are exactly what they were
// before the fan-out.
func TestPG_RunCorrelationID_SinkEnrichmentLeavesTheStoredRowAndItsChainAlone(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	r := newRun(types.RunRunning)
	r.CorrelationID = "build-42"
	persistRun(t, ctx, pool, r)

	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), RunID: &r.ID, ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "run.create", Outcome: "success", Data: json.RawMessage(`{"agent":"claude-code"}`),
	}
	if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
		t.Fatal(err)
	}
	read := func() (data, prev, row string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT data::text, COALESCE(prev_hash,''), COALESCE(row_hash,'') FROM audit_events WHERE id = $1`, ev.ID).Scan(&data, &prev, &row); err != nil {
			t.Fatal(err)
		}
		return
	}
	dataBefore, prevBefore, rowBefore := read()

	sink := &heldSink{}
	fan := sinks.NewFanout(sink)
	fan.EnrichRuns(func(ctx context.Context, id uuid.UUID) (string, bool, error) {
		return store.RunCorrelationID(ctx, pool, id)
	})
	if err := fan.Emit(ctx, ev); err != nil {
		t.Fatal(err)
	}

	var sent map[string]any
	if err := json.Unmarshal(sink.ev.Data, &sent); err != nil || sent[sinks.CorrelationKey] != "build-42" || sent["agent"] != "claude-code" {
		t.Errorf("the sink's data = %s (%v); want agent and correlation_id build-42", sink.ev.Data, err)
	}
	if sink.ev.RowHash != rowBefore || sink.ev.PrevHash != prevBefore {
		t.Errorf("the sink's hashes = %q/%q, want the stored %q/%q", sink.ev.PrevHash, sink.ev.RowHash, prevBefore, rowBefore)
	}
	if dataAfter, prevAfter, rowAfter := read(); dataAfter != dataBefore || prevAfter != prevBefore || rowAfter != rowBefore {
		t.Errorf("the stored row changed: data %s -> %s, hashes %s/%s -> %s/%s", dataBefore, dataAfter, prevBefore, rowBefore, prevAfter, rowAfter)
	}
}
