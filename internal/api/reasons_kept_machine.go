// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The kept-machine refusals (run_kept_machine.go): a create or Review that asks to keep a macOS VM's
// machine, or to resume one, and the dispatch that inserts or claims it. The same closed set and the
// same convention as reasons.go; TestReasonDocsMatchReasonsGo reads this file too. Their sentences are in
// run_kept_machine_msgs.go.
const (
	reasonKeptMachineInvalid         = "kept_machine_invalid"          // the request's machine is malformed: both keep and resume, fresh without resume, a bad name
	reasonKeptMachineSiteOff         = "kept_machine_site_off"         // macos_vm.allow_kept_machines is off for this install
	reasonKeptMachineInteractiveOnly = "kept_machine_interactive_only" // not an interactive macOS VM run
	reasonKeptMachineNotYourMac      = "kept_machine_not_your_mac"     // not the caller's own Mac, in their own pool, for a run they start themselves
	reasonKeptMachineResidentSecrets = "kept_machine_resident_secrets" // the run would put a lasting credential inside the machine
	reasonKeptMachineWithWorkFolder  = "kept_machine_with_work_folder" // keep_work_folder asked beside a kept machine
	reasonKeptMachineNotFound        = "kept_machine_not_found"        // resume: the caller has no machine by that id or name
	reasonKeptMachineInUse           = "kept_machine_in_use"           // resume: another run holds the machine
	reasonKeptMachineDeleting        = "kept_machine_deleting"         // resume: the machine is being deleted
	reasonKeptMachineExpired         = "kept_machine_expired"          // resume: the machine passed its idle limit
	reasonKeptMachineBaseTooOld      = "kept_machine_base_too_old"     // resume: the machine's base is past max_base_age_days
	reasonKeptMachineOffline         = "kept_machine_offline"          // the Mac is offline, or not the one a resumed machine is kept on
	reasonKeptMachineRunnerTooOld    = "kept_machine_runner_too_old"   // the Mac's runner does not keep machines
	reasonKeptMachinePersonLimit     = "kept_machine_person_limit"     // the caller keeps as many machines as the site allows
	reasonKeptMachineMacLimit        = "kept_machine_mac_limit"        // another machine would pass the Mac's kept-disk limit
	reasonKeptMachineNameTaken       = "kept_machine_name_taken"       // the caller already keeps a machine of that name
	reasonKeptMachineUnavailable     = "kept_machine_unavailable"      // dispatch could not make or claim the machine (a store error)
)
