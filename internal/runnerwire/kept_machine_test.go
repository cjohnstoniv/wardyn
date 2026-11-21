// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The kept-machine caps and call keep the names the contract fixes, and a runner that keeps none sends
// neither key.
func TestKeptMachineWireNames(t *testing.T) {
	b, err := json.Marshal(MacVMCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "kept") {
		t.Fatalf("a zero MacVMCaps names kept machines: %s", b)
	}
	b, err = json.Marshal(MacVMCaps{KeptMachines: true, Kept: &MacVMKept{Machines: []MacVMKeptMachine{{ID: "m", InUse: true, Scrubbed: true, ExpiresAt: time.Unix(0, 0).UTC()}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"kept_machines":true`, `"kept":{`, `"limit_bytes"`, `"worst_bytes"`, `"allocated_bytes"`, `"machines":[`, `"in_use":true`, `"scrubbed":true`, `"expires_at"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("caps JSON lacks %s: %s", key, b)
		}
	}
	if MethodKeptMachine != "kept_machine" {
		t.Fatalf("MethodKeptMachine = %q", MethodKeptMachine)
	}
	b, err = json.Marshal(KeptMachineArgs{Op: KeptMachineOpDelete, ID: "m", By: "owner"})
	if err != nil || string(b) != `{"op":"delete","id":"m","by":"owner"}` {
		t.Fatalf("args JSON %s, %v", b, err)
	}
	b, err = json.Marshal(KeptMachineResult{Outcome: KeptMachineAlreadyAbsent})
	if err != nil || string(b) != `{"outcome":"already_absent"}` {
		t.Fatalf("result JSON %s, %v", b, err)
	}
}

// A kept machine in the inventory names the run booted from it, and a machine no run holds names none:
// the org compares that run before it releases a machine, so the id survives the round trip.
func TestKeptMachineInventoryCarriesItsRun(t *testing.T) {
	in := MacVMKept{Machines: []MacVMKeptMachine{
		{ID: "a", InUse: true, RunID: "11111111-1111-1111-1111-111111111111", ExpiresAt: time.Unix(0, 0).UTC()},
		{ID: "b", ExpiresAt: time.Unix(0, 0).UTC()},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"run_id":"11111111-1111-1111-1111-111111111111"`) || strings.Count(string(b), `"run_id"`) != 1 {
		t.Fatalf("inventory JSON %s", b)
	}
	var out MacVMKept
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Machines[0].RunID != in.Machines[0].RunID || out.Machines[1].RunID != "" {
		t.Fatalf("round trip %+v", out.Machines)
	}
}

// An idle report settles a run's use only when that run's stop wrote it: an older stop's report, one a runner
// reused after a failed read, one from a runner that names no stop, and one of a machine in use settle nothing.
func TestKeptMachineObservesStopOf(t *testing.T) {
	const older, current, later = "run-1", "run-2", "run-3"
	for name, tc := range map[string]struct {
		report MacVMKeptMachine
		run    string
		want   bool
	}{
		"the current run's own stop":                   {MacVMKeptMachine{StopRunID: current}, current, true},
		"an older cached idle report":                  {MacVMKeptMachine{StopRunID: older}, current, false},
		"a later completed use":                        {MacVMKeptMachine{StopRunID: current}, later, false},
		"an unversioned report from an older runner":   {MacVMKeptMachine{}, current, false},
		"the stop of the run, but the machine is busy": {MacVMKeptMachine{StopRunID: current, InUse: true}, current, false},
		"no run asked":                                 {MacVMKeptMachine{}, "", false},
	} {
		if got := tc.report.ObservesStopOf(tc.run); got != tc.want {
			t.Errorf("%s: ObservesStopOf = %v, want %v", name, got, tc.want)
		}
	}
	b, err := json.Marshal(MacVMKeptMachine{ID: "m", StopRunID: current})
	if err != nil || !strings.Contains(string(b), `"stop_run_id":"run-2"`) {
		t.Fatalf("inventory JSON %s, %v", b, err)
	}
	if b, _ = json.Marshal(MacVMKeptMachine{ID: "m"}); strings.Contains(string(b), "stop_run_id") {
		t.Fatalf("an unversioned report names a stop: %s", b)
	}
	if KeptMachineByExpiry != "expiry" || KeptMachineOpDelete != "delete" || KeptMachineOpDeleteExpired != "delete_expired" {
		t.Fatalf("kept_machine names %q %q %q", KeptMachineByExpiry, KeptMachineOpDelete, KeptMachineOpDeleteExpired)
	}
	b, _ = json.Marshal(MacVMCaps{KeptMachines: true, KeptConditionalDelete: true})
	if !strings.Contains(string(b), `"kept_conditional_delete":true`) {
		t.Fatalf("caps JSON %s", b)
	}
	if b, _ = json.Marshal(MacVMCaps{KeptMachines: true}); strings.Contains(string(b), "kept_conditional_delete") {
		t.Fatalf("an older runner's caps name the conditional delete: %s", b)
	}
}
