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

// carryForwardSiteMacOSVM keeps what the body leaves unsaid. A body that does not name the block keeps the stored
// block: an older client cannot name it, and its round trip must not drop the organisation's base-age limit. A body
// that names the block but not allow_background_watch keeps the stored switch, so an older client or a re-applied
// managed config cannot turn a stored false back into allowed. A JSON null decodes to nil, the same as an omitted
// key, and is carried the same way. A stored nil stays nil, which reads allowed.
func carryForwardSiteMacOSVM(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["macos_vm"] {
		cfg.MacOSVM = existing.MacOSVM
		return
	}
	if existing.MacOSVM == nil || existing.MacOSVM.AllowBackgroundWatch == nil {
		return
	}
	if cfg.MacOSVM == nil {
		cfg.MacOSVM = &types.SiteMacOSVM{}
	}
	if cfg.MacOSVM.AllowBackgroundWatch == nil {
		cfg.MacOSVM.AllowBackgroundWatch = existing.MacOSVM.AllowBackgroundWatch
	}
}
