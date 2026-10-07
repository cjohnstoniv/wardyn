// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func queuedRecordingClaim(t *testing.T, pg store.RunOutputStore, id uuid.UUID) store.RecordingOutputClaim {
	t.Helper()
	if err := pg.QueueRecordingRunOutput(t.Context(), id, 0, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	c, ok, err := pg.ClaimRecordingRunOutput(t.Context(), id, 0, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %+v %v %v", c, ok, err)
	}
	return c
}

func recordingRow(id uuid.UUID, text string) store.RunOutput {
	return store.RunOutput{RunID: id, Source: "recording", Output: []byte(text), Incomplete: true}
}

func TestPG_RecordingOutputGenerationAndClaimFence(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	id := persistRun(t, t.Context(), pool, newRun(types.RunCompleted)).ID
	old := queuedRecordingClaim(t, pg, id)
	if err := pg.QueueRecordingRunOutput(t.Context(), id, 0, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := pg.ClaimRecordingRunOutput(t.Context(), id, 0, time.Minute); err != nil || ok {
		t.Fatalf("second upload started parallel read: %v %v", ok, err)
	}
	if wrote, err := pg.SaveRecordingRunOutput(t.Context(), old, recordingRow(id, "old"), 0); err != nil || wrote {
		t.Fatalf("stale generation wrote: %v %v", wrote, err)
	}
	current, ok, err := pg.ClaimRecordingRunOutput(t.Context(), id, 0, time.Minute)
	if err != nil || !ok || current.Generation <= old.Generation {
		t.Fatalf("new generation: %+v %v %v", current, ok, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE run_output_recording_recovery SET claimed_at=now()-interval '2 minutes' WHERE run_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	successor, ok, err := pg.ClaimRecordingRunOutput(t.Context(), id, 0, time.Minute)
	if err != nil || !ok || successor.Generation != current.Generation || successor.Token == current.Token {
		t.Fatalf("takeover: %+v %v %v", successor, ok, err)
	}
	if wrote, err := pg.SaveRecordingRunOutput(t.Context(), current, recordingRow(id, "stale lease"), 0); err != nil || wrote {
		t.Fatalf("stale lease wrote: %v %v", wrote, err)
	}
	if wrote, err := pg.SaveRecordingRunOutput(t.Context(), successor, recordingRow(id, "latest"), 0); err != nil || !wrote {
		t.Fatalf("successor lost claim: %v %v", wrote, err)
	}
	row, found, err := pg.GetRunOutput(t.Context(), id)
	if err != nil || !found || string(row.Output) != "latest" || !row.Incomplete {
		t.Fatalf("stored %+v %v %v", row, found, err)
	}
	if pending, err := pg.ListPendingRecordingRunOutputs(t.Context(), time.Minute, 10); err != nil || len(pending) != 0 {
		t.Fatalf("completed backlog %v %v", pending, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE run_output_recording_recovery SET claimed_at=now()-interval '2 minutes' WHERE run_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := pg.QueueRecordingRunOutput(t.Context(), id, 0, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := pg.ClaimRecordingRunOutput(t.Context(), id, 0, time.Minute); err != nil || ok {
		t.Fatalf("finalization reread an unchanged good result: %v %v", ok, err)
	}
}

func TestPG_RecordingOutputCannotReplaceIndependentFinalOrLoseBetterRecording(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	for _, source := range []string{"stdout", "pane_snapshot"} {
		id := persistRun(t, t.Context(), pool, newRun(types.RunCompleted)).ID
		claim := queuedRecordingClaim(t, pg, id)
		if err := pg.SaveFinalRunOutput(t.Context(), store.RunOutput{RunID: id, Source: source, Output: []byte("direct")}); err != nil {
			t.Fatal(err)
		}
		if wrote, err := pg.SaveRecordingRunOutput(t.Context(), claim, recordingRow(id, "recorded"), 0); err != nil || wrote {
			t.Fatalf("replaced %s: %v %v", source, wrote, err)
		}
		if err := pg.SaveFinalRunOutput(t.Context(), store.RunOutput{RunID: id, Source: "stdout", Output: []byte("late finalizer")}); err != nil {
			t.Fatal(err)
		}
		row, _, err := pg.GetRunOutput(t.Context(), id)
		if err != nil || string(row.Output) != "direct" || row.Source != source {
			t.Fatalf("changed final row %+v %v", row, err)
		}
	}
	id := persistRun(t, t.Context(), pool, newRun(types.RunCompleted)).ID
	claim := queuedRecordingClaim(t, pg, id)
	if wrote, err := pg.SaveRecordingRunOutput(t.Context(), claim, recordingRow(id, "kept recording"), 0); err != nil || !wrote {
		t.Fatalf("initial row: %v %v", wrote, err)
	}
	claim = queuedRecordingClaim(t, pg, id)
	gap := store.RunOutput{RunID: id, Source: "recording", CaptureGap: true}
	if wrote, err := pg.SaveRecordingRunOutput(t.Context(), claim, gap, 0); err != nil || wrote {
		t.Fatalf("gap replaced prior result: %v %v", wrote, err)
	}
	if err := pg.SaveFinalRunOutput(t.Context(), store.RunOutput{RunID: id, Source: "stdout", Output: []byte("obsolete stdout")}); err != nil {
		t.Fatal(err)
	}
	if row, _, err := pg.GetRunOutput(t.Context(), id); err != nil || string(row.Output) != "kept recording" {
		t.Fatalf("kept row %+v %v", row, err)
	}
}

func TestPG_RecordingOutputErasureIsSourceSpecificAndSurvivesRetentionAndRunDeletion(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run := persistRun(t, t.Context(), pool, newRun(types.RunCompleted))
	claim := queuedRecordingClaim(t, pg, run.ID)
	if wrote, err := pg.SaveRecordingRunOutput(t.Context(), claim, recordingRow(run.ID, "recording"), 0); err != nil || !wrote {
		t.Fatalf("setup %v %v", wrote, err)
	}
	late := queuedRecordingClaim(t, pg, run.ID)
	ids := []uuid.UUID{run.ID, run.ID}
	for _, source := range []string{"stdout", "pane_snapshot"} {
		id := persistRun(t, t.Context(), pool, newRun(types.RunCompleted)).ID
		ids = append(ids, id)
		if err := pg.SaveFinalRunOutput(t.Context(), store.RunOutput{RunID: id, Source: source, Output: []byte(source)}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := pg.EraseRecordingRunOutputs(t.Context(), ids); err != nil || n != 1 {
		t.Fatalf("erased %d %v", n, err)
	}
	if wrote, err := pg.SaveRecordingRunOutput(t.Context(), late, recordingRow(run.ID, "open reader"), 0); !errors.Is(err, store.ErrRecordingOutputErased) || wrote {
		t.Fatalf("late read resurrected: %v %v", wrote, err)
	}
	if err := pg.QueueRecordingRunOutput(t.Context(), run.ID, 0, 0, true); !errors.Is(err, store.ErrRecordingOutputErased) {
		t.Fatalf("late queue: %v", err)
	}
	for _, id := range ids[2:] {
		row, found, err := pg.GetRunOutput(t.Context(), id)
		if err != nil || !found || !row.RecordingErased || string(row.Output) != row.Source {
			t.Fatalf("independent source erased: %+v %v %v", row, found, err)
		}
	}
	if _, err := pg.DeleteRunOutputsOlderThan(t.Context(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM agent_runs WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	row, found, err := pg.GetRunOutput(t.Context(), run.ID)
	if err != nil || found || !row.RecordingErased {
		t.Fatalf("durable fence lost: %+v %v %v", row, found, err)
	}
	if _, ok, err := pg.ClaimRecordingRunOutput(t.Context(), run.ID, 0, 0); ok || !errors.Is(err, store.ErrRecordingOutputErased) {
		t.Fatalf("reused ID claim %v %v", ok, err)
	}
}

func TestPG_RecordingOutputRetentionAndGeneralErasureRecheckedAtCommit(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	for _, mode := range []string{"expired", "general erasure"} {
		id := persistRun(t, t.Context(), pool, newRun(types.RunCompleted)).ID
		claim := queuedRecordingClaim(t, pg, id)
		if mode == "expired" {
			if _, err := pool.Exec(t.Context(), `UPDATE agent_runs SET ended_at=now()-interval '2 days' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		} else if err := pg.EraseRunOutputs(t.Context(), []uuid.UUID{id}); err != nil {
			t.Fatal(err)
		}
		wrote, err := pg.SaveRecordingRunOutput(t.Context(), claim, recordingRow(id, "too late"), 24*time.Hour)
		if wrote || mode == "expired" && err != nil || mode == "general erasure" && !errors.Is(err, store.ErrRunOutputErased) {
			t.Fatalf("%s wrote=%v err=%v", mode, wrote, err)
		}
		var n int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM run_outputs WHERE run_id=$1`, id).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s kept %d %v", mode, n, err)
		}
		if ids, err := pg.ListPendingRecordingRunOutputs(t.Context(), time.Minute, 10); err != nil || len(ids) != 0 {
			t.Fatalf("%s still crowds the backlog: %v %v", mode, ids, err)
		}
	}
}

func TestPG_RecordingOutputOnlyTerminalBacklogIsBounded(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	live := persistRun(t, t.Context(), pool, newRun(types.RunRunning)).ID
	for _, state := range []types.RunState{types.RunCompleted, types.RunFailed} {
		id := persistRun(t, t.Context(), pool, newRun(state)).ID
		if err := pg.QueueRecordingRunOutput(t.Context(), id, 0, time.Minute, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := pg.QueueRecordingRunOutput(t.Context(), live, 0, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := pg.ClaimRecordingRunOutput(t.Context(), live, 0, time.Minute); err != nil || claimed {
		t.Fatalf("live capture claimed: %v %v", claimed, err)
	}
	ids, err := pg.ListPendingRecordingRunOutputs(t.Context(), time.Minute, 1)
	if err != nil || len(ids) != 1 || ids[0] == live {
		t.Fatalf("bounded terminal page %v %v", ids, err)
	}
}
