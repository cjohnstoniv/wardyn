// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"time"

	"github.com/google/uuid"
)

// A macOS VM run's disk report says what its runner did with the run's directory on the Mac, which holds
// the VM's disk and its clone. State and Reason are fixed codes: the runner's own error text never leaves
// the Mac.
const (
	MacOSVMDiskRemoved      = "removed"       // the run directory is deleted
	MacOSVMDiskKept         = "kept"          // it is still on the Mac; the orphan sweep retries it
	MacOSVMDiskHelperAlive  = "helper_alive"  // kept: the VM's helper process outlived its kill
	MacOSVMDiskRemoveFailed = "remove_failed" // kept: deleting the directory failed
)

// MacOSVMDisk is a run's latest disk report. Reason is set only on MacOSVMDiskKept.
type MacOSVMDisk struct {
	RunID  uuid.UUID `json:"run_id"`
	State  string    `json:"state"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// MacOSVMDiskReporter is a substrate that keeps macOS VM disk reports (the Docker driver with macOS VMs).
// A report stays once read, so a teardown's reply and every later sweep reply can carry it.
type MacOSVMDiskReporter interface {
	// MacOSVMDisk is the report of the run behind the substrate's own ref.
	MacOSVMDisk(ref string) (MacOSVMDisk, bool)
	// MacOSVMDisks is every report the substrate still keeps.
	MacOSVMDisks() []MacOSVMDisk
}

// IsMacOSVMDiskReason reports whether code is a reason a kept report may carry.
func IsMacOSVMDiskReason(code string) bool {
	return code == MacOSVMDiskHelperAlive || code == MacOSVMDiskRemoveFailed
}
