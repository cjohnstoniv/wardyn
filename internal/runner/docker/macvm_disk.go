// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// macOSVMDiskKeep is how long the driver keeps a macOS VM run's disk report. Every sweep reply in that
// time carries it again, so a reply the org never read is made good by a later sweep.
const macOSVMDiskKeep = 6 * time.Hour

// recordMacOSVMDisk keeps runID's latest disk report: what its teardown, or a later sweep, did with the
// run's directory on this Mac. A removed report is final, so a later kept never replaces it. The load,
// compare and store are one compare-and-swap, so a teardown and a sweep racing on one run can't undo it.
func (d *Driver) recordMacOSVMDisk(runID uuid.UUID, state, reason string) {
	next := runner.MacOSVMDisk{RunID: runID, State: state, Reason: reason, At: time.Now().UTC()}
	for {
		old, loaded := d.macDisks.LoadOrStore(runID, next)
		if !loaded || old.(runner.MacOSVMDisk).State == runner.MacOSVMDiskRemoved || d.macDisks.CompareAndSwap(runID, old, next) {
			return
		}
	}
}

// MacOSVMDisk is runner.MacOSVMDiskReporter's: the report of the run behind ref, left in place.
func (d *Driver) MacOSVMDisk(ref string) (runner.MacOSVMDisk, bool) {
	id, err := runIDFromAgentName(ref)
	if err != nil {
		return runner.MacOSVMDisk{}, false
	}
	v, ok := d.macDisks.Load(id)
	if !ok {
		return runner.MacOSVMDisk{}, false
	}
	return v.(runner.MacOSVMDisk), true
}

// MacOSVMDisks is runner.MacOSVMDiskReporter's: every report younger than macOSVMDiskKeep. Older ones
// are forgotten here.
func (d *Driver) MacOSVMDisks() []runner.MacOSVMDisk {
	var out []runner.MacOSVMDisk
	cutoff := time.Now().Add(-macOSVMDiskKeep)
	d.macDisks.Range(func(k, v any) bool {
		if r := v.(runner.MacOSVMDisk); r.At.Before(cutoff) {
			d.macDisks.Delete(k)
		} else {
			out = append(out, r)
		}
		return true
	})
	return out
}

var _ runner.MacOSVMDiskReporter = (*Driver)(nil)
