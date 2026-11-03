// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The refusals of a run that keeps or resumes a machine (run_kept_machine.go): a create or Review that
// asks to keep a macOS VM's machine, or to resume one, and the dispatch that inserts or claims it. The
// reasons the kept-machine routes share are in reasons_kept_machine_shared.go. The same closed set and the
// same convention as reasons.go; TestReasonDocsMatchReasonsGo reads this file too. Their sentences are in
// run_kept_machine_msgs.go.
const (
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
