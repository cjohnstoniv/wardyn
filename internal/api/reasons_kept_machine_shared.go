// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "fmt"

// The kept-machine reasons and sentences a run's create shares with the kept-machine routes, under the
// same names and values the routes declare them with, so the two meet as one definition.
const (
	reasonKeptMachineSiteOff     = "kept_machine_site_off"     // macos_vm.allow_kept_machines is off for this install
	reasonKeptMachineNotYourMac  = "kept_machine_not_your_mac" // not the caller's own Mac, in their own pool, for a run they start themselves
	reasonKeptMachineNotFound    = "kept_machine_not_found"    // resume: the caller has no machine by that id or name
	reasonKeptMachineInUse       = "kept_machine_in_use"       // resume: another run holds the machine
	reasonKeptMachineDeleting    = "kept_machine_deleting"     // resume: the machine is being deleted
	reasonKeptMachinePersonLimit = "kept_machine_person_limit" // the caller keeps as many machines as the site allows
)

func keptMachineNotFoundMsg(id string) string { return fmt.Sprintf("You have no kept machine %s.", id) }

func keptMachineDeletingMsg(name string) string {
	return fmt.Sprintf("Your machine %s is being deleted.", name)
}
