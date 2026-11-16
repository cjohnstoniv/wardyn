// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"testing"
)

// A stale pending row whose recording cannot be read stays pending while the run ended recently, and becomes a
// capture gap, reason recording_unavailable, once the run ended longer ago than the give-up, on the database's
// clock. Catches: a recording that stays unreadable for good holding the row pending forever.
func TestRecordingOutputPG_StalePendingOnAnUnreadableRecordingEndsAsAGapAfterTheGiveUp(t *testing.T) {
	l := newMaskLab(t)
	run := l.stalePendingRecordingRun("unreadable-stale-secret-value")
	b := l.recordingReplica()
	b.srv.cfg.RecordingStore = &outputRecordingStore{Store: b.srv.cfg.RecordingStore, open: func(context.Context, string) (io.ReadCloser, error) {
		return nil, errors.New("input/output error")
	}}
	sweep := func() {
		t.Helper()
		// The recovery pass may report the unreadable recording as its error; the rows are what this pins.
		if err := b.srv.SweepRunOutputs(t.Context()); err != nil && !errors.Is(err, errRecordingOutputUnavailable) {
			t.Fatal(err)
		}
	}
	sweep()
	if l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NULL`, run.ID) != 1 ||
		l.finalizeReasonCount("recording_unavailable") != 0 {
		t.Fatal("an unreadable recording just after the run ended was settled; it must stay pending for a later try")
	}
	if _, err := l.pool.Exec(t.Context(), `UPDATE agent_runs SET ended_at = now() - interval '1 hour' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	// Past the claim's cooldown, so the next sweep may read again.
	if _, err := l.pool.Exec(t.Context(), `UPDATE run_output_recording_recovery SET claimed_at = now() - interval '10 minutes' WHERE run_id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	sweep()
	if l.count(staleRecordingGapRows, run.ID) != 1 {
		t.Fatal("a stale pending row on an unreadable recording past the give-up did not end as a capture gap")
	}
	if n := l.finalizeReasonCount("recording_unavailable"); n != 1 {
		t.Fatalf("run.output.finalize rows with reason recording_unavailable = %d, want 1", n)
	}
}
