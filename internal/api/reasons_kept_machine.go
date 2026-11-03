// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The kept-machine reasons: the same closed set and convention as reasons.go, in a file of their own
// beside the other kept-machine code. TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonKeptMachineSiteOff      = "kept_machine_site_off"      // macos_vm.allow_kept_machines is not true: no kept machines on this install
	reasonKeptMachineNotYourMac   = "kept_machine_not_your_mac"  // not a person, or no Mac of the caller's own outside other people's pools
	reasonKeptMachinePersonLimit  = "kept_machine_person_limit"  // the caller keeps macos_vm.max_kept_machines_per_person machines already
	reasonKeptMachineNotFound     = "kept_machine_not_found"     // no kept machine of the caller's with that id
	reasonKeptMachineInUse        = "kept_machine_in_use"        // a run holds the machine, so it can't be deleted
	reasonKeptMachineDeleting     = "kept_machine_deleting"      // a delete of the machine has started
	reasonKeptMachineDeleteFailed = "kept_machine_delete_failed" // the Mac did not delete the machine; it is kept
)
