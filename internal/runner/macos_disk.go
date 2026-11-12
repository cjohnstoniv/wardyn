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
	// MacOSVMDiskKeptMachine: kept on purpose; the folder is a person's kept machine, not a leftover.
	MacOSVMDiskKeptMachine = "kept_machine"
)

// MacOSVMDisk is a run's latest disk report. Reason is set only on MacOSVMDiskKept.
type MacOSVMDisk struct {
	RunID  uuid.UUID `json:"run_id"`
	State  string    `json:"state"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
	// Scrub is the stop scrub's outcome for a kept machine's session; nil for any other run.
	Scrub *KeptScrub `json:"scrub,omitempty"`
	// KeptExpiresAt is the expiry the runner wrote to the machine's record at this stop, the one deadline
	// the Mac deletes by; the organisation records it as the machine's expires_at. Zero for any other
	// report, or a runner that predates it (the organisation then counts the site's idle days from the stop).
	KeptExpiresAt time.Time `json:"kept_expires_at,omitzero"`
	// KeptExpiryPending is a current runner's word that the stop wrote no record (it could not lock, read or
	// write it, or the run no longer held the machine), so there is no deadline yet. The organisation records
	// no stop from it; the Mac's inventory settles the machine, with the expiry its own recovery gives it.
	KeptExpiryPending bool `json:"kept_expiry_pending,omitempty"`
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
	return code == MacOSVMDiskHelperAlive || code == MacOSVMDiskRemoveFailed || code == MacOSVMDiskKeptMachine
}
