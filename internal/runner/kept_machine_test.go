// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"testing"
)

func TestIsMacOSVMDiskReasonAcceptsKeptMachine(t *testing.T) {
	for code, want := range map[string]bool{
		MacOSVMDiskHelperAlive:  true,
		MacOSVMDiskRemoveFailed: true,
		"kept_machine":          true,
		MacOSVMDiskRemoved:      false,
		MacOSVMDiskKept:         false,
		"":                      false,
	} {
		if got := IsMacOSVMDiskReason(code); got != want {
			t.Errorf("IsMacOSVMDiskReason(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestKeptScrubWireNames(t *testing.T) {
	b, err := json.Marshal(KeptScrub{When: "stop", Outcome: "done", Removed: map[string]int{"cas": 1}, CAs: []string{"ab"}})
	if err != nil || string(b) != `{"when":"stop","outcome":"done","removed":{"cas":1},"ca_sha256":["ab"]}` {
		t.Fatalf("KeptScrub JSON %s, %v", b, err)
	}
	b, err = json.Marshal(KeptScrub{When: "boot", Outcome: "not_run"})
	if err != nil || string(b) != `{"when":"boot","outcome":"not_run"}` {
		t.Fatalf("empty KeptScrub JSON %s, %v", b, err)
	}
	// Absent on every sandbox that is not a kept machine's.
	b, err = json.Marshal(MacOSVMDisk{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["scrub"]; ok {
		t.Fatalf("a zero MacOSVMDisk carries scrub: %s", b)
	}
	b, err = json.Marshal(SandboxSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["kept_machine"]; ok {
		t.Fatalf("a zero SandboxSpec carries kept_machine: %s", b)
	}
}
