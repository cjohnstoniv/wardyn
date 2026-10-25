// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import "github.com/cjohnstoniv/wardyn/internal/runner"

// TeardownResult is the reply of stop, kill and end from a runner of this release: the run's macOS VM
// disk report, when the runner keeps one. An older runner replies with no result, and an older org
// passes no result to decode into, so neither side sees the other's change.
type TeardownResult struct {
	MacOSVMDisk *runner.MacOSVMDisk `json:"macos_vm_disk,omitempty"`
}

// TeardownReply is what a runner answers a teardown verb for ref with: nil when sub keeps no report for it.
func TeardownReply(sub any, ref string) any {
	r, ok := sub.(runner.MacOSVMDiskReporter)
	if !ok {
		return nil
	}
	d, ok := r.MacOSVMDisk(ref)
	if !ok {
		return TeardownResult{}
	}
	return TeardownResult{MacOSVMDisk: &d}
}

// SweepDisks is the reports a sweep reply carries: every one sub keeps, nil when it keeps none.
func SweepDisks(sub any) []runner.MacOSVMDisk {
	if r, ok := sub.(runner.MacOSVMDiskReporter); ok {
		return r.MacOSVMDisks()
	}
	return nil
}
