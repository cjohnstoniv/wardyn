// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Sign-ins in a VM's desktop (migrations 0230, 0231 and 0234) against a real Postgres. Guarded by WARDYN_TEST_PG.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_DesktopVMSignIns(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	run, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.DesktopVMSignIns(ctx, run.ID); err != nil || found {
		t.Fatalf("a run with no row = %v %v", found, err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, run.ID, "memory"); err != nil {
		t.Fatal(err)
	}
	// Migration 0231's default: a row written without a generation reads 0, so the first pause takes 1.
	if row, _, err := st.DesktopVMSignIns(ctx, run.ID); err != nil || row.PauseGeneration != 0 {
		t.Fatalf("a new row's pause generation = %d %v, want 0", row.PauseGeneration, err)
	}
	for want := int64(1); want <= 2; want++ {
		if gen, err := st.NextDesktopVMPauseGeneration(ctx, run.ID); err != nil || gen != want {
			t.Fatalf("next pause generation = %d %v, want %d", gen, err, want)
		}
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if ok, err := st.PauseDesktopVMAgent(ctx, run.ID, "alice", at, 1); err != nil || ok {
		t.Fatalf("a mark under a generation another pause has taken since = %v %v, want nothing changed", ok, err)
	}
	if ok, err := st.PauseDesktopVMAgent(ctx, run.ID, "alice", at, 2); err != nil || !ok {
		t.Fatalf("pause = %v %v", ok, err)
	}
	if ok, _ := st.PauseDesktopVMAgent(ctx, run.ID, "bob", at.Add(time.Second), 2); ok {
		t.Fatal("a second pause changed the row")
	}
	row, found, err := st.DesktopVMSignIns(ctx, run.ID)
	if err != nil || !found || row.Store != "memory" || row.PausedBy != "alice" || row.PausedAt == nil || !row.PausedAt.Equal(at) ||
		row.PauseGeneration != 2 {
		t.Fatalf("paused row = %+v %v %v", row, found, err)
	}
	if got, err := st.PausedDesktopVMAgents(ctx); err != nil || len(got) != 1 || got[0].RunID != run.ID || got[0].PausedBy != "alice" || !got[0].PausedAt.Equal(at) ||
		got[0].PauseGeneration != 2 {
		t.Fatalf("paused agents = %+v %v, want the one paused run at generation 2", got, err)
	}
	if _, _, ok, err := st.ResumeDesktopVMAgent(ctx, run.ID, at.Add(time.Second), 2); err != nil || ok {
		t.Fatalf("a resume of another pause = %v %v, want nothing cleared", ok, err)
	}
	if _, _, ok, err := st.ResumeDesktopVMAgent(ctx, run.ID, at, 1); err != nil || ok {
		t.Fatalf("a resume of another generation = %v %v, want nothing cleared", ok, err)
	}
	by, pausedAt, ok, err := st.ResumeDesktopVMAgent(ctx, run.ID, at, 2)
	if err != nil || !ok || by != "alice" || !pausedAt.Equal(at) {
		t.Fatalf("resume = %q %v %v %v, want the pause it cleared", by, pausedAt, ok, err)
	}
	if got, err := st.PausedDesktopVMAgents(ctx); err != nil || len(got) != 0 {
		t.Fatalf("paused agents after the resume = %+v %v, want none", got, err)
	}
	if _, _, ok, _ := st.ResumeDesktopVMAgent(ctx, run.ID, at, 2); ok {
		t.Fatal("a second resume saw the pause again")
	}
	if row, _, _ := st.DesktopVMSignIns(ctx, run.ID); row.PausedAt != nil || row.PausedBy != "" || row.PauseGeneration != 2 {
		t.Fatalf("resumed row = %+v, want no mark and the generation kept", row)
	}
	// A guest that boots again reports its store again: the mark goes, and the generation keeps counting.
	if err := st.RecordDesktopVMSignIns(ctx, run.ID, "memory"); err != nil {
		t.Fatal(err)
	}
	if gen, err := st.NextDesktopVMPauseGeneration(ctx, run.ID); err != nil || gen != 3 {
		t.Fatalf("next pause generation after the store was recorded again = %d %v, want 3", gen, err)
	}
	none, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, none.ID, "none"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.PauseDesktopVMAgent(ctx, none.ID, "alice", at, 0); err != nil || ok {
		t.Fatalf("pause on a store that keeps none = %v %v, want nothing changed", ok, err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, none.ID, "disk"); err == nil {
		t.Fatal("a store other than memory, run_disk or none was recorded")
	}

	// A macOS VM keeps its sign-ins on the run's disk: its row is recorded and its agent paused.
	mac, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, mac.ID, "run_disk"); err != nil {
		t.Fatalf("record run_disk: %v", err)
	}
	gen, err := st.NextDesktopVMPauseGeneration(ctx, mac.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.PauseDesktopVMAgent(ctx, mac.ID, "alice", at, gen); err != nil || !ok {
		t.Fatalf("pause on a run_disk store = %v %v, want marked", ok, err)
	}
	if row, _, err := st.DesktopVMSignIns(ctx, mac.ID); err != nil || row.Store != "run_disk" || row.PausedBy != "alice" {
		t.Fatalf("run_disk row = %+v %v", row, err)
	}
}
