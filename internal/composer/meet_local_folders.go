// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import "github.com/cjohnstoniv/wardyn/internal/types"

// meetLocalFolders meets the run's local_folders walls with an overlay's: the stricter of the two. An overlay's
// omitted field inherits; only a cap above the base's is a widening.
func (m *meeter) meetLocalFolders(o *types.LocalFolderRules) {
	if o == nil {
		return
	}
	c := &m.out.Ceiling
	if o.WidensAsOverlay(c.LocalFolders) {
		m.widen("local_folders", "the base caps what a sync session from a person's computer carries, and an overlay can only lower that cap")
	}
	c.LocalFolders = c.LocalFolders.Meet(o)
}

// negativeLocalFolderCap reports an overlay cap below zero, which is no number of MiB.
func negativeLocalFolderCap(o *types.LocalFolderRules) bool {
	return o != nil && (o.MaxPushMiB < 0 || o.MaxPullMiB < 0)
}

// clampLocalFolders raises the proposal's local_folders walls to the ceiling's.
func clampLocalFolders(out *types.RunPolicySpec, ceiling *types.LocalFolderRules, warns []string) []string {
	if !out.LocalFolders.AtLeastAsStrict(ceiling) {
		warns = append(warns, "local_folders raised to the ceiling's walls")
		out.LocalFolders = out.LocalFolders.Meet(ceiling)
	}
	return warns
}
