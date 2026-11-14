// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A failed read of a gone replica's live chunks never becomes a plain gap row, which would delete them.
// The stale sweep's pass and its give-up for a run whose output waits on its recording
// (settleStaleRecordingOutput, reasons recording_over_budget and recording_erased) both reach the store
// only through writeGapRow, so after a failed read either one writes the row from the chunks, their
// bytes included, or, if the read fails again, leaves the pending row and the chunks for the next pass.
func TestMiscCovChunkSaveFailureKeepsTheChunks(t *testing.T) {
	pending := func(t *testing.T) (*outputFixture, *miscCovOutStore) {
		t.Helper()
		f, cs := miscCovOutFixture(t)
		if err := f.mem.InsertPendingRunOutput(t.Context(), f.run.ID); err != nil {
			t.Fatal(err)
		}
		return f, cs
	}
	kept := func(t *testing.T, f *outputFixture, cs *miscCovOutStore) {
		t.Helper()
		if r, ok := f.mem.row(f.run.ID); !ok || r.CapturedAt != nil || cs.gapCalls != 0 {
			t.Fatalf("row %+v (found %v), %d plain gap writes; want the pending row kept and no gap row", r, ok, cs.gapCalls)
		}
		if got := miscCovFinalizeData(t, f); len(got) != 0 {
			t.Fatalf("audit = %v, want nothing for a try that changed nothing", got)
		}
	}
	for _, next := range []string{"stale_pending", "recording_over_budget", "recording_erased"} {
		t.Run("an error then "+next+" writes the row from the chunks", func(t *testing.T) {
			f, cs := pending(t)
			cs.saveFn = func(call int) (bool, error) {
				if call == 1 {
					return false, errors.New("postgres is down")
				}
				// The chunks are still there on the next try, and the row is written from them.
				now := time.Now()
				f.mem.mu.Lock()
				f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", Output: []byte("before the crash\n"),
					CaptureGap: true, Incomplete: true, CapturedAt: &now, ClaimedAt: now}
				f.mem.mu.Unlock()
				return true, nil
			}
			f.srv.writeGapRow(t.Context(), cs, f.run.ID, "stale_pending")
			kept(t, f, cs)

			f.srv.writeGapRow(t.Context(), cs, f.run.ID, next)
			r, _ := f.mem.row(f.run.ID)
			if string(r.Output) != "before the crash\n" || !r.CaptureGap || !r.Incomplete || r.CapturedAt == nil || cs.gapCalls != 0 {
				t.Errorf("after the next try: row %+v, %d plain gap writes; want the row from the chunks and no plain gap", r, cs.gapCalls)
			}
			if got := miscCovFinalizeData(t, f); len(got) != 1 || got[0]["from_chunks"] != true || got[0]["reason"] != next {
				t.Errorf("audit = %v, want one from_chunks row with reason %s", got, next)
			}
		})
	}
	t.Run("an error then a give-up whose read fails too keeps the chunks", func(t *testing.T) {
		f, cs := pending(t)
		cs.saveErr = errors.New("postgres is down")
		f.srv.writeGapRow(t.Context(), cs, f.run.ID, "stale_pending")
		f.srv.writeGapRow(t.Context(), cs, f.run.ID, "recording_over_budget")
		kept(t, f, cs)
	})
	t.Run("a run with no pending row gets one for the sweep", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.saveErr = errors.New("postgres is down")
		f.srv.writeGapRow(t.Context(), cs, f.run.ID, "stale_pending")
		kept(t, f, cs)
	})
}
