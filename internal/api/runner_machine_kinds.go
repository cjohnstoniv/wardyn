// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// machineKindDenialsOf is the stored runners.machine_kind_denials, [] when none, so the settings routes
// always state it.
func machineKindDenialsOf(cfg types.SiteConfig) []types.MachineKindDenial {
	if cfg.Runners == nil || cfg.Runners.MachineKindDenials == nil {
		return []types.MachineKindDenial{}
	}
	return slices.Clone(cfg.Runners.MachineKindDenials)
}
