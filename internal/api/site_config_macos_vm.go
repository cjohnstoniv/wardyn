// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateSiteMacOSVM refuses a negative base-age limit (0 or absent is no limit) and a kept-machine limit outside
// its bounds (0 or absent reads the default).
func validateSiteMacOSVM(m *types.SiteMacOSVM) error {
	if m == nil {
		return nil
	}
	if m.MaxBaseAgeDays < 0 {
		return errors.New("macos_vm.max_base_age_days: must be 0 or more — use 0 for no limit")
	}
	if d := m.KeptMachineIdleDays; d < 0 || d > types.KeptMachineIdleDaysMax {
		//lint:ignore ST1005 the sentence the admin reads, as the site config refusal shows it
		return fmt.Errorf("A kept machine can sit unused for 1 to %d days before it is deleted. Set macos_vm.kept_machine_idle_days to a number from 1 to %d, or leave it out for %d.",
			types.KeptMachineIdleDaysMax, types.KeptMachineIdleDaysMax, types.KeptMachineIdleDaysDefault)
	}
	if n := m.MaxKeptMachinesPerPerson; n < 0 || n > types.MaxKeptMachinesPerPersonMax {
		//lint:ignore ST1005 the sentence the admin reads, as the site config refusal shows it
		return fmt.Errorf("A person can keep 1 to %d machines. Set macos_vm.max_kept_machines_per_person to a number from 1 to %d, or leave it out for %d.",
			types.MaxKeptMachinesPerPersonMax, types.MaxKeptMachinesPerPersonMax, types.MaxKeptMachinesPerPersonDefault)
	}
	return nil
}

// carryForwardSiteMacOSVM keeps what the body leaves unsaid. A body that does not name the block keeps the stored
// block: an older client cannot name it, and its round trip must not drop the organisation's base-age limit. A body
// that names the block but not allow_background_watch keeps the stored switch, so an older client or a re-applied
// managed config cannot turn a stored false back into allowed; allow_attended_window and allow_kept_machines are
// carried the same way, so the same save cannot turn a stored true off or a stored false on by silence. The two
// kept-machine limits are carried when the body leaves them out (0 reads as absent), so the same save cannot
// reset a stored limit to its default. A JSON null decodes to nil, the same as an omitted key, and is carried the
// same way. A stored nil stays nil: the watch reads allowed, the window and kept machines read off.
func carryForwardSiteMacOSVM(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["macos_vm"] {
		cfg.MacOSVM = existing.MacOSVM
		return
	}
	old := existing.MacOSVM
	if old == nil {
		return
	}
	if cfg.MacOSVM == nil && old.AllowBackgroundWatch == nil && old.AllowAttendedWindow == nil &&
		old.AllowKeptMachines == nil && old.KeptMachineIdleDays == 0 && old.MaxKeptMachinesPerPerson == 0 {
		return
	}
	if cfg.MacOSVM == nil {
		cfg.MacOSVM = &types.SiteMacOSVM{}
	}
	if cfg.MacOSVM.AllowBackgroundWatch == nil {
		cfg.MacOSVM.AllowBackgroundWatch = old.AllowBackgroundWatch
	}
	if cfg.MacOSVM.AllowAttendedWindow == nil {
		cfg.MacOSVM.AllowAttendedWindow = old.AllowAttendedWindow
	}
	if cfg.MacOSVM.AllowKeptMachines == nil {
		cfg.MacOSVM.AllowKeptMachines = old.AllowKeptMachines
	}
	if cfg.MacOSVM.KeptMachineIdleDays == 0 {
		cfg.MacOSVM.KeptMachineIdleDays = old.KeptMachineIdleDays
	}
	if cfg.MacOSVM.MaxKeptMachinesPerPerson == 0 {
		cfg.MacOSVM.MaxKeptMachinesPerPerson = old.MaxKeptMachinesPerPerson
	}
}
