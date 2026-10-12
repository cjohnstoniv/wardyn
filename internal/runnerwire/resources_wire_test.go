// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// TestSandboxResourcesWireGolden pins the JSON a CreateSandbox call carries for the sandbox's size.
// The soft-disk bit is DiskMiBSoft in Go and DiskMiBFilled on the wire: a runner and a control
// plane of different versions must still read each other's bit, so a rename of the member fails here.
func TestSandboxResourcesWireGolden(t *testing.T) {
	const golden = `{"CPUMillis":2000,"MemoryMiB":4096,"CPURequestMillis":500,"MemoryRequestMiB":1024,"PidsLimit":512,` +
		`"DiskMiB":8192,"DiskMiBFilled":true,"LifetimeCap":0}`
	want := runner.Resources{CPUMillis: 2000, MemoryMiB: 4096, CPURequestMillis: 500, MemoryRequestMiB: 1024, PidsLimit: 512, DiskMiB: 8192, DiskMiBSoft: true}
	got, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != golden {
		t.Fatalf("runner.Resources on the wire = %s\nwant %s", got, golden)
	}
	var back runner.Resources
	if err := json.Unmarshal([]byte(golden), &back); err != nil {
		t.Fatal(err)
	}
	if back != want {
		t.Fatalf("round trip = %+v, want %+v", back, want)
	}
	// And inside the call: a spec's resources ride CreateSandboxArgs under the same names.
	args, err := json.Marshal(CreateSandboxArgs{Spec: runner.SandboxSpec{Resources: want}})
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Spec struct{ Resources map[string]any }
	}
	if err := json.Unmarshal(args, &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Spec.Resources["DiskMiBFilled"] != true {
		t.Fatalf("CreateSandboxArgs does not carry DiskMiBFilled: %s", args)
	}
}
