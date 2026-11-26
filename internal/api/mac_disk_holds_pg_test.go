// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// holdOf is a run in state, holding drive in the table as dispatch writes it.
func (e *poolEnv) holdOf(d types.UserDrive, state types.RunState) uuid.UUID {
	e.t.Helper()
	run, err := e.pg.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: e.personP, Agent: "claude-code",
		ConfinementClass: types.CC2, State: state, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		e.t.Fatal(err)
	}
	if ok, err := e.pg.HoldMacDisk(context.Background(), d.ID, run.ID); err != nil || !ok {
		e.t.Fatalf("hold = %v, %v", ok, err)
	}
	return run.ID
}

// holdDoors runs the drive step of the create, Review and preview doors for the drive's owner.
func (e *poolEnv) holdDoors(d types.UserDrive, modes ...foldMode) map[foldMode]int {
	e.t.Helper()
	out := map[foldMode]int{}
	for _, mode := range modes {
		req := drivesRequest(d.ID.String())
		req.MachineKind = string(types.MachineMacOSVM)
		w := httptest.NewRecorder()
		f := &runFold{mode: mode, w: w, req: &req,
			r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(personCtx(e.personP))}
		if e.srv.stepDrive(f, &foldRecorder{}) {
			out[mode] = 0
			continue
		}
		if w.Code == http.StatusConflict && !strings.Contains(w.Body.String(), reasonDriveInAnotherRun) {
			e.t.Errorf("mode %d: 409 without the reason: %s", mode, w.Body)
		}
		out[mode] = w.Code
	}
	return out
}

// A create naming a disk drive another run that is not finished holds is 409 drive_in_another_run, and no run row is
// made; the same drive free of a hold, or held by a finished run, is not refused.
func TestMacDiskHold_CreateRefusedWhileHeld_PG(t *testing.T) {
	e := newLegacyEnv(t)
	mac := e.claimedRunner(e.personP)
	d := e.legacyDrive(e.personP, "Notes", types.DriveBackendMacDisk, &mac, false)
	runs := func() int { return e.count(`SELECT count(*) FROM agent_runs`) }

	if got := e.holdDoors(d, foldCreate)[foldCreate]; got != 0 {
		t.Fatalf("a free disk drive = %d, want it accepted", got)
	}
	holder := e.holdOf(d, types.RunRunning)
	before := runs()
	if got := e.holdDoors(d, foldCreate)[foldCreate]; got != http.StatusConflict {
		t.Fatalf("a drive a running run holds = %d, want 409", got)
	}
	if runs() != before {
		t.Error("a refused create made a run")
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_runs SET state = 'FAILED' WHERE id = $1`, holder); err != nil {
		t.Fatal(err)
	}
	if got := e.holdDoors(d, foldCreate)[foldCreate]; got != 0 {
		t.Errorf("a drive a failed run held = %d, want it accepted", got)
	}
}

// Review (preflight) and the policy preview refuse the same drive in the same way.
func TestMacDiskHold_PreflightRefusedWhileHeld_PG(t *testing.T) {
	e := newLegacyEnv(t)
	mac := e.claimedRunner(e.personP)
	d := e.legacyDrive(e.personP, "Notes", types.DriveBackendMacDisk, &mac, false)
	e.holdOf(d, types.RunStarting)
	for mode, code := range e.holdDoors(d, foldPreflight, foldPreview) {
		if code != http.StatusConflict {
			t.Errorf("mode %d = %d, want 409", mode, code)
		}
	}
}
