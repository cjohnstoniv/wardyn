// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import "testing"

// The runner's backstop for a macOS VM run the organisation asked to record names no control a member
// does not have: the control plane does not ask a macOS VM to record, so this is reached only by a stale
// control plane or a requirement, and the sentence says the organisation asked.
func TestMacVMRecordingRefusalNamesNoControl(t *testing.T) {
	r := newMacRig(t)
	r.d.cfg.Record = true
	_, err := r.d.refuseMacVMSpec(macSpec())
	want := "A macOS VM sandbox's terminal isn't recorded in this release, and the organisation asked to record this run."
	if err == nil || err.Error() != want {
		t.Fatalf("refusal %v, want %q", err, want)
	}
}
