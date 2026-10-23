// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Sign-ins in a VM's desktop (migration 0229) against a real Postgres. Guarded by WARDYN_TEST_PG.
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
	at := time.Now().UTC().Truncate(time.Microsecond)
	if ok, err := st.PauseDesktopVMAgent(ctx, run.ID, "alice", at); err != nil || !ok {
		t.Fatalf("pause = %v %v", ok, err)
	}
	if ok, _ := st.PauseDesktopVMAgent(ctx, run.ID, "bob", at.Add(time.Second)); ok {
		t.Fatal("a second pause changed the row")
	}
	row, found, err := st.DesktopVMSignIns(ctx, run.ID)
	if err != nil || !found || row.Store != "memory" || row.PausedBy != "alice" || row.PausedAt == nil || !row.PausedAt.Equal(at) {
		t.Fatalf("paused row = %+v %v %v", row, found, err)
	}
	by, pausedAt, ok, err := st.ResumeDesktopVMAgent(ctx, run.ID)
	if err != nil || !ok || by != "alice" || !pausedAt.Equal(at) {
		t.Fatalf("resume = %q %v %v %v, want the pause it cleared", by, pausedAt, ok, err)
	}
	if _, _, ok, _ := st.ResumeDesktopVMAgent(ctx, run.ID); ok {
		t.Fatal("a second resume saw the pause again")
	}
	if row, _, _ := st.DesktopVMSignIns(ctx, run.ID); row.PausedAt != nil || row.PausedBy != "" {
		t.Fatalf("resumed row = %+v", row)
	}
	none, err := st.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, none.ID, "none"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.PauseDesktopVMAgent(ctx, none.ID, "alice", at); err != nil || ok {
		t.Fatalf("pause on a store that keeps none = %v %v, want nothing changed", ok, err)
	}
	if err := st.RecordDesktopVMSignIns(ctx, none.ID, "disk"); err == nil {
		t.Fatal("a store other than memory or none was recorded")
	}
}
