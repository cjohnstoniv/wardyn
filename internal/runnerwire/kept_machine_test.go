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
