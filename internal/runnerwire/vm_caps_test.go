// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// The drives bit is additive: caps from a runner older than it read as no drives, and a runner
// without it sends nothing new. The control plane sends a drive to a VM only when the bit is set.
func TestVMCapsDrivesIsAdditive(t *testing.T) {
	var old VMCaps
	if err := json.Unmarshal([]byte(`{"machine_type":"pc-q35-10.0","cpu_model":"Haswell-v4","qemu_version":"10.0.13","virtiofs":true}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Drives || !old.Virtiofs {
		t.Fatalf("an older runner's caps = %+v, want virtiofs and no drives", old)
	}
	without, err := json.Marshal(VMCaps{Virtiofs: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "drives") {
		t.Fatalf("caps without the bit carry it: %s", without)
	}
	with, err := json.Marshal(VMCaps{Virtiofs: true, Drives: true})
	if err != nil {
		t.Fatal(err)
	}
	var back VMCaps
	if err := json.Unmarshal(with, &back); err != nil || !back.Drives {
		t.Fatalf("round trip of %s = %+v, %v", with, back, err)
	}
}
