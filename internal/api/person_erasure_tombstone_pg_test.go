// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPersonErasureReachesADeletedRunsRecordingPG: the run history retention sweep deletes a run and keeps its
// recording. Erasing the run's creator still deletes that recording, found through the run's tombstone, as well
// as the recording of a run still kept, and then blanks the tombstone's creator while its unit stays.
func TestPersonErasureReachesADeletedRunsRecordingPG(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{Store: pg, RecordingStore: fs})

	const person = "erase-me@example.com"
	unit := uuid.New()
	mk := func(daysAgo int) uuid.UUID {
		t.Helper()
		now := time.Now().UTC()
		id := uuid.New()
		if _, err := pg.CreateRun(ctx, types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: person, UnitID: &unit,
			Agent: "claude-code", Repo: "octocat/Hello-World", Task: "erasure", ConfinementClass: types.CC2, State: types.RunRunning,
			SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker"}); err != nil {
			t.Fatalf("create run: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'COMPLETED', sandbox_ref = '', ended_at = now() - make_interval(days => $2) WHERE id = $1`,
			id, daysAgo); err != nil {
			t.Fatal(err)
		}
		saveOutputCast(t, fs, id, `{"version":2}`+"\n")
		return id
	}
	gone, kept := mk(40), mk(1)
	if n, err := pg.DeleteExpiredRuns(ctx, 30*24*time.Hour, 100); err != nil || n != 1 {
		t.Fatalf("DeleteExpiredRuns = %d, %v; want 1", n, err)
	}

	detail, err := srv.eraseRecordingsOf(ctx, person)
	if err != nil {
		t.Fatalf("eraseRecordingsOf: %v", err)
	}
	got, _ := detail.(map[string]any)
	if got["recordings"] != 2 || got["tombstoned_runs"] != 1 || got["runs_fenced"] != 2 {
		t.Fatalf("erasure detail = %v, want 2 recordings, 1 tombstoned run, 2 runs fenced", detail)
	}
	for _, id := range []uuid.UUID{gone, kept} {
		if _, err := fs.OpenCast(ctx, id.String()); !errors.Is(err, recording.ErrNotFound) {
			t.Errorf("run %s's recording after the erasure: %v, want not found", id, err)
		}
	}
	var by string
	var gotUnit *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT created_by, unit_id FROM run_history_tombstones WHERE run_id = $1`, gone).Scan(&by, &gotUnit); err != nil ||
		by != "" || gotUnit == nil || *gotUnit != unit {
		t.Fatalf("tombstone after the erasure = %q, unit %v, %v; want no creator and the unit kept", by, gotUnit, err)
	}
}
