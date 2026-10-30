// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateSiteMacOSVM refuses a negative base-age limit; 0 or absent is no limit.
func validateSiteMacOSVM(m *types.SiteMacOSVM) error {
	if m != nil && m.MaxBaseAgeDays < 0 {
		return errors.New("macos_vm.max_base_age_days: must be 0 or more — use 0 for no limit")
	}
	return nil
}

// carryForwardSiteMacOSVM keeps the stored block when the body does not name it: an older client cannot,
// and its round trip must not drop the organisation's base-age limit.
func carryForwardSiteMacOSVM(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["macos_vm"] {
		cfg.MacOSVM = existing.MacOSVM
	}
}
