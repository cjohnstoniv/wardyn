// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// oldMacVMCaps and oldMacVMBase are MacVMCaps as it was before the status codes: the shape an older
// organisation decodes a newer runner's caps into.
type oldMacVMCaps struct {
	Max          int          `json:"max"`
	MinCPUs      int          `json:"min_cpus,omitempty"`
	MinMemoryMiB int          `json:"min_memory_mib,omitempty"`
	Harnesses    []string     `json:"harnesses,omitempty"`
	Desktop      string       `json:"desktop,omitempty"`
	SignIns      bool         `json:"sign_ins,omitempty"`
	Base         oldMacVMBase `json:"base"`
	Refusal      string       `json:"refusal,omitempty"`
}

type oldMacVMBase struct {
	State    string `json:"state"`
	GuestOS  string `json:"guest_os,omitempty"`
	Progress int    `json:"progress,omitempty"`
	Error    string `json:"error,omitempty"`
}

// macVMGolden is what the caps with only the fields that existed before the status codes marshal to,
// captured from the struct before the codes were added.
const macVMGolden = `{"max":2,"min_cpus":4,"min_memory_mib":8192,"harnesses":["claude-code"],"desktop":"control","sign_ins":true,` +
	`"base":{"state":"preparing","guest_os":"macOS 27.0","progress":42,"error":"HTTP 500"},"refusal":"a refusal"}`

func goldenCaps() MacVMCaps {
	return MacVMCaps{Max: 2, MinCPUs: 4, MinMemoryMiB: 8192, Harnesses: []string{"claude-code"}, Desktop: MacVMDesktopControl, SignIns: true,
		Base:    MacVMBase{State: MacVMBasePreparing, GuestOS: "macOS 27.0", Progress: 42, Error: "HTTP 500"},
		Refusal: "a refusal"}
}

// A fully filled MacVMCaps survives a round trip, and its JSON keys are exactly the names the console
// reads.
func TestMacVMCapsStatusFieldsRoundTrip(t *testing.T) {
	c := MacVMCaps{Max: 1, MinCPUs: 4, MinMemoryMiB: 8192, Harnesses: []string{"h"}, Desktop: MacVMDesktopObserve, SignIns: true,
		Refusal: "r", Reason: MacVMReasonLowMemory, NoSession: true, HostOSVersion: "26.4", HostReason: MacVMHostHelperBuild,
		FreeGB: 3, NeedGB: 9,
		Base: MacVMBase{State: MacVMBasePreparing, GuestOS: "g", Progress: 5, Error: "e", Step: MacVMStepDownload, StepPercent: 40,
			Wait: MacVMWaitDownloadRetry, Failure: MacVMFailureTools, DownloadBytes: 1 << 30, ImageVersion: "27.0.1", BytesPerSec: 38e6}}
	b, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back MacVMCaps
	if err := Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, c) {
		t.Fatalf("round trip: %+v, want %+v", back, c)
	}
	var top, base map[string]json.RawMessage
	_ = json.Unmarshal(b, &top)
	_ = json.Unmarshal(top["base"], &base)
	keys := func(m map[string]json.RawMessage) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	wantTop := []string{"base", "desktop", "free_gb", "harnesses", "host_os_version", "host_reason", "max", "min_cpus", "min_memory_mib",
		"need_gb", "no_session", "reason", "refusal", "sign_ins"}
	wantBase := []string{"bytes_per_sec", "download_bytes", "error", "failure", "guest_os", "image_version", "progress", "state", "step",
		"step_percent", "wait"}
	if got := keys(top); !slices.Equal(got, wantTop) {
		t.Fatalf("caps keys %v, want %v", got, wantTop)
	}
	if got := keys(base); !slices.Equal(got, wantBase) {
		t.Fatalf("base keys %v, want %v", got, wantBase)
	}
}

// Caps older and newer than each other read each other without error and without loss.
func TestMacVMCapsOldAndNewCompatibility(t *testing.T) {
	// Today's fields marshal byte-identical to the golden.
	b, err := Marshal(goldenCaps())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != macVMGolden {
		t.Fatalf("old fields marshal to\n%s\nwant\n%s", b, macVMGolden)
	}
	// ...and so does the frozen old struct, which pins the golden to the shape it was captured from.
	ob, _ := json.Marshal(oldMacVMCaps{Max: 2, MinCPUs: 4, MinMemoryMiB: 8192, Harnesses: []string{"claude-code"}, Desktop: "control", SignIns: true,
		Base: oldMacVMBase{State: "preparing", GuestOS: "macOS 27.0", Progress: 42, Error: "HTTP 500"}, Refusal: "a refusal"})
	if string(ob) != macVMGolden {
		t.Fatalf("the frozen old struct marshals to\n%s", ob)
	}
	// The golden decodes into the new struct with every new field zero.
	var fromGolden MacVMCaps
	if err := Unmarshal([]byte(macVMGolden), &fromGolden); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromGolden, goldenCaps()) {
		t.Fatalf("golden decodes to %+v", fromGolden)
	}
	// A newer runner's caps decode into the frozen old struct, whose fields match.
	c := goldenCaps()
	c.Reason, c.NoSession, c.FreeGB = MacVMReasonDiskShort, true, 60
	c.Base.Step, c.Base.StepPercent, c.Base.Wait = MacVMStepDownload, 40, MacVMWaitBattery
	nb, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var old oldMacVMCaps
	if err := Unmarshal(nb, &old); err != nil {
		t.Fatalf("an older organisation decoding new caps: %v", err)
	}
	want := oldMacVMCaps{Max: 2, MinCPUs: 4, MinMemoryMiB: 8192, Harnesses: []string{"claude-code"}, Desktop: "control", SignIns: true,
		Base: oldMacVMBase{State: "preparing", GuestOS: "macOS 27.0", Progress: 42, Error: "HTTP 500"}, Refusal: "a refusal"}
	if !reflect.DeepEqual(old, want) {
		t.Fatalf("old struct reads %+v, want %+v", old, want)
	}
	// A code this build does not know decodes as itself and marshals back unchanged.
	var future MacVMCaps
	if err := Unmarshal([]byte(`{"max":0,"base":{"state":"preparing","step":"x"},"reason":"from_the_future"}`), &future); err != nil {
		t.Fatal(err)
	}
	if future.Reason != "from_the_future" || future.Base.Step != "x" {
		t.Fatalf("future codes decode to %+v", future)
	}
	again, _ := Marshal(future)
	if string(again) != `{"max":0,"base":{"state":"preparing","step":"x"},"reason":"from_the_future"}` {
		t.Fatalf("future codes marshal to %s", again)
	}
}
