// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestFanoutRecorderBatch_HeldRowsNotReEmitted_PG: the same batch recorded twice as a replay is appended once
// and sent to the sinks once, each row with the hashes the database computed; the second call notes every row
// held and emits nothing.
func TestFanoutRecorderBatch_HeldRowsNotReEmitted_PG(t *testing.T) {
	pool := pgPool(t)
	sink := &recordedSink{}
	rec := fanoutRecorder{primary: store.Recorder{Pool: pool}, fanout: sinks.NewFanout(sink)}
	evs := plantedRows(3)
	for attempt, want := range []audit.BatchStatus{audit.BatchAppended, audit.BatchHeld} {
		ctx, out := audit.WithBatchOutcome(store.WithAuditReplay(t.Context()))
		if err := rec.RecordBatch(ctx, evs); err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
		out.Release(ctx, true)
		for _, ev := range evs {
			if st := out.Status(ev.ID); st != want {
				t.Fatalf("attempt %d: event %s noted %d, want %d", attempt+1, ev.ID, st, want)
			}
		}
	}
	got := sink.got()
	if len(got) != len(evs) {
		t.Fatalf("the sinks got %d rows over two attempts, want each of %d once", len(got), len(evs))
	}
	for i, ev := range got {
		if ev.ID != evs[i].ID || ev.RowHash == "" {
			t.Fatalf("sink row %d = %s with row_hash %q, want %s with its hash", i, ev.ID, ev.RowHash, evs[i].ID)
		}
	}
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE run_id = $1`, evs[0].RunID).Scan(&n); err != nil || n != len(evs) {
		t.Fatalf("stored rows = %d (%v), want %d", n, err, len(evs))
	}
}
