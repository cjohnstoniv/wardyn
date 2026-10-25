// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A macOS VM run's disk report end to end: the runner's stop reply carries it, the org's remote substrate
// hands it to runnerMacOSVMDisks, Postgres keeps it, and GET /api/v1/runs/{id} says it. Guarded by
// WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// kindRun is a run on the fixture's runner whose row says kind, leased, with its sandbox created and its report
// set on the runner's fake.
func (f *runnerLeaseFixture) kindRun(t *testing.T, kind types.MachineKind) types.AgentRun {
	t.Helper()
	run := f.localRun(t, 0)
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET machine_kind = $2 WHERE id = $1`, run.ID, string(kind)); err != nil {
		t.Fatal(err)
	}
	// As dispatch does, the run is leased to its runner before its sandbox is created.
	if _, err := f.srv.leaseRunOnRunner(f.ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := f.create(t, &run); err != nil {
		t.Fatal(err)
	}
	local := strings.TrimPrefix(run.SandboxRef, placement.RunnerRefPrefix(f.rn.ID.String()))
	f.fake.SetMacOSVMDisk(local, runner.MacOSVMDisk{RunID: run.ID, State: runner.MacOSVMDiskRemoved, At: time.Now().UTC()})
	return run
}

func (f *runnerLeaseFixture) getRun(t *testing.T, run types.AgentRun) map[string]json.RawMessage {
	t.Helper()
	w := do(t, f.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET run = %d %s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPG_MacOSVMDiskReportEndToEnd(t *testing.T) {
	f := newRunnerLeaseFixture(t)
	sub := f.srv.runnerEndpoint(f.rn.ID).sub

	mac := f.kindRun(t, types.MachineMacOSVM)
	if err := sub.StopSandbox(f.ctx, mac.SandboxRef); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT state FROM macos_vm_disks WHERE run_id = $1`, mac.ID).Scan(&state); err != nil || state != "removed" {
		t.Fatalf("the stored report = %q %v, want removed", state, err)
	}
	var disk struct {
		State string `json:"state"`
	}
	if raw, ok := f.getRun(t, mac)["macos_disk"]; !ok || json.Unmarshal(raw, &disk) != nil || disk.State != "removed" {
		t.Fatalf("GET macos_disk = %s, want state removed", raw)
	}

	linux := f.kindRun(t, types.MachineLinuxVM)
	if err := sub.StopSandbox(f.ctx, linux.SandboxRef); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT count(*) FROM macos_vm_disks WHERE run_id = $1`, linux.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a linux_vm run's stop stored %d reports (%v), want none", n, err)
	}
	if raw, ok := f.getRun(t, linux)["macos_disk"]; ok {
		t.Fatalf("a linux_vm run's GET carries macos_disk %s", raw)
	}
}
