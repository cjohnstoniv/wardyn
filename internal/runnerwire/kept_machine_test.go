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
