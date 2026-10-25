// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// kindRun is a RUNNING local run of kind on rn.
func (f actionFixture) kindRun(t *testing.T, rn types.Runner, kind types.MachineKind) types.AgentRun {
	t.Helper()
	run := newRun(types.RunRunning)
	run.CreatedBy, run.Placement, run.RunnerID, run.EvidenceSource, run.MachineKind = rn.Owner, types.PlacementLocal, &rn.ID, types.RunEvidenceRunnerAsserted, kind
	run, err := f.st.CreateRun(t.Context(), run)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f actionFixture) disk(t *testing.T, runID uuid.UUID) (store.MacOSVMDisk, bool) {
	t.Helper()
	d, found, err := f.st.MacOSVMDisk(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return d, found
}

// A report is kept only from the runner the macOS VM run is placed on.
func TestPG_MacOSVMDiskBinding(t *testing.T) {
	f := newActionFixture(t)
	run := f.kindRun(t, f.rn, types.MachineMacOSVM)
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := f.st.RecordMacOSVMDisk(t.Context(), f.rn.ID, run.ID, store.MacOSVMDisk{State: "removed", At: at}); err != nil {
		t.Fatal(err)
	}
	if d, found := f.disk(t, run.ID); !found || d.State != "removed" || d.Reason != "" || !d.At.Equal(at) {
		t.Fatalf("report = %+v (%v), want removed at %v", d, found, at)
	}

	other := claimed(t, f.st, t.Context(), "other")
	run2 := f.kindRun(t, f.rn, types.MachineMacOSVM)
	if err := f.st.RecordMacOSVMDisk(t.Context(), other.ID, run2.ID, store.MacOSVMDisk{State: "removed", At: at}); err != nil {
		t.Fatal(err)
	}
	if d, found := f.disk(t, run2.ID); found {
		t.Fatalf("another runner's report was kept: %+v", d)
	}

	missing := uuid.New()
	if err := f.st.RecordMacOSVMDisk(t.Context(), f.rn.ID, missing, store.MacOSVMDisk{State: "removed", At: at}); err != nil {
		t.Fatalf("a run with no row = %v, want no error", err)
	}
	if _, found := f.disk(t, missing); found {
		t.Fatal("a report was written for a run that does not exist")
	}
}

// Kept moves to kept or removed; removed never moves again.
func TestPG_MacOSVMDiskMonotonic(t *testing.T) {
	f := newActionFixture(t)
	run := f.kindRun(t, f.rn, types.MachineMacOSVM)
	base := time.Now().UTC().Truncate(time.Microsecond)
	write := func(state, reason string, at time.Time) {
		t.Helper()
		if err := f.st.RecordMacOSVMDisk(t.Context(), f.rn.ID, run.ID, store.MacOSVMDisk{State: state, Reason: reason, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	write("kept", "helper_alive", base)
	write("kept", "remove_failed", base.Add(time.Second))
	if d, _ := f.disk(t, run.ID); d.State != "kept" || d.Reason != "remove_failed" {
		t.Fatalf("after the second kept: %+v, want kept/remove_failed", d)
	}
	removedAt := base.Add(2 * time.Second)
	write("removed", "", removedAt)
	write("kept", "helper_alive", base.Add(3*time.Second))
	if d, _ := f.disk(t, run.ID); d.State != "removed" || d.Reason != "" || !d.At.Equal(removedAt) {
		t.Fatalf("after a kept over removed: %+v, want removed at %v", d, removedAt)
	}
}

// A Linux VM run and a container run on the same runner take no report.
func TestPG_MacOSVMDiskOtherKinds(t *testing.T) {
	f := newActionFixture(t)
	for _, kind := range []types.MachineKind{types.MachineLinuxVM, ""} {
		run := f.kindRun(t, f.rn, kind)
		if err := f.st.RecordMacOSVMDisk(t.Context(), f.rn.ID, run.ID, store.MacOSVMDisk{State: "removed", At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if d, found := f.disk(t, run.ID); found {
			t.Fatalf("kind %q took a report: %+v", kind, d)
		}
	}
}

// The table refuses a state or reason outside the closed sets and a reason on removed, and a run's delete
// takes its report with it.
func TestPG_MacOSVMDiskTableRules(t *testing.T) {
	f := newActionFixture(t)
	run := f.kindRun(t, f.rn, types.MachineMacOSVM)
	for _, bad := range [][2]string{{"deleted", ""}, {"kept", "rm failed"}, {"removed", "helper_alive"}} {
		if _, err := f.pool.Exec(t.Context(), `INSERT INTO macos_vm_disks (run_id, state, reason, at) VALUES ($1, $2, $3, now())`, run.ID, bad[0], bad[1]); err == nil {
			t.Fatalf("the table took state %q reason %q", bad[0], bad[1])
		}
	}
	if err := f.st.RecordMacOSVMDisk(t.Context(), f.rn.ID, run.ID, store.MacOSVMDisk{State: "removed", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `DELETE FROM agent_runs WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if d, found := f.disk(t, run.ID); found {
		t.Fatalf("the report outlived its run: %+v", d)
	}
}

// Migration 0236 gives each saved non-empty finished layout without the ssh tile one, at its bottom, once.
func TestPG_MacOSVMDiskMigrationAddsTheSSHTile(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	rows := map[string]struct{ preset, layout string }{
		"plain@example.com":  {"finished", `[{"widget":"terminal","x":0,"y":0,"w":7,"h":8},{"widget":"identity","x":6,"y":8,"w":6,"h":4}]`},
		"empty@example.com":  {"finished", `[]`},
		"hasssh@example.com": {"finished", `[{"widget":"terminal","x":0,"y":0,"w":7,"h":8},{"widget":"ssh","x":6,"y":12,"w":6,"h":5}]`},
		"live@example.com":   {"live", `[{"widget":"terminal","x":0,"y":0,"w":8,"h":20}]`},
		// The layout PUT bounds y and h only from below, so a saved bottom can pass any SQL integer type.
		"tall@example.com": {"finished", `[{"widget":"terminal","x":0,"y":9223372036854775806,"w":4,"h":9223372036854775807}]`},
		"edge@example.com": {"finished", `[{"widget":"terminal","x":0,"y":2147483647,"w":4,"h":1}]`},
	}
	stamp := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	for principal, r := range rows {
		if _, err := f.pool.Exec(ctx, `INSERT INTO ui_run_layouts (principal, preset, layout, updated_at) VALUES ($1, $2, $3::jsonb, $4)`,
			principal, r.preset, r.layout, stamp); err != nil {
			t.Fatal(err)
		}
	}
	sql, err := os.ReadFile("../db/migrations/0236_macos_vm_disks.sql")
	if err != nil {
		t.Fatal(err)
	}
	type widget struct {
		Widget     string `json:"widget"`
		X, Y, W, H int
	}
	read := func(principal, preset string) ([]widget, time.Time) {
		t.Helper()
		var raw []byte
		var updated time.Time
		if err := f.pool.QueryRow(ctx, `SELECT layout, updated_at FROM ui_run_layouts WHERE principal = $1 AND preset = $2`, principal, preset).Scan(&raw, &updated); err != nil {
			t.Fatal(err)
		}
		var ws []widget
		if err := json.Unmarshal(raw, &ws); err != nil {
			t.Fatal(err)
		}
		return ws, updated
	}
	asStored := func(layout string) []widget {
		var ws []widget
		_ = json.Unmarshal([]byte(layout), &ws)
		return ws
	}
	for run := 1; run <= 2; run++ {
		if _, err := f.pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("run %d of the migration: %v", run, err)
		}
		plain, updated := read("plain@example.com", "finished")
		want := append(asStored(rows["plain@example.com"].layout), widget{Widget: "ssh", X: 0, Y: 12, W: 4, H: 5})
		if len(plain) != len(want) || !updated.Equal(stamp) {
			t.Fatalf("run %d: plain = %+v at %v, want %+v at %v", run, plain, updated, want, stamp)
		}
		for i := range want {
			if plain[i] != want[i] {
				t.Fatalf("run %d: plain = %+v, want %+v", run, plain, want)
			}
		}
		for _, principal := range []string{"tall@example.com", "edge@example.com"} {
			got, _ := read(principal, "finished")
			if n := len(got); n != 2 || got[1] != (widget{Widget: "ssh", X: 0, Y: 2147483642, W: 4, H: 5}) {
				t.Fatalf("run %d: %s = %+v, want the ssh tile at the capped bottom", run, principal, got)
			}
		}
		for _, k := range []struct{ principal, preset string }{
			{"empty@example.com", "finished"}, {"hasssh@example.com", "finished"}, {"live@example.com", "live"},
		} {
			got, _ := read(k.principal, k.preset)
			stored := asStored(rows[k.principal].layout)
			if len(got) != len(stored) {
				t.Fatalf("run %d: %s's %s layout = %+v, want it unchanged", run, k.principal, k.preset, got)
			}
		}
	}
}
