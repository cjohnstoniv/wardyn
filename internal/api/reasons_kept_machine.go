// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The kept-machine reasons: the same closed set and convention as reasons.go, in a file of their own
// beside the other kept-machine code. TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonKeptMachineSiteOff      = "kept_machine_site_off"      // macos_vm.allow_kept_machines is not true: no kept machines on this install
	reasonKeptMachineNotYourMac   = "kept_machine_not_your_mac"  // not a person, or no Mac of the caller's own outside other people's pools
	reasonKeptMachinePersonLimit  = "kept_machine_person_limit"  // the caller keeps macos_vm.max_kept_machines_per_person machines already
	reasonKeptMachineNotFound     = "kept_machine_not_found"     // no kept machine of the caller's with that id (a resume: that id or name)
	reasonKeptMachineInUse        = "kept_machine_in_use"        // a run holds the machine, so it can't be deleted or resumed
	reasonKeptMachineDeleting     = "kept_machine_deleting"      // a delete of the machine has started
	reasonKeptMachineDeleteFailed = "kept_machine_delete_failed" // the Mac refused the delete before removing anything; it is kept
	// The Mac's answer was lost or was not a refusal, so the machine may be gone already: the delete
	// stays pending, is retried until the Mac confirms it, and the machine can't be resumed meanwhile.
	reasonKeptMachineDeleteUnconfirmed = "kept_machine_delete_unconfirmed"
	// A run that keeps or resumes a machine (run_kept_machine.go): its create or Review, and the dispatch
	// that inserts or claims the machine.
	reasonKeptMachineInvalid          = "kept_machine_invalid"           // the request's machine is malformed: both keep and resume, fresh without resume, a bad name
	reasonKeptMachineInteractiveOnly  = "kept_machine_interactive_only"  // not an interactive macOS VM run
	reasonKeptMachineResidentSecrets  = "kept_machine_resident_secrets"  // the run would put a lasting credential inside the machine
	reasonKeptMachineWithWorkFolder   = "kept_machine_with_work_folder"  // keep_work_folder asked beside a kept machine
	reasonKeptMachineExpired          = "kept_machine_expired"           // resume: the machine passed its idle limit
	reasonKeptMachineBaseTooOld       = "kept_machine_base_too_old"      // resume: the machine's base is past max_base_age_days
	reasonKeptMachineOffline          = "kept_machine_offline"           // the Mac is offline, or not the one a resumed machine is kept on
	reasonKeptMachineRunnerTooOld     = "kept_machine_runner_too_old"    // the Mac's runner does not keep machines
	reasonKeptMachineMacLimit         = "kept_machine_mac_limit"         // another machine would pass the Mac's kept-disk limit
	reasonKeptMachineNameTaken        = "kept_machine_name_taken"        // the caller already keeps a machine of that name
	reasonKeptMachineUnavailable      = "kept_machine_unavailable"       // dispatch could not make or claim the machine (a store error)
	reasonKeptMachineFreshBase        = "kept_machine_fresh_base"        // a fresh start names a base other than its machine's own
	reasonKeptMachineResetUnconfirmed = "kept_machine_reset_unconfirmed" // a fresh start was asked of the Mac and never confirmed: only another fresh start may use the machine
)
